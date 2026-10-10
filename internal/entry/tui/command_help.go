package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type helpState struct {
	viewport viewport.Model
}

func newHelpState(width, height int) *helpState {
	boxW, boxH := reportModalSize(width, height)
	contentW := paddedModalContentWidth(boxW)
	text := renderHelpText(contentW)

	vp := viewport.New(contentW, boxH-4)
	vp.SetContent(text)
	return &helpState{viewport: vp}
}

func renderHelpText(width int) string {
	titleStyle := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	nameStyle := lipgloss.NewStyle().Foreground(colorAccent2).Bold(true)
	usageStyle := lipgloss.NewStyle().Foreground(colorMuted)
	descStyle := lipgloss.NewStyle().Foreground(bodyTextColor)
	hintStyle := lipgloss.NewStyle().Foreground(colorDim)

	var b strings.Builder
	b.WriteString(titleStyle.Render("TRỢ GIÚP CÁC LỆNH HỆ THỐNG"))
	b.WriteString("\n\n")

	for i, spec := range commandSpecs() {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(nameStyle.Render("/" + spec.Name))
		if len(spec.Aliases) > 0 {
			b.WriteString(usageStyle.Render("  alias: /" + strings.Join(spec.Aliases, " /")))
		}
		b.WriteString("\n")
		b.WriteString(usageStyle.Render("Cú pháp: " + spec.Usage))
		b.WriteString("\n")
		b.WriteString(descStyle.Render(wrapText(spec.Description, width)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(titleStyle.Render("PHÍM TẮT TIỆN ÍCH"))
	b.WriteString("\n\n")
	for _, line := range []string{
		"• Gõ / để mở bảng gợi ý và tìm kiếm lệnh nhanh",
		"• Phím ↑↓ để chọn lệnh trong danh sách gợi ý",
		"• Phím Tab hoặc Enter để hoàn thành lệnh",
		"• Phím Esc để đóng bảng lệnh / modal hiện tại",
		"• Phím Tab ở màn hình chính: Chuyển đổi giữa chế độ Nhanh và Đồng sáng tác",
		"• Ctrl+R: Bật/tắt chế độ chọn sao chép văn bản (tắt bắt chuột để bôi đen copy, nhấn lại để khôi phục)",
		"• Ctrl+C (2 lần): Lưu tiến độ an toàn rồi thoát ứng dụng",
	} {
		b.WriteString(hintStyle.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func renderHelpModal(width, height int, state *helpState) string {
	if state == nil {
		return ""
	}

	boxW, boxH := reportModalSize(width, height)
	contentW := paddedModalContentWidth(boxW)

	if state.viewport.Width != contentW {
		state.viewport.Width = contentW
	}
	if state.viewport.Height != boxH-4 {
		state.viewport.Height = boxH - 4
	}

	modal := renderPaddedModalFrame(
		boxW,
		boxH,
		"Hướng dẫn lệnh",
		"  ↑↓ Cuộn · Esc Đóng",
		strings.Split(state.viewport.View(), "\n"),
	)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
}

func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.help == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.help = nil
		return m, m.textarea.Focus()
	case tea.KeyUp:
		m.help.viewport.ScrollUp(1)
		return m, nil
	case tea.KeyDown:
		m.help.viewport.ScrollDown(1)
		return m, nil
	case tea.KeyPgUp:
		m.help.viewport.HalfPageUp()
		return m, nil
	case tea.KeyPgDown:
		m.help.viewport.HalfPageDown()
		return m, nil
	default:
		return m, nil
	}
}
