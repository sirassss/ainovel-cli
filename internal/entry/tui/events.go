package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// Message types
type (
	eventMsg       host.Event
	snapshotMsg    host.UISnapshot
	doneMsg        struct{ complete bool } // complete=true means the whole book is finished, false means it stopped on an error
	abortResultMsg struct{ stopped bool }
	bootstrapMsg   struct {
		existing  bool // An existing work; enter the workbench whether or not the resume succeeded
		resumed   bool
		completed bool // The directory holds an already completed book: land on the completed-state workbench rather than the welcome screen
		err       error
	}
	reportLoadedMsg struct {
		reqID      int
		report     diag.Report
		exportPath string // Absolute path of the redacted diagnostic file; empty = export failed
		exportErr  error
		finishedAt time.Time
	}
	startResultMsg   struct{ err error }
	cocreateDeltaMsg struct {
		reqID int
		kind  string // host.CoCreateProgressThinking | host.CoCreateProgressReply
		text  string
	}
	// cocreateStreamItem is the internal payload of deltaCh, delivering the streaming kind together with the accumulated text to the TUI.
	cocreateStreamItem struct {
		kind string
		text string
	}
	cocreateDoneMsg struct {
		reqID int
		reply host.CoCreateReply
		err   error
	}
	steerResultMsg     struct{ err error }
	continueResultMsg  struct{ err error }
	spinnerTickMsg     time.Time
	toolSpinnerTickMsg time.Time // Independent tick for the event-stream tool spinner (faster, independent of the top bar / stars)
	streamDeltaMsg     string    // Streaming token delta
	streamClearMsg     struct{}  // Clear the streaming buffer (a new message begins)
	streamFlushTickMsg struct{}  // Streaming flush throttle (scheduled only when there is pending data)
	quitResetMsg       struct{}  // Reset timeout for double Ctrl+C
)

// --- Cmd functions ---

func listenEvents(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-rt.Events()
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

func listenDone(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		_, ok := <-rt.Done()
		if !ok {
			return nil
		}
		snap := rt.Snapshot()
		return doneMsg{complete: snap.Phase == "complete"}
	}
}

func tickSnapshot(rt *host.Host) tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return snapshotMsg(rt.Snapshot())
	})
}

func fetchSnapshot(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return snapshotMsg(rt.Snapshot())
	}
}

func bootstrapRuntime(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		snapshot := rt.Snapshot()
		msg := bootstrapMsg{
			existing:  snapshot.Phase != "" || snapshot.BookTitle != "",
			completed: snapshot.Phase == "complete",
		}
		label, err := rt.Resume()
		if err != nil {
			msg.err = err
			return msg
		}
		if label == "" {
			if msg.existing {
				return msg
			}
			return nil
		}
		msg.resumed = true
		return msg
	}
}

// resumeBook re-runs the resume gate once mid-session (bootstrap's Resume runs only at startup):
// after an import finishes and the panel closes, and after /reopen, it is what lands the user back on the creation workbench. It does not replay the event queue: this session's events
// have already been shown by the resident listenEvents, and replaying would echo them twice. Pending interventions (such as the continuation
// direction registered by /reopen) are first adjudicated and consumed by the Arbiter in Resume, and then the engine resumes.
func resumeBook(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		snapshot := rt.Snapshot()
		label, err := rt.Resume()
		return bootstrapMsg{
			existing: snapshot.Phase != "" || snapshot.BookTitle != "", completed: snapshot.Phase == "complete",
			resumed: label != "", err: err,
		}
	}
}

func startRuntime(rt *host.Host, prompt string) tea.Cmd {
	return func() tea.Msg {
		// Deterministically generate this book's user-rule snapshot on the startup side (normalized from the original prompt); must happen before StartPrepared.
		if err := rt.PrepareUserRules(prompt); err != nil {
			return startResultMsg{err: err}
		}
		err := rt.StartPrepared(prompt)
		return startResultMsg{err: err}
	}
}

func runCoCreate(rt *host.Host, state *cocreateState) tea.Cmd {
	history := state.session.History()
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	state.deltaCh = make(chan cocreateStreamItem, 64)
	state.doneCh = make(chan cocreateDoneMsg, 1)
	// Phase co-create carries a story-state summary and produces a "next-direction brief"; cold-start co-create clarifies requirements from scratch. The two have the same signature.
	stream := rt.CoCreateStream
	if state.stage {
		stream = rt.StageCoCreateStream
	}
	start := func() tea.Msg {
		go func() {
			reply, err := stream(ctx, history, func(kind, text string) {
				select {
				case state.deltaCh <- cocreateStreamItem{kind: kind, text: text}:
				default:
				}
			})
			state.doneCh <- cocreateDoneMsg{reply: reply, err: err}
			close(state.deltaCh)
			close(state.doneCh)
		}()
		return nil
	}
	return tea.Batch(start, listenCoCreateDelta(state), listenCoCreateDone(state))
}

func listenCoCreateDelta(state *cocreateState) tea.Cmd {
	if state == nil || state.deltaCh == nil {
		return nil
	}
	// Capture a local reference to the channel: this prevents an old listen closure from
	// misreading the new channel if state.deltaCh is reassigned later (not triggered by the current flow, but it should not be left as a maintenance trap).
	reqID := state.reqID
	ch := state.deltaCh
	return func() tea.Msg {
		item, ok := <-ch
		if !ok {
			return nil
		}
		return cocreateDeltaMsg{reqID: reqID, kind: item.kind, text: item.text}
	}
}

func listenCoCreateDone(state *cocreateState) tea.Cmd {
	if state == nil || state.doneCh == nil {
		return nil
	}
	reqID := state.reqID
	ch := state.doneCh
	return func() tea.Msg {
		result, ok := <-ch
		if !ok {
			return nil
		}
		result.reqID = reqID
		return result
	}
}

func steerRuntime(rt *host.Host, text string) tea.Cmd {
	return func() tea.Msg {
		return steerResultMsg{err: rt.Steer(text)}
	}
}

func continueRuntime(rt *host.Host, text string) tea.Cmd {
	return func() tea.Msg {
		err := rt.Continue(text)
		return continueResultMsg{err: err}
	}
}

// resumeFromCoCreate injects the next-direction brief produced by phase co-create and resumes creation.
// It reuses continueResultMsg: on success it attaches listenDone to keep running, on failure it echoes the error.
func resumeFromCoCreate(rt *host.Host, draft string) tea.Cmd {
	return func() tea.Msg {
		err := rt.ResumeFromCoCreate(draft)
		return continueResultMsg{err: err}
	}
}

// cancelCoCreate abandons phase co-create: clears the occupancy marker and stays paused. Events flow back through the events channel, so no message needs to be returned.
func cancelCoCreate(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		rt.CancelCoCreate()
		return nil
	}
}

func abortRuntime(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return abortResultMsg{stopped: rt.Abort()}
	}
}

func loadReport(dir string, reqID int) tea.Cmd {
	return func() tea.Msg {
		s := store.NewStore(dir)
		// Diagnose = creation diagnosis + runtime checks; runtime Findings are also included in the on-screen report.
		rep, rc := diag.Diagnose(s)
		// Reuse rep+rc to write the redacted diagnostic file (an export failure does not affect the on-screen report).
		exportPath, exportErr := diag.WriteExport(s, rep, rc)
		return reportLoadedMsg{
			reqID:      reqID,
			report:     rep,
			exportPath: exportPath,
			exportErr:  exportErr,
			finishedAt: time.Now(),
		}
	}
}

func tickSpinner() tea.Cmd {
	return tea.Tick(350*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

// tickToolSpinner drives the spinner of the "in progress" rows in the event stream. Independent of tickSpinner and faster (150ms).
func tickToolSpinner() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return toolSpinnerTickMsg(t)
	})
}

// tickStreamFlush coalesces streaming deltas within a 16ms window. It is started by the first pending delta
// and stops after flushing, so it does not keep waking the TUI while idle.
func tickStreamFlush() tea.Cmd {
	return tea.Tick(16*time.Millisecond, func(t time.Time) tea.Msg {
		return streamFlushTickMsg{}
	})
}

func listenStream(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		delta, ok := <-rt.Stream()
		if !ok {
			return nil
		}
		// The sentinel is dispatched as streamClearMsg, guaranteeing it reaches the TUI in emit order
		// on the same channel as normal deltas. With two channels, clearCh and streamCh are unordered, and the ✻ header was often
		// wrongly inserted at the end of the previous thinking segment.
		if delta == host.StreamClearSentinel {
			return streamClearMsg{}
		}
		return streamDeltaMsg(delta)
	}
}
