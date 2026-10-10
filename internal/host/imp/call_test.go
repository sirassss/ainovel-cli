package imp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/litellm"
)

// flakyModel returns a retryable error for the first fails calls, then responds like mockModel.
type flakyModel struct {
	mockModel
	fails int
}

func (f *flakyModel) Generate(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	if f.fails > 0 {
		f.fails--
		return nil, fastRetryErr{}
	}
	return f.mockModel.Generate(ctx, msgs, tools, opts...)
}

// fastRetryErr is retryable with a very short backoff (RetryAfter hits RetryHinter), keeping tests fast.
type fastRetryErr struct{}

func (fastRetryErr) Error() string             { return "rate limited" }
func (fastRetryErr) Retryable() bool           { return true }
func (fastRetryErr) RetryAfter() time.Duration { return time.Millisecond }

// TestCallStructuredNotifiesRetries guards retry visibility: request backoff and validation re-asks must both be echoed,
// otherwise exponential backoff can stay silent for minutes and the user would think the import hung (screenshot issue: an error only after 3 silent minutes).
// Request backoff must also carry a non-zero retryAt deadline - the UI countdown depends on it; a validation re-ask happens immediately, so retryAt is zero.
func TestCallStructuredNotifiesRetries(t *testing.T) {
	m := &flakyModel{mockModel: mockModel{responses: []string{"不是 JSON", `{"boundaries":[]}`}}, fails: 2}
	var notes []string
	var retries, reasks int
	prof := callProfile{notify: func(s string, retryAt time.Time) {
		notes = append(notes, s)
		if !retryAt.IsZero() {
			retries++
		}
		if strings.Contains(s, "hỏi lại") {
			reasks++
		}
	}}
	if _, err := callStructured[boundaryBatch](context.Background(), m, segmentContract, "sys", "p", 100, prof, nil); err != nil {
		t.Fatalf("最终应成功：%v", err)
	}
	if retries != 2 || reasks != 1 {
		t.Fatalf("应回显 2 次带截止时刻的请求退避 + 1 次校验重问，得 %d/%d：%v", retries, reasks, notes)
	}
}

// TestBriefErrIncludesAdapterFacts guards the diagnosability of error echo: the gateway message may be just
// "Provider returned error", so the echo must add the structured facts litellm carries (class / HTTP status / provider / model),
// and with the facts first - when truncating, they are preserved first; non-adapter errors stay unchanged.
func TestBriefErrIncludesAdapterFacts(t *testing.T) {
	le := &litellm.LiteLLMError{
		Type: litellm.ErrorTypeProvider, StatusCode: 502,
		Provider: "openai", Model: "gpt-x", Message: "Provider returned error",
	}
	got := briefErr(fmt.Errorf("外层包装：%w", le))
	for _, want := range []string{"lỗi dịch vụ upstream", "HTTP 502", "openai", "gpt-x", "Provider returned error"} {
		if !strings.Contains(got, want) {
			t.Fatalf("回显应包含 %q，得 %q", want, got)
		}
	}
	if !strings.HasPrefix(got, "lỗi dịch vụ upstream") {
		t.Fatalf("结构化事实应在前，得 %q", got)
	}
	if got := briefErr(errors.New("普通错误")); got != "普通错误" {
		t.Fatalf("非适配器错误应保持原样，得 %q", got)
	}
}

// TestCallStructuredCancelIsNotSemanticFailure guards cancellation semantics: a user cancel (Esc) is not a semantic failure,
// and must not be wrapped into an errSemantic of "N attempts" - that would mislead troubleshooting and write one extra misleading failures/ artifact.
func TestCallStructuredCancelIsNotSemanticFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &mockModel{responses: []string{"垃圾输出"}}
	_, err := callStructured[boundaryBatch](ctx, m, segmentContract, "sys", "p", 100, callProfile{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得 %v", err)
	}
	var se *errSemantic
	if errors.As(err, &se) {
		t.Fatal("取消不应被包装成语义失败")
	}
}

// TestCallStructuredCarriesRawOnSemanticFailure guards §14.2: on an output-layer contract violation,
// the error must carry the raw response so the runner can uniformly write a failures/ failure artifact.
func TestCallStructuredCarriesRawOnSemanticFailure(t *testing.T) {
	m := &nativeImportModel{mockModel: &mockModel{responses: []string{"垃圾输出 not json"}}}
	_, err := callStructured[boundaryBatch](context.Background(), m, segmentContract, "sys", "payload", 100, callProfile{}, nil)
	var se *errSemantic
	if !errors.As(err, &se) {
		t.Fatalf("应返回 errSemantic，得 %T：%v", err, err)
	}
	if se.Raw != "垃圾输出 not json" || !strings.Contains(se.Error(), "vi phạm hợp đồng") {
		t.Fatalf("Raw 应携带最后一次原始响应，得 %q", se.Raw)
	}
}

func TestCallStructuredCarriesRawOnProtocolFailure(t *testing.T) {
	m := &nativeImportModel{mockModel: &mockModel{
		responses: []string{"upstream malformed output"},
		stops:     []agentcore.StopReason{agentcore.StopReasonError},
	}}
	_, err := callStructured[boundaryBatch](context.Background(), m, segmentContract, "sys", "payload", 100, callProfile{}, nil)
	var se *errSemantic
	if !errors.As(err, &se) || se.Raw != "upstream malformed output" || !strings.Contains(se.Error(), "stop_reason=error") {
		t.Fatalf("协议错误应携带原始响应，得 %T：%v", err, err)
	}
}
