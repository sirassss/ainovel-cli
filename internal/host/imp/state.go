package imp

import (
	"fmt"
	"os"

	"github.com/voocel/ainovel-cli/internal/store"
)

// Action is the deterministic next step NextAction derives from workspace facts.
// Persisted state does not write a stage enum that could drift; the next action is derived only from artifacts (RFC §6.2).
type Action string

const (
	ActionIngest               Action = "ingest"
	ActionSegment              Action = "segment"
	ActionAwaitConfirmation    Action = "await_confirmation"
	ActionAnalyze              Action = "analyze"
	ActionSynthesize           Action = "synthesize"
	ActionAwaitStoryResolution Action = "await_story_resolution"
	ActionPublish              Action = "publish"
	ActionDone                 Action = "done"
)

// Facts is the minimal fact snapshot read from the workspace that determines the next action.
// It separates pure decision (NextAction) from IO (LoadState): NextAction is constant for the same Facts (RFC §20.1).
type Facts struct {
	WorkspaceReady   bool // manifest + intent + source triple is complete
	Segmented        bool
	Confirmed        bool
	ExpectedChapters int // total chapters confirmed by the segmentation (filled from phase 2 on)
	AnalyzedChapters int // count of analyses contiguous from chapter 1 with matching InputDigest (filled from phase 3 on)
	Synthesized      bool
	StoryUncertain   bool
	StoryResolved    bool
	Published        bool // formal artifacts fully agree with synthesis (filled from phase 5 on)
}

// NextAction walks the fixed linear pipeline and returns the first missing or unsatisfied action. Pure function, no IO.
func NextAction(f Facts) Action {
	switch {
	case f.Published:
		// Publishing is terminal: the formal store has been reconciled fully consistent, and the workspace is just an audit archive. Upstream artifacts
		// going stale from a prompt version / guidance upgrade no longer demand redo - otherwise a version upgrade would
		// retroactively judge an already published book back to halfway, and the Engine's cross-restart gate would lock it permanently.
		return ActionDone
	case !f.WorkspaceReady:
		return ActionIngest
	case !f.Segmented:
		return ActionSegment
	case !f.Confirmed:
		return ActionAwaitConfirmation
	case f.AnalyzedChapters < f.ExpectedChapters:
		return ActionAnalyze
	case !f.Synthesized:
		return ActionSynthesize
	case f.StoryUncertain && !f.StoryResolved:
		return ActionAwaitStoryResolution
	default:
		return ActionPublish
	}
}

// artifactFresh decides that an artifact exists and its InputDigest equals the want that should currently be rebuilt;
// missing, parse failure, schema or digest mismatch all count as not fresh (needs redo).
func artifactFresh[T any](w *Workspace, rel, want string) (bool, error) {
	a, err := readArtifact[T](w, rel)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.InputDigest == want, nil
}

// LoadState reads the current fact snapshot from the workspace (workspace only, not the formal Store).
// Linear short-circuit: each step verifies that the artifact's InputDigest matches the digest currently rebuildable from upstream; any mismatch counts as that step incomplete,
// downstream facts stay false, and NextAction redoes from there - this is what makes "changing segmentation / prompt version / source" naturally invalidate downstream (RFC §6.2/§6.3 / invariant 1).
// Published is filled in by the caller via formal publish reconciliation (uniformly through CollectFacts).
func LoadState(w *Workspace) (Facts, error) {
	var f Facts
	if !w.Active() {
		return f, nil
	}
	if !(w.has(fileManifest) && w.has(fileIntent) && w.has(fileSource)) {
		return f, nil
	}
	src, err := w.LoadSource()
	if err != nil {
		return f, fmt.Errorf("Đọc snapshot nguồn nhập: %w", err)
	}
	f.WorkspaceReady = true
	guidance, err := w.LoadGuidance()
	if err != nil {
		return f, fmt.Errorf("Đọc chỉ dẫn chia tách: %w", err)
	}

	// segmentation: binds the normalized source + user guidance + segmentation prompt version. A guidance change (--guide re-identification) naturally invalidates the old segmentation.
	segArt, err := readArtifact[Segmentation](w, fileSegmentation)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("Đọc kết quả chia tách: %w", err)
	}
	if segArt.InputDigest != segmentInputDigest(Digest(src), guidance, segmentPromptVersion) {
		return f, nil
	}
	f.Segmented = true
	seg := &segArt.Payload
	f.ExpectedChapters = len(seg.Chapters)

	// confirmation: binds the raw bytes of the segmentation artifact.
	segRaw, err := w.readBytes(fileSegmentation)
	if err != nil {
		return f, fmt.Errorf("Đọc nguyên văn kết quả chia tách: %w", err)
	}
	confirmed, err := artifactFresh[Confirmation](w, fileConfirmation, Digest(segRaw))
	if err != nil {
		return f, fmt.Errorf("Đọc xác nhận chia tách: %w", err)
	}
	if !confirmed {
		return f, nil
	}
	f.Confirmed = true

	// Per-chapter analysis: the contiguous count whose per-chapter InputDigest matches the segmentation identity / version / body text.
	f.AnalyzedChapters, err = analyzedChaptersStrict(w, seg, src, segArt.InputDigest, analyzePromptVersion)
	if err != nil {
		return f, err
	}
	if f.AnalyzedChapters < f.ExpectedChapters {
		return f, nil
	}

	// synthesis: binds the ordered per-chapter facts.
	facts, err := loadPriorFactsStrict(w, f.ExpectedChapters)
	if err != nil {
		return f, err
	}
	synArt, err := readArtifact[BookSynthesis](w, fileSynthesis)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("Đọc kết quả tổng hợp toàn sách: %w", err)
	}
	if synArt.InputDigest != synthesisInputDigest(facts) {
		return f, nil
	}
	f.Synthesized = true
	f.StoryUncertain = synArt.Payload.StoryStatus == storyUncertain

	// story resolution: when uncertain, binds the raw bytes of the synthesis artifact, or is preselected by the intent.
	synRaw, err := w.readBytes(fileSynthesis)
	if err != nil {
		return f, fmt.Errorf("Đọc nguyên văn kết quả tổng hợp toàn sách: %w", err)
	}
	resolved, err := artifactFresh[StoryResolution](w, fileStoryResolve, Digest(synRaw))
	if err != nil {
		return f, fmt.Errorf("Đọc phán quyết trạng thái truyện: %w", err)
	}
	if resolved {
		f.StoryResolved = true
	} else if in, iErr := w.LoadIntent(); iErr != nil {
		return f, fmt.Errorf("Đọc ý định nhập: %w", iErr)
	} else if in.StoryResolution != "" {
		f.StoryResolved = true
	}
	return f, nil
}

// CollectFacts combines workspace facts with formal publish reconciliation and is the unified fact entry for ResumeStatus/ResumeSummary/runner.
// The expected chapter count for publish reconciliation prefers the fresh segmentation; when the segmentation mismatches due to a prompt version / guidance upgrade,
// it falls back to the chapter count confirmed in the artifact at that time - the formal chapters of an already published book were persisted under that segmentation,
// and reconciling with a digest recomputed by the current version would match nothing.
func CollectFacts(st *store.Store, w *Workspace) (Facts, error) {
	f, err := LoadState(w)
	if err != nil {
		return f, err
	}
	expected := f.ExpectedChapters
	if expected == 0 {
		if segArt, err := readArtifact[Segmentation](w, fileSegmentation); err == nil {
			expected = len(segArt.Payload.Chapters)
		}
	}
	f.Published, err = isPublished(st, expected)
	return f, err
}

// ResumeStatus reports whether an active import workspace exists and whether it has fully completed (including formal publish reconciliation).
// Used by the cross-restart Engine gate (RFC §12.5): when active && !done, the ordinary creation flow is forbidden from consuming a half-published state.
func ResumeStatus(st *store.Store) (active, done bool, err error) {
	w := OpenWorkspace(st.Dir())
	if !w.Active() {
		return false, false, nil
	}
	f, err := CollectFacts(st, w)
	if err != nil {
		return true, false, err
	}
	return true, NextAction(f) == ActionDone, nil
}

// ResumeSummary produces a one-line hint for an unfinished import (RFC §18.2); returns an empty string when there is no unfinished import.
// Lets the host proactively notify at startup / welcome screen, so the user does not discover the book is stuck halfway through an import only when creation is rejected by the gate.
func ResumeSummary(st *store.Store) string {
	w := OpenWorkspace(st.Dir())
	if !w.Active() {
		return ""
	}
	f, err := CollectFacts(st, w)
	if err != nil {
		return "Phát hiện lỗi đọc trạng thái nhập: " + err.Error() + "; hãy chạy /import để xem và sửa"
	}
	var state string
	switch NextAction(f) {
	case ActionDone:
		return ""
	case ActionIngest, ActionSegment:
		state = "Chưa chia tách xong"
	case ActionAwaitConfirmation:
		state = fmt.Sprintf("Đã chia %d chương, chờ kiểm tra xác nhận", f.ExpectedChapters)
	case ActionAnalyze:
		state = fmt.Sprintf("Đã phân tích %d/%d chương", f.AnalyzedChapters, f.ExpectedChapters)
	case ActionSynthesize:
		state = "Phân tích từng chương xong, chờ tổng hợp toàn sách"
	case ActionAwaitStoryResolution:
		state = "Chờ làm rõ trạng thái truyện (--story=open|closed)"
	case ActionPublish:
		state = "Tổng hợp xong, chờ phát hành trạng thái chính thức"
	}
	return "Phát hiện lượt nhập chưa hoàn tất (" + state + "), nhập /import để tiếp tục từ điểm dừng"
}

// checkImportPreconditions verifies the preconditions of a new import (RFC §12.1):
// no existing work info, completed chapters or in-flight PendingCommit. How to merge an existing novel with new external text is unclear, so the first version explicitly refuses.
func checkImportPreconditions(st *store.Store) error {
	book, err := st.Book.Load()
	if err != nil {
		return fmt.Errorf("Đọc thông tin tác phẩm: %w", err)
	}
	if book != nil {
		return fmt.Errorf("Đã có tác phẩm \"%s\", từ chối gộp tiểu thuyết ngoài vào sách không rỗng", book.Title)
	}
	prog, err := st.Progress.Load()
	if err != nil {
		return fmt.Errorf("Đọc tiến độ: %w", err)
	}
	if prog != nil && len(prog.CompletedChapters) > 0 {
		return fmt.Errorf("Đã có %d chương hoàn thành, từ chối gộp tiểu thuyết ngoài vào sách không rỗng", len(prog.CompletedChapters))
	}
	pending, err := st.Signals.LoadPendingCommit()
	if err != nil {
		return fmt.Errorf("Đọc bản nộp đang dở: %w", err)
	}
	if pending != nil {
		return fmt.Errorf("Có bản nộp chương đang dở, hãy hoàn tất hoặc dọn dẹp trước rồi mới nhập")
	}
	return nil
}
