package host

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// errorKind classifies a runtime error into a stable, short label for log
// filtering and alert routing. Returns "" when no special tag applies.
//
// err is the live error chain (may be nil after JSON serialization); msg is
// the rendered string fallback used when the chain has been flattened
// (e.g. inside sub-agent JSON results).
func errorKind(err error, msg string) string {
	if kind := agentcore.ErrorKind(err); kind != "" && kind != "unknown" {
		return kind
	}
	if msg == "" {
		return ""
	}
	if kind := agentcore.ErrorKind(errors.New(msg)); kind != "unknown" {
		return kind
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "tool argument validation failed"):
		return "tool_validation"
	case strings.Contains(lower, "too many concurrent requests"):
		return "overloaded"
		// providerError appends litellm's structured type to the end of the text.
		// HTTP/2 INTERNAL_ERROR itself has no classifiable keyword, so keeping this explicit network marker is enough.
	case strings.Contains(lower, "[network,"):
		return "network"
	}
	return ""
}

// Monotonically increasing event ID counter; combined with a timestamp it generates stable IDs.
var eventIDCounter uint64

func nextEventID() string {
	return fmt.Sprintf("e%d", atomic.AddUint64(&eventIDCounter, 1))
}

// activeCall records the ID, start time and summary of a call in progress (TOOL / DISPATCH).
// The summary is filled back into the finish Event on completion so that replay (the runtime queue) can restore the row content.
type activeCall struct {
	id      string
	start   time.Time
	summary string
	depth   int
}

// observer projects Engine dispatches and Worker progress onto the Host's output channel.
// It is a pure observer and takes part in no control decision.
type observer struct {
	emitEv  func(Event)
	emitD   func(string)
	emitC   func()
	store   *storepkg.Store // for runtime queue persistence (consumed by ReplayQueue)
	agents  map[string]*agentState
	agentMu sync.Mutex

	// aborting is set by the Host at the entry of Abort()/Close() and cleared by Start/Resume/Continue.
	// While set, emitCallFinish downgrades the end events derived from context-cancel to warn and does not count them as failures
	// (this is what the user expects, and it also avoids showing them as errors). Real exceptions (non-cancel) are still reported as usual.
	aborting atomic.Bool

	streamThinking      bool
	lastThinkingByAgent map[string]string          // agent → latest accumulated thinking text (used to extract the incremental delta)
	dispatchStarts      map[string]*activeCall     // dispatched agent → DISPATCH call in progress
	toolStarts          map[string]*activeCall     // agent → TOOL call in progress
	streamExtractors    map[string]*agentExtractor // agent → content extractor for the current tool call's JSON arguments
	streamArgPrefixes   map[string]string          // agent/tool → argument stream prefix, used to recognize lightweight labels early
	streamArgLabels     map[string]string          // agent/tool → display name already recognized early from the argument stream
	retryEvents         map[string]string          // retry scope → event ID, updating the same row in place (2/7)
	streamHasContent    bool                       // whether the current streamRound has already output content (decides whether a paragraph separator is needed)
	streamLastByte      byte                       // last byte of the most recent streaming output (used to pad newlines precisely)
}

// agentExtractor records the tool name an agent is currently extracting and the extractor instance.
// The tool name is used to detect "a new tool call has started", so the cache is not polluted by leftovers from the previous round.
type agentExtractor struct {
	tool       string
	ext        *jsonFieldExtractor
	emittedAny bool // whether this extractor has produced content; used to pad a paragraph separator before the first output
}

type agentState struct {
	name    string
	state   string
	tool    string
	summary string
	turn    int
	context AgentContextSnapshot
	updated time.Time
}

func newObserver(s *storepkg.Store, emitEv func(Event), emitD func(string), emitC func()) *observer {
	return &observer{
		emitEv:              emitEv,
		emitD:               emitD,
		emitC:               emitC,
		store:               s,
		agents:              make(map[string]*agentState),
		lastThinkingByAgent: make(map[string]string),
		dispatchStarts:      make(map[string]*activeCall),
		toolStarts:          make(map[string]*activeCall),
		streamExtractors:    make(map[string]*agentExtractor),
		streamArgPrefixes:   make(map[string]string),
		streamArgLabels:     make(map[string]string),
		retryEvents:         make(map[string]string),
	}
}

// ── Engine direct-drive entry points ──
//
// The Engine runs the Worker directly, and events come from two sources:
//  1. dispatchStart/dispatchFinish — the Engine calls them directly at the dispatch boundary (DISPATCH row)
//  2. workerProgress — the Worker's progress relay (ctx ToolProgress),
//     handled uniformly by handleToolUpdate for TOOL / streamed body / thinking / retry / context
//     (TOOL row / streamed body / thinking / retry / context).

// dispatchStart records the start of a Worker dispatch and emits the DISPATCH row.
func (o *observer) dispatchStart(agent, task, reason string) {
	summary := dispatchSummary(agent, task)
	o.updateAgent(agent, func(a *agentState) {
		a.state = "working"
		a.tool = ""
		a.summary = fmt.Sprintf("engine → %s", summary)
	})
	id := nextEventID()
	o.dispatchStarts[agent] = &activeCall{id: id, start: time.Now(), summary: summary}
	o.emitAndLog(Event{
		ID:       id,
		Time:     time.Now(),
		Category: "DISPATCH",
		Agent:    agent,
		Summary:  summary,
		Detail:   dispatchDetail(task, reason),
		Level:    "info",
	})
}

// dispatchFinish settles the DISPATCH row into the finished state and resets Worker state;
// it cleans up orphan TOOL rows under that Worker's name (ProgressToolEnd may be absent on abort/error paths).
func (o *observer) dispatchFinish(agent string, runErr error) {
	o.updateAgent(agent, func(a *agentState) {
		a.state = "idle"
		a.tool = ""
	})
	delete(o.lastThinkingByAgent, agent)
	if call, ok := o.toolStarts[agent]; ok {
		delete(o.toolStarts, agent)
		delete(o.streamExtractors, agent)
		o.emitCallFinish(call, "TOOL", agent, runErr)
	}
	if call, ok := o.dispatchStarts[agent]; ok {
		delete(o.dispatchStarts, agent)
		o.emitCallFinish(call, "DISPATCH", agent, runErr)
	}
	o.streamClear()
}

// workerProgress adapts the Worker progress relay to the existing ToolExecUpdate handling.
func (o *observer) workerProgress(p agentcore.ProgressPayload) {
	payload := p
	o.handleToolUpdate(agentcore.Event{Type: agentcore.EventToolExecUpdate, Progress: &payload})
}

func (o *observer) finalize() {
	o.agentMu.Lock()
	defer o.agentMu.Unlock()
	for _, a := range o.agents {
		a.state = "idle"
		a.tool = ""
	}
}

// setAborting is called by the Host at lifecycle switches such as Abort/Close/Start, controlling
// whether derived "context canceled" events need to be suppressed (to avoid duplicating "the user paused manually").
func (o *observer) setAborting(v bool) { o.aborting.Store(v) }

func (o *observer) retryEventID(scope string, attempt int) string {
	if strings.TrimSpace(scope) == "" {
		scope = "engine"
	}
	if o.retryEvents == nil {
		o.retryEvents = make(map[string]string)
	}
	if attempt <= 1 || o.retryEvents[scope] == "" {
		o.retryEvents[scope] = nextEventID()
	}
	return o.retryEvents[scope]
}

// emitAndLog is for the "start" state of call-type events: it sends to the TUI but does not write to the runtime queue,
// avoiding the duplication of "one row for start, another for completion" on replay. slog is recorded uniformly by host.emitEvent.
func (o *observer) emitAndLog(ev Event) {
	o.emitEv(ev)
}

// persistEvent writes the event to the runtime queue (slog is recorded uniformly by host.emitEvent).
func (o *observer) persistEvent(ev Event) {
	if o.store == nil || o.store.Runtime == nil {
		return
	}
	priority := domain.RuntimePriorityBackground
	switch {
	case ev.Level == "error":
		priority = domain.RuntimePriorityControl
	case ev.Category == "SYSTEM" || ev.Category == "ERROR":
		priority = domain.RuntimePriorityControl
	}
	if _, err := o.store.Runtime.AppendQueue(domain.RuntimeQueueItem{
		Time:     ev.Time,
		Priority: priority,
		Category: ev.Category,
		Summary:  ev.Summary,
		Payload:  ev,
	}); err != nil {
		slog.Warn("saving run event failed", "module", "observer", "category", ev.Category, "err", err)
	}
}

func (o *observer) updateAgent(name string, fn func(*agentState)) {
	if name == "" {
		return
	}
	o.agentMu.Lock()
	defer o.agentMu.Unlock()
	a, ok := o.agents[name]
	if !ok {
		a = &agentState{name: name, state: "idle"}
		o.agents[name] = a
	}
	fn(a)
	a.updated = time.Now()
}

func (o *observer) agentSnapshots() []AgentSnapshot {
	o.agentMu.Lock()
	defer o.agentMu.Unlock()
	snaps := make([]AgentSnapshot, 0, len(o.agents))
	for _, a := range o.agents {
		snaps = append(snaps, AgentSnapshot{
			Name:      a.name,
			State:     a.state,
			Summary:   a.summary,
			Tool:      a.tool,
			Turn:      a.turn,
			Context:   a.context,
			UpdatedAt: a.updated,
		})
	}
	return snaps
}
