package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/agents"
	"github.com/voocel/ainovel-cli/internal/agents/ctxpack"
	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/host/exp"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/host/sim"
	runtimelog "github.com/voocel/ainovel-cli/internal/logger"
	modelreg "github.com/voocel/ainovel-cli/internal/models"
	"github.com/voocel/ainovel-cli/internal/notify"
	"github.com/voocel/ainovel-cli/internal/revision"
	"github.com/voocel/ainovel-cli/internal/rules"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
	"github.com/voocel/ainovel-cli/internal/userrules"
)

// Host is the runtime shell: lifecycle / intervention entry / event projection / model management.
// Scheduling and execution live in engine (the deterministic loop); semantic rulings live in arbiter (LLM-as-function).
type Host struct {
	cfg             bootstrap.Config
	bundle          assets.Bundle
	store           *storepkg.Store
	bookLease       *bookLease
	styleStats      *tools.StyleStatsIndex
	models          *bootstrap.ModelSet
	engine          *engine
	thinkingApplier agents.ApplyThinking // Adjusting reasoning effort via /model is propagated to each Worker
	writerRestore   *ctxpack.WriterRestorePack
	userRules       *userrules.Service
	observer        *observer
	usage           *UsageTracker
	usageCancel     context.CancelFunc  // Stops autoSaveLoop and triggers one last flush
	budget          *BudgetSentinel     // Budget policy; nil when not enabled (methods are nil-safe)
	gate            *ChapterAdvanceGate // Unified policy component for chapter permits and one-shot pauses
	notifier        *notify.Notifier    // Unattended-run alerts; nil when not enabled (Send is nil-safe)
	configPath      string              // Config write target: /config and /model write to whichever file is currently in effect (the project-level one if it exists, otherwise the global one)
	logCleanup      func()
	fileLogErr      error

	events   chan Event
	streamCh chan string
	done     chan struct{}

	mu         sync.Mutex
	lifecycle  lifecycle
	cocreating bool   // Stage co-creation hold: within the paused window it blocks concurrent involvement of import/simulate/continue
	exclusive  string // Background exclusive job hold (import/simulate/revision): non-empty means some job is running, which blocks concurrent exclusive entry points
	// exclusiveCancel is the cancel function of the current exclusive job: a budget hard stop / manual pause must be able to stop an import that is burning money,
	// not just the Engine — abortWithEvent cancels it when the Engine is not running (the budget sentinel's
	// abort callback and a manual Abort share the same stop mechanism). releaseExclusive clears it as well.
	exclusiveCancel context.CancelFunc
	closeOnce       sync.Once
	asyncWG         sync.WaitGroup
	closing         bool

	interMu sync.Mutex // Intervention rulings are serialized FIFO (at most one in-flight consultation at a time)

	outputMu     sync.RWMutex
	outputClosed bool

	// runCtx bounds the host-side LLM ruling calls (startup ruling / intervention triage); Close cancels it,
	// so that no ruling is still in flight and uninterruptible at exit.
	runCtx    context.Context
	runCancel context.CancelFunc
}

type lifecycle string

const (
	lifecycleIdle      lifecycle = "idle"
	lifecycleRunning   lifecycle = "running"
	lifecyclePaused    lifecycle = "paused"
	lifecycleCompleted lifecycle = "completed"
)

// New creates a Host.
func New(cfg bootstrap.Config, bundle assets.Bundle, options ...NewOption) (*Host, error) {
	cfg.FillDefaults()
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}
	var opts newOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	bookLease, err := acquireBookLease(cfg.OutputDir)
	if err != nil {
		return nil, err
	}
	keepBookLease := false
	var logCleanup func()
	defer func() {
		if keepBookLease {
			return
		}
		if err := bookLease.Close(); err != nil {
			slog.Error("releasing held novel directory failed", "module", "host", "dir", cfg.OutputDir, "err", err)
		}
		if logCleanup != nil {
			logCleanup()
		}
	}()

	var fileLogErr error
	if opts.logFile != "" {
		logCleanup, fileLogErr = runtimelog.SetupFile(cfg.OutputDir, opts.logFile, opts.logAlsoStderr, opts.logAttrs...)
		if fileLogErr != nil {
			logCleanup = nil
			slog.Warn("file log unavailable, continuing with the current process logger", "module", "host", "file", opts.logFile, "err", fileLogErr)
		}
	}

	slog.Info("starting", "module", "boot", "provider", cfg.Provider, "model", cfg.ModelName, "output", cfg.OutputDir)

	// Start a background goroutine to refresh model metadata (window/price) from OpenRouter, with a 24h disk cache.
	modelreg.StartPricingRefresh(modelreg.DefaultRegistry(), bootstrap.DefaultConfigDir())

	store := storepkg.NewStore(cfg.OutputDir)
	// The labels of derived Markdown follow the work's language: these views are read back into context by novel_context,
	// and a mismatch between label language and body language pulls the model toward the other language.
	store.SetLanguage(cfg.NormalizedLanguage())
	if err := store.Init(); err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}
	// RunMeta is the source of truth for all control semantics, and must be validated before constructing models/background tasks.
	// An unknown advance mode returns a structured error directly; guessing a fallback and continuing to write to disk is forbidden.
	if err := store.RunMeta.Init(cfg.Style, cfg.Provider, cfg.ModelName); err != nil {
		return nil, fmt.Errorf("init run meta: %w", err)
	}

	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		return nil, fmt.Errorf("create models: %w", err)
	}
	slog.Info("models ready", "module", "boot", "summary", models.Summary())

	usage := NewUsageTracker(models, store)
	// Prefer reading meta/usage.json; in the following cases do a one-time backfill from sessions/*.jsonl:
	//   - the file does not exist (before the first persistence)
	//   - the schema version does not match (old format discarded after a future upgrade)
	//   - the file exists but is corrupt / IO error (bad data must not reset the totals to zero permanently)
	// SaveNow right after the backfill to fix the result, so the next startup hits it with a plain Load.
	loaded, loadErr := usage.LoadFromStore()
	if loadErr != nil {
		slog.Warn("loading usage failed, will try to backfill from sessions", "module", "usage", "err", loadErr)
	}
	if !loaded {
		if n, err := usage.ReplaySessions(cfg.OutputDir); err != nil {
			slog.Warn("usage replay failed", "module", "usage", "err", err)
		} else if n > 0 {
			slog.Info("usage backfilled from session", "module", "usage", "messages", n)
			if err := usage.SaveNow(); err != nil {
				slog.Warn("saving after usage backfill failed", "module", "usage", "err", err)
			}
		}
	}
	usageCtx, usageCancel := context.WithCancel(context.Background())
	usage.StartAutoSave(usageCtx)

	// onGuardBlock forward declaration: the event-surfacing closure can only be attached after h is constructed.
	var onGuardBlock func(agent, reason string, consecutive int32)
	styleStats := tools.NewStyleStatsIndex(store)
	workers, restore, applyThinking := agents.BuildWorkers(cfg, store, styleStats, models, bundle, usage.Record,
		func(agent, reason string, consecutive int32) {
			if onGuardBlock != nil {
				onGuardBlock(agent, reason, consecutive)
			}
		})
	store.Signals.ClearStaleSignals()

	h := &Host{
		cfg:             cfg,
		bundle:          bundle,
		store:           store,
		bookLease:       bookLease,
		styleStats:      styleStats,
		models:          models,
		thinkingApplier: applyThinking,
		writerRestore:   restore,
		userRules:       userrules.NewService(store, models.Default, rules.DefaultOptions()),
		usage:           usage,
		usageCancel:     usageCancel,
		configPath:      bootstrap.EffectiveConfigPath(),
		logCleanup:      logCleanup,
		fileLogErr:      fileLogErr,
		events:          make(chan Event, 100),
		streamCh:        make(chan string, 256),
		done:            make(chan struct{}, 4),
		lifecycle:       lifecycleIdle,
	}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	h.observer = newObserver(store, h.emitEvent, h.emitDelta, h.emitClear)
	// The host-side Arbiter and Worker share the same ToolProgress → observer → workbench chain.
	h.runCtx = agentcore.WithToolProgress(h.runCtx, h.observer.workerProgress)
	if cfg.Notify.IsEnabled() {
		h.notifier = notify.New(cfg.Notify.Command, cfg.Notify.Events)
	}
	// Budget sentinel: the Engine calls HandleBoundary directly at every loop boundary (no longer via event subscription).
	if sentinel := NewBudgetSentinel(cfg.Budget,
		func() float64 { c, _, _, _, _ := usage.Totals(); return c },
		func(reason string) { h.abortWithEvent(reason, "error") },
		func(level, summary string) {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
			h.notifier.Send(notify.Notification{Kind: notify.KindBudget, Level: level, Title: "ainovel: Ngân sách", Body: summary})
		},
	); sentinel != nil {
		h.budget = sentinel
		usage.SetOnCost(sentinel.OnCost)
		// Billing blind-spot alert: when the model does not report usage, cost stays 0 and the budget never triggers — if the fuse is not connected, someone must be told.
		usage.SetOnMissingUsage(func() {
			const blind = "Điểm mù ngân sách: model không trả về dữ liệu usage, thống kê chi phí bằng 0, giới hạn ngân sách sẽ không kích hoạt (model tùy chỉnh hãy xác nhận giá trong bảng đăng ký hoặc include_usage của upstream)"
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: blind, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindBudget, Level: "warn", Title: "ainovel: Ngân sách", Body: blind})
		})
	}
	// Unified advance gate: executes a one-shot hold, and blocks new chapters without a permit in review mode.
	h.gate = NewChapterAdvanceGate(store,
		func(reason string) {
			h.abortWithEvent(reason, "info")
			h.notifier.Send(notify.Notification{Kind: notify.KindAdvanceGate, Level: "info", Title: "ainovel: Chờ duyệt", Body: reason})
		},
		func(level, summary string) {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
			h.notifier.Send(notify.Notification{Kind: notify.KindAdvanceGate, Level: level, Title: "ainovel: Tiến chương", Body: summary})
		},
	)
	// StopGuard block surfacing: blocked is a high-frequency self-healing action, going only into the on-screen event stream (pushing would flood);
	// escalated / hard_stop mean this round's subtask is scrapped, so the event and notify are emitted as a pair (architecture §2.3).
	onGuardBlock = func(agent, reason string, n int32) {
		switch reason {
		case "escalated":
			body := fmt.Sprintf("%s chạy %d lần liên tiếp mà không lưu kết quả cần thiết, nhiệm vụ lượt này bị chấm dứt và trả về Engine xử lý", agent, n)
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent, Summary: "StopGuard leo thang: " + body, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindStopGuard, Level: "warn", Title: "ainovel: StopGuard", Body: body})
		case "hard_stop":
			body := fmt.Sprintf("%s bị provider từ chối trả lời (safety/content_filter), nhiệm vụ lượt này bị chấm dứt ngay", agent)
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent, Summary: "StopGuard leo thang: " + body, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindStopGuard, Level: "warn", Title: "ainovel: StopGuard", Body: body})
		default: // blocked
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent,
				Summary: fmt.Sprintf("StopGuard: %s định kết thúc khi chưa lưu kết quả cần thiết, đã chặn và thúc giục (lần thứ %d liên tiếp)", agent, n), Level: "info"})
		}
	}
	// Engine: the deterministic execution engine (docs/engine-rfc.md). The arbiter uses the Default model (transitional limitation,
	// see engine-arbiter.md §4.2).
	h.engine = &engine{
		store:           store,
		workers:         workers,
		arbiterModel:    newUsageTrackedModel(models.Default, "arbiter", usage.Record),
		failurePrompt:   bundle.Prompts.ArbiterFailure,
		planStartPrompt: bundle.Prompts.ArbiterPlanStart,
		style:           cfg.Style,
		// Synchronous re-consultation: blocks the engine loop for one ruling (a few seconds), in exchange for "the intervention takes effect before later creative work".
		reconsult: h.handleIntervention,
		observer:  h.observer,
		budget:    h.budget,
		gate:      h.gate,
		refresh:   h.refreshWriterRestore,
		emitEvent: h.emitEvent,
		notify: func(kind, level, title, body string) {
			h.notifier.Send(notify.Notification{Kind: kind, Level: level, Title: title, Body: body})
		},
		onPause: func(summary string) { h.abortWithEvent(summary, "warn") },
		onDone:  h.runEnded,
	}

	keepBookLease = true
	return h, nil
}

// ── Lifecycle ──

// PrepareUserRules generates this book's user-rules snapshot in new-book mode (deterministic on the startup side, not part of the main creative Run).
//
// The input is the user's **original** creative requirement (not wrapped by BuildStartPrompt) — normalization needs the user's rules themselves,
// not the startup scaffolding. The entry must call it once before StartPrepared (both the quick/cocreate new-book paths go through here).
//
// Normalization failure only degrades without an error (enhancement path); only when the snapshot cannot be written to disk is an error returned to abort starting the book —
// subsequent runs would have no stable source of truth (see the design doc, "Failure and degradation").
func (h *Host) PrepareUserRules(rawPrompt string) error {
	if err := h.refuseNewBookOverExisting(); err != nil {
		return err
	}
	svc := userrules.NewService(h.store, h.models.Default, rules.DefaultOptions())
	snap, err := svc.Build(context.Background(), rawPrompt)
	if err != nil {
		return fmt.Errorf("Không lưu được snapshot quy tắc người dùng nên không thể tiếp tục: %w", err)
	}
	logUserRulesSnapshot(snap)
	return nil
}

// ensureUserRules makes sure the snapshot exists on the recovery path; when missing it is generated from
// system_defaults + the rules file.
func (h *Host) ensureUserRules() {
	svc := userrules.NewService(h.store, h.models.Default, rules.DefaultOptions())
	snap, err := svc.GetOrBuild(context.Background())
	if err != nil {
		slog.Warn("reading/creating user rules snapshot failed, falling back to built-in defaults at runtime", "module", "rules", "err", err)
		return
	}
	logUserRulesSnapshot(snap)
}

// logUserRulesSnapshot is the startup echo: lets the user see how the system understood the rules (reuses logging, adds no new mechanism).
func logUserRulesSnapshot(snap *rules.Snapshot) {
	if snap == nil {
		return
	}
	slog.Info("user rules snapshot",
		"module", "rules",
		"status", string(snap.Status),
		"sources", snap.Sources,
		"forbidden_phrases", len(snap.Structured.ForbiddenPhrases),
		"fatigue_words", len(snap.Structured.FatigueWords),
	)
	if snap.Status == rules.StatusDegraded {
		slog.Warn("some rules could not be parsed, running with raw preferences (snapshot can be regenerated)",
			"module", "rules", "uncertain", snap.Uncertain)
	}
}

// StartPrepared starts creation with the user's **original** creative requirement: the plan_start ruling picks the planner and expands
// the requirement, and the ruling result is fixed as
// a fact (PlanStartRecord) before the Engine starts — recovery always relies on facts already on disk and never redoes an existing ruling.
// The input fact (StartPrompt) is written to disk before the ruling: if the ruling fails it is the basis for the engine's make-up ruling,
// and a startup failure can self-heal from any recovery entry (Resume/Continue), so it is not a dead end.
func (h *Host) StartPrepared(rawRequirement string) error {
	h.mu.Lock()
	if h.lifecycle == lifecycleRunning {
		h.mu.Unlock()
		return fmt.Errorf("already running")
	}
	if h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước")
	}
	h.mu.Unlock()

	rawRequirement = strings.TrimSpace(rawRequirement)
	if rawRequirement == "" {
		return fmt.Errorf("prompt is required")
	}
	if err := h.refuseNewBookOverExisting(); err != nil {
		return err
	}
	if err := upgradeProject(h.store); err != nil {
		return err
	}
	if err := h.budget.Refuse(); err != nil {
		return err
	}
	if err := h.store.Checkpoints.Reset(); err != nil {
		return fmt.Errorf("reset checkpoints: %w", err)
	}
	if err := h.store.Progress.Init(0); err != nil {
		return fmt.Errorf("init progress: %w", err)
	}
	// The input fact is written to disk before the ruling: after a ruling failure (model fault etc.) StartPrompt is still there,
	// and on recovery/continue the engine makes up the ruling from it (planStartFallback), so a startup failure is no longer a dead end.
	if err := h.store.RunMeta.SetStartPrompt(rawRequirement); err != nil {
		return fmt.Errorf("Ghi nhận yêu cầu sáng tác: %w", err)
	}

	// Startup ruling: a failure reports an explicit error and aborts (the user is present at startup, and an error beats a guess).
	start := time.Now()
	decision, derr := runObservedDecision(h.observer, "Phân xử khởi động", func() (arbiter.PlanStartDecision, error) {
		return arbiter.DecidePlanStart(h.runCtx, h.arbiterModel(),
			h.bundle.Prompts.ArbiterPlanStart, rawRequirement, h.cfg.Style)
	})
	rec := storepkg.DecisionRecord{Kind: "plan_start", Decider: "arbiter", Input: rawRequirement,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	var recErr error
	if rec, recErr = h.store.Decisions.Append(rec); recErr != nil {
		slog.Warn("saving startup audit record failed", "module", "host", "err", recErr)
	}
	if derr != nil {
		return fmt.Errorf("Không phân xử khởi động được: %w", derr)
	}
	if err := h.store.RunMeta.SetPlanStart(domain.PlanStartRecord{
		RawPrompt: rawRequirement, Planner: decision.Planner, PlannerTask: decision.Task, DecisionID: rec.ID,
	}); err != nil {
		return fmt.Errorf("Ghi nhận phân xử khởi động: %w", err)
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: fmt.Sprintf("Bắt đầu sáng tác (Architect: %s — %s)", decision.Planner, decision.Reason), Level: "info"})
	if !h.startEngine(&flow.Instruction{Agent: decision.Planner, Task: decision.Task, Reason: decision.Reason}) {
		return fmt.Errorf("Engine đang chạy hoặc đang dừng, không thể bắt đầu truyện mới")
	}
	return nil
}

// refuseNewBookOverExisting refuses to start a new book in a book directory that already has written chapters: StartPrepared resets
// checkpoints and progress, so a mis-touch silently wipes the whole book's progress chain (pressing Enter by mistake on the welcome page
// after an import completes is the most typical scenario). Only the completed chapter count is checked — leftovers from the planning phase / a failed startup have no written chapters,
// and are let through to preserve the self-healing paths of co-creation Ctrl+S same-session retry and recovery make-up rulings.
func (h *Host) refuseNewBookOverExisting() error {
	progress, err := h.store.Progress.Load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if progress == nil || len(progress.CompletedChapters) == 0 {
		return nil
	}
	book, err := h.store.Book.Load()
	if err != nil {
		return err
	}
	if book == nil {
		return fmt.Errorf("Thư mục đầu ra đã có chương nhưng không có thông tin tác phẩm")
	}
	name := book.Title
	return fmt.Errorf("Thư mục đầu ra đã có tiến độ sáng tác của \"%s\" (%d chương), tạo mới sẽ đặt lại tiến độ và checkpoint: viết tiếp hãy đi qua cổng khôi phục (khởi động lại ứng dụng sẽ tự khôi phục), truyện mới hãy đổi thư mục đầu ra",
		name, len(progress.CompletedChapters))
}

// startEngine is the unified engine start entry (shared by Start/Resume/Continue/intervention restart).
// lifecycle must be set to running before the goroutine starts: the engine may end immediately (book complete / no route),
// and runEnded will land lifecycle in a terminal state; if the order were reversed, with runEnded running first and running written here afterwards,
// the UI would show "running" forever while the engine has actually stopped.
func (h *Host) startEngine(initial *flow.Instruction) bool {
	// Cross-restart gate: while an unfinished import workspace exists, the ordinary Engine is forbidden from consuming a half-published state (RFC §12.5).
	active, done, importErr := imp.ResumeStatus(h.store)
	if importErr != nil {
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error",
			Summary: "Không đọc được trạng thái nhập truyện, đã ngăn sáng tác thông thường ghi đè nội dung hiện có: " + importErr.Error()})
		return false
	}
	if active && !done {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
			Summary: "Có lượt nhập truyện ngoài chưa hoàn tất, hãy chạy /import để khôi phục hoàn tất rồi mới tiếp tục sáng tác"})
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	// While a background exclusive job (import/simulate) is in progress the engine must not jump ahead, to avoid competing with its writes. This is the unified backstop of all engine start paths
	// (Resume/Continue restart / auto relay / next) — the entry guard is the first line, and this is the last.
	if h.exclusive != "" {
		return false
	}
	// lifecycle may already be paused, but the old Engine goroutine is still running its exit defer.
	// The Engine's real state must be checked at the same time; otherwise lifecycle would be changed back to running while start
	// is actually a no-op, and then the old runEnded would land it as idle.
	if h.engine.isRunning() {
		return false
	}
	h.observer.setAborting(false)
	previous := h.lifecycle
	h.lifecycle = lifecycleRunning
	if !h.engine.start(initial) {
		h.lifecycle = previous
		return false
	}
	return true
}

// Reopen forcibly reopens a concluded book into the creative state. Both completion and reopening are re-decisions: completion can be ruled by the architect,
// while reopening can only be explicitly initiated by the user (/reopen), with no model ruling. When direction is non-empty it is registered as a pending intervention,
// and on recovery it is first injected through the Arbiter ruling (the same channel as an intervention made while stopped), then the engine continues (the end-of-volume route dispatches a continuation volume).
func (h *Host) Reopen(direction string) error {
	h.mu.Lock()
	switch {
	case h.lifecycle == lifecycleRunning:
		h.mu.Unlock()
		return fmt.Errorf("Engine sáng tác đang chạy, không cần mở lại")
	case h.cocreating:
		h.mu.Unlock()
		return fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước")
	case h.exclusive != "":
		ex := h.exclusive
		h.mu.Unlock()
		return fmt.Errorf("%s đang chạy, hãy hoàn tất rồi mới mở lại", ex)
	}
	h.mu.Unlock()
	if err := h.requireCleanChapters(); err != nil {
		return err
	}

	if err := h.store.Progress.ReopenContinue(); err != nil {
		return err
	}
	reopenEvent := Event{Time: time.Now(), Category: "SYSTEM", Summary: "Đã mở lại cuốn sách ở trạng thái sáng tác (người dùng hủy phán quyết kết thúc truyện)", Level: "info"}
	if d := strings.TrimSpace(direction); d != "" {
		reopenEvent.Detail = reopenEvent.Summary + "\nHướng viết tiếp: " + d
	}
	h.emitEvent(reopenEvent)
	if d := strings.TrimSpace(direction); d != "" {
		if err := h.store.RunMeta.SetPendingSteer(d); err != nil {
			return fmt.Errorf("Đã mở lại, nhưng không đăng ký được hướng viết tiếp: %v; hãy nhập lại hướng đi trực tiếp vào ô nhập", err)
		}
	}
	return nil
}

// Resume is recovery mode: generates a resume prompt from checkpoint + progress and starts.
func (h *Host) Resume() (string, error) {
	h.mu.Lock()
	if h.lifecycle == lifecycleRunning {
		h.mu.Unlock()
		return "", fmt.Errorf("already running")
	}
	if h.cocreating {
		h.mu.Unlock()
		return "", fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước")
	}
	if h.exclusive != "" {
		ex := h.exclusive
		h.mu.Unlock()
		return "", fmt.Errorf("%s đang chạy, hãy hoàn tất rồi mới khôi phục sáng tác", ex)
	}
	h.mu.Unlock()
	if err := upgradeProject(h.store); err != nil {
		return "", err
	}

	label, err := resumeLabel(h.store)
	if err != nil {
		return "", err
	}
	if label == "" {
		return "", nil // New-book mode, no recovery
	}
	if err := h.requireCleanChapters(); err != nil {
		return label, err
	}
	if err := h.budget.Refuse(); err != nil {
		return "", err
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Khôi phục sáng tác: " + label, Level: "info"})
	for _, w := range h.store.CheckConsistency() {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Cảnh báo nhất quán: " + w, Level: "warn"})
	}
	// Make sure the user-rules snapshot exists; if it already exists, read it cheaply.
	h.ensureUserRules()
	h.refreshWriterRestore()
	// Pending interventions (left over while stopped / residue from a crash during a ruling) must go before the engine's resume ruling —
	// otherwise the engine may race ahead of the ruling and keep writing chapters that contradict the intervention. Executed synchronously (blocking for a few seconds is acceptable,
	// the UI already shows "resuming creation"); after doIntervention succeeds it clears PendingSteer itself and
	// brings the engine up with restart=true. With no pending intervention → continue straight away.
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return label, fmt.Errorf("Đọc can thiệp đang chờ xử lý: %w", err)
	}
	if meta != nil && meta.PendingSteer != "" {
		if err := h.doIntervention(meta.PendingSteer, true); err != nil {
			return label, err
		}
	} else {
		// Restore only facts, not the session (RFC §6): the Engine recomputes routing from the store and continues.
		if !h.startEngine(nil) {
			return label, fmt.Errorf("Engine đang hoàn tất lượt dừng trước, hãy thử khôi phục lại sau")
		}
	}
	// lifecycle is managed by startEngine / runEnded and is not overwritten here —
	// overwriting it when the engine ends immediately (book complete etc.) would change the terminal state back to running.
	return label, nil
}

// handleIntervention adapts the Engine's callback for re-consultation with no return value; the error has already been emitted as an event by doIntervention.
func (h *Host) handleIntervention(text string) {
	_ = h.doIntervention(text, false)
}

// doIntervention is the unified ruling path for user interventions: Collect → Decide → execute.
// Serialized FIFO (at most one in-flight consultation at a time); answer/rules execute immediately, while control-state actions
// (hold/reopen/dispatch) are queued for boundary commit while the engine runs, and executed immediately when stopped.
// With restart=true (Continue semantics), make sure the engine is running after the intervention is handled.
func (h *Host) doIntervention(text string, restart bool) error {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	// Crash protection: persist (PendingSteer) before the ruling, and clear atomically after a successful apply or after the failure has been echoed to the user
	// (ClearHandledSteer also resets FlowSteering). Crash during a ruling → replayed on the next Resume.
	if err := h.store.RunMeta.SetPendingSteer(text); err != nil {
		wrapped := fmt.Errorf("Không lưu được can thiệp, đã dừng phân xử: %w", err)
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Agent: "arbiter",
			Summary: wrapped.Error(), Detail: wrapped.Error(), Level: "error"})
		return wrapped
	}
	clearPending := func() error {
		if err := h.store.ClearHandledSteer(); err != nil {
			return fmt.Errorf("Không xóa được can thiệp đã xử lý: %w", err)
		}
		return nil
	}

	facts, err := arbiter.CollectInterventionFacts(h.store)
	if err != nil {
		wrapped := fmt.Errorf("Không thu thập được dữ kiện can thiệp, chưa gọi Arbiter: %w", err)
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Agent: "arbiter",
			Summary: wrapped.Error(), Detail: wrapped.Error(), Level: "error"})
		return wrapped
	}
	facts.Running = h.engine.isRunning()

	start := time.Now()
	decision, derr := runObservedDecision(h.observer, "Phân xử can thiệp của người dùng", func() (arbiter.InterventionDecision, error) {
		return arbiter.DecideIntervention(h.runCtx, h.arbiterModel(),
			h.bundle.Prompts.ArbiterIntervention, facts, text)
	})

	rec := storepkg.DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: text,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if cp := h.store.Checkpoints.LatestGlobal(); cp != nil {
		rec.CheckpointSeq = cp.Seq
	}
	if data, err := json.Marshal(facts); err == nil {
		rec.Facts = data
	}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	if _, err := h.store.Decisions.Append(rec); err != nil {
		wrapped := fmt.Errorf("Không lưu được nhật ký kiểm tra can thiệp, từ chối thực hiện hành động: %w", err)
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Agent: "arbiter",
			Summary: wrapped.Error(), Detail: wrapped.Error(), Level: "error"})
		return wrapped
	}

	if derr != nil {
		// Better to do nothing than to act wrongly: produce no writes at all. Call errors and
		// output validation errors share the same error channel and must be echoed as is, not disguised uniformly as "failed to understand".
		// Already told face to face → clear pending (otherwise the next Resume would automatically replay the same failed intervention).
		h.emitEvent(newInterventionFailureEvent(derr))
		if err := clearPending(); err != nil {
			return fmt.Errorf("%v; %w", derr, err)
		}
		return derr
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Phân xử: " + decision.Reason, Level: "info"})
	if decision.Answer != "" {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: decision.Answer, Level: "info"})
	}
	// Persistence failure of any action → keep PendingSteer (on recovery the whole thing is replayed and re-ruled;
	// hold/reopen are idempotent and dispatch re-consults on new facts, so replay is safe).
	var actionErr error
	if decision.Rules != "" {
		if snap, _, err := h.userRules.AddRuntimeRule(h.runCtx, decision.Rules); err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "Không lưu được quy tắc viết: " + err.Error(), Level: "error"})
			actionErr = err
		} else if snap != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Quy tắc viết đã được cập nhật và lưu", Level: "info"})
		}
	}

	if decision.Hold != nil || decision.Reopen != nil || decision.Dispatch != nil {
		op := controlOp{hold: decision.Hold, reopen: decision.Reopen, dispatch: decision.Dispatch, text: text, facts: facts}
		if !h.engine.enqueue(op) {
			// Engine not running: execute immediately; on persistence failure → keep PendingSteer, and on recovery replay the whole intervention.
			if err := h.engine.applyControlOp(context.Background(), op); err != nil {
				h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "Không thực hiện được hành động can thiệp, đã giữ lại; khi khôi phục/tiếp tục sẽ tự thử lại"})
				return err
			}
			// reopen/dispatch express an intent to continue creating, so bring the engine up.
			if decision.Reopen != nil || decision.Dispatch != nil {
				restart = true
			}
		}
	}
	if actionErr != nil {
		// Keep PendingSteer: on recovery/continue the whole thing is replayed and re-ruled.
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
			Summary: "Một số hành động can thiệp chưa thành công, can thiệp được giữ lại; khi khôi phục/tiếp tục sẽ tự thử lại"})
		return actionErr
	}
	// The actions were successfully applied/enqueued, so clear crash protection (engine-side failures or exit races after enqueue are covered by the engine
	// storing PendingSteer back).
	if err := clearPending(); err != nil {
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error", Summary: err.Error()})
		return err
	}

	if restart && !h.engine.isRunning() {
		if err := h.budget.Refuse(); err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: err.Error(), Level: "warn"})
			return err
		}
		h.refreshWriterRestore()
		if !h.startEngine(nil) {
			// By now the intervention actions have taken effect and PendingSteer is cleared; only the engine could not be brought up immediately — we cannot falsely claim "saved".
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "Can thiệp đã có hiệu lực nhưng Engine chưa thể chạy tiếp ngay; hãy tiếp tục ở ô nhập sau hoặc khởi động lại ứng dụng để khôi phục"})
			return fmt.Errorf("Can thiệp đã có hiệu lực nhưng Engine chưa thể chạy tiếp ngay")
		}
	}
	return nil
}

func newInterventionFailureEvent(err error) Event {
	detail := err.Error()
	return Event{
		Time:     time.Now(),
		Category: "ERROR",
		Agent:    "arbiter",
		Summary:  "Không phân xử được can thiệp: " + detail + " (chưa thay đổi gì)",
		Detail:   detail,
		Kind:     errorKind(err, detail),
		Level:    "error",
	}
}

// arbiterModel returns the ruling model with usage tracking (tokens/cost go into the budget and usage systems).
func (h *Host) arbiterModel() agentcore.ChatModel {
	return newUsageTrackedModel(h.models.Default, "arbiter", h.usage.Record)
}

// Continue is called when the user types into the input box after a stop: intervention ruling + make sure the engine runs again.
func (h *Host) Continue(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("text is required")
	}
	h.mu.Lock()
	if h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước")
	}
	if h.exclusive != "" {
		ex := h.exclusive
		h.mu.Unlock()
		// During an exclusive job it must be blocked before the ruling: otherwise the Arbiter would already have changed PendingSteer / rules / control state before the engine is stopped by the gate.
		return fmt.Errorf("%s đang chạy, hãy hoàn tất rồi mới tiếp tục sáng tác", ex)
	}
	h.mu.Unlock()
	if err := h.requireCleanChapters(); err != nil {
		return err
	}
	if err := h.budget.Refuse(); err != nil {
		return err
	}

	err, launched := h.runAsync(func() error {
		h.emitEvent(Event{Time: time.Now(), Category: "USER", Summary: "[Tiếp tục] " + text, Level: "info"})
		return h.doIntervention(text, true)
	})
	if !launched {
		return fmt.Errorf("Host đang đóng, không thể tiếp tục sáng tác")
	}
	return err
}

// SetAdvanceMode deterministically switches the chapter advance mode. It only writes the user's run intent,
// does not call the Arbiter, and does not implicitly start an Engine that is already paused.
func (h *Host) SetAdvanceMode(mode domain.ChapterAdvanceMode) error {
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if err := h.store.RunMeta.SetAdvanceMode(mode); err != nil {
		return err
	}
	label := "tự động tiến chương"
	if mode == domain.ChapterAdvanceReview {
		label = "duyệt từng chương"
	}
	summary := "Chế độ tiến chương đã chuyển sang " + label
	h.mu.Lock()
	state := h.lifecycle
	h.mu.Unlock()
	if mode == domain.ChapterAdvanceAuto && state != lifecycleRunning && state != lifecycleCompleted {
		summary += "; hiện vẫn đang tạm dừng, nhập lệnh tiếp tục để chạy lại"
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "info"})
	return nil
}

// AdvanceOneChapter authorizes one exact chapter and starts the Engine in per-chapter approval mode.
func (h *Host) AdvanceOneChapter() error {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	h.mu.Lock()
	running, cocreating, ex := h.lifecycle == lifecycleRunning, h.cocreating, h.exclusive
	h.mu.Unlock()
	if running || h.engine.isRunning() {
		return fmt.Errorf("Sáng tác vẫn đang chạy hoặc đang hoàn tất tạm dừng, hãy chạy /next sau")
	}
	if cocreating {
		return fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước")
	}
	if ex != "" {
		return fmt.Errorf("%s đang chạy, hãy hoàn tất rồi mới chạy /next", ex)
	}
	if err := h.requireCleanChapters(); err != nil {
		return err
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("RunMeta chưa được khởi tạo")
	}
	if meta.AdvanceMode != domain.ChapterAdvanceReview {
		return fmt.Errorf("/next chỉ dùng cho chế độ duyệt từng chương, hãy chạy /review on trước")
	}
	if meta.AdvanceHold != nil {
		return fmt.Errorf("Vẫn còn ý định tạm dừng một lần đang chờ xử lý (%s), hãy khôi phục hoặc hoàn tất can thiệp hiện tại trước", meta.AdvanceHold.Reason)
	}
	if err := h.budget.Refuse(); err != nil {
		return err
	}
	progress, err := h.store.Progress.Load()
	if err != nil {
		return err
	}
	if progress == nil || progress.Phase != domain.PhaseWriting {
		phase := "<nil>"
		if progress != nil {
			phase = string(progress.Phase)
		}
		return fmt.Errorf("Giai đoạn hiện tại không thể cấp phép chương mới (phase=%s)", phase)
	}
	target := progress.NextChapter()
	if target <= 0 {
		return fmt.Errorf("Không thể suy ra chương kế từ tiến độ hiện tại")
	}
	if err := h.store.RunMeta.GrantAdvancePermit(target); err != nil {
		return err
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: fmt.Sprintf("Đã cho phép chương %d; sau khi nộp chương này sẽ hoàn tất đánh giá cần thiết và bảo trì cấu trúc cung/tập, rồi lại chờ cho phép", target), Level: "info"})
	h.refreshWriterRestore()
	if !h.startEngine(nil) {
		// A permit is persisted per chapter number and idempotent for the same target, so a later retry by the caller does not authorize twice.
		return fmt.Errorf("Giấy phép chương đã lưu, nhưng Engine vẫn đang hoàn tất lượt dừng trước; hãy thử lại /next sau")
	}
	return nil
}

// Steer submits a user intervention (available any time while running; when stopped, the actions decide whether to bring up the engine after the ruling).
// The TUI waits for the result via tea.Cmd, so it receives real ruling/persistence errors without blocking the interface.
func (h *Host) Steer(text string) error {
	err, launched := h.runAsync(func() error {
		h.emitEvent(Event{Time: time.Now(), Category: "USER", Summary: "[Can thiệp] " + text, Level: "info"})
		return h.doIntervention(text, false)
	})
	if !launched {
		return fmt.Errorf("Host đang đóng, không thể nộp can thiệp")
	}
	return err
}

// Abort pauses the current engine loop.
func (h *Host) Abort() bool {
	return h.abortWithEvent("Người dùng đã tạm dừng sáng tác thủ công", "warn")
}

// abortWithEvent performs the pause with an event for the given reason. Budget stops and manual pauses share the same stop mechanism,
// differing only in the event wording (budget stop = an Abort instruction the user signed in advance, semantically equivalent to a manual pause).
func (h *Host) abortWithEvent(summary, level string) bool {
	h.mu.Lock()
	running := h.lifecycle == lifecycleRunning
	if running {
		h.lifecycle = lifecyclePaused
	}
	cancelExclusive := h.exclusiveCancel
	h.mu.Unlock()
	if running {
		// The flag must be set before engine.abort: cancel propagation immediately triggers stream init / worker
		// failure events, and the observer uses this flag to recognize them as abort-derived noise and suppress them.
		h.observer.setAborting(true)
		h.engine.abort()
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
		return true
	}
	// The Engine is not running but an exclusive job (import etc.) is: it is burning money too, and a budget hard stop / manual pause must
	// be able to stop it — otherwise the budget policy is a dead letter for imports (docs/import-pipeline.md §13.1).
	if cancelExclusive != nil {
		cancelExclusive()
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
		return true
	}
	return false
}

// Close terminates the engine and closes the event channel.
//
// Usage persistence semantics: first cancel autoSaveLoop (it flushes the last dirty state itself),
// then do one more synchronous SaveNow to wrap up. After termination, the last few hundred tokens of in-flight LLM calls
// that are lost are backfilled automatically by the session jsonl replay at the next startup.
func (h *Host) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closing = true
		cancelExclusive := h.exclusiveCancel
		h.mu.Unlock()

		h.observer.setAborting(true)
		if h.runCancel != nil {
			h.runCancel() // Interrupt the in-flight host-side ruling calls and the supervisor forwarding
		}
		if cancelExclusive != nil {
			cancelExclusive()
		}
		h.engine.abort()
		h.engine.wait()
		h.asyncWG.Wait()

		if h.usageCancel != nil {
			h.usageCancel()
			h.usageCancel = nil
		}
		h.usage.WaitAutoSave()
		if err := h.usage.SaveNow(); err != nil {
			slog.Warn("saving usage before exit failed", "module", "usage", "err", err)
		}
		h.closeOutputChannels()
		if err := h.bookLease.Close(); err != nil {
			slog.Error("releasing held novel directory failed", "module", "host", "dir", h.cfg.OutputDir, "err", err)
		}
		if h.logCleanup != nil {
			h.logCleanup()
			h.logCleanup = nil
		}
	})
}

// FileLogError returns the file-logger initialization error from the construction phase; it does not change during the Host lifecycle.
func (h *Host) FileLogError() error {
	return h.fileLogErr
}

// runEnded is called back by engine.onDone when the engine loop ends (for any reason): it settles the terminal state from store facts.
//   - Phase=Complete  → mark completed, emit the "creation complete" event
//   - otherwise       → mark idle/paused, emit the "creation stopped" event
func (h *Host) runEnded() {
	h.observer.finalize()

	h.mu.Lock()
	progress, err := h.store.Progress.Load()
	if err != nil {
		if h.lifecycle == lifecycleRunning {
			h.lifecycle = lifecycleIdle
		}
		h.mu.Unlock()
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error",
			Summary: "Không đọc được tiến độ khi Engine kết thúc: " + err.Error()})
		select {
		case h.done <- struct{}{}:
		default:
		}
		return
	}
	book, err := h.store.Book.Load()
	if err != nil {
		h.lifecycle = lifecycleIdle
		h.mu.Unlock()
		h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error",
			Summary: "Không đọc được thông tin tác phẩm khi Engine kết thúc: " + err.Error()})
		select {
		case h.done <- struct{}{}:
		default:
		}
		return
	}
	if progress != nil && progress.Phase == domain.PhaseComplete {
		if book == nil {
			h.lifecycle = lifecycleIdle
			h.mu.Unlock()
			h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Level: "error",
				Summary: "Engine kết thúc nhưng không có thông tin tác phẩm"})
			select {
			case h.done <- struct{}{}:
			default:
			}
			return
		}
		h.lifecycle = lifecycleCompleted
		// Book-complete wrap-up: generated deterministically (the store already has all facts and no LLM call is spent; see the last section of the RFC).
		summary := completionSummary(*progress, *book)
		h.mu.Unlock()
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "success"})
		h.notifier.Send(notify.Notification{
			Kind: notify.KindRunEnd, Level: "info", Title: "ainovel: Sáng tác hoàn tất",
			Body: h.runEndBody("", summary),
		})
	} else {
		wasRunning := h.lifecycle == lifecycleRunning
		if wasRunning {
			h.lifecycle = lifecycleIdle
		}
		completed := 0
		title := ""
		if progress != nil {
			completed = len(progress.CompletedChapters)
		}
		if book != nil {
			title = book.Title
		}
		h.mu.Unlock()
		if wasRunning {
			summary := fmt.Sprintf("Engine đã dừng (đã hoàn thành %d chương)", completed)
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "warn"})
			h.notifier.Send(notify.Notification{
				Kind: notify.KindRunEnd, Level: "warn", Title: "ainovel: Sáng tác dừng",
				Body: h.runEndBody(title, summary),
			})
		}
	}

	select {
	case h.done <- struct{}{}:
	default:
	}
}

// runEndBody assembles the run_end notification body: book title + progress summary + cumulative spend.
func (h *Host) runEndBody(title, summary string) string {
	if name := strings.TrimSpace(title); name != "" {
		summary = "\"" + name + "\"" + summary
	}
	cost, _, _, _, _ := h.usage.Totals()
	if cost > 0 {
		summary += fmt.Sprintf(" · chi phí $%.2f", cost)
	}
	return summary
}

// ── Channels ──

// StreamClearSentinel is sent as a single item via streamCh to signal "clear the current streaming round".
// A separate clearCh is no longer used — with two unordered channels the ✻ header often landed at the end of the previous round.
const StreamClearSentinel = "\x00\x00CLEAR\x00\x00"

func (h *Host) Events() <-chan Event  { return h.events }
func (h *Host) Stream() <-chan string { return h.streamCh }
func (h *Host) Done() <-chan struct{} { return h.done }
func (h *Host) Dir() string           { return h.store.Dir() }

// ── Event emission ──

func (h *Host) emitEvent(ev Event) {
	h.outputMu.RLock()
	defer h.outputMu.RUnlock()
	if h.outputClosed {
		return
	}
	// The read lock guarantees events are fully written before closing; events after closing are rejected outright.
	LogEvent(ev)
	select {
	case h.events <- ev:
	default:
		select {
		case <-h.events:
		default:
		}
		select {
		case h.events <- ev:
		default:
		}
	}
}

func (h *Host) emitDelta(delta string) {
	h.outputMu.RLock()
	defer h.outputMu.RUnlock()
	if h.outputClosed {
		return
	}
	select {
	case h.streamCh <- delta:
	default:
		select {
		case <-h.streamCh:
		default:
		}
		select {
		case h.streamCh <- delta:
		default:
		}
	}
}

func (h *Host) closeOutputChannels() {
	h.outputMu.Lock()
	defer h.outputMu.Unlock()
	if h.outputClosed {
		return
	}
	h.outputClosed = true
	close(h.done)
	close(h.events)
	close(h.streamCh)
}

func (h *Host) emitClear() {
	// Goes through the "sentinel" via streamCh, guaranteeing in-order delivery to the TUI in the same channel as emitDelta.
	h.emitDelta(StreamClearSentinel)
}

// ── Snapshot (TUI state aggregation) ──

func (h *Host) Snapshot() UISnapshot {
	h.mu.Lock()
	state := h.lifecycle
	provider, model, _ := h.models.CurrentSelection("default")
	modelWindow, _ := h.cfg.ResolveContextWindow(provider, model)
	thinkingLevel := h.cfg.ResolveReasoningEffort("default")
	style := h.cfg.Style
	h.mu.Unlock()

	// Dynamically resolve the current model's context window; after a /model or /config switch the next Snapshot reflects it automatically.
	cost, tokIn, tokOut, cacheRead, cacheWrite := h.usage.Totals()
	saved := h.usage.SavedUSD()
	overallCapable := h.usage.OverallCacheCapable()
	recentRead, recentInput, recentSamples := h.usage.OverallRecent()
	perAgent := h.usage.PerAgent()
	cacheStats := make([]AgentCacheStat, 0, len(perAgent))
	for _, a := range perAgent {
		cacheStats = append(cacheStats, AgentCacheStat{
			Role:            a.Role,
			Input:           a.Input,
			Output:          a.Output,
			CacheRead:       a.CacheRead,
			CacheWrite:      a.CacheWrite,
			Cost:            a.Cost,
			Saved:           a.Saved,
			CacheCapable:    a.CacheCapable,
			RecentCacheRead: a.RecentCacheRead,
			RecentInput:     a.RecentInput,
			RecentSamples:   a.RecentSamples,
		})
	}
	perModel := h.usage.PerModel()
	modelStats := make([]AgentCacheStat, 0, len(perModel))
	for _, a := range perModel {
		modelStats = append(modelStats, AgentCacheStat{
			Model:        a.Model,
			Input:        a.Input,
			Output:       a.Output,
			CacheRead:    a.CacheRead,
			CacheWrite:   a.CacheWrite,
			Cost:         a.Cost,
			Saved:        a.Saved,
			CacheCapable: a.CacheCapable,
		})
	}

	snap := UISnapshot{
		Provider:               provider,
		ModelName:              model,
		ModelContextWindow:     modelWindow,
		ThinkingLevel:          thinkingLevel,
		Style:                  style,
		RuntimeState:           string(state),
		IsRunning:              state == lifecycleRunning,
		TotalInputTokens:       tokIn,
		TotalOutputTokens:      tokOut,
		TotalCacheReadTokens:   cacheRead,
		TotalCacheWriteTokens:  cacheWrite,
		TotalCostUSD:           cost,
		TotalSavedUSD:          saved,
		BudgetLimitUSD:         h.budget.Limit(),
		OverallCacheCapable:    overallCapable,
		OverallRecentCacheRead: recentRead,
		OverallRecentInput:     recentInput,
		OverallRecentSamples:   recentSamples,
		TotalCacheBreaks:       h.usage.OverallCacheBreaks(),
		CachePerAgent:          cacheStats,
		CachePerModel:          modelStats,
		MissingAssistantUsage:  h.usage.MissingAssistantUsage(),
	}

	if book, _ := h.store.Book.Load(); book != nil {
		snap.BookTitle = book.Title
		snap.Synopsis = truncate(book.Synopsis, 200)
	}
	progress, _ := h.store.Progress.Load()
	if progress != nil {
		snap.Phase = string(progress.Phase)
		snap.Flow = string(progress.Flow)
		snap.CurrentChapter = progress.CurrentChapter
		snap.TotalChapters = progress.TotalChapters
		snap.CompletedCount = len(progress.CompletedChapters)
		snap.TotalWordCount = progress.TotalWordCount
		snap.InProgressChapter = progress.InProgressChapter
		snap.PendingRewrites = progress.PendingRewrites
		snap.RewriteReason = progress.RewriteReason
		snap.Layered = progress.Layered
		if progress.CurrentVolume > 0 {
			snap.CurrentVolumeArc = fmt.Sprintf("Tập %d · Cung %d", progress.CurrentVolume, progress.CurrentArc)
		}
	}
	if meta, _ := h.store.RunMeta.Load(); meta != nil {
		snap.PendingSteer = meta.PendingSteer
		snap.AdvanceMode = string(meta.AdvanceMode)
		snap.AdvancePermitChapter = meta.AdvancePermitChapter
		if meta.AdvanceHold != nil {
			snap.HasAdvanceHold = true
			snap.AdvanceHoldReason = meta.AdvanceHold.Reason
		}
	}

	snap.Agents = h.observer.agentSnapshots()
	snap.StatusLabel = deriveStatusLabel(snap)

	// Recovery label
	if label, err := resumeLabel(h.store); err == nil && label != "" {
		snap.RecoveryLabel = label
	}

	h.fillDetails(&snap, progress)

	return snap
}

// fillDetails fills the detail area: settings, characters, the latest commit/review/summary.
func (h *Host) fillDetails(snap *UISnapshot, progress *domain.Progress) {
	if premise, _ := h.store.Outline.LoadPremise(); premise != "" {
		snap.Premise = truncate(premise, 80)
	}
	if outline, _ := h.store.Outline.LoadOutline(); len(outline) > 0 {
		completed := make(map[int]struct{})
		if progress != nil {
			completed = make(map[int]struct{}, len(progress.CompletedChapters))
			for _, chapter := range progress.CompletedChapters {
				completed[chapter] = struct{}{}
			}
		}
		for _, e := range outline {
			title := e.Title
			if _, ok := completed[e.Chapter]; ok {
				committedTitle, err := h.store.Summaries.LoadSummaryTitle(e.Chapter)
				if err != nil {
					slog.Warn("projecting chapter title failed", "module", "host.snapshot", "chapter", e.Chapter, "err", err)
				} else if strings.TrimSpace(committedTitle) != "" {
					title = committedTitle
				}
			}
			snap.Outline = append(snap.Outline, OutlineSnapshot{
				Chapter: e.Chapter, Title: title, CoreEvent: e.CoreEvent,
			})
		}
	}
	if progress != nil && progress.Layered {
		if compass, _ := h.store.Outline.LoadCompass(); compass != nil {
			snap.CompassDirection = compass.EndingDirection
			snap.CompassScale = compass.EstimatedScale
		}
		if volumes, _ := h.store.Outline.LoadLayeredOutline(); len(volumes) > 0 {
			for _, v := range volumes {
				if v.Index > progress.CurrentVolume {
					snap.NextVolumeTitle = v.Title
					break
				}
			}
		}
	}
	if chars, _ := h.store.Characters.Load(); len(chars) > 0 {
		for _, c := range chars {
			label := c.Name
			if c.Role != "" {
				label += " (" + c.Role + ")"
			}
			snap.Characters = append(snap.Characters, label)
		}
	}
	if ledger, _ := h.store.Cast.Load(); len(ledger) > 0 {
		snap.SupportingCount = len(ledger)
		recent, _ := h.store.Cast.RecentActive(5)
		for _, e := range recent {
			label := e.Name
			if e.BriefRole != "" {
				label += " (" + e.BriefRole + ")"
			}
			snap.RecentSupporting = append(snap.RecentSupporting, label)
		}
	}
	if progress != nil && len(progress.CompletedChapters) > 0 {
		lastCh := progress.CompletedChapters[len(progress.CompletedChapters)-1]
		wc := progress.ChapterWordCounts[lastCh]
		snap.LastCommitSummary = fmt.Sprintf("Chương %d %d chữ", lastCh, wc)
	}
	currentCh := 1
	if progress != nil && len(progress.CompletedChapters) > 0 {
		currentCh = progress.CompletedChapters[len(progress.CompletedChapters)-1]
	}
	if review, err := h.store.World.LoadLastReview(currentCh); err == nil && review != nil {
		snap.LastReviewSummary = fmt.Sprintf("verdict=%s %d vấn đề", review.Verdict, len(review.Issues))
		if len(review.AffectedChapters) > 0 {
			snap.LastReviewSummary += fmt.Sprintf(" ảnh hưởng %v", review.AffectedChapters)
		}
	}
	if cp := h.store.Checkpoints.LatestGlobal(); cp != nil {
		snap.LastCheckpointName = fmt.Sprintf("%s.%s", cp.Scope, cp.Step)
	}
	if progress != nil {
		for i := len(progress.CompletedChapters) - 1; i >= 0 && len(snap.RecentSummaries) < 2; i-- {
			ch := progress.CompletedChapters[i]
			if summary, err := h.store.Summaries.LoadSummary(ch); err == nil && summary != nil {
				snap.RecentSummaries = append(snap.RecentSummaries,
					fmt.Sprintf("Chương %d: %s", ch, truncate(summary.Summary, 50)))
			}
		}
	}
}

func deriveStatusLabel(s UISnapshot) string {
	switch {
	case s.Phase == string(domain.PhaseComplete):
		return "COMPLETE"
	case s.Flow == string(domain.FlowReviewing):
		return "REVIEW"
	case s.Flow == string(domain.FlowRewriting) || s.Flow == string(domain.FlowPolishing):
		return "REWRITE"
	case s.RuntimeState == "running":
		return "RUNNING"
	default:
		return "READY"
	}
}

// ── Model management ──

func (h *Host) ConfiguredProviders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	providers := make([]string, 0, len(h.cfg.Providers))
	for name := range h.cfg.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	return providers
}

func (h *Host) ConfiguredModels(provider string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.CandidateModels(provider)
}

func (h *Host) CurrentModelSelection(role string) (string, string, bool) {
	return h.models.CurrentSelection(role)
}

func (h *Host) SwitchModel(role, provider, model string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if provider == "" || model == "" {
		return fmt.Errorf("provider and model are required")
	}
	if err := h.models.Swap(role, provider, model); err != nil {
		return err
	}
	if role == "" || role == "default" {
		h.cfg.Provider = provider
		h.cfg.ModelName = model
	} else {
		if h.cfg.Roles == nil {
			h.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := h.cfg.Roles[role]
		rc.Provider = provider
		rc.Model = model
		h.cfg.Roles[role] = rc
	}
	// Switching models does not change the stored reasoning-effort intent: it is clamped by the new model's capability only when issued.
	if h.configPath != "" {
		if err := bootstrap.SaveConfig(h.configPath, h.cfg); err != nil {
			slog.Warn("saving config failed", "module", "host", "err", err)
		}
	}
	h.applyThinkingLocked(role)
	// When switching to an unregistered model, print one warn line telling the user the 128k fallback is in use — long novels are prone to premature compaction.
	logRole := role
	if logRole == "" {
		logRole = "default"
	}
	window, source := h.cfg.ResolveContextWindow(provider, model)
	bootstrap.LogContextWindowChoice(logRole, model, window, source)

	// No resident context needs to be propagated: the ContextManagers of writer/architect/editor go through
	// ContextManagerFactory and are rebuilt automatically at the next spawn per the new model's window.

	h.emitEvent(Event{
		Time:     time.Now(),
		Category: "SYSTEM",
		Summary:  fmt.Sprintf("Đã chuyển model: %s → %s/%s", role, provider, model),
		Level:    "info",
	})
	return nil
}

// concreteThinkingRoles are the concrete roles to which a reasoning effort can be applied (consistent with the agents.ApplyThinking routing).
// When default is called, re-apply per role via each role's ResolveReasoningEffort.
var concreteThinkingRoles = []string{"architect", "writer", "editor"}

// CurrentThinking returns a role's currently effective raw reasoning-effort string (for the /model panel to sync the current value).
func (h *Host) CurrentThinking(role string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.ResolveReasoningEffort(strings.ToLower(strings.TrimSpace(role)))
}

func (h *Host) AvailableThinking(role string) []agentcore.ThinkingLevel {
	h.mu.Lock()
	model := h.models.ForRole(strings.ToLower(strings.TrimSpace(role)))
	h.mu.Unlock()
	return agents.AvailableThinkingForModel(model)
}

// resolveThinkingForRoleLocked computes the reasoning effort actually in effect for a role: take its raw intent
// (ResolveReasoningEffort: role level → top-level default), then clamp by the capability of that role's current model.
// Clamping happens only on this "effective path" and is not written back to config — storage always keeps the user's raw intent.
func (h *Host) resolveThinkingForRoleLocked(role string) agentcore.ThinkingLevel {
	parsed, _ := agents.ParseThinkingLevel(h.cfg.ResolveReasoningEffort(role))
	resolved, _ := agents.ResolveThinkingForModel(h.models.ForRole(role), parsed)
	return resolved
}

// applyThinkingLocked pushes the effective effort to live agents; each role is clamped by its own model.
func (h *Host) applyThinkingLocked(role string) {
	if h.thinkingApplier == nil {
		return
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || role == "default" {
		for _, r := range concreteThinkingRoles {
			h.thinkingApplier(r, h.resolveThinkingForRoleLocked(r))
		}
		return
	}
	h.thinkingApplier(role, h.resolveThinkingForRoleLocked(role))
}

// SetRoleThinking sets the reasoning effort of a role (or default): validate → persist → propagate to live agents → event.
// Mirrors the structure of SwitchModel; orthogonal to model selection and can be adjusted on its own. An empty level = no override (inherit).
func (h *Host) SetRoleThinking(role, level string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	parsed, err := agents.ParseThinkingLevel(level)
	if err != nil {
		return err
	}
	role = strings.ToLower(strings.TrimSpace(role))
	// Storage keeps the raw intent: the user's chosen effort is persisted directly, and clamping happens only on issuing (applyThinkingLocked) per model capability.
	if role == "" || role == "default" {
		h.cfg.ReasoningEffort = string(parsed)
	} else {
		if h.cfg.Roles == nil {
			h.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := h.cfg.Roles[role]
		rc.ReasoningEffort = string(parsed)
		h.cfg.Roles[role] = rc
	}
	if h.configPath != "" {
		if err := bootstrap.SaveConfig(h.configPath, h.cfg); err != nil {
			slog.Warn("saving config failed", "module", "host", "err", err)
		}
	}

	// Propagate to live: a concrete role applies directly; for default, iterate over each concrete role and re-apply via ResolveReasoningEffort
	// (those already overridden at role level keep their own, those not overridden pick up the new default).
	h.applyThinkingLocked(role)

	logRole := role
	if logRole == "" {
		logRole = "default"
	}
	shown := string(parsed)
	if shown == "" {
		shown = "Mặc định (kế thừa)"
	}
	h.emitEvent(Event{
		Time:     time.Now(),
		Category: "SYSTEM",
		Summary:  fmt.Sprintf("Đã đổi mức suy luận: %s → %s", logRole, shown),
		Level:    "info",
	})
	return nil
}

// ── Event replay ──

func (h *Host) ReplayQueue(afterSeq int64) ([]domain.RuntimeQueueItem, error) {
	if h.store == nil || h.store.Runtime == nil {
		return nil, nil
	}
	return h.store.Runtime.LoadQueueAfter(afterSeq)
}

// ── Co-creation ──

// CoCreateStream is a cold-start co-creation: clarify requirements from scratch and produce the creative instruction for the whole book.
func (h *Host) CoCreateStream(ctx context.Context, history []CoCreateMessage, onProgress func(kind, text string)) (CoCreateReply, error) {
	return coCreateStream(ctx, h.models, h.store.Sessions, coCreatePromptFor(h.store.Language()), history, onProgress)
}

// StageCoCreateStream is stage co-creation: plan the follow-up direction on top of the content already written.
// System prompt = stage prompt + a summary of the current story state, so the assistant knows "what has been written so far".
func (h *Host) StageCoCreateStream(ctx context.Context, history []CoCreateMessage, onProgress func(kind, text string)) (CoCreateReply, error) {
	return coCreateStream(ctx, h.models, h.store.Sessions, stageSystemPrompt(h.store), history, onProgress)
}

// stagePlanPrefix wraps the co-creation output, the "follow-up direction brief", into a stage-planning intervention for the Arbiter to rule on.
// It attaches only a [stage plan] fact marker + a neutral statement and does not hard-code "how to land it" — the concrete routing (compass / architect /
// user_rules) is left to the "stage planning" criterion in arbiter-intervention.md, avoiding a second source of truth against the prompt
// and also not blocking style requirements from going to user_rules (keeping "classification rulings belong to the LLM"). Continue then stacks the [user intervention] prefix on top.
const stagePlanPrefix = "[Kế hoạch giai đoạn] Tôi đã tạm dừng sáng tác và cùng trợ lý đồng sáng tác chỉnh lý hướng đi tiếp theo dưới đây, hãy phân xử cách thực hiện theo phân loại can thiệp của bạn, rồi tiếp tục sáng tác. Hướng đi tiếp theo như sau:\n\n"

// PauseForCoCreate enters stage co-creation: sets the co-creation hold flag and, if running, pauses the Engine as well.
// Returns false when entry is impossible (the whole book is complete or already co-creating); the caller can ignore it.
// The hold flag blocks concurrent involvement of import/simulate/start/resume/continue within the co-creation window —
// after a pause while running, lifecycle=paused and the existing ==running mutual exclusion no longer holds, so this flag fills the gap;
// an already-stopped state (idle/paused) is also allowed to enter, and after planning it continues via Continue.
func (h *Host) PauseForCoCreate() bool {
	h.mu.Lock()
	if h.cocreating || h.lifecycle == lifecycleCompleted {
		h.mu.Unlock()
		return false
	}
	h.cocreating = true
	running := h.lifecycle == lifecycleRunning
	h.mu.Unlock()

	// While running, reuse abortWithEvent to stop (running→paused + setAborting + Abort + event), in the same order as a manual
	// pause, without copying it out again; when already stopped (idle/paused) only set the flag, and after planning continue via Continue.
	if running {
		h.abortWithEvent("Vào đồng sáng tác theo giai đoạn, sáng tác đã tạm dừng", "info")
	} else {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Vào đồng sáng tác theo giai đoạn", Level: "info"})
	}
	return true
}

// ResumeFromCoCreate ends stage co-creation: injects the follow-up direction produced by co-creation as an intervention and resumes creation.
// After clearing the hold flag, reuse Continue's inject-while-stopped path (subject to the budget precheck).
// Note: returning early without clearing the flag when draft is empty is intentional (co-creation has not ended yet); the TUI-side canStart() guard
// uses the same "non-empty" criterion as here, guaranteeing this path is unreachable, so cocreating does not leak because of it.
func (h *Host) ResumeFromCoCreate(draft string) error {
	draft = strings.TrimSpace(draft)
	if draft == "" {
		return fmt.Errorf("draft is required")
	}
	h.mu.Lock()
	if !h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("not in co-create")
	}
	h.cocreating = false
	h.mu.Unlock()

	// PauseForCoCreate's abort is asynchronous: wait until the engine loop has truly converged before continuing, returning to the same
	// "truly stopped" premise as Continue after a manual pause. The co-creation window is on a human-interaction timescale, so short polling goes unnoticed.
	for h.engine.isRunning() {
		time.Sleep(20 * time.Millisecond)
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Đồng sáng tác theo giai đoạn hoàn tất, đã đưa hướng đi tiếp theo vào và khôi phục sáng tác", Level: "info"})
	return h.Continue(stagePlanPrefix + draft)
}

// CancelCoCreate abandons stage co-creation: clears the hold flag and stays paused (the user can continue in the input box or restart Resume).
func (h *Host) CancelCoCreate() {
	h.mu.Lock()
	if !h.cocreating {
		h.mu.Unlock()
		return
	}
	h.cocreating = false
	h.mu.Unlock()
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "Đã thoát đồng sáng tác theo giai đoạn, sáng tác vẫn tạm dừng (có thể tiếp tục ở ô nhập)", Level: "info"})
}

// ── Tools ──

func (h *Host) refreshWriterRestore() {
	if h.writerRestore != nil {
		h.writerRestore.Refresh(h.store)
	}
}

func (h *Host) CheckChapterRevisions() ([]int, error) {
	pending, err := h.store.Revisions.LoadPending()
	if err != nil {
		return nil, fmt.Errorf("Đọc bản ghi khôi phục sửa đổi: %w", err)
	}
	if pending != nil {
		chapters := make([]int, 0, len(pending.Items))
		for _, item := range pending.Items {
			chapters = append(chapters, item.Chapter)
		}
		return chapters, nil
	}
	changes, err := revision.Scan(h.store)
	if err != nil {
		return nil, err
	}
	return revision.ChangedChapters(changes), nil
}

func (h *Host) SyncChapterRevisions(ctx context.Context) (*revision.Result, error) {
	if err := h.acquireExclusive("Đồng bộ sửa đổi chương"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.exclusiveCancel = cancel
	h.mu.Unlock()
	defer h.releaseExclusive()

	pending, err := h.store.Revisions.LoadPending()
	if err != nil {
		return nil, err
	}
	if pending == nil {
		changes, err := revision.Scan(h.store)
		if err != nil {
			return nil, err
		}
		if len(changes) == 0 {
			return &revision.Result{}, nil
		}
		if err := h.budget.Refuse(); err != nil {
			return nil, err
		}
	}
	model := h.models.ForRoleWithFailover("editor", func(ev bootstrap.FailoverEvent) {
		slog.Warn("provider failover during chapter revision", "module", "revision", "role", ev.Role,
			"reason", ev.Reason, "from", fmt.Sprintf("%s/%s", ev.FromProvider, ev.FromModel),
			"to", fmt.Sprintf("%s/%s", ev.ToProvider, ev.ToModel), "err", ev.Err)
	})
	model = newUsageTrackedModel(model, "editor", h.usage.Record)
	service := revision.NewService(h.store, model, h.bundle.Prompts.RevisionAnalyze, h.styleStats)
	return service.Sync(ctx)
}

func (h *Host) requireCleanChapters() error {
	chapters, err := h.CheckChapterRevisions()
	if err != nil {
		return fmt.Errorf("Kiểm tra sửa đổi chương từ bên ngoài: %w", err)
	}
	if len(chapters) > 0 {
		return fmt.Errorf("Phát hiện nội dung chương đã bị sửa từ bên ngoài: %v; hãy chạy /sync trước", chapters)
	}
	return nil
}

func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// ImportFrom starts one semantic-compile import of an external novel: ingest → segment → analyze → synthesize → publish.
// The model rules only on open semantics (boundaries / facts / synthesis), while Go owns coordinates / coverage / idempotency; mutually exclusive with Engine runs,
// and after the import completes AdvanceHold decides whether to continue writing.
// The returned event channel is closed by imp.Run, and the caller is responsible for consuming it (dropped when full, to avoid blocking the pipeline goroutine).
func (h *Host) ImportFrom(ctx context.Context, opts imp.Options) (<-chan imp.Event, error) {
	// Budget precheck before start follows the same discipline as Start/Resume/Continue: an import is model calls throughout the whole pipeline,
	// and it must not start when the budget is already exceeded (§13.1 "fold into the existing budget sentinel").
	if err := h.budget.Refuse(); err != nil {
		return nil, err
	}
	if err := h.acquireExclusive("Nhập truyện"); err != nil {
		return nil, err
	}
	// Register the cancel function: a budget hard stop / manual pause cancels the import's own context via abortWithEvent
	// (otherwise the sentinel would only go pause the Engine, which is not running, and the import would keep burning money).
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.exclusiveCancel = cancel
	h.mu.Unlock()

	deps := imp.Deps{
		Store:         h.store,
		CommitChapter: tools.NewCommitChapterTool(h.store, h.styleStats),
		Segment:       h.importCaller("segment"),
		Analyze:       h.importCaller("analyze"),
		Synthesize:    h.importCaller("synthesize"),
		Prompts: imp.Prompts{
			Segment:    h.bundle.Prompts.ImportSegment,
			Analyze:    h.bundle.Prompts.ImportAnalyze,
			Synthesize: h.bundle.Prompts.ImportSynthesize,
			Range:      h.bundle.Prompts.ImportRange,
		},
	}
	ch, err := imp.Run(ctx, deps, opts)
	if err != nil {
		h.releaseExclusive()
		return nil, err
	}
	return h.superviseImport(ch, opts), nil
}

// ImportResumeHint returns a one-line hint for an unfinished import (empty string if none), for the TUI to proactively tell the user at startup (RFC §18.2).
// Call it only once at startup: it recomputes the InputDigest of each artifact in the workspace internally, so it is not suitable for snapshot polling.
func (h *Host) ImportResumeHint() string {
	return imp.ResumeSummary(h.store)
}

// importCaller resolves the model tier of an import semantic function (RFC §13.1): if the roles config has import_<fn>
// use that tier (usage is also booked to that role), otherwise fall back to architect. This is call configuration and changes no semantic contract.
func (h *Host) importCaller(fn string) imp.Caller {
	role := "import_" + fn
	if _, _, explicit := h.models.CurrentSelection(role); !explicit {
		role = "architect"
	}
	model := h.models.ForRoleWithFailover(role, func(ev bootstrap.FailoverEvent) {
		slog.Warn("provider failover during import", "module", "import", "role", ev.Role,
			"reason", ev.Reason,
			"from", fmt.Sprintf("%s/%s", ev.FromProvider, ev.FromModel),
			"to", fmt.Sprintf("%s/%s", ev.ToProvider, ev.ToModel),
			"err", ev.Err)
	})
	model = newUsageTrackedModel(model, role, h.usage.Record)
	return imp.Caller{Model: model, Runtime: h.importModelRuntime(role, model)}
}

// importModelRuntime probes the call capabilities of the model for the chosen tier role, for imp's dual budget / thinking adaptation (RFC §13/§21).
// Fields whose probe fails are left at zero values, and imp falls back to conservative defaults, guaranteeing correct operation even without capability info.
// Structured output is read fresh by imp's llmcontract from model facts before every request, and is not cached again in Runtime.
func (h *Host) importModelRuntime(role string, model agentcore.ChatModel) imp.ModelRuntime {
	var rt imp.ModelRuntime
	provider, name, _ := h.models.CurrentSelection(role)
	if name == "" {
		name = bootstrap.ModelName(model)
		provider = bootstrap.ModelProvider(model)
	}
	// context / completion limits: the registry is the only trusted source (the wrapped model's Info() contains no window).
	rt.ContextTokens, _ = h.cfg.ResolveContextWindow(provider, name)
	if entry, ok := modelreg.DefaultRegistry().Resolve(name); ok {
		rt.MaxOutputTokens = entry.MaxTokens
	}
	// thinking: resolve from the role's reasoning effort and model capability; if unsupported, do not send (same policy as the arbiter).
	if level, err := agents.ParseThinkingLevel(h.cfg.ResolveReasoningEffort(role)); err == nil {
		if resolved, ok := agents.ResolveThinkingForModel(model, level); ok {
			rt.Thinking = resolved
		}
	}
	return rt
}

// Simulate reads the simulate directory and generates or incrementally updates an imitation profile.
func (h *Host) Simulate(ctx context.Context) (<-chan sim.Event, error) {
	if err := h.acquireExclusive("Tạo style profile"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.exclusiveCancel = cancel
	h.mu.Unlock()

	wd, err := os.Getwd()
	if err != nil {
		h.releaseExclusive()
		return nil, fmt.Errorf("get working dir: %w", err)
	}
	deps := sim.Deps{
		Store: h.store,
		LLM:   h.models.ForRole("architect"),
		Prompts: sim.Prompts{
			Source: h.bundle.Prompts.SimulationSource,
			Merge:  h.bundle.Prompts.SimulationMerge,
		},
	}
	ch, err := sim.Run(ctx, deps, sim.Options{SourceDir: filepath.Join(wd, "simulate")})
	if err != nil {
		h.releaseExclusive()
		return nil, err
	}
	return superviseExclusive(h, ch), nil
}

// ImportSimulationProfile imports a previously generated imitation profile.
func (h *Host) ImportSimulationProfile(ctx context.Context, path string) (<-chan sim.Event, error) {
	if err := h.acquireExclusive("Nhập style profile"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.exclusiveCancel = cancel
	h.mu.Unlock()
	ch, err := sim.RunImport(ctx, h.store, path)
	if err != nil {
		h.releaseExclusive()
		return nil, err
	}
	return superviseExclusive(h, ch), nil
}

// acquireExclusive atomically takes the background exclusive-job slot (import/simulate/revision): refuses while the Engine is running, within the stage co-creation window,
// or when an exclusive job is already running. On success the hold is registered, and when the job ends releaseExclusive must be called to release it — otherwise two imports
// or an import plus a simulate would race to modify the same state. This closes the earlier gap of checking only ==running/cocreating without registering the job itself.
func (h *Host) acquireExclusive(action string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closing:
		return fmt.Errorf("Host đang đóng, không thể %s", action)
	// engine.isRunning() must be checked: Abort first sets lifecycle=paused and then asynchronously waits for the goroutine to exit,
	// and within that window lifecycle is no longer running but the engine may still be writing the store (the same discipline as the startup gate).
	case h.lifecycle == lifecycleRunning || h.engine.isRunning():
		return fmt.Errorf("Engine sáng tác đang chạy hoặc đang dừng, hãy thử %s sau", action)
	case h.cocreating:
		return fmt.Errorf("Đồng sáng tác theo giai đoạn đang diễn ra, hãy kết thúc đồng sáng tác trước rồi %s", action)
	case h.exclusive != "":
		return fmt.Errorf("%s đang chạy, hãy hoàn tất rồi mới %s", h.exclusive, action)
	}
	h.exclusive = action
	return nil
}

// releaseExclusive releases the background exclusive-job slot (along with the registered cancel function).
func (h *Host) releaseExclusive() {
	h.mu.Lock()
	cancel := h.exclusiveCancel
	h.exclusive = ""
	h.exclusiveCancel = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel() // Job has ended: release the derived context; no side effect on a runner that has already exited
	}
}

// superviseExclusive forwards exclusive-job events, and releases the hold slot when the channel closes (job ends).
func superviseExclusive[T any](h *Host, src <-chan T) <-chan T {
	out := make(chan T, 32)
	if !h.launchAsync(func() {
		defer close(out)
		defer h.releaseExclusive()
		for ev := range src {
			select {
			case out <- ev:
			case <-h.runCtx.Done():
				// Keep draining the source channel during closing, so the producer is not blocked by a terminal event and unable to exit.
				for range src {
				}
				return
			}
		}
	}) {
		close(out)
		h.releaseExclusive()
	}
	return out
}

// superviseImport is the sole owner of "whether to hand over after import completes": it forwards import events, and on successful completion first releases the exclusive slot,
// then decides and executes the handover, and finally writes the real handover result into the Continued field of the StageDone event. The TUI renders only from this,
// and no longer guesses the run state from a local --continue flag (eliminating the timing races caused by Runner/Host/TUI each interpreting it on their own).
func (h *Host) superviseImport(src <-chan imp.Event, opts imp.Options) <-chan imp.Event {
	out := make(chan imp.Event, 32)
	if !h.launchAsync(func() {
		defer close(out)
		released := false
		release := func() {
			if !released {
				released = true
				h.releaseExclusive()
			}
		}
		defer release()
		for ev := range src {
			if ev.Stage == imp.StageDone {
				release() // Release the exclusive slot first, so the handover's startEngine can pass the exclusive gate
				ev.Continued = h.continueAfterImport(opts)
			}
			select {
			case out <- ev:
			case <-h.runCtx.Done():
				for range src {
				}
				return
			}
		}
	}) {
		close(out)
		h.releaseExclusive()
	}
	return out
}

// launchAsync registers a background task within the Host lifecycle. closing and WaitGroup.Add are protected by the same
// lock, guaranteeing that no new Add appears after Close starts Wait.
func (h *Host) launchAsync(fn func()) bool {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return false
	}
	h.asyncWG.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.asyncWG.Done()
		fn()
	}()
	return true
}

// runAsync reuses the Host's existing background-task registration while returning business errors to the caller.
func (h *Host) runAsync(fn func() error) (error, bool) {
	result := make(chan error, 1)
	if !h.launchAsync(func() { result <- fn() }) {
		return nil, false
	}
	return <-result, true
}

// continueAfterImport decides and executes the real automatic handover of --continue, returning whether the Engine has started.
// Effective handover intent = this opts or the workspace's persisted intent (covering the no-argument /import recovery after a crash);
// only the auto advance mode hands over, with adaptive arc-expansion planning taking over an open story or letting a concluded story wrap up; review mode leaves it to the user's /next.
func (h *Host) continueAfterImport(opts imp.Options) bool {
	want := opts.ContinueAfter
	if !want {
		in, err := imp.OpenWorkspace(h.store.Dir()).LoadIntent()
		if err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "Nhập truyện đã xong, nhưng không đọc được ý định tự động nối tiếp: " + err.Error()})
		} else if in != nil {
			want = in.ContinueAfterImport
		}
	}
	if !want {
		return false
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil || meta == nil {
		slog.Warn("reading RunMeta for auto-continue after import failed", "module", "host", "err", err)
		return false
	}
	if meta.AdvanceMode != domain.ChapterAdvanceAuto {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "info",
			Summary: "Nhập truyện xong; hiện ở chế độ duyệt từng chương, nhập tiếp tục hoặc /next để nối tiếp viết"})
		return false
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "info", Summary: "Nhập truyện xong, tự động nối tiếp viết"})
	if !h.startEngine(nil) {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
			Summary: "Không tự động nối tiếp được, hãy nhập lệnh tiếp tục để khôi phục thủ công"})
		return false
	}
	return true
}

// Export exports completed chapters to an external file (currently only TXT is supported).
//
// Unlike ImportFrom: export is a read-only operation (does not touch Progress / Checkpoint),
// so it **does not require the Engine to stop** — the "current finished product" can be exported at any time, even mid-writing.
// It reads only a consistent snapshot of Progress.CompletedChapters + final chapter drafts + outline + premise.
func (h *Host) Export(ctx context.Context, opts exp.Options) (*exp.Result, error) {
	return exp.Run(ctx, exp.Deps{Store: h.store}, opts)
}
