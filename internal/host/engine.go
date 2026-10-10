package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"

	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/notify"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// engine is the deterministic execution engine: read facts → Route → precheck → run a Worker directly →
// check progress → loop; semantic scenarios consult the Arbiter on demand. It executes decisions and takes no part in literary judgment
// (docs/engine-rfc.md). Single goroutine, serial; control state changes only at loop boundaries.
type engine struct {
	store   *storepkg.Store
	workers *subagent.Runner

	arbiterModel    agentcore.ChatModel
	failurePrompt   string
	planStartPrompt string // Startup-ruling system prompt: when the ruling never completed, the engine uses StartPrompt to make up the ruling on the spot
	style           string // Style name; passed to DecidePlanStart for the make-up ruling
	// reconsult sends a stale intervention back into the host's full ruling path (persistence/audit/applying all actions),
	// executed asynchronously; the engine only discards stale dispatches and does not do a partial re-ruling itself.
	reconsult func(text string)

	observer  *observer
	budget    *BudgetSentinel
	gate      *ChapterAdvanceGate
	refresh   func() // Refresh the RestorePack before every writer dispatch
	emitEvent func(Event)
	notify    func(kind, level, title, body string)
	onPause   func(summary string) // The engine pauses itself (deadlock / failure-ruling abort): goes through the host's unified pause semantics (lifecycle=paused)
	onDone    func()               // run ended (for any reason); the host decides the terminal state from store facts

	mu      sync.Mutex
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	running bool
	pending []controlOp       // Control-state actions of an intervention, committed at the boundary
	next    *flow.Instruction // Instruction to run first in the next round (plan_start / arbiter dispatch)
	// deferGateForNext lives and dies with next only: hold+dispatch must first run the paired
	// editor/writer so it builds the rewrite queue, and only then can the Gate judge rewrites_drained.
	deferGateForNext bool

	// Deadlock tracking: counted whenever Route still yields the same instruction key after the previous round executed.
	// A Router instruction is a projection of the task's postcondition; real completion changes the next instruction.
	lastKey string
	repeats int
	// Failure retry: the same instruction key is retried only once, then the Arbiter is asked.
	failedKey string
	// Keep the latest Worker error for the same instruction so the deadlock ruling sees the real cause of failure.
	lastWorkerErrorKey string
	lastWorkerError    error
}

// deadlockConsultAt / deadlockAbortAt: once repeats reaches the former, ask the Arbiter; once it reaches the latter, hard-trip.
// A deterministic Engine must give a clear upper bound for loops that make no progress (RFC §5).
const (
	deadlockConsultAt = 3
	deadlockAbortAt   = 5
)

// controlOp is an action in an intervention ruling that modifies control state (committed at the boundary; RFC §3).
// text/facts keep the original consultation context: when a dispatch fails reconciliation, ask again with the new facts.
type controlOp struct {
	hold     *arbiter.AdvanceHoldOp
	reopen   *arbiter.ReopenOp
	dispatch *arbiter.DispatchOp
	text     string
	facts    arbiter.InterventionFacts
}

// start launches the engine loop; no-op if already running (returns false).
func (e *engine) start(initial *flow.Instruction) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = agentcore.WithToolProgress(ctx, e.observer.workerProgress)
	e.cancel = cancel
	e.running = true
	// When initial is empty, do not overwrite e.next: an intervention made while stopped may already have queued a
	// ruling dispatch via applyControlOp (e.g. editor rework); start(nil) wiping it would make Route send the writer to continue writing,
	// the opposite of what the user intended.
	if initial != nil {
		e.next = initial
		e.deferGateForNext = false
	}
	e.lastKey, e.repeats, e.failedKey = "", 0, ""
	e.lastWorkerErrorKey, e.lastWorkerError = "", nil
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.run(ctx)
	}()
	return true
}

// abort cancels the current loop (pause semantics; checkpoints guarantee nothing is lost).
func (e *engine) abort() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// wait waits for the current Engine goroutine to exit completely. Host.Close cancels first and then calls it,
// guaranteeing that the event channel is closed and the process exits only after the write tools and runEnded have finished.
func (e *engine) wait() {
	e.wg.Wait()
}

func (e *engine) isRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// enqueue puts an intervention's control-state actions into the boundary queue (while the engine is running); returns false if not running,
// in which case the caller should execute them immediately itself.
func (e *engine) enqueue(op controlOp) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return false
	}
	e.pending = append(e.pending, op)
	return true
}

func (e *engine) run(ctx context.Context) {
	defer func() {
		e.mu.Lock()
		e.running = false
		e.cancel = nil
		leftover := e.pending
		e.pending = nil
		e.mu.Unlock()
		// Exit race: when enqueue runs concurrently with exit, leftover intervention actions must not be dropped silently:
		// hold/reopen are idempotent fact writes, so re-run them with an independent ctx; dispatch has no engine to send it to,
		// so restore PendingSteer persistence (the host may already have cleared it on "enqueue succeeded"), and the next
		// Resume/Continue replays the whole intervention.
		for _, op := range leftover {
			if op.dispatch != nil {
				if op.text != "" {
					if err := e.store.RunMeta.SetPendingSteer(op.text); err != nil {
						slog.Warn("saving leftover intervention failed", "module", "engine", "err", err)
					}
				}
				e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "Engine đã dừng, phân công theo phân xử chưa được thực hiện; can thiệp được giữ lại, khi tiếp tục sáng tác sẽ tự phân xử lại"})
				op.dispatch = nil
			}
			if op.hold != nil || op.reopen != nil {
				if err := e.applyControlOp(context.Background(), op); err != nil {
					e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error",
						Summary: "Không nộp bù được can thiệp khi Engine thoát: " + err.Error()})
				}
			}
		}
		e.onDone()
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		// hold+dispatch must first let the paired dispatch build the rework facts; in other cases the Gate is checked uniformly before dispatch,
		// guaranteeing that a boundary hold and a review without a permit do not run an extra Worker.
		deferGate := e.applyPendingOps(ctx) || e.nextDefersGate()
		if !deferGate {
			if e.gate.HandleBoundary() {
				return
			}
		}

		inst := e.takeNext()
		if inst == nil {
			state, err := flow.LoadState(e.store)
			if err != nil {
				e.pauseWithNotify(notify.KindWorkerFailure, "Không đọc được dữ kiện định tuyến, đã tạm dừng: "+err.Error())
				return
			}
			// The volume summary may already be on disk while the process has not yet got to MarkComplete. When all aggregate artifacts are present,
			// first make the completion judgment from facts, then hand over to the Router, so that the final volume is not mistakenly dispatched to continue a volume.
			if state.AggregateRefresh == nil && state.Progress != nil && state.Progress.Layered &&
				state.Progress.Phase == domain.PhaseWriting && state.ArcBoundary != nil &&
				state.ArcBoundary.IsVolumeEnd && state.HasArcReview && state.HasArcSummary && state.HasVolumeSummary {
				complete, reconcileErr := tools.ReconcileLayeredCompletion(e.store)
				if reconcileErr != nil {
					e.pauseWithNotify(notify.KindWorkerFailure, "Không khôi phục được trạng thái kết thúc truyện, đã tạm dừng: "+reconcileErr.Error())
					return
				}
				if complete {
					continue
				}
			}
			inst = flow.Route(state)
		}
		if inst == nil {
			var err error
			inst, err = e.planStartFallback(ctx)
			if err != nil {
				e.pauseWithNotify(notify.KindPlanStart, "Không đọc được dữ kiện khôi phục kế hoạch, đã tạm dừng: "+err.Error())
				return
			}
		}
		if inst == nil {
			// Semantic scenario or terminal state: book complete → deterministic wrap-up; otherwise (leftover Steering, etc.)
			// → stop naturally and wait for the user's Continue / intervention.
			return
		}
		replaced, err := e.precheck(inst)
		if err != nil {
			e.pauseWithNotify(notify.KindWorkerFailure, "Không kiểm tra được điều kiện trước khi phân công, đã tạm dừng: "+err.Error())
			return
		}
		if replaced != nil {
			inst = replaced
		}
		allowed, gateErr := e.gate.Allow(inst)
		if gateErr != nil {
			e.pauseWithNotify(notify.KindAdvanceGate, "Lỗi điều khiển tiến chương, đã tạm dừng: "+gateErr.Error())
			return
		}
		if !allowed {
			return
		}
		if stop := e.trackDeadlock(ctx, &inst); stop {
			return
		}
		if inst == nil {
			continue // The deadlock ruling asks for the route to be recomputed
		}

		err = e.runWorker(ctx, inst)
		if ctx.Err() != nil {
			return
		}
		e.rememberWorkerError(inst, err)
		if err != nil {
			// trackDeadlock records this attempt ahead of dispatch. Errors where no valid Worker
			// semantic execution happened must not be counted as "no progress on the same task".
			e.discardNonSemanticDeadlockAttempt(inst, err)
			if stop := e.handleWorkerError(ctx, inst, err); stop {
				return
			}
		}

		// Policy boundary: budget stop-loss takes precedence over acceptance/advance pauses.
		if e.budget.HandleBoundary() {
			return
		}
		if e.gate.HandleBoundary() {
			return
		}
	}
}

func (e *engine) takeNext() *flow.Instruction {
	e.mu.Lock()
	defer e.mu.Unlock()
	inst := e.next
	e.next = nil
	e.deferGateForNext = false
	return inst
}

func (e *engine) nextDefersGate() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.next != nil && e.deferGateForNext
}

// planStartFallback covers two windows where planning facts are missing and Route cannot derive a planner:
//  1. The ruling is on disk but the first save_foundation has not happened → continue from the fixed PlanStartRecord,
//     without re-ruling (RFC §6); once the first foundation is on disk the tier is in place, and the make-up branch takes over.
//  2. The ruling never completed (model failure at startup) but the input fact StartPrompt exists → make up the ruling on the spot.
//     This is a retry of the first ruling and does not violate "recovery does not depend on re-ruling", which applies to rulings that already exist.
//     If the make-up ruling fails, take an explicit pause: a startup failure must not stop silently.
func (e *engine) planStartFallback(ctx context.Context) (*flow.Instruction, error) {
	progress, err := e.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w", err)
	}
	if progress == nil {
		return nil, nil
	}
	if progress.Phase == domain.PhaseWriting || progress.Phase == domain.PhaseComplete {
		return nil, nil
	}
	meta, err := e.store.RunMeta.Load()
	if err != nil {
		return nil, fmt.Errorf("load run meta: %w", err)
	}
	if meta == nil || meta.PlanningTier != "" {
		return nil, nil
	}
	missing, err := e.store.FoundationMissing()
	if err != nil {
		return nil, fmt.Errorf("load foundation state: %w", err)
	}
	if len(missing) == 0 {
		return nil, nil
	}
	if meta.PlanStart != nil {
		return &flow.Instruction{
			Agent:  meta.PlanStart.Planner,
			Task:   meta.PlanStart.PlannerTask,
			Reason: "Bắt đầu lập kế hoạch theo phân xử khởi động đã được cố định",
		}, nil
	}
	if meta.StartPrompt == "" {
		return nil, nil
	}
	return e.retryPlanStart(ctx, meta.StartPrompt), nil
}

// retryPlanStart makes up the startup decision and fixes it (the ruling lands as facts before execution, same shape as StartPrepared).
func (e *engine) retryPlanStart(ctx context.Context, prompt string) *flow.Instruction {
	start := time.Now()
	decision, derr := runObservedDecision(e.observer, "Phân xử khởi động bổ sung", func() (arbiter.PlanStartDecision, error) {
		return arbiter.DecidePlanStart(ctx, e.arbiterModel, e.planStartPrompt, prompt, e.style)
	})
	rec := storepkg.DecisionRecord{Kind: "plan_start", Decider: "arbiter", Input: prompt,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	rec, recErr := e.store.Decisions.Append(rec)
	if recErr != nil {
		slog.Warn("saving supplemental startup audit record failed", "module", "engine", "err", recErr)
	}
	if derr != nil {
		e.pauseWithNotify(notify.KindPlanStart, "Không phân xử khởi động được, đã tạm dừng (hãy kiểm tra cấu hình model/mạng rồi tiếp tục): "+derr.Error())
		return nil
	}
	if err := e.store.RunMeta.SetPlanStart(domain.PlanStartRecord{
		RawPrompt: prompt, Planner: decision.Planner, PlannerTask: decision.Task, DecisionID: rec.ID,
	}); err != nil {
		e.pauseWithNotify(notify.KindPlanStart, "Không thể lưu phân xử khởi động, đã tạm dừng: "+err.Error())
		return nil
	}
	e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "info",
		Summary: fmt.Sprintf("Đã bổ sung phân xử khởi động (Architect: %s — %s)", decision.Planner, decision.Reason)})
	return &flow.Instruction{Agent: decision.Planner, Task: decision.Task, Reason: decision.Reason}
}

// precheck is the deterministic incarnation of the former ToolGate: an illegal dispatch is rewritten directly, with no teaching text needed.
func (e *engine) precheck(inst *flow.Instruction) (*flow.Instruction, error) {
	progress, err := e.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w", err)
	}
	if progress != nil && progress.Phase == domain.PhaseComplete {
		// The only legal way out in the book-complete state is reopen (an intervention action); any dispatch is discarded directly.
		slog.Warn("dispatch after book completion discarded", "module", "engine", "agent", inst.Agent)
		return &flow.Instruction{}, nil // Clear it: next round Route returns nil and the engine stops naturally
	}
	if inst.Agent == "writer" {
		if progress == nil || progress.Phase != domain.PhaseWriting {
			phase := "<nil>"
			if progress != nil {
				phase = string(progress.Phase)
			}
			return nil, fmt.Errorf("writer chỉ được phân công ở giai đoạn writing (hiện phase=%s): %w", phase, errInvalidWriteTarget)
		}
		ch, err := writerTargetChapter(e.store)
		if err != nil {
			return nil, err
		}
		if ch > 0 {
			if err := tools.EnsureChapterExpanded(e.store, ch); err != nil {
				if !errors.Is(err, errs.ErrToolPrecondition) {
					return nil, err
				}
				// The target chapter is not yet expanded → deterministically reroute to architect_long to expand it (the original gate's teaching text
				// was addressed to the LLM; the Engine just does the right thing).
				return &flow.Instruction{
					Agent:  "architect_long",
					Task:   fmt.Sprintf("Cung kế tiếp đang là cung phác thảo (%s). Gọi save_foundation(type=expand_arc) để mở rộng cung kế; nếu tập hiện tại đã viết xong thì dùng type=append_volume để thêm và mở rộng tập kế.", err),
					Reason: "Chương mục tiêu để viết chưa được mở rộng, hãy mở rộng trước rồi viết tiếp",
				}, nil
			}
		}
		e.refresh()
	}
	return nil, nil
}

// writerTargetChapter derives the chapter the writer's next dispatch will actually write (head of the rewrite queue, otherwise the next chapter).
func writerTargetChapter(st *storepkg.Store) (int, error) {
	progress, err := st.Progress.Load()
	if err != nil {
		return 0, fmt.Errorf("load progress: %w", err)
	}
	if progress == nil {
		return 0, fmt.Errorf("progress chưa được khởi tạo")
	}
	if len(progress.PendingRewrites) > 0 {
		return progress.PendingRewrites[0], nil
	}
	return progress.NextChapter(), nil
}

// trackDeadlock maintains the deadlock count: seeing the same Agent+Task consecutively means the previous round
// did not satisfy the routing postcondition. Intermediate checkpoints inside a Worker such as plan/draft/edit
// are used only for recovery and observation and must not reset the Engine-level count (issue #84).
// When repeats reaches the threshold, consult the Arbiter; at the hard limit, trip directly.
// Returns stop=true when this round should end the loop; inst may be rewritten by the Arbiter (reroute) or set to nil (recompute).
func (e *engine) trackDeadlock(ctx context.Context, inst **flow.Instruction) (stop bool) {
	in := *inst
	if in == nil || in.Agent == "" {
		*inst = nil
		return false
	}
	key := instructionKey(in)
	if key == e.lastKey {
		e.repeats++
	} else {
		e.lastKey, e.repeats = key, 1
	}
	if e.repeats < deadlockConsultAt {
		return false
	}
	if e.repeats >= deadlockAbortAt {
		e.pauseStuck(notify.KindDeadlock, in, fmt.Sprintf("Ngắt mạch bế tắc: chỉ thị %d lần liên tiếp không tiến triển (%s), đã tạm dừng chờ bạn can thiệp", e.repeats, in.Agent))
		return true
	}
	// Arbiter deadlock consultation (repeats ∈ [consultAt, abortAt)). A retry ruling does not reset the count.
	facts := e.failureFacts("deadlock", in, e.workerErrorFor(in))
	decision, err := runObservedDecision(e.observer, "Phân xử bế tắc", func() (arbiter.FailureDecision, error) {
		return arbiter.DecideFailure(ctx, e.arbiterModel, e.failurePrompt, facts)
	})
	e.recordFailureDecision("deadlock", in, facts, decision, err)
	if err != nil {
		e.pauseWithNotify(notify.KindDeadlock, "Không phân xử bế tắc được, đã tạm dừng chờ bạn can thiệp: "+err.Error())
		return true
	}
	switch decision.Action {
	case "retry":
		return false
	case "reroute":
		*inst = &flow.Instruction{Agent: decision.Dispatch.Agent, Task: decision.Dispatch.Task, Reason: decision.Reason}
		return false
	default: // abort
		e.pauseStuck(notify.KindDeadlock, in, "Phân xử bế tắc: "+decision.Reason)
		return true
	}
}

// runWorker runs a sub-agent directly once: DISPATCH event + progress relay + result parsing.
func (e *engine) runWorker(ctx context.Context, inst *flow.Instruction) error {
	e.observer.dispatchStart(inst.Agent, inst.Task, inst.Reason)
	// Mark the Writer task in progress ahead of time (consistent with the old Dispatcher: the UI outline immediately shows "▸ in progress").
	if inst.Agent == "writer" && inst.Chapter > 0 {
		if err := e.store.Progress.ValidateChapterWork(inst.Chapter); err != nil {
			runErr := fmt.Errorf("%w: %w", errInvalidWriteTarget, err)
			e.observer.dispatchFinish(inst.Agent, runErr)
			return runErr
		}
		if err := e.store.Progress.StartChapter(inst.Chapter); err != nil {
			runErr := fmt.Errorf("%w: không đánh dấu được chương %d đang tiến hành: %w", errInvalidWriteTarget, inst.Chapter, err)
			e.observer.dispatchFinish(inst.Agent, runErr)
			return runErr
		}
	}

	// Worker progress is relayed to the observer via the ctx ToolProgress.
	runCtx := agentcore.WithToolProgress(ctx, func(p agentcore.ProgressPayload) {
		e.observer.workerProgress(p)
	})
	_, err := e.workers.Run(runCtx, inst.Agent, inst.Task)
	if err == nil {
		// Clear failure tracking on success: the next failure of the same key again gets the "retry once first" allowance.
		e.failedKey = ""
	}
	e.observer.dispatchFinish(inst.Agent, err)
	return err
}

// handleWorkerError retries the same instruction once first, then hands the error type and current facts to the Arbiter.
// The Engine does not hard-code which execution errors are "necessarily unrecoverable"; semantic rerouting is decided by the model, and the Store boundary continues
// to block illegal writes.
func (e *engine) handleWorkerError(ctx context.Context, inst *flow.Instruction, werr error) (stop bool) {
	msg := werr.Error()

	key := instructionKey(inst)
	if e.failedKey != key {
		// First failure: retry the original instruction once (the next round's Route recomputes; fact-driven, so naturally idempotent).
		e.failedKey = key
		return false
	}
	e.failedKey = ""
	facts := e.failureFacts("worker_failure", inst, werr)
	decision, err := runObservedDecision(e.observer, "Phân xử lỗi", func() (arbiter.FailureDecision, error) {
		return arbiter.DecideFailure(ctx, e.arbiterModel, e.failurePrompt, facts)
	})
	e.recordFailureDecision("worker_failure", inst, facts, decision, err)
	if err != nil {
		e.pauseWithNotify(notify.KindWorkerFailure, "Phân xử lỗi không dùng được, đã tạm dừng chờ bạn can thiệp: "+msg+contentFilterAdvice(werr))
		return true
	}
	switch decision.Action {
	case "retry":
		return false
	case "reroute":
		e.mu.Lock()
		e.next = &flow.Instruction{Agent: decision.Dispatch.Agent, Task: decision.Dispatch.Task, Reason: decision.Reason}
		e.deferGateForNext = false
		e.mu.Unlock()
		return false
	default: // abort
		e.pauseStuck(notify.KindWorkerFailure, inst, "Phân xử lỗi: "+decision.Reason+contentFilterAdvice(werr))
		return true
	}
}

// pauseStuck pauses when the engine gives up on an instruction: the rework chapter is dequeued first, then stop. Used only for exits where the engine has itself judged
// the instruction a dead end (deadlock trip, deadlock/failure ruling abort); infrastructure failures such as an unavailable ruling still go through
// pauseWithNotify, since that is an external problem and should not cost a chapter's rework.
func (e *engine) pauseStuck(kind string, inst *flow.Instruction, body string) {
	if e.dropStuckRewrite(inst) {
		body += fmt.Sprintf("; chương %d đã được đưa ra khỏi hàng đợi viết lại (giữ bản chính thức trước đó), khi tiếp tục sáng tác sẽ tiến từ các chương phía sau", inst.Chapter)
	}
	e.pauseWithNotify(kind, body)
}

// dropStuckRewrite removes a stuck rework chapter from the queue. PendingRewrites is a persisted fact; if it is not dequeued when the engine gives up on
// the instruction, a restart will immediately replay the same dead instruction and lock the whole book permanently (issue #110).
// Returns true if it was actually dequeued.
func (e *engine) dropStuckRewrite(inst *flow.Instruction) bool {
	if inst == nil || inst.Agent != "writer" || inst.Chapter <= 0 {
		return false
	}
	progress, err := e.store.Progress.Load()
	if err != nil || progress == nil || !slices.Contains(progress.PendingRewrites, inst.Chapter) {
		return false
	}
	if err := e.store.Progress.CompleteRewrite(inst.Chapter); err != nil {
		slog.Warn("dequeuing stuck rewrite chapter failed", "module", "engine", "chapter", inst.Chapter, "err", err)
		return false
	}
	return true
}

// discardNonSemanticDeadlockAttempt undoes the semantic attempt that trackDeadlock
// recorded in advance for this dispatch. It excludes only stable error types where the model call was not fully executed; content_filter
// stays on its existing self-healing path, while real no-progress cases such as max_turns, stop_guard and cancellation are still counted.
func (e *engine) discardNonSemanticDeadlockAttempt(inst *flow.Instruction, werr error) {
	if inst == nil || !isNonSemanticWorkerFailure(werr) {
		return
	}
	key := instructionKey(inst)
	if e.lastKey != key || e.repeats <= 0 {
		return
	}
	e.repeats--
	if e.repeats == 0 {
		e.lastKey = ""
	}
}

// isNonSemanticWorkerFailure recognizes only errors where "this model execution produced nothing whose semantics can be judged".
// It relies first on agentcore's error-chain contract; when the error chain has been flattened by the provider, it reuses the log classification.
func isNonSemanticWorkerFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, agentcore.ErrContextOverflow) || errors.Is(err, agentcore.ErrStreamPartial) {
		return true
	}
	providerErr := agentcore.ClassifyProvider(err)
	classified := errors.Is(providerErr, agentcore.ErrProviderStreamIdle) ||
		errors.Is(providerErr, agentcore.ErrProviderQuota) ||
		errors.Is(providerErr, agentcore.ErrProviderRateLimit) ||
		errors.Is(providerErr, agentcore.ErrProviderTimeout) ||
		errors.Is(providerErr, agentcore.ErrProviderAuth) ||
		errors.Is(providerErr, agentcore.ErrProviderNetwork) ||
		errors.Is(providerErr, agentcore.ErrProviderOverloaded)
	return classified || errorKind(err, err.Error()) == "overloaded"
}

func instructionKey(inst *flow.Instruction) string {
	if inst == nil {
		return ""
	}
	return inst.Agent + "\x00" + inst.Task
}

func (e *engine) rememberWorkerError(inst *flow.Instruction, workerErr error) {
	if workerErr == nil || inst == nil {
		e.lastWorkerErrorKey, e.lastWorkerError = "", nil
		return
	}
	e.lastWorkerErrorKey, e.lastWorkerError = instructionKey(inst), workerErr
}

func (e *engine) workerErrorFor(inst *flow.Instruction) error {
	if e.lastWorkerErrorKey != instructionKey(inst) {
		return nil
	}
	return e.lastWorkerError
}

// contentFilterAdvice attaches a way out the user can act on to a pause caused by a content-moderation block.
// Moderation is a provider black box; pre-checking and evasion are both infeasible, so all we can do is hand the decision to the user;
// the block itself does not trip the breaker early: re-dispatching with a different context has a real self-healing rate (measured on ch21-24),
// so run through "free retry → arbitration" before pausing.
func contentFilterAdvice(werr error) string {
	if !errors.Is(werr, agentcore.ErrProviderContentFilter) {
		return ""
	}
	return ". Đây là provider chặn nội dung (không phải lỗi cục bộ), có thể: dùng /model chuyển sang provider không có tầng kiểm duyệt rồi nhập \"tiếp tục\"; hoặc sửa lại cách diễn đạt của bản nháp chương này (drafts/) rồi tiếp tục; thử lại nguyên trạng nhiều khả năng vẫn bị chặn"
}

// errInvalidWriteTarget marks an illegal write target rejected by the runWorker precheck, so the error chain and
// Arbiter facts keep stable semantics; whether to retry or reroute is still decided by the unified failure flow.
var errInvalidWriteTarget = errors.New("Mục tiêu viết không hợp lệ")

func (e *engine) failureFacts(kind string, inst *flow.Instruction, workerErr error) arbiter.FailureFacts {
	f := arbiter.FailureFacts{Kind: kind, Agent: inst.Agent, Task: inst.Task, Repeats: e.repeats}
	if workerErr != nil {
		f.Error = workerErr.Error()
		f.ErrorKind = errorKind(workerErr, f.Error)
		if f.ErrorKind == "" {
			f.ErrorKind = "unknown"
		}
	}
	missing, err := e.store.FoundationMissing()
	if err != nil {
		f.FactWarnings = append(f.FactWarnings, "Đọc trạng thái thiết lập nền tảng thất bại: "+err.Error())
	} else {
		f.FoundationGap = missing
	}
	p, err := e.store.Progress.Load()
	if err != nil {
		f.FactWarnings = append(f.FactWarnings, "Đọc tiến độ sáng tác thất bại: "+err.Error())
	}
	if p != nil {
		f.Phase = string(p.Phase)
		f.NextChapter = p.NextChapter()
		f.PendingQueue = p.PendingRewrites
	}
	return f
}

func (e *engine) recordFailureDecision(kind string, inst *flow.Instruction, facts arbiter.FailureFacts, d arbiter.FailureDecision, derr error) {
	rec := storepkg.DecisionRecord{Kind: kind, Decider: "arbiter", Input: inst.Agent + ": " + inst.Task, Reason: d.Reason}
	if data, err := json.Marshal(facts); err == nil {
		rec.Facts = data
	}
	if derr == nil {
		if data, err := json.Marshal(d); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	if _, err := e.store.Decisions.Append(rec); err != nil {
		slog.Warn("saving audit record failed", "module", "engine", "kind", kind, "err", err)
	}
}

// applyPendingOps commits an intervention's control-state actions at the loop boundary; the loop drains the queue: a synchronous re-consultation
// (reconsult) appends new actions during application, and they must all be consumed within this boundary, otherwise
// an extra worker would be dispatched in between (an intervention must take effect before later creative work).
// Returns whether a hold+dispatch pair exists whose paired dispatch must run first; in that case the caller defers the Gate check.
func (e *engine) applyPendingOps(ctx context.Context) (deferGate bool) {
	for {
		e.mu.Lock()
		ops := e.pending
		e.pending = nil
		e.mu.Unlock()
		if len(ops) == 0 {
			return deferGate
		}
		for _, op := range ops {
			pairedHoldDispatch := op.hold != nil && !op.hold.Cancel && op.dispatch != nil
			err := e.applyControlOp(ctx, op)
			if err != nil {
				// Action persistence failed: the host has already cleared PendingSteer on "enqueue succeeded",
				// so store the whole intervention back here, and on recovery/continue re-rule and retry (actions are idempotent + re-consultation uses the new facts).
				if op.text != "" {
					if serr := e.store.RunMeta.SetPendingSteer(op.text); serr != nil {
						slog.Warn("saving intervention failed", "module", "engine", "err", serr)
					}
				}
				e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "Không thực hiện được hành động can thiệp, đã giữ lại; sẽ tự thử lại khi khôi phục/tiếp tục"})
			} else if pairedHoldDispatch && e.nextDefersGate() {
				// Only when both the hold and the paired dispatch have landed successfully may this Gate be bypassed.
				// Continuing to bypass when the hold write failed or the dispatch was dropped because its facts went stale would
				// let an unprotected Worker move forward.
				deferGate = true
			}
		}
	}
}

// applyControlOp executes a single control-state action (hold writes RunMeta directly, reopen calls the tool kernel, dispatch reconciles first).
// When the engine is not running the host calls it directly on the intervention path; returns the first persistence failure (the caller uses it to decide whether to
// keep PendingSteer for recovery replay).
func (e *engine) applyControlOp(ctx context.Context, op controlOp) error {
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if op.dispatch != nil {
		// Expect must be checked before paired actions such as hold land. Otherwise, after a dispatch goes stale the old hold
		// would linger and conflict with the hold from the re-ruling on new facts, ending up pausing without making the change.
		fresh, err := arbiter.CollectInterventionFacts(e.store)
		if err != nil {
			return fmt.Errorf("Làm mới dữ kiện can thiệp: %w", err)
		}
		if fresh.Phase != op.facts.Phase || fresh.Flow != op.facts.Flow ||
			fresh.QueueHead() != op.facts.QueueHead() {
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "Phân công theo phân xử đã lỗi thời (dữ kiện đã tiến), phân xử lại theo dữ kiện mới nhất"})
			e.recordStale(op)
			if op.text != "" && e.reconsult != nil {
				// Synchronous re-consultation: an intervention must take effect before later creative work; asynchronous would let the engine dispatch another worker
				// before the new ruling lands. New actions are drained by applyPendingOps within this boundary.
				e.reconsult(op.text)
			}
			return nil
		}
	}
	if op.hold != nil {
		if op.hold.Cancel {
			meta, err := e.store.RunMeta.Load()
			if err != nil {
				e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "Không đọc được tạm dừng một lần: " + err.Error(), Level: "error"})
				return err
			}
			if meta != nil && meta.AdvanceHold != nil {
				if err := e.store.RunMeta.ClearAdvanceHold(*meta.AdvanceHold); err != nil {
					e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "Không hủy được tạm dừng một lần: " + err.Error(), Level: "error"})
					return err
				}
			}
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Đã hủy tạm dừng một lần", Level: "info"})
		} else {
			hold := domain.AdvanceHold{After: op.hold.After, TargetChapter: op.hold.TargetChapter, Reason: op.hold.Reason}
			if err := e.store.RunMeta.SetAdvanceHold(hold); err != nil {
				e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "Không đặt được tạm dừng một lần: " + err.Error(), Level: "error"})
				return err // A linked dispatch must not execute when the hold has not landed
			}
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Đã đặt tạm dừng một lần: " + op.hold.Reason, Level: "info"})
		}
	}
	if op.reopen != nil {
		args, _ := json.Marshal(map[string]any{"chapters": op.reopen.Chapters, "reason": op.reopen.Reason})
		if _, err := tools.NewReopenBookTool(e.store).Execute(ctx, args); err != nil {
			e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "Không mở lại để viết lại được: " + err.Error(), Level: "error"})
			fail(err)
		} else {
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
				Summary: fmt.Sprintf("Đã mở lại cả truyện để viết lại: chương %v vào hàng đợi", op.reopen.Chapters), Level: "info"})
		}
	}
	if op.dispatch != nil {
		// Expect was checked before any paired state was written. CheckpointSeq is kept for audit only and does not take part in
		// reconciliation: when the intervention arrives the worker is usually running, so seq has necessarily advanced.
		e.mu.Lock()
		// Known window (best-effort boundary, see engine-arbiter.md clarification ③): the dispatch lives only in memory from here on,
		// and a hard kill (kill -9, defers do not run) before the worker starts loses this dispatch intent;
		// a normal exit / Abort is covered by the run's defer, which stores PendingSteer back.
		e.next = &flow.Instruction{Agent: op.dispatch.Agent, Task: interventionDispatchTask(op.dispatch.Task, op.text), Reason: "Phân xử can thiệp của người dùng"}
		e.deferGateForNext = op.hold != nil && !op.hold.Cancel
		e.mu.Unlock()
	}
	return firstErr
}

// interventionDispatchTask preserves the user's original intervention, so the Arbiter does not inadvertently widen the
// modification target when paraphrasing the task. Downstream may read wider context to judge, but may treat the original text only as the source of action authorization.
func interventionDispatchTask(task, original string) string {
	task = strings.TrimSpace(task)
	if strings.TrimSpace(original) == "" {
		return task
	}
	return task + "\n\nNguyên văn can thiệp của người dùng (nguồn ủy quyền sửa đổi duy nhất của lần này; ngữ cảnh chỉ để hiểu, không được mở rộng mục tiêu hay phạm vi):\n" + original
}

func (e *engine) recordStale(op controlOp) {
	rec := storepkg.DecisionRecord{Kind: "decision_stale", Decider: "engine", Input: op.text}
	if data, err := json.Marshal(op.facts); err == nil {
		rec.Facts = data
	}
	if _, err := e.store.Decisions.Append(rec); err != nil {
		slog.Warn("recording stale decision failed", "module", "engine", "err", err)
	}
}

// pauseWithNotify is the engine's own pause (deadlock trip / failure-ruling abort): off-screen notification + the host's unified
// pause semantics (onPause → abortWithEvent: lifecycle=paused + on-screen event + cancel ctx).
func (e *engine) pauseWithNotify(kind, body string) {
	e.notify(kind, "warn", "ainovel: Engine tạm dừng", body)
	if e.onPause != nil {
		e.onPause(body)
		return
	}
	e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: body, Level: "warn"})
	e.abort()
}

// completionSummary is the deterministic wrap-up report for a completed book; it spends no LLM call.
func completionSummary(progress domain.Progress, book domain.BookMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\"%s\" đã sáng tác xong: tổng %d chương, %d chữ", book.Title, len(progress.CompletedChapters), progress.TotalWordCount)
	return b.String()
}
