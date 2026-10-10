package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/utils"
)

const maxEvents = 500

// maxStreamRounds limits how many rounds the stream panel keeps. Each LLM call ending triggers a streamClear
// that opens a new round; a single-chapter writer takes about 3~5 rounds (agent header / thinking / draft / commit), so 32 rounds is roughly
// a look back over the streaming output of the last 6~10 chapters. Committed chapter text is persisted in store/drafts; anything beyond the limit is dropped so that
// each token delta does not trigger an O(full text) re-render. Steady-state memory is capped at about 512KB, far below the lag threshold.
const maxStreamRounds = 32

type focusPane int

const (
	focusEvents focusPane = iota
	focusStream
	focusDetail
	focusState // Left status sidebar (scrollable)

	focusPaneCount // Total number of focus targets, used for Tab cycling
)

type appMode int

const (
	modeNew     appMode = iota // Waiting for the user to enter the novel request
	modeRunning                // Creating (including stopped on error; input can resume)
	modeDone                   // Creation complete
)

// Spinner frame sequence shared by the top bar / streaming activity (bubbles.Spinner.MiniDot).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner frame sequence dedicated to the "in progress" rows of the event stream (bubbles.Spinner.Dot).
// 7 dots and 1 gap rotate clockwise around a 3x3 grid, visually like a complete loading circle.
// It uses its own frame index and a faster tick, and does not affect the rhythm of the top bar and star animations.
var toolSpinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// Model is the top-level state of the TUI.
type Model struct {
	runtime        *host.Host
	cocreate       *cocreateState
	help           *helpState
	modelSwitch    *modelSwitchState
	modelConfig    *modelConfigState
	report         *reportState
	version        string
	importer       *importState
	importSeq      int
	simulator      *simulationState
	simSeq         int
	compItems      []commandPaletteItem
	compIdx        int
	compActive     bool
	commandToken   string // The currently registered command token; only this segment is rendered, arguments are not colored
	snapshot       host.UISnapshot
	events         []host.Event
	eventIndex     map[string]int   // event.ID -> index in m.events; call-type events are updated in place when they arrive
	viewport       viewport.Model   // Event stream viewport
	streamVP       viewport.Model   // Streaming output viewport
	detailVP       viewport.Model   // Right detail viewport
	stateVP        viewport.Model   // Left status sidebar viewport (scrollable)
	streamBuf      *strings.Builder // Accumulated streaming text buffer
	streamRounds   []string
	textarea       textarea.Model
	width          int
	height         int
	autoScroll     bool
	streamScroll   bool      // Stream panel auto-follow
	streamDirty    bool      // streamRounds has deltas not yet flushed
	flushPending   bool      // A stream flush has been scheduled, avoiding restarting the timer for every delta
	lastKeyAt      time.Time // Time of the last non-Enter key; throttles KeyEnter so a pasted \n stream does not trigger a submit by mistake
	inputHistory   []string  // Submitted input history (deduplicated: adjacent entries are not repeated)
	historyIdx     int       // Current browse index; == len(inputHistory) means "not browsing, editing the draft"
	historyDraft   string    // Draft saved before entering history browsing, restored when returning to the end
	focusPane      focusPane
	hoverPane      focusPane
	hoverActive    bool
	mode           appMode
	starting       bool // The UI has entered the workbench and Host is running startup initialization
	startupMode    startupMode
	importHint     string // Hint that an unfinished import was detected at startup (shown on the welcome screen; cleared once an import is started)
	cocreateSeq    int
	reportSeq      int
	err            error
	spinnerIdx     int
	toolSpinnerIdx int  // Independent frame index for the in-progress rows of the event stream (150ms tick, does not affect the top bar / stars)
	toolTicking    bool // Tool animation timer has started; stops automatically when there are no running events
	cursorIdx      int  // Streaming cursor frame index (advances with the main animation)
	streamRound    int  // Streaming output round counter
	quitPending    bool // Double Ctrl+C exit confirmation
	abortPending   bool // A manual pause waiting for Done to return
	mouseOff       bool // When true, mouse reporting is disabled so the user can drag-select and copy natively; toggling again restores it
}

// NewModel creates the TUI Model.
func NewModel(rt *host.Host, version string) Model {
	ta := textarea.New()
	ta.Placeholder = placeholderForNewMode(startupModeQuick)
	ta.CharLimit = 5000
	ta.SetHeight(1)
	// MaxHeight=6 makes overlong input auto-wrap by width and display as multiple lines (visual cap of 6 lines).
	ta.MaxHeight = 6
	ta.ShowLineNumbers = false
	ta.Focus()

	// By default Enter does not insert a newline (handleEnterKey submits);
	// explicit newline is rebound to ctrl+j (unix \n) and alt+enter (GUI habit).
	// The terminal protocol cannot distinguish Shift+Enter from Enter, so Shift+Enter is not supported.
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")

	vp := viewport.New(80, 20)
	vp.SetContent("")

	svp := viewport.New(80, 10)
	svp.SetContent("")

	dvp := viewport.New(40, 20)
	dvp.SetContent("")

	stvp := viewport.New(32, 20)
	stvp.SetContent("")

	// Detect an unfinished import once at startup (LoadState recomputes the artifact digest, not part of snapshot polling);
	// if a half-finished book is not announced proactively, the user only discovers it when creation is rejected by a gate (RFC §18.2).
	importHint := ""
	if rt != nil {
		importHint = rt.ImportResumeHint()
	}

	return Model{
		runtime:      rt,
		version:      strings.TrimSpace(version),
		autoScroll:   true,
		streamScroll: true,
		mode:         modeNew,
		startupMode:  startupModeQuick,
		importHint:   importHint,
		textarea:     ta,
		viewport:     vp,
		streamVP:     svp,
		detailVP:     dvp,
		stateVP:      stvp,
		streamBuf:    &strings.Builder{},
		eventIndex:   make(map[string]int),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		listenEvents(m.runtime),
		listenDone(m.runtime),
		listenStream(m.runtime),
		tickSnapshot(m.runtime),
		bootstrapRuntime(m.runtime),
		tickSpinner(),
	)
}

func (m *Model) paneAtMouse(x, y int) (focusPane, bool) {
	if m.width == 0 || m.height == 0 {
		return focusEvents, false
	}

	topH, _, bodyH := m.layoutHeights()
	if bodyH < 1 {
		return focusEvents, false
	}

	bodyStartY := topH
	bodyEndY := topH + bodyH
	if y < bodyStartY || y >= bodyEndY {
		return focusEvents, false
	}

	leftW := m.sidebarWidth()
	rightW := m.detailWidth()
	centerStartX := leftW
	rightStartX := m.width - rightW

	if x >= rightStartX {
		return focusDetail, true
	}
	if x < centerStartX {
		return focusState, true
	}

	eventH, _ := m.splitHeights(bodyH)
	if y-bodyStartY < eventH {
		return focusEvents, true
	}
	return focusStream, true
}

func (m *Model) paneHighlighted(pane focusPane) bool {
	if m.focusPane == pane {
		return true
	}
	return m.hoverActive && m.hoverPane == pane
}

// hasRunningEvent reports whether any call-type event is unfinished (its spinner is still turning).
// toolSpinnerTick uses this to decide whether a re-render is worthwhile: with no running events the spinner frame does not affect the output,
// and the whole refreshEventViewport would be definitively wasted work.
func (m *Model) hasRunningEvent() bool {
	for i := range m.events {
		if m.events[i].Running() {
			return true
		}
	}
	return false
}

// flushStreamIfDirty renders the accumulated streamRounds into the viewport and marks them flushed.
// It returns whether it actually flushed, so the caller can decide whether to GotoBottom.
func (m *Model) flushStreamIfDirty() bool {
	if !m.streamDirty {
		return false
	}
	m.refreshStreamViewport()
	m.streamDirty = false
	return true
}

// refreshEventViewport re-renders the event stream content and sets the viewport.
func (m *Model) refreshEventViewport() {
	centerW := m.eventFlowWidth()
	content := renderEventContent(m.events, centerW, m.toolSpinnerIdx)
	snap := m.snapshot
	if m.starting {
		snap.IsRunning = true
	}
	if activity := renderEventActivity(snap, m.spinnerIdx, centerW); activity != "" {
		if strings.TrimSpace(content) != "" {
			content += "\n" + activity
		} else {
			content = activity
		}
	}
	m.viewport.SetContent(content)
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

func (m *Model) refreshStreamViewport() {
	cursor := ""
	if m.snapshot.IsRunning {
		cursor = renderStreamCursor(m.cursorIdx)
	}
	m.streamVP.SetContent(renderStreamContent(m.streamRounds, m.streamVP.Width, cursor))
}

func (m *Model) refreshDetailViewport() {
	rightW := m.detailWidth()
	if rightW <= 4 {
		return
	}
	m.detailVP.SetContent(renderDetailContent(m.snapshot, rightW-4))
}

// refreshStateViewport pushes the left status sidebar content into the viewport.
// The sidebar content is derived purely from the snapshot, so it must be refreshed when either the snapshot or the size changes.
func (m *Model) refreshStateViewport() {
	leftW := m.sidebarWidth()
	if leftW <= 4 {
		return
	}
	m.stateVP.SetContent(renderStateContent(m.snapshot, leftW-4))
}

// updateViewportSize updates the viewport sizes from the current window size.
func (m *Model) updateViewportSize() {
	centerW := m.eventFlowWidth()
	rightW := m.detailWidth()
	bodyH := m.bodyHeight()
	eventH, streamH := m.splitHeights(bodyH)
	m.viewport.Width = centerW - 2
	m.viewport.Height = eventH - 1 // -1 for the event panel header row
	m.streamVP.Width = centerW - 2
	m.streamVP.Height = streamH - 1 // -1 for the stream panel header row
	m.detailVP.Width = rightW - 2
	m.detailVP.Height = bodyH
	leftW := m.sidebarWidth()
	m.stateVP.Width = max(1, leftW-2)
	m.stateVP.Height = max(1, bodyH-1) // -1 for the top margin; the bottom row shows content directly
	// After the height or content shrinks, the freely scrolling left and right columns may sit at an out-of-range offset (bubbles'
	// SetContent only guards against going past the last line), and the viewport pads the bottom with blank lines. SetYOffset clamps itself.
	m.stateVP.SetYOffset(m.stateVP.YOffset)
	m.detailVP.SetYOffset(m.detailVP.YOffset)
}

// splitHeights computes the height allocation between the event stream and the streaming output.
func (m *Model) splitHeights(bodyH int) (eventH, streamH int) {
	eventH = bodyH * 40 / 100
	if eventH < 3 {
		eventH = 3
	}
	streamH = bodyH - eventH - 1 // -1 for the separator line
	if streamH < 3 {
		streamH = 3
	}
	return
}

func (m *Model) inputWidth() int {
	if m.width == 0 {
		return 60
	}
	return m.width - 6 // border + padding + prompt "❯ "
}

func (m *Model) currentInputWidth() int {
	if m.cocreate != nil {
		return coCreateInputWidth(m.width, m.height)
	}
	return m.inputWidth()
}

// refitTextareaHeight estimates the number of visual rows from the current content and dynamically calls SetHeight.
// Visual rows = the sum of each logical line (split on \n) after wrapping by width. Together with MaxHeight=6
// this gives "overlong content / explicit newlines auto-expand to multiple lines, at most 6".
func (m *Model) refitTextareaHeight() {
	w := m.textarea.Width()
	if w <= 0 {
		return
	}
	// In co-create mode the input is fixed to 1 row: multi-line textarea content is scrolled by the textarea itself
	// following the cursor. Otherwise the inputBox height would follow the content, shrinking the left conversation column and
	// making the input drift vertically, breaking layout stability.
	if m.cocreate != nil {
		m.textarea.SetHeight(1)
		return
	}
	text := m.textarea.Value()
	if text == "" {
		m.textarea.SetHeight(1)
		return
	}
	// Subtract 2 redundant columns (the textarea's internal prompt symbol + cursor); being off by one extra row is acceptable.
	contentW := w - 2
	if contentW < 1 {
		contentW = 1
	}
	total := 0
	for line := range strings.SplitSeq(text, "\n") {
		lw := lipgloss.Width(line)
		if lw == 0 {
			total++
			continue
		}
		total += (lw + contentW - 1) / contentW
	}
	if total < 1 {
		total = 1
	}
	m.textarea.SetHeight(total) // SetHeight clamps by MaxHeight internally
}

// resizeTextarea sets the width and the content-based height together.
// Replaces the scattered SetWidth(currentInputWidth()) calls, ensuring the height follows when the width changes.
func (m *Model) resizeTextarea() {
	m.textarea.SetWidth(m.currentInputWidth())
	m.refitTextareaHeight()
}

// maxInputHistory limits the history length, avoiding memory growth in long sessions.
const maxInputHistory = 200

// pushInputHistory appends a successfully submitted entry to the history, deduplicating adjacent entries. It also resets the browse index.
func (m *Model) pushInputHistory(text string) {
	if text == "" {
		return
	}
	if n := len(m.inputHistory); n == 0 || m.inputHistory[n-1] != text {
		m.inputHistory = append(m.inputHistory, text)
		if len(m.inputHistory) > maxInputHistory {
			m.inputHistory = m.inputHistory[len(m.inputHistory)-maxInputHistory:]
		}
	}
	m.historyIdx = len(m.inputHistory)
	m.historyDraft = ""
}

// tryHistoryUp moves to an older history entry; it returns whether the key was handled.
// On first entering history browsing it saves the current textarea content as the draft, and restores it when returning to the end.
// The caller must decide for itself whether to bypass this in multi-line cases (letting the textarea handle in-line cursor movement).
func (m *Model) tryHistoryUp() bool {
	if len(m.inputHistory) == 0 || m.historyIdx <= 0 {
		return false
	}
	if m.historyIdx == len(m.inputHistory) {
		m.historyDraft = m.textarea.Value()
	}
	m.historyIdx--
	m.textarea.SetValue(m.inputHistory[m.historyIdx])
	m.textarea.CursorEnd()
	m.syncCommandInputHighlight()
	m.refitTextareaHeight()
	return true
}

// tryHistoryDown moves to a newer history entry; reaching the end restores the draft.
func (m *Model) tryHistoryDown() bool {
	if m.historyIdx >= len(m.inputHistory) {
		return false
	}
	m.historyIdx++
	if m.historyIdx == len(m.inputHistory) {
		m.textarea.SetValue(m.historyDraft)
		m.historyDraft = ""
	} else {
		m.textarea.SetValue(m.inputHistory[m.historyIdx])
	}
	m.textarea.CursorEnd()
	m.syncCommandInputHighlight()
	m.refitTextareaHeight()
	return true
}

// textareaIsMultiline reports whether the current textarea content contains explicit newlines; used to decide whether ↑↓ walks history or moves within the text.
func (m *Model) textareaIsMultiline() bool {
	return strings.Contains(m.textarea.Value(), "\n")
}

// inputHints generates the bottom hint text from the current state.
// copySuffix is always appended at the end, so the user can see how to select and copy in any non-urgent state;
// when the mouse is off, a prominent red hint reminds them to press the key again to restore mouse interaction.
func (m *Model) inputHints() string {
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	if m.quitPending {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Bold(true).Render("Nhấn Ctrl+C một lần nữa để thoát")
	}
	limitHint := m.inputLimitHint()
	suffix := limitHint + " · Ctrl+R Chế độ copy"
	if m.mode == modeNew {
		suffix = limitHint
	}
	if m.mouseOff && m.mode != modeNew {
		return lipgloss.NewStyle().Foreground(colorAccent).Bold(true).
			Render("✂ Chế độ bôi đen copy: Kéo chuột chọn văn bản để sao chép · Ctrl+R Quay lại")
	}
	if m.cocreate != nil {
		scrollHint := " · Tab Cuộn hội thoại"
		if m.cocreate.focusPrompt {
			scrollHint = " · Tab Cuộn chỉ thị"
		}
		switch {
		case m.cocreate.awaiting:
			return dimStyle.Render("Chờ AI phản hồi · Esc Thoát đồng sáng tác" + scrollHint + suffix)
		case m.cocreate.canStart():
			startLabel := "Ctrl+S Bắt đầu sáng tác"
			if m.cocreate.stage {
				startLabel = "Ctrl+S Áp dụng & tiếp tục"
			}
			return dimStyle.Render("Enter Gửi · " + startLabel + " · Esc Thoát" + scrollHint + suffix)
		default:
			return dimStyle.Render("Enter Gửi · Esc Thoát đồng sáng tác" + scrollHint + suffix)
		}
	}
	if m.mode == modeNew {
		if m.startupMode == startupModeQuick {
			return dimStyle.Render("Tab Đổi chế độ · Gõ / để xem lệnh · Enter Bắt đầu viết ngay · Esc Xóa" + suffix)
		}
		return dimStyle.Render("Tab Đổi chế độ · Gõ / để xem lệnh · Enter Bắt đầu đồng sáng tác · Esc Xóa" + suffix)
	}
	switch m.snapshot.RuntimeState {
	case "pausing":
		return dimStyle.Render("Đang tạm dừng sáng tác · Vui lòng chờ vòng hiện tại kết thúc" + suffix)
	case "paused":
		return dimStyle.Render("Gõ / để xem lệnh · Enter Tiếp tục sáng tác · Esc Xóa" + suffix)
	}
	return dimStyle.Render("Gõ / để xem lệnh · Tab/click đổi panel · ↑↓ Cuộn · End Về cuối · Ctrl+L Xóa màn hình · Esc Tạm dừng · Enter Gửi" + suffix)
}

func (m *Model) inputLimitHint() string {
	limit := m.textarea.CharLimit
	if limit <= 0 {
		return ""
	}
	used := m.textarea.Length()
	if used < limit*4/5 {
		return ""
	}
	return fmt.Sprintf(" · Đã nhập %d/%d", used, limit)
}

func (m *Model) eventFlowWidth() int {
	if m.width == 0 {
		return 80
	}
	leftW := m.sidebarWidth()
	rightW := m.detailWidth()
	return m.width - leftW - rightW
}

func (m *Model) sidebarWidth() int {
	if m.width == 0 {
		return 32
	}
	return m.width * 23 / 100
}

func (m *Model) detailWidth() int {
	if m.width == 0 {
		return 40
	}
	return m.width * 27 / 100
}

func (m *Model) bodyHeight() int {
	_, _, bodyH := m.layoutHeights()
	return bodyH
}

func (m *Model) currentSpinnerFrame() string {
	if !m.snapshot.IsRunning && !m.starting {
		return ""
	}
	return spinnerFrames[m.spinnerIdx%len(spinnerFrames)]
}

func (m *Model) outputDir() string {
	if m.runtime == nil {
		return ""
	}
	return m.runtime.Dir()
}

func defaultSteerPlaceholder() string {
	return "Nhập can thiệp cốt truyện, ví dụ: đẩy tuyến tình cảm lên chương 4"
}

func (m *Model) syncRuntimePlaceholder() {
	if m.mode != modeRunning || m.cocreate != nil {
		return
	}
	if m.starting {
		m.textarea.Placeholder = "Đang khởi tạo sáng tác..."
		return
	}
	switch m.snapshot.RuntimeState {
	case "completed":
		m.textarea.Placeholder = donePlaceholder
	case "pausing":
		m.textarea.Placeholder = "Đang tạm dừng sáng tác..."
	case "paused":
		if m.snapshot.AdvanceMode == "review" && m.snapshot.Phase == "writing" {
			m.textarea.Placeholder = "Chờ duyệt: nhập ý kiến chỉnh sửa, hoặc /next để duyệt chương tiếp"
		} else {
			m.textarea.Placeholder = "Sáng tác đã tạm dừng, nhập nội dung bất kỳ để tiếp tục"
		}
	default:
		if !m.snapshot.IsRunning {
			if m.snapshot.AdvanceMode == "review" && m.snapshot.Phase == "writing" {
				m.textarea.Placeholder = "Chờ duyệt: nhập ý kiến chỉnh sửa, hoặc /next để duyệt chương tiếp"
			} else {
				m.textarea.Placeholder = "Sáng tác bị gián đoạn, nhập nội dung bất kỳ để tiếp tục"
			}
		} else {
			m.textarea.Placeholder = defaultSteerPlaceholder()
		}
	}
}

func (m *Model) renderBottomBar() string {
	inputView := highlightCommandToken(m.textarea.View(), m.textarea.Value(), m.commandToken)
	inputBox := renderInputBox(
		inputView,
		m.inputHints(),
		m.snapshot,
		m.outputDir(),
		m.width,
	)
	if m.mode != modeNew || m.cocreate != nil {
		return inputBox
	}
	return renderStartupModeBar(m.width, m.startupMode) + "\n" + inputBox
}

func (m *Model) layoutHeights() (topH, inputH, bodyH int) {
	if m.width == 0 || m.height == 0 {
		return 1, 4, 20
	}
	topH = lipgloss.Height(renderTopBar(m.snapshot, m.width, m.currentSpinnerFrame(), m.version))
	inputH = lipgloss.Height(m.renderBottomBar())
	bodyH = m.height - topH - inputH
	if bodyH < 3 {
		bodyH = 3
	}
	return
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Đang tải..."
	}
	if m.width < 100 {
		return lipgloss.NewStyle().
			Width(m.width).Height(m.height).
			AlignHorizontal(lipgloss.Center).
			AlignVertical(lipgloss.Center).
			Render("Độ rộng terminal quá nhỏ, vui lòng mở rộng tối thiểu 100 cột")
	}
	if m.cocreate != nil {
		return renderCoCreateModal(m.width, m.height, m.cocreate, errorText(m.err), m.textarea.View(), m.spinnerIdx, m.quitPending)
	}
	if m.help != nil {
		return renderHelpModal(m.width, m.height, m.help)
	}
	if m.report != nil {
		return renderReportModal(m.width, m.height, m.report)
	}
	if m.importer != nil {
		// Import does not depend on the Engine run state; the animation frame takes spinnerIdx directly (currentSpinnerFrame returns empty when the engine is stopped).
		return renderImportModal(m.width, m.height, m.importer, m.spinnerIdx)
	}
	if m.simulator != nil {
		return renderSimulationModal(m.width, m.height, m.simulator)
	}

	topBar := renderTopBar(m.snapshot, m.width, m.currentSpinnerFrame(), m.version)
	inputBox := m.renderBottomBar()
	_, inputH, bodyH := m.layoutHeights()

	var body string
	if m.mode == modeNew {
		errMsg := ""
		if m.err != nil {
			errMsg = m.err.Error()
		}
		body = renderWelcome(m.width, bodyH, errMsg, m.startupMode, m.importHint)
	} else {
		leftW := m.sidebarWidth()
		rightW := m.detailWidth()
		centerW := m.width - leftW - rightW
		eventH, streamH := m.splitHeights(bodyH)

		if m.viewport.Width != centerW-2 || m.viewport.Height != eventH-1 {
			m.viewport.Width = centerW - 2
			m.viewport.Height = eventH - 1 // -1 for the event panel header row
		}
		if m.streamVP.Width != centerW-2 || m.streamVP.Height != streamH-1 {
			m.streamVP.Width = centerW - 2
			m.streamVP.Height = streamH - 1 // -1 for the stream panel header row
		}

		eventFlow := renderEventFlowViewport(m.viewport, centerW, eventH, m.paneHighlighted(focusEvents))
		streamPanel := renderStreamPanel(m.streamVP, centerW, streamH, m.paneHighlighted(focusStream), m.snapshot.IsRunning || m.starting, m.spinnerIdx)
		center := lipgloss.JoinVertical(lipgloss.Left, eventFlow, streamPanel)

		left := renderStatePanel(m.stateVP, leftW, bodyH, m.paneHighlighted(focusState))
		right := renderDetailPanel(m.detailVP, rightW, bodyH, m.paneHighlighted(focusDetail))
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, center, right)
	}

	view := lipgloss.JoinVertical(lipgloss.Left, topBar, body, inputBox)

	// Popup overlay: floats above the bottom of body without affecting the layout
	if m.modelSwitch != nil {
		commandBar := renderModelSwitchBar(m.width, m.modelSwitch)
		view = overlayAboveInput(view, commandBar, inputH)
	} else if m.modelConfig != nil {
		view = overlayAboveInput(view, renderModelConfigModal(m.width, m.modelConfig), inputH)
	} else if m.compActive {
		commandBar := renderCommandPalette(m.width, m.compItems, m.compIdx)
		view = overlayAboveInput(view, commandBar, inputH)
	}
	return view
}

// sendCoCreate starts one round of co-create request, handling reqID, textarea and placeholder in one place.
func (m *Model) sendCoCreate() tea.Cmd {
	m.cocreateSeq++
	m.cocreate.reqID = m.cocreateSeq
	m.cocreate.awaiting = true
	m.resizeTextarea()
	m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
	m.textarea.Blur()
	return runCoCreate(m.runtime, m.cocreate)
}

func (m Model) handleCoCreateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.cocreate == nil {
		return m, nil
	}
	state := m.cocreate

	// Keys ↑↓/PgUp/PgDn/Home/End scroll; Tab switches the scroll focus between the left conversation column ↔ the right creation-instruction column
	// (default left column, where the user reviews the main content). The welcome screen has mouse reporting off to keep native copy, so when the right column overflows, press Tab
	// to switch focus and scroll with the keyboard. Left column: scrolling up turns follow off, and scrolling to the bottom turns follow back on (streaming follow).
	switch msg.Type {
	case tea.KeyTab:
		state.focusPrompt = !state.focusPrompt
		return m, nil
	case tea.KeyUp, tea.KeyPgUp:
		if state.focusPrompt {
			var cmd tea.Cmd
			state.promptVP, cmd = state.promptVP.Update(msg)
			return m, cmd
		}
		state.convFollow = false
		var cmd tea.Cmd
		state.convVP, cmd = state.convVP.Update(msg)
		return m, cmd
	case tea.KeyDown, tea.KeyPgDown:
		if state.focusPrompt {
			var cmd tea.Cmd
			state.promptVP, cmd = state.promptVP.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		state.convVP, cmd = state.convVP.Update(msg)
		if state.convVP.AtBottom() {
			state.convFollow = true
		}
		return m, cmd
	case tea.KeyHome:
		if state.focusPrompt {
			state.promptVP.GotoTop()
			return m, nil
		}
		state.convFollow = false
		state.convVP.GotoTop()
		return m, nil
	case tea.KeyEnd:
		if state.focusPrompt {
			state.promptVP.GotoBottom()
			return m, nil
		}
		state.convFollow = true
		state.convVP.GotoBottom()
		return m, nil
	case tea.KeyEsc:
		return m.exitCoCreate()
	}

	// While waiting for the AI reply, editing keys (character input / backspace / cursor / Ctrl+U / multi-line newline) are let through --
	// so the user can pre-type the next sentence while the AI is thinking. Blocking of submit keys is pushed down into each case,
	// so that the Enter throttle runs before the awaiting block -- that way a pasted \n fragment can still be turned into a space.

	switch msg.Type {
	case tea.KeyCtrlS:
		if state.awaiting {
			return m, nil
		}
		if !state.canStart() {
			return m, nil
		}
		// Phase co-create: inject the "next-direction brief" and resume creation, returning to the running workbench.
		if state.stage {
			draft := state.draftPrompt()
			m.cocreate = nil
			m.err = nil
			m.resizeTextarea()
			m.textarea.Placeholder = defaultSteerPlaceholder()
			return m, tea.Batch(resumeFromCoCreate(m.runtime, draft), m.textarea.Focus())
		}
		// Cold-start co-create: start creation with the organized creation instructions.
		prompt, err := state.buildPrompt()
		if err != nil {
			m.err = err
			return m, nil
		}
		cmd := m.enterStarting(prompt)
		return m, tea.Batch(startRuntime(m.runtime, prompt), cmd)
	case tea.KeyEnter:
		// Alt+Enter -> explicit newline, handed to textarea.Update (KeyMap.InsertNewline is bound to this key)
		if msg.Alt {
			break
		}
		// Interval since the last character key is too short -> treat as a \n fragment of a paste stream: insert a space instead of submitting.
		// This must be decided before the awaiting block -- otherwise a pasted \n fragment during awaiting would be blocked,
		// swallowing "abc\ndef" into "abcdef", inconsistent with the base path semantics.
		if !m.lastKeyAt.IsZero() && time.Since(m.lastKeyAt) < 50*time.Millisecond {
			var cmd tea.Cmd
			state.resetSuggestionInput()
			m.textarea, cmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
			m.refitTextareaHeight()
			return m, cmd
		}
		// A genuine submit intent: blocked during awaiting (requests cannot be sent concurrently)
		if state.awaiting {
			return m, nil
		}
		text := utils.CleanInputLine(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		m.err = nil
		state.appendUser(text)
		m.textarea.Reset()
		m.refitTextareaHeight()
		cmd := m.sendCoCreate()
		return m, cmd
	case tea.KeyCtrlU:
		state.resetSuggestionInput()
		m.textarea.Reset()
		m.refitTextareaHeight()
		return m, nil
	}

	// Number keys 1/2/3 can combine suggestions in sequence: the first fills in, later ones append with a semicolon, and repeated picks are ignored.
	// Any manual edit exits the quick-combine state, and later digits keep normal input semantics.
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && !state.awaiting {
		if r := msg.Runes[0]; r >= '1' && r <= '3' {
			if value, handled := state.appendSuggestion(int(r-'1'), m.textarea.Value()); handled {
				m.textarea.SetValue(value)
				m.textarea.CursorEnd()
				m.refitTextareaHeight()
				return m, nil
			}
		}
	}

	// Forward regular input to the textarea
	if msg.Type == tea.KeyRunes && (containsSGRFragment(string(msg.Runes)) || isCSILeak(msg.Runes)) {
		return m, nil
	}
	var ok bool
	if msg, ok = cleanHumanKeyRunes(msg); !ok {
		return m, nil
	}
	state.resetSuggestionInput()
	if msg.Type == tea.KeyRunes {
		m.lastKeyAt = time.Now()
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refitTextareaHeight()
	return m, cmd
}

// exitCoCreate exits co-create mode, cancels the in-flight LLM request, and restores the input box state.
func (m Model) exitCoCreate() (tea.Model, tea.Cmd) {
	if m.cocreate.cancel != nil {
		m.cocreate.cancel()
	}
	stage := m.cocreate.stage
	initial := m.cocreate.initialInput()
	m.cocreate = nil
	m.resizeTextarea()
	// Phase co-create cancelled: clear the occupancy marker, stay paused, and return to the running workbench input state (do not refill the synthesized opener).
	if stage {
		m.textarea.SetValue("")
		m.textarea.Placeholder = defaultSteerPlaceholder()
		return m, tea.Batch(cancelCoCreate(m.runtime), fetchSnapshot(m.runtime), m.textarea.Focus())
	}
	m.textarea.SetValue(initial)
	m.textarea.Placeholder = placeholderForNewMode(m.startupMode)
	return m, m.textarea.Focus()
}

// overlayAboveInput floats the overlay on top of the bottom of the base view (above the inputBox)
// without changing the overall layout height. It covers only the overlay card's own width, letting the underlying content show through on the right.
func overlayAboveInput(base, overlay string, inputLineCount int) string {
	baseLines := strings.Split(base, "\n")
	overLines := strings.Split(strings.TrimRight(overlay, "\n"), "\n")

	endY := len(baseLines) - inputLineCount
	startY := endY - len(overLines)
	if startY < 0 {
		startY = 0
	}

	for i, ol := range overLines {
		y := startY + i
		if y >= 0 && y < endY {
			olW := lipgloss.Width(ol)
			// Cut off the leftmost olW visible characters of the base, then concatenate the overlay + the remaining right-side content
			right := ansi.TruncateLeft(baseLines[y], olW, "")
			baseLines[y] = ol + right
		}
	}
	return strings.Join(baseLines, "\n")
}

// isCSILeak detects whether KeyRunes are a leaked fragment of a CSI escape sequence.
// When the terminal sends the arrow key \x1b[A, fast key presses may split the sequence:
// \x1b is parsed as Escape, and "[" or "[A" leaks into the textarea as KeyRunes.
func isCSILeak(runes []rune) bool {
	if len(runes) == 0 || runes[0] != '[' {
		return false
	}
	for _, r := range runes[1:] {
		if (r >= '0' && r <= '9') || r == ';' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '~' {
			continue
		}
		return false
	}
	return true
}

// containsSGRFragment detects whether the text contains a fragment of an SGR mouse sequence (the "<digits;digits;" pattern).
func containsSGRFragment(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		j := i + 1
		if j >= len(s) || s[j] < '0' || s[j] > '9' {
			continue
		}
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j < len(s) && s[j] == ';' {
			return true
		}
	}
	return false
}

func cleanHumanKeyRunes(msg tea.KeyMsg) (tea.KeyMsg, bool) {
	if msg.Type != tea.KeyRunes {
		return msg, true
	}
	cleaned := utils.CleanInputRunes(msg.Runes)
	if cleaned == "" {
		return msg, false
	}
	msg.Runes = []rune(cleaned)
	return msg, true
}
