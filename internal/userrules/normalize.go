// Package userrules is the service layer for user-rule normalization: it normalizes the natural-language rules of each source via a structured LLM call
// into candidate structured fields, which rules.BuildSnapshot then deterministically merges into this book's snapshot.
//
// Layered responsibilities:
//   - rules package: pure data + deterministic merging (Snapshot / Candidate / BuildSnapshot / SystemDefaults)
//   - this package: LLM normalization + orchestration + persistence (depends on agentcore + store + rules)
//
// Normalization is an enhancement path, not a precondition of main authoring: failure of any source degrades to raw preferences, and main authoring must continue.
package userrules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
	"github.com/voocel/ainovel-cli/internal/rules"
)

// normalizeMaxTokens is the output cap of a single normalization (thinking tokens and the JSON output share this budget).
// The normalization JSON itself is tiny (usually <1k); most of the budget is left for the thinking of "reasoning models whose thinking cannot be turned off" -
// if it is too narrow, thinking crowds out the JSON, causing truncation and parse failures. max_tokens is a cap, not a billed amount, so raising it does not increase cost.
const normalizeMaxTokens = 8192

// normalizeContract sits next to the boundary DTO: all fields required, fatigue_words uses an array of objects
// (strict mode forbids maps with dynamic keys), and both modes share the same DTO convention.
var normalizeContract = llmcontract.Contract{
	Name:        "userrules_normalize",
	Description: "Chuẩn hóa quy tắc viết bằng ngôn ngữ tự nhiên của người dùng thành các trường có cấu trúc",
	Schema: schema.Object(
		schema.Property("structured", schema.Object(
			schema.Property("genre", schema.String("Thể loại; không có thì là chuỗi rỗng")).Required(),
			schema.Property("forbidden_chars", schema.Array("Ký tự bị cấm xuất hiện", schema.String("Ký tự"))).Required(),
			schema.Property("forbidden_phrases", schema.Array("Cụm từ bị cấm xuất hiện (khớp chính xác theo mặt chữ)", schema.String("Cụm từ"))).Required(),
			schema.Property("fatigue_words", schema.Array("Từ gây nhàm và số lần xuất hiện tối đa mỗi chương", schema.Object(
				schema.Property("word", schema.String("Từ gây nhàm")).Required(),
				schema.Property("max_per_chapter", schema.Int("Số lần xuất hiện tối đa mỗi chương (số nguyên dương)")).Required(),
			))).Required(),
		)).Required(),
		schema.Property("preferences", schema.String("Sở thích văn phong/nhân vật/thẩm mỹ bằng ngôn ngữ tự nhiên; không có thì là chuỗi rỗng")).Required(),
		schema.Property("uncertain", schema.Array("Các mục cố ý không nâng lên structured, kèm lý do", schema.String("Mục"))).Required(),
	),
}

// Normalizer normalizes the natural-language rules of a single source into rules.Candidate.
type Normalizer struct {
	model agentcore.ChatModel
}

// NewNormalizer builds a normalizer from a ChatModel. Normalization is a one-time startup tool,
// so a more capable model should be passed in (such as ModelSet's default model), not necessarily the weaker model used for writing.
//
// Normalization does not override thinking: an explicit off is itself a reasoning parameter that only some models support,
// and ordinary chat models reject it. The provider/model default is kept, and normalizeMaxTokens
// reserves the output budget for models whose thinking cannot be turned off.
func NewNormalizer(model agentcore.ChatModel) *Normalizer {
	return &Normalizer{model: model}
}

// Normalize normalizes one source. On failure it returns an error (with the real cause), and the caller decides how to degrade
// (Service.normalizeOrDegrade produces a degraded candidate) - technical errors no longer masquerade as normal results,
// and terminal errors (authentication/permission etc.) are not retried.
func (n *Normalizer) Normalize(ctx context.Context, source, text string) (rules.Candidate, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return rules.Candidate{Source: source}, nil
	}
	if n == nil || n.model == nil {
		return rules.Candidate{}, fmt.Errorf("Chưa cấu hình model chuẩn hóa")
	}

	out, err := llmcontract.Execute(ctx, n.model, llmcontract.Request[normalizerOutput]{
		Contract:     normalizeContract,
		SystemPrompt: normalizerSystemPrompt,
		Payload:      text,
		Options:      []agentcore.CallOption{agentcore.WithMaxTokens(normalizeMaxTokens)},
		Validate: func(out *normalizerOutput) error {
			_, err := out.toCandidate(source)
			return err
		},
		Agent: "rules",
		Hooks: llmcontract.Hooks{
			Resolved: func(res llmcontract.Resolution) {
				slog.Debug("rules normalization structured mode resolved", "module", "rules", "source", source,
					"contract", normalizeContract.Name, "structured_mode", res.Mode,
					"capability_source", res.Source, "provider", res.Provider, "model", res.Model,
					"schema_fingerprint", normalizeContract.Fingerprint())
			},
			Correction: func(ev llmcontract.Correction) {
				slog.Warn("rules normalization output self-correction", "module", "rules", "source", source,
					"attempt", ev.Attempt, "layer", ev.Layer, "structured_mode", ev.Mode, "err", ev.Err)
			},
		},
	})
	if err != nil {
		return rules.Candidate{}, fmt.Errorf("Chuẩn hóa thất bại: %w", err)
	}
	return out.toCandidate(source)
}

// degraded builds a degraded candidate: when normalization fails, the original text is treated as a style preference and no mechanical rules are extracted.
// uncertain labels the source (so that "which sources could not be parsed" can be echoed), but carries no technical error details - technical errors go only to the log.
func degraded(source, text string) rules.Candidate {
	return rules.Candidate{
		Source:      source,
		Preferences: text,
		Uncertain:   []string{source + ": chuẩn hóa thất bại, đã xử lý nguyên văn như sở thích văn phong (chưa rút ra quy tắc cơ học)"},
		Degraded:    true,
	}
}

// normalizerOutput is the boundary DTO agreed with the normalizer (shared by both modes): uncertain is a fixed
// string array and fatigue_words a fixed array of objects - the shape is pinned by the contract, with no more multi-shape guessing.
type normalizerOutput struct {
	Structured  normalizerStructured `json:"structured"`
	Preferences string               `json:"preferences"`
	Uncertain   []string             `json:"uncertain"`
}

type normalizerStructured struct {
	Genre            string             `json:"genre"`
	ForbiddenChars   []string           `json:"forbidden_chars"`
	ForbiddenPhrases []string           `json:"forbidden_phrases"`
	FatigueWords     []fatigueWordEntry `json:"fatigue_words"`
}

type fatigueWordEntry struct {
	Word          string `json:"word"`
	MaxPerChapter int    `json:"max_per_chapter"`
}

// toCandidate validates the boundary DTO and converts it to a domain candidate: a fatigue entry needs a non-empty word and a positive integer cap
// (validation errors can be fed back to the model for correction), while the domain side remains map[string]int.
func (o normalizerOutput) toCandidate(source string) (rules.Candidate, error) {
	var fatigue map[string]int
	for _, e := range o.Structured.FatigueWords {
		word := strings.TrimSpace(e.Word)
		if word == "" {
			return rules.Candidate{}, fmt.Errorf("fatigue_words chứa mục từ rỗng")
		}
		if e.MaxPerChapter < 1 {
			return rules.Candidate{}, fmt.Errorf("fatigue_words[%q].max_per_chapter phải là số nguyên dương, got %d", word, e.MaxPerChapter)
		}
		if fatigue == nil {
			fatigue = make(map[string]int, len(o.Structured.FatigueWords))
		}
		fatigue[word] = e.MaxPerChapter
	}
	return rules.Candidate{
		Source: source,
		Structured: rules.Structured{
			Genre:            strings.TrimSpace(o.Structured.Genre),
			ForbiddenChars:   nonEmpty(o.Structured.ForbiddenChars),
			ForbiddenPhrases: nonEmpty(o.Structured.ForbiddenPhrases),
			FatigueWords:     fatigue,
		},
		Preferences: strings.TrimSpace(o.Preferences),
		Uncertain:   nonEmpty(o.Uncertain),
	}, nil
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// normalizerSystemPrompt describes only the normalization semantics; the output structure is maintained in a single place by normalizeContract.
// Conservative promotion has been verified on 10 real examples (including the threshold-invention trap) (10/10).
const normalizerSystemPrompt = `Bạn là "Bộ chuẩn hóa quy tắc" của hệ thống viết tiểu thuyết AI. Bạn đọc quy tắc viết dài hạn (bằng ngôn ngữ tự nhiên) từ một nguồn của người dùng, nâng các quy tắc rõ ràng và kiểm tra được bằng máy lên structured, phần còn lại đưa vào preferences hoặc uncertain.

[Nâng cấp thận trọng - quan trọng nhất]
- Chỉ ghi vào structured khi người dùng nói rõ ràng, không mơ hồ.
- forbidden_chars/forbidden_phrases là mức error: chỉ nâng khi có lệnh cấm rõ ràng kiểu "không được xuất hiện X / cấm dùng X / đừng viết X".
- fatigue_words: chỉ nâng khi có đồng thời "từ rõ ràng" và "ngưỡng số lần rõ ràng"; những câu như "ít dùng X / đừng hay dùng X" không có con số thì đưa vào preferences, tuyệt đối không tự bịa ngưỡng.
- Mong muốn về số chữ/dung lượng ("mỗi chương 3000 chữ", "ngắn hơn chút") luôn đưa vào preferences: độ dài chương là vấn đề nhịp kể, để tự nhiên nắm bắt khi sáng tác, không kiểm tra cơ học.
- Cái gì không kiểm tra được bằng máy, không có ngưỡng rõ ràng, phụ thuộc ngữ cảnh thì luôn đưa vào preferences.
- Nguyên tắc: thà sót không đưa vào structured còn hơn nâng nhầm (sẽ báo sai ở mọi chương).

preferences giữ lại sở thích về văn phong, nhân vật và thẩm mỹ bằng một đoạn ngôn ngữ tự nhiên dễ đọc.
uncertain nêu các mục bạn cố ý không nâng lên structured và lý do.`
