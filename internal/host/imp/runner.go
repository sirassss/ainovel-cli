package imp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/logger"
	"github.com/voocel/ainovel-cli/internal/store"
)

// The prompt/schema versions are included in each stage's InputDigest; increment them when upgrading a prompt contract, so downstream artifacts naturally invalidate.
const (
	segmentPromptVersion = "seg-v2" // v2: boundaries land only at real separators, titles copied verbatim (works with the title echo validation)
	analyzePromptVersion = "analyze-v1"
	confirmMethodAuto    = "auto_authorized"
	confirmMethodUser    = "user_confirmed" // explicit manual confirmation by pressing y after the TUI preview
)

// Prompts are the system prompts of the semantic functions. Synthesis has two stages: Synthesize produces the whole-book BookSynthesis,
// Range produces the RangeDigest of contiguous ranges for a long book; the two output different structures and must each use its own prompt.
type Prompts struct {
	Segment    string
	Analyze    string
	Synthesize string
	Range      string
}

// RunBudgets are the input/output budgets of the semantic functions. The first version uses conservative constants;
// in future they should be derived from the current architect model's context window / completion limit, so batches grow naturally with capability (RFC §9.2/§21).
type RunBudgets struct {
	MaxUnitBytes         int
	SegmentChunkBytes    int
	SegmentContextMargin int
	SegmentMaxTokens     int
	Analyze              AnalyzeBudget
	SynthesizeRangeBytes int
	SynthesizeMaxTokens  int
}

// DefaultRunBudgets returns the conservative default budgets, used as a fallback when model capability is unknown (probe failed).
func DefaultRunBudgets() RunBudgets {
	return RunBudgets{
		MaxUnitBytes:         8000,
		SegmentChunkBytes:    24000,
		SegmentContextMargin: 20,
		SegmentMaxTokens:     8192,
		Analyze:              AnalyzeBudget{ContextBytes: 24000, MaxOutputTokens: 8000, PerChapterOutput: 900, PromptOverhead: 2000},
		SynthesizeRangeBytes: 16000,
		SynthesizeMaxTokens:  8192,
	}
}

// ModelRuntime carries the model capability facts needed by imp semantic calls, injected by the Host after boundary probing (RFC §13/§17).
// It lets the dual budget grow naturally with context/completion and sends thinking according to capability; at all-zero values it falls back to the conservative defaults,
// with behavior identical to before capabilities were wired in. Structured output does not send response_format by provider capability (see the callProfile comment).
type ModelRuntime struct {
	ContextTokens   int                     // input context limit (tokens)
	MaxOutputTokens int                     // single-call visible output limit (tokens)
	Thinking        agentcore.ThinkingLevel // already resolved by capability; ThinkingAuto("") means not sent explicitly
}

// profile derives the call capability options (thinking) of this runtime.
func (rt ModelRuntime) profile() callProfile {
	return callProfile{thinking: rt.Thinking}
}

// Caller is the model tier of a semantic function: model + that model's capability facts (RFC §13.1/§17).
// segment/analyze/synthesize each hold a tier, and budgets and call options are derived from each one's own tier,
// so the small window of a cheap tier constrains only its own function and does not drag down other stages.
type Caller struct {
	Model   callModel
	Runtime ModelRuntime
}

// budgetsFromRuntime derives each semantic function's budget from the model's real context/completion limits (RFC §9.2/§21).
// This is what makes "switching to a stronger model automatically enlarges batches and reduces call counts" hold; when capability is unknown it falls back to the conservative defaults.
func budgetsFromRuntime(rt ModelRuntime) RunBudgets {
	if rt.ContextTokens <= 0 || rt.MaxOutputTokens <= 0 {
		return DefaultRunBudgets()
	}
	const bytesPerToken = 3 // conservative token->byte conversion for Chinese UTF-8 (underestimating capacity is safer)
	out := rt.MaxOutputTokens
	// Input budget: the context minus visible output and ~10% reasoning/system reserve, then converted to bytes.
	reserve := rt.ContextTokens / 10
	inTokens := rt.ContextTokens - out - reserve
	if inTokens < 2000 {
		inTokens = 2000
	}
	inBytes := inTokens * bytesPerToken
	return RunBudgets{
		MaxUnitBytes:         min(inBytes/2, 32000),
		SegmentChunkBytes:    inBytes,
		SegmentContextMargin: 20,
		SegmentMaxTokens:     out,
		Analyze: AnalyzeBudget{
			ContextBytes:     inBytes,
			MaxOutputTokens:  out,
			PerChapterOutput: 900,
			PromptOverhead:   2000,
		},
		SynthesizeRangeBytes: inBytes,
		SynthesizeMaxTokens:  out,
	}
}

// Confirmation is the segmentation confirmation artifact, bound to the current segmentation (RFC §8.4).
type Confirmation struct {
	Method   string `json:"method"`
	Chapters int    `json:"chapters"`
}

// StoryResolution is the user's ruling on an uncertain story state, bound to the current synthesis (RFC §10.4).
type StoryResolution struct {
	Choice string `json:"choice"` // open / closed
}

// Deps are the runner's narrow dependencies (RFC §17). The three semantic functions each declare a model tier;
// the Host defaults all of them to architect, and the config layer can point the more mechanical functions at a cheaper tier (RFC §13.1).
type Deps struct {
	Store         *store.Store
	CommitChapter ChapterCommitter
	Segment       Caller
	Analyze       Caller
	Synthesize    Caller // range digest and book synthesis share the same tier (the same synthesis stage)
	Prompts       Prompts
	Budgets       RunBudgets
}

// budgetsFromDeps derives budgets from the capability of each semantic function's own tier (RFC §9.2/§13.1).
func budgetsFromDeps(d Deps) RunBudgets {
	seg := budgetsFromRuntime(d.Segment.Runtime)
	ana := budgetsFromRuntime(d.Analyze.Runtime)
	syn := budgetsFromRuntime(d.Synthesize.Runtime)
	return RunBudgets{
		MaxUnitBytes:         seg.MaxUnitBytes,
		SegmentChunkBytes:    seg.SegmentChunkBytes,
		SegmentContextMargin: seg.SegmentContextMargin,
		SegmentMaxTokens:     seg.SegmentMaxTokens,
		Analyze:              ana.Analyze,
		SynthesizeRangeBytes: syn.SynthesizeRangeBytes,
		SynthesizeMaxTokens:  syn.SynthesizeMaxTokens,
	}
}

// Run executes the full import pipeline: LoadState -> NextAction -> execute one action -> reload facts.
// It runs in its own goroutine; the returned event channel is closed by this function.
func Run(ctx context.Context, deps Deps, opts Options) (<-chan Event, error) {
	if deps.Store == nil || deps.CommitChapter == nil ||
		deps.Segment.Model == nil || deps.Analyze.Model == nil || deps.Synthesize.Model == nil {
		return nil, fmt.Errorf("deps không đầy đủ")
	}
	if deps.Budgets == (RunBudgets{}) {
		deps.Budgets = budgetsFromDeps(deps)
	}
	// The import flow log is its own file: the full transcript of an import (events, retries, full error chain) is not mixed with
	// the engine/TUI logs, so troubleshooting needs to look at only this one file. A creation failure must be echoed - the panel will direct the user to
	// view logs/import.log, and a silent fallback would point to a file that does not exist (Debug-First).
	log, closeLog, logErr := logger.FileLogger(deps.Store.Dir(), "import.log")
	log.Info("imp model runtime",
		"segment_ctx", deps.Segment.Runtime.ContextTokens,
		"analyze_ctx", deps.Analyze.Runtime.ContextTokens,
		"synthesize_ctx", deps.Synthesize.Runtime.ContextTokens,
		"analyze_max_output", deps.Analyze.Runtime.MaxOutputTokens,
		"analyze_context_bytes", deps.Budgets.Analyze.ContextBytes)
	events := make(chan Event, 32)
	go func() {
		defer close(events)
		defer closeLog()
		r := &runner{deps: deps, opts: opts, events: events, ws: OpenWorkspace(deps.Store.Dir()), log: log}
		if logErr != nil {
			r.emit(StageIngesting, 0, 0, fmt.Sprintf("Không tạo được tệp nhật ký nhập truyện (%v), lần này dùng nhật ký mặc định", logErr), nil)
		}
		r.run(ctx)
	}()
	return events, nil
}

type runner struct {
	deps   Deps
	opts   Options
	events chan Event
	ws     *Workspace
	act    Action       // action currently executing, for the failure artifact to label the stage
	log    *slog.Logger // import-specific log (logs/import.log); falls back to the default logger when nil
}

func (r *runner) emit(stage Stage, current, total int, msg string, err error) {
	r.send(Event{Time: time.Now(), Stage: stage, Current: current, Total: total, Message: msg, Err: err})
}

func (r *runner) send(ev Event) {
	r.logEvent(ev)
	// Terminal and stop-point events carry the only success/failure / must-act signal (if the confirmation preview or the --story prompt is lost the user does not know what to do),
	// and must be delivered reliably; only intermediate progress events may be dropped under backlog.
	if ev.Stage == StageError || ev.Stage == StageDone ||
		ev.Stage == StageAwaitingConfirmation || ev.Stage == StageAwaitingStoryStatus {
		r.events <- ev
		return
	}
	select {
	case r.events <- ev:
	default: // drop progress when the channel is full, never block the pipeline
	}
}

// logEvent transcribes every progress event into the import-specific log (<book root>/logs/import.log): the panel's retry line is overwritten in place,
// the panel disappears on Esc, and the log is the only complete flow record available for post-mortem (§14.1).
func (r *runner) logEvent(ev Event) {
	log := r.log
	if log == nil {
		log = slog.Default()
	}
	args := []any{"stage", string(ev.Stage)}
	if ev.Total > 0 {
		args = append(args, "progress", fmt.Sprintf("%d/%d", ev.Current, ev.Total))
	}
	if ev.Err != nil {
		args = append(args, "err", ev.Err)
	}
	level := slog.LevelInfo
	switch {
	case ev.Stage == StageError:
		level = slog.LevelError // the failure terminal state is the one line in the log most worth filtering out, and must not be logged as INFO
	case ev.Level == "warn":
		level = slog.LevelWarn
	}
	log.Log(context.Background(), level, ev.Message, args...)
}

func (r *runner) fail(msg string, err error) {
	r.saveFailure(err)
	r.emit(StageError, 0, 0, msg, err)
}

// saveFailure uniformly writes failures carrying a raw response to failures/ (the third landing point in RFC §14.2),
// a fallback shared by all semantic functions such as segment/synthesize; the analysis truncation-salvage path has already written finer metadata in place.
// A failure without a raw response (IO, cancellation, precondition validation) has no model output to save and writes nothing.
func (r *runner) saveFailure(err error) {
	var se *errSemantic
	var tr *errTruncated
	switch {
	case errors.As(err, &se):
		r.ws.writeFailure(FailureMeta{Stage: string(r.act), Detail: err.Error()}, se.Raw)
	case errors.As(err, &tr):
		r.ws.writeFailure(FailureMeta{Stage: string(r.act), Detail: err.Error(), StopReason: "length"}, tr.Raw)
	}
}

// facts combines workspace facts with formal publish reconciliation.
func (r *runner) facts() (Facts, error) {
	return CollectFacts(r.deps.Store, r.ws)
}

// profileFor derives the call options of a tier and echoes request backoff / validation re-asks to that stage's event stream -
// retry backoff can stay silent for over 2 minutes cumulatively, and without an echo the user would think it hung (§14.1).
// Key is given only to request backoff (which carries a deadline): it is transient state within a single call, and the UI updates one line in place ("attempt N" ticking).
// A validation re-ask is a cross-call semantic event - segmentation calls chunk by chunk, each chunk re-asks independently, and a shared Key would let a later chunk overwrite an earlier one and
// swallow the troubleshooting clue (measured: the panel was left with a single line whose unit_id kept changing), so each gets its own line and history is kept.
func (r *runner) profileFor(c Caller, stage Stage) callProfile {
	prof := c.Runtime.profile()
	prof.log = r.log
	prof.notify = func(msg string, retryAt time.Time) {
		ev := Event{Time: time.Now(), Stage: stage, Message: msg, Level: "warn", RetryAt: retryAt}
		if !retryAt.IsZero() {
			ev.Key = "retry:" + string(stage)
		}
		r.send(ev)
	}
	prof.progress = func(current, total int, msg string) {
		r.send(Event{Time: time.Now(), Stage: stage, Current: current, Total: total, Message: msg})
	}
	return prof
}

// applyGuidance persists this run's explicit --guide guidance as a workspace semantic input (RFC §18.3).
// Guidance is one of the inputs of the segmentation InputDigest: a content change naturally makes the old segmentation and all its downstream mismatch and redo,
// with no hand-written invalidation rules. Skipped first when the workspace is not yet established; written in the next loop round after ingest.
func (r *runner) applyGuidance() error {
	g := strings.TrimSpace(r.opts.Guidance)
	if g == "" || !r.ws.Active() {
		return nil
	}
	existing, err := r.ws.LoadGuidance()
	if err != nil {
		return fmt.Errorf("Đọc chỉ dẫn chia tách đã có: %w", err)
	}
	if existing == g {
		return nil
	}
	// Once publishing starts, formal artifacts cannot be overwritten (§12.2): re-segmenting then inevitably hits the "refuse to overwrite" wall in publish,
	// and before hitting the wall it would first repay the full chain of segmentation/analysis/synthesis model calls - move the failure forward to the zero-cost point.
	// book is the first write of publishing; its existence means publishing has started (the import precondition check guarantees the book was originally empty).
	book, err := r.deps.Store.Book.Load()
	if err != nil {
		return fmt.Errorf("Đọc book chính thức: %w", err)
	}
	if book != nil {
		return fmt.Errorf("Foundation chính thức đã bắt đầu phát hành, --guide chia lại sẽ xung đột với nội dung đã phát hành và bị từ chối ghi đè, không còn nhận chỉ dẫn chia tách")
	}
	return r.ws.writeAtomic(fileGuidance, []byte(g))
}

// checkSourceIdentity blocks "workspace in progress but a different source file passed in": ingest runs only when there is no workspace,
// and without a comparison, /import B.txt would silently continue from A's breakpoint, finish publishing A, and read not a single byte of B (RFC §12.1/§18.2).
// Passing the same file path again is a common habit (/import with the same path to resume), so compare by content digest rather than refusing all paths.
func (r *runner) checkSourceIdentity() error {
	if r.opts.SourcePath == "" || !r.ws.Active() {
		return nil
	}
	m, err := r.ws.LoadManifest()
	if err != nil {
		return nil // an unreadable identity triple goes to ingest's corruption diagnosis, and is not reported again here
	}
	raw, err := os.ReadFile(r.opts.SourcePath)
	if err != nil {
		return fmt.Errorf("Đọc tệp nguồn %s: %w", r.opts.SourcePath, err)
	}
	if Digest(raw) != m.RawSHA256 {
		return fmt.Errorf("Đã có lượt nhập %q đang diễn ra, tệp nguồn lần này khác nội dung của nó: hãy hoàn tất hoặc bỏ lượt nhập cũ (xóa meta/import/) rồi mới nhập sách mới", m.SourceName)
	}
	return nil
}

func (r *runner) run(ctx context.Context) {
	if err := r.checkSourceIdentity(); err != nil {
		r.fail("Kiểm tra danh tính tệp nguồn", err)
		return
	}
	var previous *Facts
	for {
		if ctx.Err() != nil {
			r.fail("Người dùng đã hủy", ctx.Err())
			return
		}
		if err := r.applyGuidance(); err != nil {
			r.fail("Ghi chỉ dẫn chia tách", err)
			return
		}
		facts, err := r.facts()
		if err != nil {
			r.fail("Đọc trạng thái nhập", err)
			return
		}
		if previous != nil && facts == *previous {
			r.fail("Nhập truyện bị đình trệ", fmt.Errorf("Dữ kiện không đổi sau khi thực hiện hành động, hành động kế tiếp vẫn là %q", NextAction(facts)))
			return
		}
		snapshot := facts
		previous = &snapshot
		act := NextAction(facts)
		r.act = act
		err = nil
		switch act {
		case ActionIngest:
			err = r.ingest(ctx)
		case ActionSegment:
			err = r.segment(ctx)
		case ActionAwaitConfirmation:
			if !r.confirm() {
				return // interactive mode: wait for user confirmation, stop here
			}
		case ActionAnalyze:
			err = r.analyze(ctx)
		case ActionSynthesize:
			err = r.synthesize(ctx)
		case ActionAwaitStoryResolution:
			if !r.resolveStoryStatus() {
				return // no explicit ruling: stop here and wait for --story=open|closed
			}
		case ActionPublish:
			err = r.publish(ctx)
		case ActionDone:
			r.emit(StageDone, 0, 0, "Nhập truyện xong, chờ duyệt rồi viết tiếp", nil)
			return
		default:
			err = fmt.Errorf("Hành động không xác định %q", act)
		}
		if err != nil {
			r.fail("Không nhập được truyện", err)
			return
		}
	}
}

func (r *runner) ingest(ctx context.Context) error {
	// Reaching ingest while the directory already exists = the identity triple (manifest/source/intent) is missing or corrupt:
	// createWorkspace would refuse with "already exists (/import without arguments can resume)", and rerunning without arguments returns here because of
	// WorkspaceReady=false and asks for a source path - the two hints fight each other and leave the user nowhere to go.
	if r.ws.Active() {
		return fmt.Errorf("meta/import/ đã tồn tại nhưng danh tính vùng làm việc không dùng được (manifest/source/intent thiếu hoặc hỏng), hãy xác nhận thủ công rồi xóa thư mục này và nhập lại")
	}
	if err := checkImportPreconditions(r.deps.Store); err != nil {
		return err
	}
	if r.opts.SourcePath == "" {
		return fmt.Errorf("Lượt nhập mới cần đường dẫn tệp nguồn")
	}
	r.emit(StageIngesting, 0, 0, "Đang đọc, giải mã, chuẩn hóa và chụp snapshot tệp nguồn...", nil)
	_, m, err := Ingest(r.deps.Store.Dir(), r.opts.SourcePath, r.opts.intent())
	if err != nil {
		return err
	}
	r.emit(StageIngesting, 0, 0, fmt.Sprintf("Snapshot nguồn sẵn sàng: %s (mã hóa %s, %d byte)", m.SourceName, m.Encoding, m.SizeBytes), nil)
	return nil
}

func (r *runner) segment(ctx context.Context) error {
	src, err := r.ws.LoadSource()
	if err != nil {
		return err
	}
	units := buildSourceUnits(src, r.deps.Budgets.MaxUnitBytes)
	guidance, err := r.ws.LoadGuidance()
	if err != nil {
		return fmt.Errorf("Đọc chỉ dẫn chia tách: %w", err)
	}
	r.emit(StageSegmenting, 0, 0, fmt.Sprintf("Đang nhận diện ranh giới chương theo ngữ nghĩa (%d đơn vị tọa độ)...", len(units)), nil)
	digest := segmentInputDigest(Digest(src), guidance, segmentPromptVersion)
	// The chunk cache identity additionally binds MaxUnitBytes: the unit table is uniquely determined by (normalized source, MaxUnitBytes), and changing the model
	// tier changes MaxUnitBytes, which reshapes the virtual slices of over-long lines - the ID sequence (L1.1...) and chunk endpoints are reproducible but the byte
	// ranges have changed, and matching by endpoint IDs alone would reuse misaligned old boundaries (anchor mismatch fails deterministically, or silently mis-splits).
	chunkIdentity := fmt.Sprintf("%s\x00units:%d", digest, r.deps.Budgets.MaxUnitBytes)
	seg, err := Segment(ctx, r.deps.Segment.Model, r.deps.Prompts.Segment, src, units, guidance,
		r.deps.Budgets.SegmentChunkBytes, r.deps.Budgets.SegmentContextMargin, r.deps.Budgets.SegmentMaxTokens,
		r.profileFor(r.deps.Segment, StageSegmenting), r.ws, chunkIdentity)
	if err != nil {
		return err
	}
	if err := writeArtifact(r.ws, fileSegmentation, digest, *seg); err != nil {
		return err
	}
	// The final segmentation is persisted and the chunk-level cache has done its job; a cleanup failure does not affect correctness (the digest is still consistent), but must leave a trace.
	if cerr := r.ws.clearDir(dirSegmentChunks); cerr != nil {
		r.emit(StageSegmenting, 0, 0, fmt.Sprintf("Không dọn được cache theo khối (không ảnh hưởng kết quả chia): %v", cerr), nil)
	}
	r.emit(StageSegmenting, len(seg.Chapters), len(seg.Chapters),
		fmt.Sprintf("Chia tách xong: %d chương, %d vùng phụ", len(seg.Chapters), len(seg.Matter)), nil)
	return nil
}

// confirm handles segmentation confirmation. --yes accepts automatically and writes the confirmation artifact; otherwise it shows the preview and stops.
func (r *runner) confirm() bool {
	seg, err := readArtifact[Segmentation](r.ws, fileSegmentation)
	if err != nil {
		r.fail("Đọc kết quả chia tách", err)
		return false
	}
	in, err := r.ws.LoadIntent()
	if err != nil {
		r.fail("Đọc ý định nhập", err)
		return false
	}
	accept := r.opts.AcceptSegmentation
	auto := r.opts.AutoConfirm || (in != nil && in.AutoConfirm)
	// A segmentation where semantic tolerance occurred (Notes non-empty: empty-chapter absorption / start fallback / overlap dedup) is not blindly let through by --yes:
	// the structure was deterministically rewritten and must be manually reviewed - otherwise under --yes nobody sees the tolerance notes, which amounts to a silent rewrite.
	// Pressing y after the TUI preview takes AcceptSegmentation (an explicit ruling made after viewing the preview) and is not subject to this.
	blockedByNotes := auto && !accept && len(seg.Payload.Notes) > 0
	if blockedByNotes {
		auto = false
	}
	if !auto && !accept {
		msg := buildConfirmPreview(&seg.Payload)
		if blockedByNotes {
			msg += "  ! Có ghi chú dung sai khi chia tách, --yes không tự động chấp nhận, hãy kiểm tra thủ công\n"
		}
		r.emit(StageAwaitingConfirmation, len(seg.Payload.Chapters), len(seg.Payload.Chapters), msg, nil)
		return false
	}
	raw, err := r.ws.readBytes(fileSegmentation)
	if err != nil {
		r.fail("Đọc kết quả chia tách", err)
		return false
	}
	method, doneMsg := confirmMethodAuto, "Đã tự động chấp nhận chia tách (--yes)"
	if accept {
		method, doneMsg = confirmMethodUser, "Đã xác nhận chia tách (kiểm tra thủ công)"
	}
	conf := Confirmation{Method: method, Chapters: len(seg.Payload.Chapters)}
	if err := writeArtifact(r.ws, fileConfirmation, Digest(raw), conf); err != nil {
		r.fail("Ghi kết quả xác nhận", err)
		return false
	}
	r.emit(StageAwaitingConfirmation, len(seg.Payload.Chapters), len(seg.Payload.Chapters), doneMsg, nil)
	return true
}

// buildConfirmPreview assembles the segmentation confirmation preview: chapter count, auxiliary regions, all chapter titles and uncertain markers (RFC §8.4).
// Listed in full; the panel viewport is scrollable; there is no truncation cap.
func buildConfirmPreview(seg *Segmentation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Đã chia %d chương", len(seg.Chapters))
	if len(seg.Matter) > 0 {
		fmt.Fprintf(&b, ", %d vùng phụ", len(seg.Matter))
	}
	if len(seg.Uncertain) > 0 {
		fmt.Fprintf(&b, " (%d chương nghi vấn)", len(seg.Uncertain))
	}
	b.WriteString(", hãy kiểm tra:\n")
	uncertain := make(map[int]bool, len(seg.Uncertain))
	for _, n := range seg.Uncertain {
		uncertain[n] = true
	}
	for _, c := range seg.Chapters {
		fmt.Fprintf(&b, "  Chương %d %s", c.Number, c.Title)
		if uncertain[c.Number] {
			b.WriteString("  [nghi vấn]")
		}
		b.WriteByte('\n')
	}
	for _, mt := range seg.Matter {
		fmt.Fprintf(&b, "  [%s] %s\n", mt.Kind, mt.Title)
	}
	// The tolerance notes from segmentation time (e.g. an empty-body placeholder title merged into the previous section) must be presented at the manual stop point, otherwise the absorption becomes a silent rewrite.
	for _, n := range seg.Notes {
		fmt.Fprintf(&b, "  ! %s\n", n)
	}
	// Action hints (y confirm / --guide re-segment / Esc) are rendered uniformly by the TUI pause block; only facts are left here, to avoid two copies of text drifting apart.
	return b.String()
}

func (r *runner) analyze(ctx context.Context) error {
	src, err := r.ws.LoadSource()
	if err != nil {
		return err
	}
	segArt, err := readArtifact[Segmentation](r.ws, fileSegmentation)
	if err != nil {
		return err
	}
	seg := &segArt.Payload
	total := len(seg.Chapters)
	// The per-chapter digest binds only this chapter's body text, not batch context or the preceding ledger. If chapter K needs re-analysis due to missing/mismatch,
	// old artifacts after it whose digest happens to match would be reused with an already stale ledger. Before analysis starts, clean the tail beyond the fresh prefix,
	// to enforce "re-analyzing a chapter invalidates all analyses after it", after which forward analysis no longer produces a stale tail (RFC §9.6 / #4a).
	if err := discardAnalysesAfter(r.ws, analyzedChapters(r.ws, seg, src, segArt.InputDigest, analyzePromptVersion), total); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := analyzedChapters(r.ws, seg, src, segArt.InputDigest, analyzePromptVersion)
		if start >= total {
			break
		}
		r.emit(StageAnalyzing, start, total, fmt.Sprintf("Phân tích các lô liên tiếp từ chương %d...", start+1), nil)
		done, err := AnalyzeNext(ctx, r.deps.Analyze.Model, r.deps.Prompts.Analyze, r.ws, src, seg, segArt.InputDigest, analyzePromptVersion, r.deps.Budgets.Analyze, r.profileFor(r.deps.Analyze, StageAnalyzing))
		if err != nil {
			return err
		}
		if done == 0 {
			break
		}
	}
	r.emit(StageAnalyzing, total, total, "Trích xuất dữ kiện từng chương xong", nil)
	return nil
}

func (r *runner) synthesize(ctx context.Context) error {
	segArt, err := readArtifact[Segmentation](r.ws, fileSegmentation)
	if err != nil {
		return err
	}
	total := len(segArt.Payload.Chapters)
	facts := loadPriorFacts(r.ws, total)
	if len(facts) != total {
		return fmt.Errorf("Phân tích từng chương chưa đầy đủ: %d/%d", len(facts), total)
	}
	r.emit(StageSynthesizing, 0, total, "Đang tổng hợp ngữ nghĩa toàn sách theo phân tầng...", nil)
	syn, err := Synthesize(ctx, r.deps.Synthesize.Model, r.deps.Prompts.Synthesize, r.deps.Prompts.Range, r.ws, facts,
		r.deps.Budgets.SynthesizeRangeBytes, r.deps.Budgets.SynthesizeMaxTokens, r.profileFor(r.deps.Synthesize, StageSynthesizing))
	if err != nil {
		return err
	}
	if err := writeArtifact(r.ws, fileSynthesis, synthesisInputDigest(facts), *syn); err != nil {
		return err
	}
	r.emit(StageSynthesizing, total, total, fmt.Sprintf("Tổng hợp xong: %d tập, trạng thái truyện %s", len(syn.Structure), syn.StoryStatus), nil)
	return nil
}

func (r *runner) publish(ctx context.Context) error {
	synArt, err := readArtifact[BookSynthesis](r.ws, fileSynthesis)
	if err != nil {
		return err
	}
	segArt, err := readArtifact[Segmentation](r.ws, fileSegmentation)
	if err != nil {
		return err
	}
	seg := &segArt.Payload
	src, err := r.ws.LoadSource()
	if err != nil {
		return err
	}
	total := len(seg.Chapters)
	facts := loadPriorFacts(r.ws, total)
	if len(facts) != total {
		return fmt.Errorf("Phân tích chưa đầy đủ trước khi phát hành: %d/%d", len(facts), total)
	}
	closed, err := r.resolveStory(&synArt.Payload)
	if err != nil {
		return err
	}
	manifest, err := r.ws.LoadManifest()
	if err != nil {
		return err
	}
	f, err := AssembleFoundation(&synArt.Payload, facts, closed, manifest.SourceName)
	if err != nil {
		return err
	}
	r.emit(StageValidating, 0, total, "Lắp ráp Foundation đã qua kiểm tra", nil)

	r.emit(StagePublishing, 0, total, "Đang phát hành Foundation chính thức...", nil)
	if err := publishFoundation(r.deps.Store, f); err != nil {
		return err
	}
	// The import-complete Hold must be persisted before any chapter commit: if a crash happens between "last chapter commit" and "set Hold",
	// after restart isPublished=true -> the import is judged complete but the Hold was never set, and the Engine would mistake the imported book for an ordinary halted run and continue writing.
	// Placing it after publishFoundation (RunMeta already initialized) and before chapter commits closes that window completely; on a publish rerun it is
	// set again idempotently (--continue sets no Hold and leaves it to automatic relay, RFC §12.4).
	if err := r.setCompletionHold(); err != nil {
		return fmt.Errorf("Thiết lập Hold hoàn tất nhập: %w", err)
	}
	for i, c := range seg.Chapters {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.emit(StagePublishing, c.Number, total, fmt.Sprintf("Phát hành chương %d/%d: %s", c.Number, total, c.Title), nil)
		if err := publishChapter(ctx, r.deps.Store, r.deps.CommitChapter, c.Number, seg.Content(src, i), facts[i]); err != nil {
			return err
		}
	}
	return nil
}

// storyChoice returns the effective ruling for an uncertain state: first the persisted ruling bound to the current synthesis, then this run's opts, then the original intent.
// A persisted ruling must verify that InputDigest agrees with the current synthesis - after re-synthesis the old ruling is invalid, and an old open/closed must not be silently
// applied to the new result, otherwise the user would not be asked again (RFC §10.4). An explicit --story (intent) is a standing user instruction across syntheses and may be kept.
func (r *runner) storyChoice() (string, error) {
	if raw, err := r.ws.readBytes(fileSynthesis); err == nil {
		if art, aerr := readArtifact[StoryResolution](r.ws, fileStoryResolve); aerr == nil && art.InputDigest == Digest(raw) {
			return art.Payload.Choice, nil
		} else if aerr != nil && !os.IsNotExist(aerr) {
			return "", fmt.Errorf("Đọc phán quyết trạng thái truyện: %w", aerr)
		}
	} else {
		return "", fmt.Errorf("Đọc kết quả tổng hợp: %w", err)
	}
	if r.opts.StoryResolution != "" {
		return r.opts.StoryResolution, nil
	}
	in, err := r.ws.LoadIntent()
	if err != nil {
		return "", fmt.Errorf("Đọc ý định nhập: %w", err)
	}
	return in.StoryResolution, nil
}

// resolveStoryStatus writes story-resolution.json (bound to the current synthesis) when the state is uncertain and an explicit ruling exists,
// so downstream NextAction naturally passes; with no ruling it shows the wait and stops.
func (r *runner) resolveStoryStatus() bool {
	choice, err := r.storyChoice()
	if err != nil {
		r.fail("Đọc phán quyết trạng thái truyện", err)
		return false
	}
	if choice != storyOpen && choice != storyClosed {
		r.emit(StageAwaitingStoryStatus, 0, 0, "Tổng hợp phán định trạng thái truyện là uncertain, hãy chỉ rõ bằng --story=open|closed rồi thử lại", nil)
		return false
	}
	raw, err := r.ws.readBytes(fileSynthesis)
	if err != nil {
		r.fail("Đọc kết quả tổng hợp", err)
		return false
	}
	if err := writeArtifact(r.ws, fileStoryResolve, Digest(raw), StoryResolution{Choice: choice}); err != nil {
		r.fail("Lưu phán quyết trạng thái truyện", err)
		return false
	}
	return true
}

// resolveStory gives the story wrap-up state from the synthesis result and the user's explicit ruling (RFC §10.4).
func (r *runner) resolveStory(syn *BookSynthesis) (bool, error) {
	switch syn.StoryStatus {
	case storyClosed:
		return true, nil
	case storyOpen:
		return false, nil
	case storyUncertain:
		choice, err := r.storyChoice()
		if err != nil {
			return false, err
		}
		switch choice {
		case storyClosed:
			return true, nil
		case storyOpen:
			return false, nil
		default:
			return false, fmt.Errorf("Trạng thái truyện uncertain, cần --story=open|closed")
		}
	default:
		return false, fmt.Errorf("story_status không xác định: %q", syn.StoryStatus)
	}
}

// setCompletionHold sets an import-complete Hold; only --continue skips it (RFC §12.4).
// An error must propagate - the Hold is the only guarantee of "do not wrongly continue writing after import", and a silent failure means the protection is lost.
func (r *runner) setCompletionHold() error {
	in, err := r.ws.LoadIntent()
	if err != nil {
		return fmt.Errorf("Đọc ý định nhập: %w", err)
	}
	if r.opts.ContinueAfter || (in != nil && in.ContinueAfterImport) {
		return nil
	}
	return r.deps.Store.RunMeta.SetAdvanceHold(domain.AdvanceHold{
		After:  domain.AdvanceHoldAtBoundary,
		Reason: "Nhập tiểu thuyết ngoài hoàn tất, chờ duyệt rồi viết tiếp",
	})
}
