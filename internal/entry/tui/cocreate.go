package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
)

type startupMode int

const (
	startupModeQuick startupMode = iota
	startupModeCoCreate
)

func (m startupMode) label() string {
	switch m {
	case startupModeCoCreate:
		return "Đồng sáng tác"
	default:
		return "Bắt đầu nhanh"
	}
}

func (m startupMode) subtitle() string {
	switch m {
	case startupModeCoCreate:
		return "Trò chuyện làm rõ yêu cầu cùng AI trước khi viết"
	default:
		return "Nhập một câu ý tưởng để bắt đầu viết ngay"
	}
}

func placeholderForNewMode(mode startupMode) string {
	switch mode {
	case startupModeCoCreate:
		return "Nhập ý tưởng cốt lõi của bạn, Enter để trao đổi cùng AI"
	default:
		return "Nhập yêu cầu sáng tác truyện, Enter để bắt đầu viết ngay"
	}
}

func placeholderForCoCreate(state *cocreateState) string {
	if state == nil {
		return placeholderForNewMode(startupModeCoCreate)
	}
	switch {
	case state.awaiting:
		return "AI đang tổng hợp yêu cầu của bạn..."
	case state.canStart():
		if state.stage {
			return "Tiếp tục bổ sung, hoặc nhấn Ctrl+S để áp dụng định hướng và tiếp tục viết"
		}
		return "Tiếp tục bổ sung, hoặc nhấn Ctrl+S để bắt đầu sáng tác"
	default:
		return "Tiếp tục bổ sung yêu cầu, Enter để gửi cho AI"
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

type cocreateState struct {
	session             *startup.CoCreateSession
	stage               bool
	awaiting            bool
	reqID               int
	cancel              context.CancelFunc
	deltaCh             chan cocreateStreamItem
	doneCh              chan cocreateDoneMsg
	convVP              viewport.Model
	promptVP            viewport.Model
	convFollow          bool
	selectedSuggestions []string
	focusPrompt         bool
}

func newCoCreateState(initial string) *cocreateState {
	makeVP := func() viewport.Model {
		vp := viewport.New(0, 0)
		vp.MouseWheelEnabled = true
		vp.MouseWheelDelta = 3
		return vp
	}
	return &cocreateState{
		session:    startup.NewCoCreateSession(strings.TrimSpace(initial)),
		awaiting:   true,
		convVP:     makeVP(),
		promptVP:   makeVP(),
		convFollow: true,
	}
}

const stageCoCreateOpener = "Tôi muốn tạm dừng một chút để cùng bạn lập kế hoạch hướng đi tiếp theo."
const stageCoCreateSystemLine = "Đã tạm dừng sáng tác để vào chế độ Đồng sáng tác giai đoạn — AI sẽ kết hợp tiến độ hiện tại để cùng bạn định hướng cốt truyện."

func newStageCoCreateState() *cocreateState {
	s := newCoCreateState(stageCoCreateOpener)
	s.stage = true
	return s
}

func (s *cocreateState) appendUser(text string) {
	s.resetSuggestionInput()
	s.session.AppendUser(text)
}

func (s *cocreateState) apply(reply host.CoCreateReply) {
	s.awaiting = false
	s.resetSuggestionInput()
	s.session.ApplyReply(reply)
}

func (s *cocreateState) applyDelta(kind, text string) {
	s.session.ApplyDelta(kind, text)
}

func (s *cocreateState) canStart() bool {
	return s.session.CanStart()
}

func (s *cocreateState) initialInput() string {
	return s.session.InitialInput()
}

func (s *cocreateState) streamReply() string {
	return s.session.StreamReply()
}

func (s *cocreateState) draftPrompt() string {
	return s.session.DraftPrompt()
}

func (s *cocreateState) ready() bool {
	return s.session.Ready()
}

func (s *cocreateState) suggestions() []string {
	return s.session.Suggestions()
}

func (s *cocreateState) appendSuggestion(index int, current string) (string, bool) {
	suggestions := s.suggestions()
	if index < 0 || index >= len(suggestions) {
		return "", false
	}
	if len(s.selectedSuggestions) == 0 {
		if strings.TrimSpace(current) != "" {
			return "", false
		}
	} else if current != strings.Join(s.selectedSuggestions, "; ") {
		s.resetSuggestionInput()
		return "", false
	}

	suggestion := strings.TrimSpace(suggestions[index])
	for _, selected := range s.selectedSuggestions {
		if selected == suggestion {
			return current, true
		}
	}

	s.selectedSuggestions = append(s.selectedSuggestions, suggestion)
	return strings.Join(s.selectedSuggestions, "; "), true
}

func (s *cocreateState) resetSuggestionInput() {
	s.selectedSuggestions = nil
}

func (s *cocreateState) buildPrompt() (string, error) {
	return s.session.BuildPrompt()
}

func renderStartupModeBar(width int, mode startupMode) string {
	quick := renderStartupModePill(mode == startupModeQuick, "Bắt đầu nhanh")
	cocreate := renderStartupModePill(mode == startupModeCoCreate, "Đồng sáng tác")
	title := lipgloss.NewStyle().
		Foreground(colorAccent).
		Bold(true).
		Render("Chế độ khởi động")
	divider := lipgloss.NewStyle().
		Foreground(colorDim).
		Render("·")
	line := title + " " + divider + " " + quick + "  " + cocreate
	return lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Render(line)
}

func renderStartupModePill(active bool, label string) string {
	style := lipgloss.NewStyle().Padding(0, 1)
	if active {
		style = style.Foreground(lipgloss.Color("#1c1a14")).Background(colorAccent).Bold(true)
	} else {
		style = style.Foreground(colorMuted)
	}
	return style.Render(label)
}

func coCreateColumns(bodyW int) (leftW, rightW int) {
	leftW = bodyW * 58 / 100
	if leftW < 42 {
		leftW = bodyW / 2
	}
	rightW = bodyW - leftW
	if rightW < 28 {
		rightW = 28
		leftW = bodyW - rightW
	}
	return leftW, rightW
}

func renderCoCreateBody(width, height int, state *cocreateState, errMsg, inputView string, spinnerFrame int) string {
	if state == nil {
		return ""
	}
	leftW, rightW := coCreateColumns(width)
	innerW := leftW - 1

	inputBox := lipgloss.NewStyle().
		Width(innerW-6).
		Border(baseBorder).
		BorderForeground(colorDim).
		Padding(0, 1).
		Margin(0, 1).
		Render(inputView)

	suggestionsBox := renderCoCreateSuggestions(innerW, state)
	suggestionsH := 0
	if suggestionsBox != "" {
		suggestionsH = lipgloss.Height(suggestionsBox)
	}

	convH := height - lipgloss.Height(inputBox) - suggestionsH
	if convH < 4 {
		convH = 4
	}

	convPanel := renderCoCreateConversationPanel(innerW, convH, state, errMsg, spinnerFrame)

	var stack string
	if suggestionsBox == "" {
		stack = lipgloss.JoinVertical(lipgloss.Left, convPanel, inputBox)
	} else {
		stack = lipgloss.JoinVertical(lipgloss.Left, convPanel, suggestionsBox, inputBox)
	}

	leftCol := lipgloss.NewStyle().
		Border(baseBorder, false, true, false, false).
		BorderForeground(colorDim).
		Render(stack)

	rightPanel := renderCoCreatePromptPanel(rightW, height, state)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftCol, rightPanel)
}

func extractReplyForDisplay(content string) string {
	rest := content
	if rIdx := strings.Index(content, "<reply>"); rIdx >= 0 {
		rest = content[rIdx+len("<reply>"):]
	}
	if cIdx := strings.Index(rest, "</reply>"); cIdx >= 0 {
		return strings.TrimSpace(rest[:cIdx])
	}
	cut := len(rest)
	for _, mark := range []string{"<draft>", "<ready>", "<suggestions>"} {
		if idx := strings.Index(rest, mark); idx >= 0 && idx < cut {
			cut = idx
		}
	}
	if cut == len(rest) && !strings.Contains(content, "<") {
		return content
	}
	return strings.TrimSpace(rest[:cut])
}

func renderCoCreateSuggestions(width int, state *cocreateState) string {
	if state == nil || state.awaiting {
		return ""
	}
	sugs := state.suggestions()
	if len(sugs) == 0 {
		return ""
	}
	if len(sugs) > 3 {
		sugs = sugs[:3]
	}

	digits := []string{"❶", "❷", "❸"}
	digitStyle := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	bodyStyle := lipgloss.NewStyle().Foreground(colorMuted)
	hintStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)

	lines := []string{hintStyle.Render("Gợi ý từ AI (nhấn phím 1/2/3 để chọn, có thể chỉnh sửa trước khi gửi):")}
	for i, s := range sugs {
		lines = append(lines, digitStyle.Render(digits[i]+" ")+bodyStyle.Render(strings.TrimSpace(s)))
	}

	return lipgloss.NewStyle().
		Width(width-2).
		Padding(0, 2).
		Render(strings.Join(lines, "\n"))
}

func coCreateModalSize(width, height int) (boxW, boxH int) {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 24
	}
	boxW = min(max(width*76/100, 88), width-4)
	boxH = min(max(height*72/100, 22), height-4)
	if boxW < 64 {
		boxW = max(width-2, 42)
	}
	if boxH < 14 {
		boxH = max(height-2, 12)
	}
	return boxW, boxH
}

func coCreateInputWidth(width, height int) int {
	boxW, _ := coCreateModalSize(width, height)
	bodyW := boxW - 4
	leftW, _ := coCreateColumns(bodyW)
	inputW := leftW - 9
	if inputW < 20 {
		inputW = 20
	}
	return inputW
}

func renderCoCreateModal(width, height int, state *cocreateState, errMsg, inputView string, spinnerFrame int, quitPending bool) string {
	if state == nil {
		return ""
	}

	boxW, boxH := coCreateModalSize(width, height)
	contentH := boxH - 5
	if contentH < 10 {
		contentH = 10
	}

	titleText, subtitleText := "Đồng Sáng Tác Kế hoạch", "Trao đổi làm rõ nhu cầu trước khi bắt đầu viết truyện"
	if state.stage {
		titleText, subtitleText = "Đồng Sáng Tác Giai Đoạn", "Lập kế hoạch các bước tiếp theo trước khi tiếp tục sáng tác"
	}
	headerStyle := lipgloss.NewStyle().Width(boxW).AlignHorizontal(lipgloss.Center)
	title := headerStyle.Foreground(colorMuted).Bold(true).Render(titleText)
	subtitle := headerStyle.Foreground(colorDim).Italic(true).Render(subtitleText)

	var hintLine string
	hintStyle := lipgloss.NewStyle().Width(boxW).AlignHorizontal(lipgloss.Center)
	if quitPending {
		hintLine = hintStyle.Foreground(lipgloss.Color("243")).Bold(true).Render("Nhấn Ctrl+C một lần nữa để thoát")
	} else {
		hintLine = hintStyle.Foreground(colorDim).Italic(true).Render(coCreateHint(state))
	}

	body := renderCoCreateBody(boxW-4, contentH, state, errMsg, inputView, spinnerFrame)
	box := lipgloss.NewStyle().
		Width(boxW).
		Height(contentH).
		Border(baseBorder).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Render(body)

	stack := lipgloss.JoinVertical(lipgloss.Center, title, subtitle, "", box, "", hintLine)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, stack)
}

func coCreateHint(state *cocreateState) string {
	switch {
	case state == nil:
		return "Enter Gửi · Esc Thoát"
	case state.awaiting:
		return "AI đang phản hồi · ↑↓ Cuộn hội thoại · Esc Thoát"
	case state.canStart():
		action := "Ctrl+S Bắt đầu sáng tác"
		if state.stage {
			action = "Ctrl+S Áp dụng & Tiếp tục"
		}
		return "Enter Tiếp tục bổ sung · " + action + " · ↑↓ Cuộn hội thoại · Esc Thoát"
	default:
		return "Enter Gửi · ↑↓ Cuộn hội thoại · Esc Thoát"
	}
}

func renderCoCreateConversationPanel(width, height int, state *cocreateState, errMsg string, spinnerFrame int) string {
	contentW := width - 2
	if contentW < 12 {
		contentW = 12
	}
	wrapW := max(12, contentW-4)

	userRole := lipgloss.NewStyle().Foreground(colorAccent2).Bold(true).Render("Bạn")
	aiRole := lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("AI")
	userBody := lipgloss.NewStyle().Foreground(colorAccent2)
	aiBody := lipgloss.NewStyle().Foreground(bodyTextColor)
	thinkingStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)
	thinkingTag := lipgloss.NewStyle().Foreground(colorDim).Bold(true).Render("AI suy nghĩ")
	sysStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)

	var lines []string
	for i, item := range state.session.History() {
		isUser := item.Role != "assistant"
		if isUser && state.stage && i == 0 {
			for j, line := range wrapStreamText(stageCoCreateSystemLine, wrapW) {
				prefix := "• "
				if j > 0 {
					prefix = "  "
				}
				lines = append(lines, sysStyle.Render(prefix+line))
			}
			lines = append(lines, "")
			continue
		}
		if isUser {
			lines = append(lines, userRole)
			for _, line := range wrapStreamText(strings.TrimSpace(item.Content), wrapW) {
				lines = append(lines, userBody.Render("▌ "+line))
			}
		} else {
			lines = append(lines, aiRole)
			display := extractReplyForDisplay(item.Content)
			for _, line := range wrapStreamText(strings.TrimSpace(display), wrapW) {
				lines = append(lines, aiBody.Render("  "+line))
			}
		}
		lines = append(lines, "")
	}

	if state.awaiting {
		if t := state.session.StreamThinking(); t != "" {
			lines = append(lines, thinkingTag)
			for _, line := range wrapStreamText(t, wrapW) {
				lines = append(lines, thinkingStyle.Render("  "+line))
			}
			lines = append(lines, "")
		}
		if state.streamReply() != "" {
			lines = append(lines, aiRole)
			for _, line := range wrapStreamText(state.streamReply(), wrapW) {
				lines = append(lines, aiBody.Render("  "+line))
			}
			lines = append(lines, "")
		}
		lines = append(lines, strings.TrimLeft(renderEventSparkle(spinnerFrame, contentW), " "))
	}
	if errMsg != "" {
		lines = append(lines, "")
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render("! "+errMsg))
	}

	vpH := height - 1
	if vpH < 1 {
		vpH = 1
	}
	if state.convVP.Width != contentW || state.convVP.Height != vpH {
		state.convVP.Width = contentW
		state.convVP.Height = vpH
	}
	state.convVP.SetContent(strings.Join(lines, "\n"))
	if state.convFollow {
		state.convVP.GotoBottom()
	}

	style := lipgloss.NewStyle().
		Width(contentW).
		Height(height).
		Padding(0, 1)
	return style.Render(panelTitleStyle.Render(":: Hội Thoại Đồng Sáng Tác") + "\n" + state.convVP.View())
}

func renderCoCreatePromptPanel(width, height int, state *cocreateState) string {
	readyLabel := "Đã sẵn sàng sáng tác"
	if state.stage {
		readyLabel = "Đã có thể áp dụng & tiếp tục"
	}
	status := lipgloss.NewStyle().Foreground(colorDim).Render("Đang tiếp tục trao đổi...")
	if state.ready() {
		status = lipgloss.NewStyle().Foreground(colorAccent).Render(readyLabel)
	}
	if state.awaiting {
		status = lipgloss.NewStyle().Foreground(colorMuted).Italic(true).Render("AI đang tổng hợp...")
	}

	contentW := width - 2
	if contentW < 8 {
		contentW = 8
	}

	emptyHint := "AI sẽ liên tục tổng hợp bản yêu cầu chỉ thị sáng tác hoàn chỉnh tại đây."
	panelTitle := ":: Chỉ Thị Sáng Tác Hiện Tại"
	if state.stage {
		emptyHint = "AI sẽ liên tục tổng hợp bản định hướng giai đoạn tiếp theo tại đây."
		panelTitle = ":: Định Hướng Giai Đoạn Tiếp Theo"
	}
	text := strings.TrimSpace(state.draftPrompt())
	if text == "" {
		text = lipgloss.NewStyle().Foreground(colorDim).Italic(true).Render(emptyHint)
	} else {
		text = renderMarkdownPreview(text, max(12, contentW-2))
	}
	vpHeight := height - 5
	if vpHeight < 3 {
		vpHeight = 3
	}
	if state.promptVP.Width != contentW || state.promptVP.Height != vpHeight {
		state.promptVP.Width = contentW
		state.promptVP.Height = vpHeight
	}
	state.promptVP.MouseWheelEnabled = true
	state.promptVP.SetContent(text)

	hint := ""
	if state.promptVP.TotalLineCount() > state.promptVP.VisibleLineCount() {
		switch {
		case state.promptVP.AtTop():
			hint = "↓ Còn nội dung bên dưới, có thể cuộn chuột hoặc PgDn để xem"
		case state.promptVP.AtBottom():
			hint = "↑ Còn nội dung bên trên, có thể cuộn chuột hoặc PgUp để xem"
		default:
			hint = "↑↓ Có thể tiếp tục cuộn để xem thêm"
		}
	}

	style := lipgloss.NewStyle().
		Width(contentW).
		Height(height).
		Padding(0, 1)

	body := panelTitleStyle.Render(panelTitle) + "\n" + status + "\n\n" + state.promptVP.View()
	if hint != "" {
		body += "\n\n" + lipgloss.NewStyle().
			Width(contentW).
			AlignHorizontal(lipgloss.Center).
			Foreground(colorDim).
			Italic(true).
			Render(hint)
	}
	return style.Render(body)
}

func renderMarkdownPreview(text string, width int) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}

	h1Style := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	h2Style := lipgloss.NewStyle().Foreground(colorAccent2).Bold(true)
	h3Style := lipgloss.NewStyle().Foreground(colorMuted).Bold(true)
	bulletStyle := lipgloss.NewStyle().Foreground(colorAccent2).Bold(true)
	codeStyle := lipgloss.NewStyle().Foreground(colorMuted).Italic(true)

	var out []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			out = append(out, "")
			continue
		}

		switch {
		case strings.HasPrefix(line, "# "):
			title := strings.TrimSpace(strings.TrimPrefix(line, "# "))
			out = append(out, h1Style.Render(title))
		case strings.HasPrefix(line, "## "):
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			out = append(out, h2Style.Render(title))
		case strings.HasPrefix(line, "### "):
			title := strings.TrimSpace(strings.TrimPrefix(line, "### "))
			out = append(out, h3Style.Render(title))
		case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "):
			body := strings.TrimSpace(line[2:])
			wrapped := wrapStreamText(body, max(8, width-4))
			for i, item := range wrapped {
				if i == 0 {
					out = append(out, bulletStyle.Render("• ")+cardContentStyle.Render(item))
				} else {
					out = append(out, "  "+cardContentStyle.Render(item))
				}
			}
		case isOrderedMarkdownItem(line):
			prefix, body := splitOrderedMarkdownItem(line)
			wrapped := wrapStreamText(body, max(8, width-len(prefix)-2))
			for i, item := range wrapped {
				if i == 0 {
					out = append(out, bulletStyle.Render(prefix+" ")+cardContentStyle.Render(item))
				} else {
					out = append(out, strings.Repeat(" ", len(prefix)+1)+cardContentStyle.Render(item))
				}
			}
		case strings.HasPrefix(line, "> "):
			body := strings.TrimSpace(strings.TrimPrefix(line, "> "))
			for _, item := range wrapStreamText(body, max(8, width-4)) {
				out = append(out, codeStyle.Render("│ "+item))
			}
		default:
			for _, item := range wrapStreamText(line, width) {
				out = append(out, cardContentStyle.Render(item))
			}
		}
	}
	return strings.Join(out, "\n")
}

func isOrderedMarkdownItem(line string) bool {
	if len(line) < 3 {
		return false
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' '
}

func splitOrderedMarkdownItem(line string) (prefix, body string) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) {
		return "", strings.TrimSpace(line)
	}
	return line[:i+1], strings.TrimSpace(line[i+2:])
}
