package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/utils"
)

const maxPromptEventCols = 160

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The body height depends on the live height of the top and bottom bars (the new-book page's mode bar and multi-line input both change it),
	// so sync once before every message, to avoid the viewport staying at the old height and padding the panel bottom with blank lines. Idempotent and cheap.
	if m.width > 0 {
		m.updateViewportSize()
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeTextarea()
		m.updateViewportSize()
		m.refreshDetailViewport()
		m.refreshStateViewport()
		return m, nil
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)
	case tea.MouseMsg:
		return m.handleMouseMsg(msg)
	default:
		if next, cmd, handled := m.handleRuntimeMsg(msg); handled {
			return next, cmd
		}
		return m.handleTextareaMsg(msg)
	}
}

func (m Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := m.handleOverlayKeyMsg(msg); handled {
		return next, cmd
	}

	if msg.Type == tea.KeyCtrlC {
		if m.quitPending {
			return m, tea.Quit
		}
		m.quitPending = true
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return quitResetMsg{} })
	}
	m.quitPending = false

	if next, cmd, handled := m.handleCommandPaletteKey(msg); handled {
		return next, cmd
	}

	return m.handleBaseKeyMsg(msg)
}

func (m Model) handleOverlayKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case m.cocreate != nil:
		return m.handleBlockingModalKey(msg, m.handleCoCreateKey)
	case m.modelConfig != nil:
		return m.handleBlockingModalKey(msg, m.handleModelConfigKey)
	case m.help != nil:
		return m.handleBlockingModalKey(msg, m.handleHelpKey)
	case m.modelSwitch != nil:
		return m.handleBlockingModalKey(msg, m.handleModelSwitchKey)
	case m.report != nil:
		return m.handleBlockingModalKey(msg, m.handleReportKey)
	case m.importer != nil:
		return m.handleBlockingModalKey(msg, m.handleImportKey)
	case m.simulator != nil:
		return m.handleBlockingModalKey(msg, m.handleSimulationKey)
	default:
		return m, nil, false
	}
}

func (m Model) handleBlockingModalKey(msg tea.KeyMsg, next func(tea.KeyMsg) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd, bool) {
	if msg.Type == tea.KeyCtrlC {
		if m.quitPending {
			return m, tea.Quit, true
		}
		m.quitPending = true
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return quitResetMsg{} }), true
	}
	m.quitPending = false
	// Cross-modal global shortcut: mouse reporting must be toggleable while a modal is open too, otherwise under
	// screen-locking modals such as co-create/help/report the user cannot drag-select to copy natively.
	if msg.Type == tea.KeyCtrlR {
		next, cmd := m.toggleMouseReporting()
		return next, cmd, true
	}
	model, cmd := next(msg)
	return model, cmd, true
}

// toggleMouseReporting toggles mouse reporting. On -> off lets the user drag-select and copy natively;
// off -> on restores click-to-focus / wheel scrolling. Shared by the base path and the blocking modal path.
func (m Model) toggleMouseReporting() (Model, tea.Cmd) {
	// The welcome screen (modeNew) never enables mouse reporting, so native drag already allows copying; Ctrl+R is ignored here
	// to avoid accidentally turning reporting on and breaking native copy. Mouse reporting is turned on by enterRunning when entering the workbench.
	if m.mode == modeNew {
		return m, nil
	}
	m.mouseOff = !m.mouseOff
	if m.mouseOff {
		return m, tea.DisableMouse
	}
	return m, tea.EnableMouseCellMotion
}

// donePlaceholder is the input placeholder shown once the work is complete.
const donePlaceholder = "Truyện đã hoàn thành · có thể nhập yêu cầu sửa đổi (vd: \"viết lại chương 3\"), /reopen để viết tiếp tập mới, hoặc /export để xuất truyện"

// enterRunning enters the creation workbench: it turns on mouse reporting (the workbench needs click-to-switch panels / wheel /
// dragging the sidebar). The returned command must be Batch-ed by the caller into the final return value.
func (m *Model) enterRunning() tea.Cmd {
	m.mode = modeRunning
	m.mouseOff = false
	return tea.EnableMouseCellMotion
}

func (m Model) handleCommandPaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if !m.compActive {
		return m, nil, false
	}

	switch msg.Type {
	case tea.KeyEsc:
		m.clearCommandPalette()
		return m, nil, true
	case tea.KeyUp:
		if m.compIdx > 0 {
			m.compIdx--
		}
		return m, nil, true
	case tea.KeyDown:
		if m.compIdx < len(m.compItems)-1 {
			m.compIdx++
		}
		return m, nil, true
	case tea.KeyTab:
		m.acceptCommandCompletion()
		return m, nil, true
	case tea.KeyEnter:
		item, ok := m.acceptCommandCompletion()
		if !ok {
			return m, nil, true
		}
		if item.AutoExecute {
			m.textarea.Reset()
			next, cmd := m.handleSlashCommand(slashCommand{name: item.Name})
			return next, cmd, true
		}
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m Model) handleBaseKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Throttle defense: a pasted \n on a terminal without bracketed paste degrades into consecutive KeyEnter;
	// a human pressing Enter is usually > 100ms after the previous character, while < 50ms is very likely a fragment of a paste stream.
	// Record only KeyRunes (the character stream) -- function keys (↑↓/Tab/Ctrl-x) must not pollute the throttle,
	// otherwise pressing Enter right after the user picks from history would be wrongly swallowed.
	if msg.Type == tea.KeyRunes {
		m.lastKeyAt = time.Now()
	}
	switch msg.Type {
	case tea.KeyEscape:
		if m.mode == modeRunning && m.snapshot.IsRunning {
			return m, abortRuntime(m.runtime)
		}
		m.textarea.Reset()
		m.historyIdx = len(m.inputHistory)
		m.historyDraft = ""
		m.refitTextareaHeight()
		m.clearCommandPalette()
		return m, nil
	case tea.KeyCtrlL:
		m.resetOutputPanels()
		return m, nil
	case tea.KeyCtrlU:
		// Clear the current input; also exit history browsing state.
		m.textarea.Reset()
		m.historyIdx = len(m.inputHistory)
		m.historyDraft = ""
		m.refitTextareaHeight()
		m.clearCommandPalette()
		return m, nil
	case tea.KeyCtrlR:
		return m.toggleMouseReporting()
	case tea.KeyTab:
		if m.mode == modeNew {
			if m.cocreate != nil {
				return m, nil
			}
			if m.startupMode == startupModeQuick {
				m.startupMode = startupModeCoCreate
			} else {
				m.startupMode = startupModeQuick
			}
			m.textarea.Placeholder = placeholderForNewMode(m.startupMode)
			return m, nil
		}
		m.focusPane = (m.focusPane + 1) % focusPaneCount
		return m, nil
	case tea.KeyEnter:
		// Alt+Enter is an explicit newline, handed to textarea.Update (KeyMap.InsertNewline is bound to this key).
		if msg.Alt {
			break
		}
		// Interval since the last non-Enter key is too short -> treat as a \n fragment of a paste stream:
		// replace it with a space to keep the visual gap, consistent with the cleanHumanKeyRunes path ("abc\ndef" -> "abc def").
		// Defends against terminal environments where bracketed paste fails (old SSH / some tmux configurations).
		if !m.lastKeyAt.IsZero() && time.Since(m.lastKeyAt) < 50*time.Millisecond {
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
			m.refitTextareaHeight()
			return m, cmd
		}
		return m.handleEnterKey()
	case tea.KeyUp:
		// Multi-line input: let the textarea handle in-line cursor movement (falls through to textarea.Update after the switch)
		if m.textareaIsMultiline() {
			break
		}
		// Single line: prefer walking history, and fall back to event-stream scrolling when there is no usable history
		if m.tryHistoryUp() {
			return m, nil
		}
		return m.handleVerticalScrollKey(msg, true)
	case tea.KeyDown:
		if m.textareaIsMultiline() {
			break
		}
		if m.tryHistoryDown() {
			return m, nil
		}
		return m.handleVerticalScrollKey(msg, false)
	case tea.KeyPgUp:
		return m.handleVerticalScrollKey(msg, true)
	case tea.KeyPgDown:
		return m.handleVerticalScrollKey(msg, false)
	case tea.KeyEnd:
		switch m.focusPane {
		case focusStream:
			m.streamScroll = true
			m.streamVP.GotoBottom()
		case focusDetail:
			m.detailVP.GotoBottom()
		case focusState:
			m.stateVP.GotoBottom()
		default:
			m.autoScroll = true
			m.viewport.GotoBottom()
		}
		return m, nil
	}

	if msg.Type == tea.KeyRunes && (containsSGRFragment(string(msg.Runes)) || isCSILeak(msg.Runes)) {
		return m, nil
	}
	var ok bool
	if msg, ok = cleanHumanKeyRunes(msg); !ok {
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refitTextareaHeight()
	m.updateCommandPalette()
	return m, cmd
}

func (m Model) handleEnterKey() (tea.Model, tea.Cmd) {
	text := utils.CleanInputLine(m.textarea.Value())
	if text == "" {
		return m, nil
	}
	m.clearCommandPalette()
	if cmd, ok := parseSlashCommand(text); ok {
		m.pushInputHistory(text)
		m.textarea.Reset()
		m.refitTextareaHeight()
		return m.handleSlashCommand(cmd)
	}

	m.pushInputHistory(text)
	m.textarea.Reset()
	m.refitTextareaHeight()
	switch m.mode {
	case modeNew:
		m.err = nil
		if m.startupMode == startupModeQuick {
			prompt, err := startup.PrepareQuick(text)
			if err != nil {
				m.err = err
				return m, nil
			}
			cmd := m.enterStarting(prompt)
			return m, tea.Batch(startRuntime(m.runtime, prompt), cmd)
		}
		m.cocreate = newCoCreateState(text)
		return m, m.sendCoCreate()
	case modeRunning:
		// Do not echo the USER event locally -- the Host.Continue/Steer entry already emits a "USER" event,
		// which flows back to the TUI via the events channel. Architecture §2.3: the observation layer only observes and produces no facts.
		if !m.snapshot.IsRunning {
			return m, continueRuntime(m.runtime, text)
		}
		return m, steerRuntime(m.runtime, text)
	case modeDone:
		// User input after completion (rework / continuation request): wake a new run. In the stopped state Continue goes through Inject
		// to resume automatically, the Arbiter adjudicates the user intervention; when reworking an already written chapter, the Engine reopens the whole book and enqueues it.
		// Switch back to modeRunning to re-enter the workbench; when this run finishes
		// doneMsg(complete) will set modeDone again. Slash commands are already handled earlier above and do not go through this branch.
		m.mode = modeRunning
		return m, continueRuntime(m.runtime, text)
	default:
		return m, nil
	}
}

func (m Model) handleVerticalScrollKey(msg tea.KeyMsg, upward bool) (tea.Model, tea.Cmd) {
	if m.focusPane == focusStream {
		if upward {
			m.streamScroll = false
		}
		var cmd tea.Cmd
		m.streamVP, cmd = m.streamVP.Update(msg)
		if !upward && m.streamVP.AtBottom() {
			m.streamScroll = true
		}
		return m, cmd
	}
	if m.focusPane == focusDetail {
		var cmd tea.Cmd
		m.detailVP, cmd = m.detailVP.Update(msg)
		return m, cmd
	}
	if m.focusPane == focusState {
		var cmd tea.Cmd
		m.stateVP, cmd = m.stateVP.Update(msg)
		return m, cmd
	}
	if upward {
		m.autoScroll = false
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	if !upward && m.viewport.AtBottom() {
		m.autoScroll = true
	}
	return m, cmd
}

func (m Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.cocreate != nil {
		// Mouse is routed by X coordinate: left half of the screen = conv panel, right half = prompt panel.
		// The modal is centered with conv taking the left ~58%, so using the screen midline is accurate enough.
		// Wheel scrolling in the conv area by the user automatically stops follow (so it can stay stably at a history position).
		var cmd tea.Cmd
		if msg.X < m.width/2 {
			m.cocreate.convFollow = false
			m.cocreate.convVP, cmd = m.cocreate.convVP.Update(msg)
			if m.cocreate.convVP.AtBottom() {
				m.cocreate.convFollow = true
			}
		} else {
			m.cocreate.promptVP, cmd = m.cocreate.promptVP.Update(msg)
		}
		return m, cmd
	}
	if m.modelSwitch != nil || m.modelConfig != nil {
		return m, nil
	}
	if pane, ok := m.paneAtMouse(msg.X, msg.Y); ok {
		m.hoverPane = pane
		m.hoverActive = true
		if msg.Action == tea.MouseActionPress {
			m.focusPane = pane
		}
	} else {
		m.hoverActive = false
	}

	var cmd tea.Cmd
	if m.focusPane == focusStream {
		m.streamVP, cmd = m.streamVP.Update(msg)
		if msg.Action == tea.MouseActionPress {
			m.streamScroll = m.streamVP.AtBottom()
		}
		return m, cmd
	}
	if m.focusPane == focusDetail {
		m.detailVP, cmd = m.detailVP.Update(msg)
		return m, cmd
	}
	if m.focusPane == focusState {
		m.stateVP, cmd = m.stateVP.Update(msg)
		return m, cmd
	}
	m.viewport, cmd = m.viewport.Update(msg)
	if msg.Action == tea.MouseActionPress {
		m.autoScroll = m.viewport.AtBottom()
	}
	return m, cmd
}

func (m Model) handleRuntimeMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case eventMsg:
		hadRunningEvent := m.hasRunningEvent()
		ev := host.Event(msg)
		m.applyEventProjection(ev)
		m.refreshEventViewport()
		cmd := listenEvents(m.runtime)
		if !hadRunningEvent && m.hasRunningEvent() && !m.toolTicking {
			m.toolTicking = true
			cmd = tea.Batch(cmd, tickToolSpinner())
		}
		return m, cmd, true
	case bootstrapMsg:
		// Whether a work already exists decides where the UI lands; a successful resume only decides whether the engine runs. When a data upgrade,
		// budget or revision gate fails, stay on the workbench showing the existing book, and never fall back to the welcome screen.
		if (msg.existing || msg.resumed) && m.mode == modeNew && !msg.completed {
			enableMouse := m.enterRunning()
			m.resizeTextarea()
			m.textarea.Placeholder = defaultSteerPlaceholder()
			if msg.err != nil {
				m.err = msg.err
			}
			return m, tea.Batch(fetchSnapshot(m.runtime), enableMouse), true
		}
		// Completed book: land on the completed-state workbench (enterRunning turns the mouse on, then switch to modeDone), not the welcome screen --
		// the welcome screen says nothing about an existing book, so users would think it was lost; /reopen, /export and rework input all live on the workbench.
		if msg.completed && m.mode == modeNew {
			enableMouse := m.enterRunning()
			m.mode = modeDone
			m.resizeTextarea()
			m.textarea.Placeholder = donePlaceholder
			if msg.err != nil {
				m.err = msg.err
			}
			return m, tea.Batch(fetchSnapshot(m.runtime), enableMouse, m.textarea.Focus()), true
		}
		// In-session resume such as /reopen re-enters the creation workbench from the completed state.
		if msg.resumed && m.mode == modeDone {
			enableMouse := m.enterRunning()
			m.resizeTextarea()
			m.textarea.Placeholder = defaultSteerPlaceholder()
			return m, tea.Batch(fetchSnapshot(m.runtime), enableMouse), true
		}
		if msg.err != nil {
			m.err = msg.err
		}
		return m, fetchSnapshot(m.runtime), true
	case snapshotMsg:
		next := host.UISnapshot(msg)
		detailChanged := !sameDetailSnapshot(m.snapshot, next)
		runningChanged := m.snapshot.IsRunning != next.IsRunning
		m.snapshot = next
		m.syncRuntimePlaceholder()
		m.refreshEventViewport()
		if runningChanged {
			m.refreshStreamViewport()
		}
		if detailChanged {
			m.refreshDetailViewport()
		}
		m.refreshStateViewport()
		return m, tickSnapshot(m.runtime), true
	case doneMsg:
		m.snapshot.IsRunning = false
		m.refreshEventViewport()
		m.refreshStreamViewport()
		m.refreshStateViewport()
		if msg.complete {
			m.abortPending = false
			m.mode = modeDone
			// The completed state does not lock the input box: automatic continuation stops, but the user can still enter rework requests (modeDone input goes through
			// Continue to wake a new run, and the Arbiter adjudicates rework or continued creation; commands such as /export, /model
			// must also stay usable, so the input box must stay focused (issues #27, #38).
			m.textarea.Placeholder = donePlaceholder
			return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime), m.textarea.Focus()), true
		}
		if m.abortPending {
			m.abortPending = false
			m.snapshot.RuntimeState = "paused"
			m.syncRuntimePlaceholder()
		} else {
			m.textarea.Placeholder = "Sáng tác bị gián đoạn, nhập nội dung bất kỳ để tiếp tục"
		}
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime)), true
	case abortResultMsg:
		if msg.stopped {
			m.abortPending = true
			m.textarea.Placeholder = "Đang tạm dừng sáng tác..."
		}
		return m, nil, true
	case reportLoadedMsg:
		if m.report == nil || msg.reqID != m.report.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.report.load(msg.report, paddedModalContentWidth(boxW), msg.exportPath, msg.exportErr, msg.finishedAt)
		return m, nil, true
	case importEventMsg:
		if m.importer == nil || msg.reqID != m.importer.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.importer.appendEvent(msg.ev, paddedModalContentWidth(boxW))
		if msg.ev.Stage == imp.StageError {
			return m, nil, true
		}
		if msg.ev.Stage == imp.StageDone {
			if msg.ev.Continued {
				m.importer = nil
				enableMouse := m.enterRunning()
				m.resizeTextarea()
				m.textarea.Placeholder = defaultSteerPlaceholder()
				return m, tea.Batch(enableMouse, m.textarea.Focus()), true
			}
			return m, nil, true
		}
		return m, listenImportEvent(msg.reqID, msg.ch), true
	case importClosedMsg:
		if m.importer == nil || msg.reqID != m.importer.reqID || m.importer.done {
			return m, nil, true
		}
		m.importer.paused = true
		boxW, _ := reportModalSize(m.width, m.height)
		m.importer.refresh(paddedModalContentWidth(boxW))
		return m, nil, true
	case simEventMsg:
		if m.simulator == nil || msg.reqID != m.simulator.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.simulator.appendEvent(msg.ev, paddedModalContentWidth(boxW))
		if msg.terminal() {
			return m, nil, true
		}
		return m, listenSimulationEvent(msg.reqID, msg.ch), true
	case exportDoneMsg:
		if msg.err != nil {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: "Không xuất được: " + msg.err.Error(), Level: "error",
			})
		} else if msg.result != nil {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "SYSTEM", Summary: formatExportSuccess(msg.result), Level: "success",
			})
		}
		m.refreshEventViewport()
		return m, nil, true
	case revisionDoneMsg:
		if msg.err != nil {
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "Không đồng bộ được chương: " + msg.err.Error(), Level: "error"})
		} else if msg.checkOnly {
			summary := "Không phát hiện chương nào bị chỉnh sửa từ bên ngoài"
			if len(msg.chapters) > 0 {
				summary = fmt.Sprintf("Phát hiện nội dung chương đã bị chỉnh sửa từ bên ngoài: %v; chạy /sync để tiếp nhận", msg.chapters)
			}
			m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "info"})
		} else {
			m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: formatRevisionResult(msg.result), Level: "success"})
		}
		m.refreshEventViewport()
		return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
	case modelConfigSavedMsg:
		if m.modelConfig == nil {
			return m, nil, true
		}
		if msg.err != nil {
			m.modelConfig.saving = false
			m.modelConfig.message = msg.err.Error()
			return m, nil, true
		}
		m.modelConfig = nil
		return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
	case modelConfigConnectionMsg:
		if m.modelConfig == nil {
			return m, nil, true
		}
		m.modelConfig.testing = false
		m.modelConfig.testCancel = nil
		if errors.Is(msg.err, context.Canceled) {
			m.modelConfig.message = "Kiểm tra kết nối đã bị hủy"
		} else if msg.err != nil {
			m.modelConfig.message = msg.err.Error()
		} else {
			m.modelConfig.message = "Kiểm tra kết nối thành công: " + msg.model
		}
		return m, nil, true
	case startResultMsg:
		next, cmd := m.handleStartResultMsg(msg)
		return next, cmd, true
	case cocreateDeltaMsg:
		if m.cocreate == nil || msg.reqID != m.cocreate.reqID {
			return m, nil, true
		}
		m.cocreate.applyDelta(msg.kind, msg.text)
		return m, listenCoCreateDelta(m.cocreate), true
	case cocreateDoneMsg:
		next, cmd := m.handleCoCreateDoneMsg(msg)
		return next, cmd, true
	case steerResultMsg:
		if msg.err != nil {
			m.err = msg.err
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: msg.err.Error(), Level: "error"})
			m.refreshEventViewport()
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
		}
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime)), true
	case continueResultMsg:
		if msg.err != nil {
			m.err = msg.err
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: msg.err.Error(), Level: "error",
			})
			m.refreshEventViewport()
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
		}
		m.err = nil
		m.textarea.Placeholder = defaultSteerPlaceholder()
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime), m.textarea.Focus()), true
	case spinnerTickMsg:
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		m.cursorIdx++
		if m.snapshot.IsRunning {
			// The top bar, activity hints and streaming cursor share a low-frequency animation, avoiding multiple resident timers repeatedly triggering a full-screen View.
			m.refreshEventViewport()
			m.refreshStreamViewport()
			m.streamDirty = false
		}
		if s := m.importer; s != nil && !s.done && !s.paused {
			s.frame = m.cursorIdx
			boxW, _ := reportModalSize(m.width, m.height)
			s.refresh(paddedModalContentWidth(boxW))
		}
		return m, tickSpinner(), true
	case toolSpinnerTickMsg:
		m.toolSpinnerIdx = (m.toolSpinnerIdx + 1) % len(toolSpinnerFrames)
		if m.hasRunningEvent() {
			m.refreshEventViewport()
			return m, tickToolSpinner(), true
		}
		m.toolTicking = false
		return m, nil, true
	case streamDeltaMsg:
		if len(m.streamRounds) == 0 {
			m.streamRounds = append(m.streamRounds, "")
		}
		m.streamRounds[len(m.streamRounds)-1] += string(msg)
		// Do not refresh immediately; the first delta starts a 16ms coalescing window, and later deltas reuse that timer.
		m.streamDirty = true
		cmd := listenStream(m.runtime)
		if !m.flushPending {
			m.flushPending = true
			cmd = tea.Batch(cmd, tickStreamFlush())
		}
		return m, cmd, true
	case streamClearMsg:
		// Round boundary: flush the accumulated deltas first so the new round can align visually
		if m.flushStreamIfDirty() && m.streamScroll {
			m.streamVP.GotoBottom()
		}
		if len(m.streamRounds) == 0 {
			m.streamRounds = append(m.streamRounds, "")
		} else if strings.TrimSpace(m.streamRounds[len(m.streamRounds)-1]) != "" {
			m.streamRounds = append(m.streamRounds, "")
		}
		m.trimStreamRounds()
		m.streamRound = len(m.streamRounds)
		m.refreshStreamViewport()
		if m.streamScroll {
			m.streamVP.GotoBottom()
		}
		return m, listenStream(m.runtime), true
	case streamFlushTickMsg:
		m.flushPending = false
		if m.flushStreamIfDirty() && m.streamScroll {
			m.streamVP.GotoBottom()
		}
		return m, nil, true
	case quitResetMsg:
		m.quitPending = false
		return m, nil, true
	default:
		return m, nil, false
	}
}

func sameDetailSnapshot(a, b host.UISnapshot) bool {
	return a.Synopsis == b.Synopsis &&
		a.Premise == b.Premise &&
		a.Layered == b.Layered &&
		a.CurrentVolumeArc == b.CurrentVolumeArc &&
		a.NextVolumeTitle == b.NextVolumeTitle &&
		a.CompassDirection == b.CompassDirection &&
		a.CompassScale == b.CompassScale &&
		a.SupportingCount == b.SupportingCount &&
		a.CompletedCount == b.CompletedCount &&
		a.InProgressChapter == b.InProgressChapter &&
		a.LastCommitSummary == b.LastCommitSummary &&
		a.LastReviewSummary == b.LastReviewSummary &&
		slices.Equal(a.Outline, b.Outline) &&
		slices.Equal(a.Characters, b.Characters) &&
		slices.Equal(a.RecentSupporting, b.RecentSupporting) &&
		slices.Equal(a.RecentSummaries, b.RecentSummaries)
}

func (m Model) handleStartResultMsg(msg startResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		wasStarting := m.starting
		m.starting = false
		if m.mode != modeNew {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: msg.err.Error(), Level: "error",
			})
			m.refreshEventViewport()
		}
		if m.cocreate != nil {
			m.cocreate.awaiting = false
			m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus())
		}
		if wasStarting {
			// After Enter we are already on the workbench; a startup-phase LLM error is shown right in the current workbench
			// and does not fall back to the welcome screen.
			m.mode = modeRunning
			m.snapshot.IsRunning = false
			m.snapshot.RuntimeState = "idle"
			m.textarea.Placeholder = "Không khởi tạo được, vui lòng kiểm tra cấu hình hoặc dùng /model để đổi model"
			m.refreshStreamViewport()
			m.refreshStateViewport()
			return m, m.textarea.Focus()
		}
		if m.mode == modeNew {
			m.textarea.Placeholder = placeholderForNewMode(m.startupMode)
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus())
		}
		return m, fetchSnapshot(m.runtime)
	}
	m.starting = false

	if m.mode == modeNew {
		m.cocreate = nil
		enableMouse := m.enterRunning()
		m.resizeTextarea()
		m.textarea.Placeholder = defaultSteerPlaceholder()
		return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus(), enableMouse)
	}

	return m, fetchSnapshot(m.runtime)
}

func (m *Model) enterStarting(rawPrompt string) tea.Cmd {
	m.cocreate = nil
	m.err = nil
	m.starting = true
	m.snapshot.IsRunning = true
	m.snapshot.RuntimeState = "running"
	enableMouse := m.enterRunning()
	m.resetOutputPanels()
	m.resizeTextarea()
	m.textarea.Placeholder = "Đang khởi tạo sáng tác..."
	m.applyStartupPromptEvent(rawPrompt)
	m.applyEvent(host.Event{
		Time: time.Now(), Category: "SYSTEM", Summary: "Đang khởi tạo sáng tác", Level: "info",
	})
	m.refreshEventViewport()
	m.refreshStreamViewport()
	m.refreshStateViewport()
	return tea.Batch(m.textarea.Focus(), enableMouse)
}

func (m *Model) applyStartupPromptEvent(rawPrompt string) {
	text := utils.CleanInputLine(rawPrompt)
	if text == "" {
		return
	}
	m.applyEvent(host.Event{
		Time:     time.Now(),
		Category: "USER",
		Summary:  "Yêu cầu sáng tác: " + truncate(text, maxPromptEventCols),
		Detail:   text,
		Level:    "info",
	})
}

func (m Model) handleCoCreateDoneMsg(msg cocreateDoneMsg) (tea.Model, tea.Cmd) {
	if m.cocreate == nil || msg.reqID != m.cocreate.reqID {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.cocreate.awaiting = false
		m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
		return m, m.textarea.Focus()
	}
	m.err = nil
	m.cocreate.apply(msg.reply)
	m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
	return m, m.textarea.Focus()
}

func (m Model) handleTextareaMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refitTextareaHeight()
	m.updateCommandPalette()
	return m, cmd
}

// applyEvent records an event generated locally by the TUI and updates the projection. Host events were already logged
// at their source, so the event subscription path should call applyEventProjection directly to avoid duplicate records.
func (m *Model) applyEvent(ev host.Event) {
	host.LogEvent(ev)
	m.applyEventProjection(ev)
}

// applyEventProjection applies one event to m.events:
// - with an ID that already exists -> update in place (merge completion-state fields, keep the first Time / Summary)
// - a new event -> append, and record it in eventIndex when needed
// - beyond maxEvents -> truncate with a sliding window and rebuild the index
func (m *Model) applyEventProjection(ev host.Event) {
	if ev.ID != "" {
		if idx, ok := m.eventIndex[ev.ID]; ok && idx >= 0 && idx < len(m.events) {
			existing := &m.events[idx]
			if !ev.FinishedAt.IsZero() {
				existing.FinishedAt = ev.FinishedAt
			}
			if ev.Duration > 0 {
				existing.Duration = ev.Duration
			}
			if ev.Failed {
				existing.Failed = true
			}
			if ev.Level != "" {
				existing.Level = ev.Level
			}
			if ev.Detail != "" {
				existing.Detail = ev.Detail
			}
			if ev.Kind != "" {
				existing.Kind = ev.Kind
			}
			// A non-empty Summary may overwrite (the end state may carry extra information); otherwise keep the first
			if ev.Summary != "" {
				existing.Summary = ev.Summary
			}
			// A retry event with the same ID is updated across attempts; the new deadline must follow so the countdown resets with it
			if !ev.RetryAt.IsZero() {
				existing.RetryAt = ev.RetryAt
			}
			return
		}
	}

	m.events = append(m.events, ev)
	if ev.ID != "" {
		m.eventIndex[ev.ID] = len(m.events) - 1
	}
	if len(m.events) > maxEvents {
		drop := len(m.events) - maxEvents
		m.events = m.events[drop:]
		m.rebuildEventIndex()
	}
}

// trimStreamRounds truncates streamRounds to maxStreamRounds segments; the excess is dropped from the front.
// When called: after every streamClear that opens a new round.
func (m *Model) trimStreamRounds() {
	if len(m.streamRounds) <= maxStreamRounds {
		return
	}
	drop := len(m.streamRounds) - maxStreamRounds
	m.streamRounds = m.streamRounds[drop:]
}

func (m *Model) rebuildEventIndex() {
	m.eventIndex = make(map[string]int, len(m.events))
	for i, e := range m.events {
		if e.ID != "" {
			m.eventIndex[e.ID] = i
		}
	}
}

func (m *Model) resetOutputPanels() {
	m.events = nil
	m.eventIndex = make(map[string]int)
	m.viewport.SetContent("")
	m.viewport.GotoTop()
	m.streamBuf.Reset()
	m.streamRounds = nil
	m.streamVP.SetContent("")
	m.streamVP.GotoTop()
	m.streamRound = 0
}
