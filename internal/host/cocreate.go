package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/store"
)

// Cold-start co-creation: clarify requirements from scratch and produce the creative directive for the whole book.
const coCreateSystemPrompt = `Bạn là trợ lý đồng sáng tác tiểu thuyết. Nhiệm vụ của bạn không phải là bắt đầu viết truyện ngay, mà là qua nhiều lượt hội thoại ngắn giúp người dùng làm rõ yêu cầu sáng tác, đồng thời liên tục chỉnh lý thành một chỉ thị sáng tác tiếng Việt có thể giao thẳng cho engine sáng tác.

Mỗi lượt trả lời phải xuất đúng định dạng XML sau, gồm bốn thẻ lần lượt xuất hiện, mỗi thẻ đều phải có thẻ mở và thẻ đóng đúng:

<reply>
Câu trả lời tiếng Việt tự nhiên dành cho người dùng: trước hết phản hồi đầu vào của người dùng, sau đó đặt tối đa 1 đến 2 câu hỏi then chốt nhất hiện tại. Nếu thông tin đã đủ để bắt đầu sáng tác, hãy báo người dùng có thể bấm Ctrl+S để bắt đầu.
</reply>

<draft>
Bản nháp chỉ thị sáng tác đầy đủ hiện tại, dùng Markdown: bắt đầu thẳng từ tiêu đề cấp hai, ví dụ "## Chủ đề", "## Yếu tố then chốt", "## Thông tin cần làm rõ"; liệt kê ý chính bằng gạch đầu dòng. Mỗi lượt đều phải **cập nhật tích lũy** trên kết luận đã có, tiếp thu ý định mới nhất của người dùng; kể cả lượt này không có gì mới cũng phải viết lại nguyên vẹn toàn bộ bản nháp — không được lược bớt, không được viết chỗ giữ chỗ kiểu "(giữ nguyên như lượt trước)".
</draft>
` + coCreateProtocolTail

// Stage co-creation: part of the novel is already written, and this plans the direction of the "next stage". The caller must append a summary of the current story state
// to this prompt (the "## 当前故事状态" section), so the model plans on top of what is already written. // han:keep names a zh-mode section heading
const stageCoCreateSystemPrompt = `Bạn là trợ lý "đồng sáng tác theo giai đoạn" cho tiểu thuyết. Cuốn tiểu thuyết này đã được viết một phần (tiến độ xem ở "Trạng thái truyện hiện tại" bên dưới). Người dùng tạm dừng, muốn cùng bạn lập kế hoạch hướng đi của "giai đoạn tiếp theo", rồi mới tiếp tục sáng tác.

Nhiệm vụ của bạn không phải viết tiếp nội dung chương, mà là qua nhiều lượt hội thoại ngắn giúp người dùng nghĩ rõ đoạn phía sau (vài chương tới / cung kế / tập kế) sẽ đi về đâu, đồng thời liên tục chỉnh lý thành một "brief hướng đi tiếp theo" để engine sáng tác dựa vào đó tiến hành.

Nguyên tắc sắt: mọi đề xuất phải nhất quán với tình tiết, nhân vật, chi tiết gài trước đã xảy ra trong "Trạng thái truyện hiện tại", tuyệt đối không lật lại hay bỏ qua nội dung đã viết; chỉ lập kế hoạch "phía sau đi thế nào", không thiết kế lại cả cuốn sách.

Mỗi lượt trả lời phải xuất đúng định dạng XML sau, gồm bốn thẻ lần lượt xuất hiện, mỗi thẻ đều phải có thẻ mở và thẻ đóng đúng:

<reply>
Câu trả lời tiếng Việt tự nhiên dành cho người dùng: trước hết phản hồi đầu vào của người dùng, sau đó đặt tối đa 1 đến 2 câu hỏi then chốt nhất hiện tại. Nếu hướng đi tiếp theo đã đủ rõ, hãy báo người dùng có thể bấm Ctrl+S để giao hướng đi cho engine sáng tác và tiếp tục.
</reply>

<draft>
"Brief hướng đi tiếp theo" đầy đủ hiện tại, dùng Markdown: bắt đầu thẳng từ tiêu đề cấp hai, ví dụ "## Hướng đi tiếp theo", "## Bước ngoặt then chốt", "## Chi tiết gài trước cần thu", "## Nhịp độ và dung lượng"; liệt kê ý chính bằng gạch đầu dòng. Mỗi lượt đều phải **cập nhật tích lũy** trên kết luận đã có, tiếp thu ý định mới nhất của người dùng; kể cả lượt này không có gì mới cũng phải viết lại nguyên vẹn toàn bộ brief — không được lược bớt, không được viết chỗ giữ chỗ kiểu "(giữ nguyên như lượt trước)".
</draft>
` + coCreateProtocolTail

// coCreateProtocolTail is the output-protocol tail shared by both co-creation modes (<ready> / <suggestions> + output rules).
// The two modes differ only in the opening context and the semantics of <draft>; the protocol is identical.
const coCreateProtocolTail = `
<ready>false</ready>

<suggestions>
1-3 câu "người dùng có thể muốn nói tiếp", mỗi dòng một câu bắt đầu bằng "- ". Đây là gợi ý khi người dùng bí ý,
bấm phím số để điền vào ô nhập, người dùng có thể sửa lại rồi gửi.

Yêu cầu:
- Đứng ở giọng người dùng, như người dùng nói với bạn, đừng viết thành câu hỏi ngược của trợ lý.
- Mỗi câu không quá 25 chữ, đa dạng cấu trúc câu, tránh rập khuôn.
- Đưa ra xu hướng / lựa chọn / ý định bổ sung, đừng viết hộ người dùng trọn vẹn một thiết lập.
</suggestions>

Quy phạm đầu ra:
- Bắt buộc dùng bốn thẻ XML: <reply> / <draft> / <ready> / <suggestions>, mỗi thẻ đều phải mở và đóng đầy đủ.
- Tên thẻ chỉ được viết thường bằng tiếng Anh, không được đổi thành <REPLY> / <REWRITE> / <trả lời> hay bất kỳ biến thể nào.
- Ngoài thẻ không được thêm bất kỳ giải thích, suy nghĩ hay rào mã nào.
- Trong <draft> cho phép Markdown nhiều dòng, cứ xuống dòng trực tiếp, không cần escape.
- <ready> chỉ ghi true hoặc false. Thông tin đã đủ thì điền true.
- Khi <ready>true</ready> thì <suggestions> có thể để trống (giữ thẻ rỗng <suggestions></suggestions> là được).
`

// CoCreateProgressKind identifies the content type of a streaming callback.
const (
	CoCreateProgressThinking = "thinking"
	CoCreateProgressReply    = "reply"
)

// Four-part XML tag output. The XML style is more robust than square-bracket markers: Claude/GPT training data contains
// plenty of formats like <thinking>...</thinking>, so the model almost never rewrites <reply> into <REWRITE>
// or other variants; closing tags also make mid-stream truncation more precise (no need to look for the next marker to cut off the tail).
const (
	tagReply       = "reply"
	tagDraft       = "draft"
	tagReady       = "ready"
	tagSuggestions = "suggestions"
)

func coCreateStream(ctx context.Context, models *bootstrap.ModelSet, sessions *store.SessionStore, sysPrompt string, history []CoCreateMessage, onProgress func(kind, text string)) (reply CoCreateReply, err error) {
	if len(history) == 0 {
		return CoCreateReply{}, fmt.Errorf("cocreate history is empty")
	}

	model := models.ForRole("thinking")

	msgs := []agentcore.Message{agentcore.SystemMsg(sysPrompt)}
	for _, item := range history {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(item.Role)) {
		case "assistant":
			msgs = append(msgs, assistantMsg(content))
		default:
			msgs = append(msgs, agentcore.UserMsg(content))
		}
	}

	var raw, thinking strings.Builder

	// Diagnosing sporadic problems such as "cocreate empty response" requires seeing what the model actually returned.
	// Every round is written to <output>/meta/sessions/cocreate.jsonl in full, alongside the session logs of formal creation.
	start := time.Now()
	defer func() {
		if sessions == nil {
			return
		}
		if logErr := sessions.LogCoCreate(coCreateLogEntry{
			Time:         time.Now(),
			DurationMS:   time.Since(start).Milliseconds(),
			InputHistory: history,
			RawResponse:  raw.String(),
			RawLen:       len([]rune(raw.String())),
			Thinking:     thinking.String(),
			ParsedReply:  reply.Message,
			ParsedDraft:  reply.Prompt,
			ParsedReady:  reply.Ready,
			ParsedSugs:   reply.Suggestions,
			Error:        errString(err),
		}); logErr != nil {
			slog.Warn("saving co-create session log failed", "module", "cocreate", "err", logErr)
		}
	}()

	streamCh, err := model.GenerateStream(ctx, msgs, nil, agentcore.WithMaxTokens(2048))
	if err != nil {
		return CoCreateReply{}, fmt.Errorf("cocreate generate: %w", err)
	}

	var streamed bool
	for ev := range streamCh {
		switch ev.Type {
		case agentcore.StreamEventThinkingDelta:
			thinking.WriteString(ev.Delta)
			if onProgress != nil {
				onProgress(CoCreateProgressThinking, thinking.String())
			}
		case agentcore.StreamEventTextDelta:
			streamed = true
			raw.WriteString(ev.Delta)
			if onProgress != nil {
				onProgress(CoCreateProgressReply, extractReplyPreview(raw.String()))
			}
		case agentcore.StreamEventDone:
			if !streamed {
				raw.WriteString(ev.Message.TextContent())
			}
		case agentcore.StreamEventError:
			if ev.Err != nil {
				return CoCreateReply{}, fmt.Errorf("cocreate generate: %w", ev.Err)
			}
			return CoCreateReply{}, fmt.Errorf("cocreate generate failed")
		}
	}

	// Channel fallback: reasoning models (R1/GLM-Z1/QwQ etc.) occasionally write the whole answer into
	// reasoning_content without switching back to the final answer channel, leaving raw empty while thinking holds
	// all four parts. See meta/sessions/cocreate.jsonl for a real observation: parse thinking directly as raw,
	// and since the protocol layer already degrades gracefully (with no [REPLY] marker the whole text is the reply), the UI experience is unchanged after this rescue.
	rawText := raw.String()
	if strings.TrimSpace(rawText) == "" {
		if t := strings.TrimSpace(thinking.String()); t != "" {
			rawText = t
		}
	}
	reply, err = parseCoCreateResponse(rawText)
	return reply, err
}

// coCreateLogEntry is the structure of one line written to meta/sessions/cocreate.jsonl.
// Field names follow the habit of querying jsonl directly (snake_case), which makes jq filtering convenient.
type coCreateLogEntry struct {
	Time         time.Time         `json:"time"`
	DurationMS   int64             `json:"duration_ms"`
	InputHistory []CoCreateMessage `json:"input_history"`
	RawResponse  string            `json:"raw_response"`
	RawLen       int               `json:"raw_len"`
	Thinking     string            `json:"thinking,omitempty"`
	ParsedReply  string            `json:"parsed_reply"`
	ParsedDraft  string            `json:"parsed_draft"`
	ParsedReady  bool              `json:"parsed_ready"`
	ParsedSugs   []string          `json:"parsed_sugs,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func assistantMsg(text string) agentcore.Message {
	return agentcore.Message{
		Role:      agentcore.RoleAssistant,
		Content:   []agentcore.ContentBlock{agentcore.TextBlock(text)},
		Timestamp: time.Now(),
	}
}

// parseCoCreateResponse parses the XML-tag output. If the model did not follow the protocol (answered in plain natural language),
// the whole text is shown as the reply and draft is left empty so the session keeps the previous round.
func parseCoCreateResponse(raw string) (CoCreateReply, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CoCreateReply{}, fmt.Errorf("cocreate empty response")
	}

	reply, draft, ready, suggestions := splitCoCreateMarkers(raw)
	if reply == "" {
		// The model did not follow the XML protocol: use the whole text as the reply.
		return CoCreateReply{Message: raw, Prompt: "", Ready: false, Raw: raw}, nil
	}
	return CoCreateReply{
		Message:     reply,
		Prompt:      draft,
		Ready:       ready,
		Suggestions: suggestions,
		Raw:         raw,
	}, nil
}

// splitCoCreateMarkers splits the text by the four XML tags.
// Tags may be missing (mid-stream or omitted by the model); the corresponding fields are then empty / false / nil.
// When a closing tag is missing, extractTagContent reads to the end of the string and still parses on a best-effort basis.
func splitCoCreateMarkers(s string) (reply, draft string, ready bool, suggestions []string) {
	reply = extractTagContent(s, tagReply)
	draft = extractTagContent(s, tagDraft)
	readyStr := strings.ToLower(extractTagContent(s, tagReady))
	ready = readyStr == "true" || readyStr == "yes"
	suggestions = parseSuggestions(extractTagContent(s, tagSuggestions))
	return
}

// extractTagContent extracts the text between <tag>...</tag> from s.
// It covers three sporadic failure scenarios so the parse does not fall straight to the degraded path and lose fields:
//  1. open tag without close (mid-stream) → cut before the next known open tag
//  2. close tag without open (model typo, e.g. <suggestions> written as <uggestions>) → from the end of the most recent known
//     complete closing tag to before </tag>
//  3. reply has no open tag at all (the model opened directly in natural language and appended </reply> at the end) → from the start to </reply>
func extractTagContent(s, tag string) string {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	oIdx := strings.Index(s, open)
	if oIdx >= 0 {
		rest := s[oIdx+len(open):]
		if cIdx := strings.Index(rest, closeTag); cIdx >= 0 {
			return strings.TrimSpace(rest[:cIdx])
		}
		// open without close → cut before the next known open tag
		for _, other := range []string{"<reply>", "<draft>", "<ready>", "<suggestions>"} {
			if other == open {
				continue
			}
			if idx := strings.Index(rest, other); idx >= 0 {
				rest = rest[:idx]
			}
		}
		return strings.TrimSpace(rest)
	}

	// close without open → from the end of the most recent known complete closing tag to </tag>.
	if cIdx := strings.Index(s, closeTag); cIdx >= 0 {
		prefix := s[:cIdx]
		start := 0
		for _, t := range []string{"</reply>", "</draft>", "</ready>", "</suggestions>"} {
			if t == closeTag {
				continue
			}
			if i := strings.LastIndex(prefix, t); i >= 0 {
				if end := i + len(t); end > start {
					start = end
				}
			}
		}
		return strings.TrimSpace(prefix[start:])
	}
	return ""
}

// parseSuggestions extracts each line of the <suggestions> section and strips list prefixes such as "- " / "* " / "1. ".
// At most 3 entries are kept; empty lines, lines that are too short (<2 characters), and lines that look entirely like an XML tag (leftovers of the typo open-tag fallback,
// e.g. <uggestions>) are ignored.
func parseSuggestions(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A line that looks entirely like an XML tag → skip (prevents a typo open tag from polluting the output)
		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
			continue
		}
		// strip the list prefix
		switch {
		case strings.HasPrefix(line, "- "):
			line = strings.TrimSpace(line[2:])
		case strings.HasPrefix(line, "* "):
			line = strings.TrimSpace(line[2:])
		case isOrderedSuggestion(line):
			line = stripOrderedPrefix(line)
		}
		if len([]rune(line)) < 2 {
			continue
		}
		out = append(out, line)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// isOrderedSuggestion reports whether the line starts like "1. " / "12. " (digits + dot + space).
func isOrderedSuggestion(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' '
}

func stripOrderedPrefix(line string) string {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) {
		return line
	}
	return strings.TrimSpace(line[i+2:])
}

// extractReplyPreview is the streaming preview: while raw is still growing it gives the UI a displayable piece of text.
// It finds the content after <reply> and cuts before </reply> or the next open tag <draft>.
// When the model half-complies (omits the <reply> open tag), everything from the start to </reply> or <draft> counts as the reply.
func extractReplyPreview(raw string) string {
	trimmed := strings.TrimSpace(raw)
	open := "<" + tagReply + ">"
	closeTag := "</" + tagReply + ">"
	draftOpen := "<" + tagDraft + ">"

	rest := trimmed
	if rIdx := strings.Index(trimmed, open); rIdx >= 0 {
		rest = trimmed[rIdx+len(open):]
	}
	if cIdx := strings.Index(rest, closeTag); cIdx >= 0 {
		return strings.TrimSpace(rest[:cIdx])
	}
	if dIdx := strings.Index(rest, draftOpen); dIdx >= 0 {
		rest = rest[:dIdx]
	}
	return strings.TrimSpace(rest)
}

// coCreatePromptFor picks the co-creation system prompt by the work's language (only "zh" uses the Chinese version).
func coCreatePromptFor(lang string) string {
	if lang == "zh" {
		return coCreateSystemPromptZH
	}
	return coCreateSystemPrompt
}
