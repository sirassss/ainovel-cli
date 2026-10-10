package imp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
	"github.com/voocel/ainovel-cli/internal/llmretry"
	"github.com/voocel/litellm"
)

// callModel is the kernel's minimal dependency on the model, so tests can inject a mock.
type callModel interface {
	Generate(ctx context.Context, messages []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error)
}

// errTruncated means the model stopped because of length (a capacity error). It carries the raw text so the caller can decide to fail or salvage a prefix (§9.5).
type errTruncated struct {
	Raw string
}

func (e *errTruncated) Error() string {
	return "Đầu ra của model bị cắt do giới hạn độ dài (stop=length)"
}

// errSemantic means an output-layer failure that cannot be repaired by re-asking; it carries the raw response
// so the runner can uniformly write a failures/ failure artifact (§14.2); shared by all semantic functions.
type errSemantic struct {
	Raw string
	Err error
}

func (e *errSemantic) Error() string { return e.Err.Error() }
func (e *errSemantic) Unwrap() error { return e.Err }

// callProfile carries thinking and observability options, derived from the ModelRuntime probed by the Host.
// The structured protocol is chosen independently by callStructured from model facts and the static Contract.
type callProfile struct {
	thinking agentcore.ThinkingLevel
	// notify is optional: echoes request backoff retries / validation re-asks to the UI; silent when nil (§14.1).
	// retryAt non-zero = deadline of the next retry; the UI renders a per-second countdown from it (events carry only the deadline; remaining time is computed at render time).
	notify func(msg string, retryAt time.Time)
	// progress is optional: echoes internal progress of long-running stages (segmentation chunk N/M, range summary N/M); silent when nil.
	// Segmentation/synthesis call the model chunk by chunk / range by range inside the function, and one chunk can take minutes; without it the panel stays silent for the whole stretch and looks hung (§14.1).
	progress func(current, total int, msg string)
	// log is optional: import-specific log (logs/import.log); falls back to the default logger when nil.
	log *slog.Logger
}

func (p callProfile) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// step echoes one ordinary progress item (internal progress of a long-running stage).
func (p callProfile) step(current, total int, format string, args ...any) {
	if p.progress != nil {
		p.progress(current, total, fmt.Sprintf(format, args...))
	}
}

// say echoes one long-call status. A retry can stay silent for several minutes (exponential backoff accumulates over 2 minutes),
// and without an echo the user would think it hung.
func (p callProfile) say(format string, args ...any) {
	p.sayRetry(time.Time{}, format, args...)
}

// sayRetry echoes a status carrying the retry deadline, for the UI countdown.
func (p callProfile) sayRetry(retryAt time.Time, format string, args ...any) {
	if p.notify != nil {
		p.notify(fmt.Sprintf(format, args...), retryAt)
	}
}

// snippet compresses multi-line text into a one-line short summary for UI echo: collapses whitespace and truncates to max runes.
func snippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// briefErr compresses an error into one line of short text for UI echo (the full error chain still goes to the log and failure artifacts).
// The adapter's structured facts come first: when truncating, preserve "what kind of error, what status code" first; the gateway message can be sacrificed.
func briefErr(err error) string {
	s := err.Error()
	if d := modelErrDetail(err); d != "" {
		s = d + ": " + s
	}
	return snippet(s, 100)
}

// errTypeLabels translates litellm error classifications into short labels readable at a glance.
var errTypeLabels = map[litellm.ErrorType]string{
	litellm.ErrorTypeAuth:            "lỗi xác thực",
	litellm.ErrorTypeRateLimit:       "bị giới hạn tốc độ",
	litellm.ErrorTypeNetwork:         "lỗi mạng",
	litellm.ErrorTypeValidation:      "tham số yêu cầu không hợp lệ",
	litellm.ErrorTypeProvider:        "lỗi dịch vụ upstream",
	litellm.ErrorTypeTimeout:         "hết thời gian",
	litellm.ErrorTypeQuota:           "không đủ hạn mức",
	litellm.ErrorTypeModel:           "model không khả dụng",
	litellm.ErrorTypeInternal:        "lỗi nội bộ",
	litellm.ErrorTypeContextOverflow: "vượt context window",
	litellm.ErrorTypeOverloaded:      "upstream quá tải",
	litellm.ErrorTypeContentFilter:   "bị bộ lọc nội dung chặn",
}

// modelErrDetail extracts the adapter's structured facts from the error chain (error class, HTTP status, provider, model).
// The gateway's message is often just a vague "Provider returned error", from which one cannot tell whether it is a config error,
// an upstream fault or rate limiting; litellm always carries these facts, they just do not enter the Error() text. The agentcore adapter's
// Unwrap explicitly allows callers that know litellm to retrieve the original error via errors.As. Returns an empty string for non-model-call errors.
func modelErrDetail(err error) string {
	var le *litellm.LiteLLMError
	if !errors.As(err, &le) {
		return ""
	}
	parts := make([]string, 0, 4)
	if label := errTypeLabels[le.Type]; label != "" {
		parts = append(parts, label)
	}
	if le.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", le.StatusCode))
	}
	if le.Provider != "" {
		parts = append(parts, le.Provider)
	}
	if le.Model != "" {
		parts = append(parts, le.Model)
	}
	return strings.Join(parts, ", ")
}

// callOptions assembles the CallOptions for this call: always carries the output limit; thinking is optional by capability.
// thinking is sent only when not Auto - sending any level (including off) to a model that does not support thinking is an invalid parameter (same policy as arbiter).
func (p callProfile) callOptions(maxTokens int) []agentcore.CallOption {
	opts := []agentcore.CallOption{agentcore.WithMaxTokens(maxTokens)}
	if p.thinking != agentcore.ThinkingAuto {
		opts = append(opts, agentcore.WithThinking(p.thinking))
	}
	return opts
}

// callStructured adapts the unified structured executor for the import layer and maps generic failures to import-artifact semantics.
func callStructured[T any](ctx context.Context, m callModel, contract llmcontract.Contract, systemPrompt, payload string, maxTokens int, prof callProfile, validate func(*T) error) (T, error) {
	out, err := llmcontract.Execute(ctx, m, llmcontract.Request[T]{
		Contract:     contract,
		SystemPrompt: systemPrompt,
		Payload:      payload,
		Options:      prof.callOptions(maxTokens),
		Validate:     validate,
		Agent:        "import",
		Hooks: llmcontract.Hooks{
			Resolved: func(res llmcontract.Resolution) {
				prof.logger().Debug("imp structured mode resolved",
					"contract", contract.Name, "structured_mode", res.Mode,
					"capability_source", res.Source, "provider", res.Provider,
					"model", res.Model, "schema_fingerprint", contract.Fingerprint())
			},
			RequestRetry: func(ev llmretry.Event) {
				prof.sayRetry(time.Now().Add(ev.Delay), "Gọi model lỗi (%s), thử lại lần %d", briefErr(ev.Err), ev.Attempt)
				prof.logger().Warn("imp retrying model request", "attempt", ev.Attempt, "delay", ev.Delay, "err", ev.Err)
			},
			Correction: func(ev llmcontract.Correction) {
				prof.say("Kiểm tra đầu ra không đạt (%s), hỏi lại lần %d kèm phản hồi lỗi", briefErr(ev.Err), ev.Attempt+1)
				prof.logger().Warn("imp structured output self-correction", "attempt", ev.Attempt,
					"layer", ev.Layer, "structured_mode", ev.Mode, "err", ev.Err)
			},
		},
	})
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	var failure *llmcontract.Failure
	if !errors.As(err, &failure) {
		return out, fmt.Errorf("imp: %w", err)
	}
	switch failure.Kind {
	case llmcontract.FailureLength:
		return out, &errTruncated{Raw: failure.Raw}
	case llmcontract.FailureSafety, llmcontract.FailureContract, llmcontract.FailureProtocol:
		if failure.Raw != "" {
			return out, &errSemantic{Raw: failure.Raw, Err: fmt.Errorf("imp: %w", failure)}
		}
	case llmcontract.FailureRequest:
		if detail := modelErrDetail(failure); detail != "" {
			return out, fmt.Errorf("imp: không gọi được model (%s): %w", detail, failure)
		}
	}
	return out, fmt.Errorf("imp: %w", failure)
}
