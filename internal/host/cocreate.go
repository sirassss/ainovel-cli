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

// 冷启动共创：从零澄清需求，产出整本书的创作指令。
const coCreateSystemPrompt = `Bạn là trợ lý đồng sáng tác tiểu thuyết. Nhiệm vụ của bạn không phải là bắt đầu viết truyện ngay, mà là qua nhiều lượt hội thoại ngắn giúp người dùng làm rõ yêu cầu sáng tác, đồng thời liên tục chỉnh lý thành một chỉ thị sáng tác tiếng Việt có thể giao thẳng cho engine sáng tác.

Mỗi lượt trả lời phải xuất đúng định dạng XML sau, gồm bốn thẻ lần lượt xuất hiện, mỗi thẻ đều phải có thẻ mở và thẻ đóng đúng:

<reply>
Câu trả lời tiếng Việt tự nhiên dành cho người dùng: trước hết phản hồi đầu vào của người dùng, sau đó đặt tối đa 1 đến 2 câu hỏi then chốt nhất hiện tại. Nếu thông tin đã đủ để bắt đầu sáng tác, hãy báo người dùng có thể bấm Ctrl+S để bắt đầu.
</reply>

<draft>
Bản nháp chỉ thị sáng tác đầy đủ hiện tại, dùng Markdown: bắt đầu thẳng từ tiêu đề cấp hai, ví dụ "## Chủ đề", "## Yếu tố then chốt", "## Thông tin cần làm rõ"; liệt kê ý chính bằng gạch đầu dòng. Mỗi lượt đều phải **cập nhật tích lũy** trên kết luận đã có, tiếp thu ý định mới nhất của người dùng; kể cả lượt này không có gì mới cũng phải viết lại nguyên vẹn toàn bộ bản nháp — không được lược bớt, không được viết chỗ giữ chỗ kiểu "(giữ nguyên như lượt trước)".
</draft>
` + coCreateProtocolTail

// 阶段共创：小说已写了一部分，规划"后续阶段"的走向。调用方需把当前故事状态摘要
// 追加到本 prompt 之后（"## 当前故事状态" 段），让模型在已写内容的基础上规划。
const stageCoCreateSystemPrompt = `Bạn là trợ lý "đồng sáng tác theo giai đoạn" cho tiểu thuyết. Cuốn tiểu thuyết này đã được viết một phần (tiến độ xem ở "Trạng thái truyện hiện tại" bên dưới). Người dùng tạm dừng, muốn cùng bạn quy hoạch hướng đi của "giai đoạn tiếp theo", rồi mới tiếp tục sáng tác.

Nhiệm vụ của bạn không phải viết tiếp chính văn, mà là qua nhiều lượt hội thoại ngắn giúp người dùng nghĩ rõ đoạn phía sau (vài chương tới / cung kế / tập kế) sẽ đi về đâu, đồng thời liên tục chỉnh lý thành một "brief hướng đi tiếp theo" để engine sáng tác dựa vào đó tiến hành.

Nguyên tắc sắt: mọi đề xuất phải nhất quán với tình tiết, nhân vật, phục bút đã xảy ra trong "Trạng thái truyện hiện tại", tuyệt đối không lật lại hay bỏ qua nội dung đã viết; chỉ quy hoạch "phía sau đi thế nào", không thiết kế lại cả cuốn sách.

Mỗi lượt trả lời phải xuất đúng định dạng XML sau, gồm bốn thẻ lần lượt xuất hiện, mỗi thẻ đều phải có thẻ mở và thẻ đóng đúng:

<reply>
Câu trả lời tiếng Việt tự nhiên dành cho người dùng: trước hết phản hồi đầu vào của người dùng, sau đó đặt tối đa 1 đến 2 câu hỏi then chốt nhất hiện tại. Nếu hướng đi tiếp theo đã đủ rõ, hãy báo người dùng có thể bấm Ctrl+S để giao hướng đi cho engine sáng tác và tiếp tục.
</reply>

<draft>
"Brief hướng đi tiếp theo" đầy đủ hiện tại, dùng Markdown: bắt đầu thẳng từ tiêu đề cấp hai, ví dụ "## Hướng đi tiếp theo", "## Bước ngoặt then chốt", "## Phục bút cần thu", "## Nhịp độ và dung lượng"; liệt kê ý chính bằng gạch đầu dòng. Mỗi lượt đều phải **cập nhật tích lũy** trên kết luận đã có, tiếp thu ý định mới nhất của người dùng; kể cả lượt này không có gì mới cũng phải viết lại nguyên vẹn toàn bộ brief — không được lược bớt, không được viết chỗ giữ chỗ kiểu "(giữ nguyên như lượt trước)".
</draft>
` + coCreateProtocolTail

// coCreateProtocolTail 是两种共创模式共用的输出协议尾部（<ready> / <suggestions> + 输出规范）。
// 两模式只在开场语境与 <draft> 语义上不同，协议完全一致。
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

// CoCreateProgressKind 标识流式回调的内容类型。
const (
	CoCreateProgressThinking = "thinking"
	CoCreateProgressReply    = "reply"
)

// 四段式 XML 标签输出。XML 风格比方括号 marker 更鲁棒——Claude/GPT 训练数据里
// 大量 <thinking>...</thinking> 这类格式，模型几乎不会把 <reply> 改写成 <REWRITE>
// 或其他变体；闭合标签也让流式中段截断更精确（不依赖找下一个 marker 来断尾）。
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

	// 排查 "cocreate empty response" 等偶发问题需要看到模型实际返回什么。
	// 每轮全程落盘到 <output>/meta/sessions/cocreate.jsonl，与正式创作的 session 日志同位。
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
			slog.Warn("Lưu nhật ký phiên đồng sáng tác thất bại", "module", "cocreate", "err", logErr)
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

	// Channel fallback：思考型模型（R1/GLM-Z1/QwQ 等）偶发把完整答案写进
	// reasoning_content 后没切回 final answer 通道，导致 raw 为空但 thinking 含
	// 完整四段。实测见 meta/sessions/cocreate.jsonl —— 直接拿 thinking 当 raw 解析，
	// 协议层已有降级处理（无 [REPLY] 标记时整段当 reply），救场后 UI 体验无差别。
	rawText := raw.String()
	if strings.TrimSpace(rawText) == "" {
		if t := strings.TrimSpace(thinking.String()); t != "" {
			rawText = t
		}
	}
	reply, err = parseCoCreateResponse(rawText)
	return reply, err
}

// coCreateLogEntry 是写入 meta/sessions/cocreate.jsonl 的一行结构。
// 字段命名贴近 jsonl 直查习惯（snake_case），方便 jq 过滤。
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

// parseCoCreateResponse 解析 XML 标签输出。模型若没遵守协议（直接说自然语言），
// 整段作为 reply 显示，draft 留空让 session 保留上一轮。
func parseCoCreateResponse(raw string) (CoCreateReply, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CoCreateReply{}, fmt.Errorf("cocreate empty response")
	}

	reply, draft, ready, suggestions := splitCoCreateMarkers(raw)
	if reply == "" {
		// 模型没遵守 XML 协议：整段作为 reply。
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

// splitCoCreateMarkers 按四个 XML 标签切分文本。
// 标签可能缺失（流式中段或模型遗漏），缺失部分对应字段为空 / false / nil。
// 缺失闭标签时，extractTagContent 会取到字符串末尾，仍尽力解析。
func splitCoCreateMarkers(s string) (reply, draft string, ready bool, suggestions []string) {
	reply = extractTagContent(s, tagReply)
	draft = extractTagContent(s, tagDraft)
	readyStr := strings.ToLower(extractTagContent(s, tagReady))
	ready = readyStr == "true" || readyStr == "yes"
	suggestions = parseSuggestions(extractTagContent(s, tagSuggestions))
	return
}

// extractTagContent 从 s 中抠出 <tag>...</tag> 之间的文本。
// 三种偶发故障场景兜底，避免直接走降级丢字段：
//  1. 有开无闭（流式中段）→ 切到下一个已知开标签前
//  2. 无开有闭（模型 typo，如 <suggestions> 写成 <uggestions>）→ 从最近一个已知
//     完整闭合标签的结束位置开始，到 </tag> 之前
//  3. reply 完全无开标签（模型直接以自然语言开篇，末尾贴 </reply>）→ 从开头到 </reply>
func extractTagContent(s, tag string) string {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	oIdx := strings.Index(s, open)
	if oIdx >= 0 {
		rest := s[oIdx+len(open):]
		if cIdx := strings.Index(rest, closeTag); cIdx >= 0 {
			return strings.TrimSpace(rest[:cIdx])
		}
		// 有开无闭 → 切到下一个已知开标签前
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

	// 无开有闭 → 从最近一个已知完整闭合标签的结束位置开始，到 </tag>。
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

// parseSuggestions 把 <suggestions> 段每行抠出来，去掉 "- " / "* " / "1. " 等列表前缀。
// 最多保留 3 条；空行、过短（<2 字）、整行像 XML 标签的（typo 开标签兜底残留，
// 例如 <uggestions>）忽略。
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
		// 整行像 XML 标签 → 跳过（防 typo 开标签污染）
		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
			continue
		}
		// 剥列表前缀
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

// isOrderedSuggestion 判断行首是否形如 "1. " / "12. "（数字+点+空格）。
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

// extractReplyPreview 流式预览：raw 还在生长时给 UI 一段可显示的文本。
// 找到 <reply> 之后的内容，切到 </reply> 或下一个开标签 <draft> 之前。
// 模型半遵守（漏 <reply> 开标签）时，开头到 </reply> 或 <draft> 都算 reply。
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
