package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/chapterfacts"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/revision"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// CommitChapterTool commits a chapter: load the body text → save the final text → generate the summary → update state → update progress.
type CommitChapterTool struct {
	store      *store.Store
	styleStats *StyleStatsIndex
}

// NewCommitChapterTool creates the commit tool. styleStats must be shared with novel_context,
// so that the same statistics index is refreshed after additions, rewrites and recovery completion.
func NewCommitChapterTool(store *store.Store, styleStats *StyleStatsIndex) *CommitChapterTool {
	if styleStats == nil {
		panic("tools: NewCommitChapterTool requires StyleStatsIndex")
	}
	return &CommitChapterTool{store: store, styleStats: styleStats}
}

func (t *CommitChapterTool) chapterStyleDelta(chapter int) (domain.StyleDelta, error) {
	record, err := t.store.ChapterRecords.Load(chapter)
	if err != nil || record == nil {
		return domain.StyleDelta{}, err
	}
	return record.StyleDelta, nil
}

// commitOutput embeds extension fields on top of domain.CommitResult, keeping the domain package independent of rules.
// Since the embedded fields are promoted by the JSON marshaler, the serialized result is equivalent to a flat structure.
type commitOutput struct {
	domain.CommitResult
	RuleViolations []rules.Violation `json:"rule_violations,omitempty"`
}

// commitArgs is the normalized structured payload of the commit Saga. On first execution it is written to PendingCommit together with the body snapshot;
// crash recovery always replays this frozen intent and ignores the arguments and draft produced by a new Worker.
type commitArgs struct {
	Chapter int `json:"chapter"`
	domain.ChapterFacts
}

func (t *CommitChapterTool) Name() string { return "commit_chapter" }
func (t *CommitChapterTool) Description() string {
	return "Nộp bản chính thức của chương. Nạp bản nháp, lưu thành bản chính thức, cập nhật dòng thời gian, chi tiết gài trước, quan hệ, trạng thái nhân vật và tiến độ." +
		"Trả về các sự kiện có cấu trúc: next_chapter / review_required / arc_end / volume_end / needs_expansion / book_complete / flow, v.v."
}
func (t *CommitChapterTool) Label() string { return "Nộp chương" }

// Write tool (cross-domain recoverable Saga: full payload → final text/state → progress → checkpoint); concurrency is forbidden.
func (t *CommitChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *CommitChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *CommitChapterTool) StrictSchema() bool                     { return true }

func (t *CommitChapterTool) Schema() map[string]any {
	props := []schema.Prop{schema.Property("chapter", schema.Int("Số chương")).Required()}
	props = append(props, chapterfacts.Properties(true)...)
	return schema.Object(props...)
}

func (t *CommitChapterTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var requested commitArgs
	if err := json.Unmarshal(args, &requested); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if requested.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	existingPending, err := t.store.Signals.LoadPendingCommit()
	if err != nil {
		return nil, fmt.Errorf("load pending commit: %w: %w", errs.ErrStoreRead, err)
	}
	if existingPending != nil && existingPending.Chapter != requested.Chapter {
		return nil, fmt.Errorf("Có bản nộp chương chưa khôi phục: chương %d (giai đoạn %s), hãy khôi phục hoặc nộp lại chương này trước: %w", existingPending.Chapter, existingPending.Stage, errs.ErrToolConflict)
	}
	if existingPending != nil {
		switch existingPending.Stage {
		case domain.CommitStageStarted, domain.CommitStageStateApplied, domain.CommitStageProgressMarked, domain.CommitStageSignalSaved:
		default:
			return nil, fmt.Errorf("Giai đoạn pending commit không hợp lệ: %q: %w", existingPending.Stage, errs.ErrToolConflict)
		}
	}

	a := requested
	if existingPending != nil && existingPending.Stage != domain.CommitStageProgressMarked && existingPending.Stage != domain.CommitStageSignalSaved {
		if len(existingPending.Payload) == 0 {
			return nil, fmt.Errorf("Chương %d có bản nộp dang dở của phiên bản cũ nhưng thiếu payload để phát lại; từ chối ghi đè bằng tham số mới sinh, hãy khôi phục từ checkpoint gần nhất hoặc đối chiếu thủ công meta/pending_commit.json: %w",
				existingPending.Chapter, errs.ErrToolConflict)
		}
		if err := json.Unmarshal(existingPending.Payload, &a); err != nil {
			return nil, fmt.Errorf("decode pending commit payload: %w: %w", errs.ErrStoreRead, err)
		}
		if a.Chapter != existingPending.Chapter {
			return nil, fmt.Errorf("Số chương trong payload pending commit không khớp: ghi nhận=%d payload=%d: %w", existingPending.Chapter, a.Chapter, errs.ErrToolConflict)
		}
	}

	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if progress == nil {
		return nil, fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
	}
	completed := slices.Contains(progress.CompletedChapters, a.Chapter)
	if existingPending != nil && (existingPending.Stage == domain.CommitStageProgressMarked || existingPending.Stage == domain.CommitStageSignalSaved) {
		if !completed {
			return nil, fmt.Errorf("pending commit đã tới %s nhưng progress chưa đánh dấu hoàn thành chương %d: %w", existingPending.Stage, a.Chapter, errs.ErrToolConflict)
		}
		return t.finishPendingCommit(*existingPending, progress)
	}
	if existingPending == nil || existingPending.Stage == domain.CommitStageStarted {
		if err := t.validateCommitArgs(a); err != nil {
			// An older version may have left a frozen commit behind before it detected the illegal rewrite facts. Keeping this
			// immutable payload would only make every retry repeat the same error, so the freeze is lifted explicitly,
			// letting the Writer fix the arguments and resubmit; neither the body text nor the chapter records are changed here.
			if existingPending != nil && existingPending.Rewrite &&
				(errors.Is(err, errs.ErrToolArgs) || errors.Is(err, errs.ErrToolPrecondition)) {
				if clearErr := t.store.Signals.ClearPendingCommit(); clearErr != nil {
					return nil, fmt.Errorf("Kiểm tra bản nộp viết lại thất bại (%v), và dọn bản nộp đang đóng băng cũng thất bại: %w: %w", err, errs.ErrStoreWrite, clearErr)
				}
				return nil, fmt.Errorf("Bản nộp viết lại còn sót từ phiên bản cũ không qua kiểm tra, đã gỡ đóng băng; hãy sửa rồi nộp lại: %w", err)
			}
			return nil, err
		}
	}

	if existingPending != nil && existingPending.Rewrite {
		if !completed {
			return nil, fmt.Errorf("Nộp bản viết lại yêu cầu chương %d đã có bản chính thức: %w", a.Chapter, errs.ErrToolConflict)
		}
		return t.executeRewriteCommit(a, progress, *existingPending, true)
	}
	if existingPending == nil && completed {
		if slices.Contains(progress.PendingRewrites, a.Chapter) {
			content, err := t.validateRewriteDraft(a.Chapter, a.Title, progress)
			if err != nil {
				return nil, err
			}
			payload, err := json.Marshal(a)
			if err != nil {
				return nil, fmt.Errorf("marshal rewrite payload: %w", err)
			}
			now := time.Now().Format(time.RFC3339)
			mode := "rewrite"
			if progress.Flow == domain.FlowPolishing {
				mode = "polish"
			}
			pending := domain.PendingCommit{Chapter: a.Chapter, Stage: domain.CommitStageStarted,
				Rewrite: true, RewriteMode: mode, Payload: payload, DraftContent: content,
				Summary: a.Summary, HookType: a.HookType,
				DominantStrand: a.DominantStrand, StartedAt: now, UpdatedAt: now}
			if err := t.store.Signals.SavePendingCommit(pending); err != nil {
				return nil, fmt.Errorf("save rewrite pending commit: %w: %w", errs.ErrStoreWrite, err)
			}
			return t.executeRewriteCommit(a, progress, pending, false)
		}
		return t.buildSkipResult(a.Chapter, progress)
	}

	// A new commit must pass the current phase / rewrite queue validation; an existing ordinary PendingCommit is the recovery protocol,
	// and is allowed to continue finishing across the interruption window of "Progress already landed first / Phase already complete".
	if existingPending == nil {
		if err := t.store.Progress.ValidateChapterWork(a.Chapter); err != nil {
			// A queue conflict is kept as is (already classified as ErrToolConflict); other IO errors are classified as Precondition.
			if errors.Is(err, errs.ErrToolConflict) {
				return nil, err
			}
			return nil, fmt.Errorf("Chương hiện không cho phép nộp: %w: %w", errs.ErrToolPrecondition, err)
		}
		if progress.Flow != domain.FlowRewriting && progress.Flow != domain.FlowPolishing {
			expected := progress.NextChapter()
			if a.Chapter != expected {
				return nil, fmt.Errorf("Viết tiếp thông thường chỉ được nộp chương kế tiếp %d, nhưng nhận được chương %d: %w", expected, a.Chapter, errs.ErrToolConflict)
			}
		}
	}

	// Layered-mode out-of-range interception: it must precede any write operation, otherwise an out-of-range commit would corrupt the chapter file, summary and
	// Progress. boundary is reused for the arc/volume signals in step 6b below.
	var boundary *store.ArcBoundary
	if progress.Layered {
		b, bErr := t.store.Outline.CheckArcBoundary(a.Chapter)
		if bErr != nil {
			return nil, fmt.Errorf("Phát hiện biên cung thất bại chapter=%d: %w: %w", a.Chapter, errs.ErrStoreRead, bErr)
		}
		if b == nil {
			return nil, fmt.Errorf(
				"Chương %d nằm ngoài phạm vi dàn ý phân tầng: trước khi viết phải expand_arc để mở rộng cung hoặc append_volume để thêm tập; nếu cả truyện đã kết thúc thì gọi save_foundation type=complete_book: %w",
				a.Chapter, errs.ErrToolPrecondition)
		}
		boundary = b
	}

	// 1. Freeze the chapter body. On the first commit it is read from the draft and landed together with PendingCommit; on recovery
	// only that snapshot is used, to prevent a new Worker from overwriting the draft before the retry and producing "old facts + new body text".
	var content string
	if existingPending != nil {
		content = existingPending.DraftContent
		if content == "" {
			return nil, fmt.Errorf("Bản nộp dang dở của chương %d thiếu draft_content, không thể chứng minh nội dung chương khôi phục khớp với bản nộp gốc: %w",
				a.Chapter, errs.ErrToolConflict)
		}
	} else {
		var loadErr error
		content, _, loadErr = t.store.Drafts.LoadChapterContent(a.Chapter)
		if loadErr != nil {
			return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, loadErr)
		}
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	// The title is rendered by the engine: models often omit the title line, write the wrong level, or carry over a wrong chapter number from the planning phase.
	content = domain.ApplyChapterHeading(content, a.Title, a.Chapter)
	wordCount := utf8.RuneCountInString(content)

	var pending domain.PendingCommit
	if existingPending != nil {
		pending = *existingPending
	} else {
		payload, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("marshal commit payload: %w", err)
		}
		now := time.Now().Format(time.RFC3339)
		pending = domain.PendingCommit{
			Chapter: a.Chapter, Stage: domain.CommitStageStarted, Payload: payload, DraftContent: content,
			Summary: a.Summary, HookType: a.HookType, DominantStrand: a.DominantStrand,
			StartedAt: now, UpdatedAt: now,
		}
		if err := t.store.Signals.SavePendingCommit(pending); err != nil {
			return nil, fmt.Errorf("save pending commit: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// StageStarted may mean no artifact has been written yet, or a crash midway through the state delta; all operations of the full payload
	// must be idempotent, so they are replayed uniformly. StageStateApplied goes straight to Progress.
	if pending.Stage == domain.CommitStageStarted {
		// 2. Save the final text
		if err := t.store.Drafts.SaveFinalChapter(a.Chapter, content); err != nil {
			return nil, fmt.Errorf("save final chapter: %w: %w", errs.ErrStoreWrite, err)
		}
		style, err := t.chapterStyleDelta(a.Chapter)
		if err != nil {
			return nil, fmt.Errorf("load chapter style: %w: %w", errs.ErrStoreRead, err)
		}
		if _, err := t.store.ChapterRecords.Accept(a.Chapter, domain.ChapterOriginGenerated, content, a.ChapterFacts, style); err != nil {
			return nil, fmt.Errorf("save chapter record: %w: %w", errs.ErrStoreWrite, err)
		}

		// 3. Save the summary
		summary := domain.ChapterSummary{
			Chapter: a.Chapter, Title: a.Title, Summary: a.Summary, Characters: a.Characters, KeyEvents: a.KeyEvents,
		}
		if err := t.store.Summaries.SaveSummary(summary); err != nil {
			return nil, fmt.Errorf("save summary: %w: %w", errs.ErrStoreWrite, err)
		}

		// 4. Update the state delta
		if len(a.TimelineEvents) > 0 {
			for i := range a.TimelineEvents {
				a.TimelineEvents[i].Chapter = a.Chapter
			}
			if err := t.store.World.AppendTimelineEvents(a.TimelineEvents); err != nil {
				return nil, fmt.Errorf("append timeline: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		if len(a.ForeshadowUpdates) > 0 {
			if err := t.store.World.UpdateForeshadow(a.Chapter, a.ForeshadowUpdates); err != nil {
				return nil, fmt.Errorf("update foreshadow: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		if len(a.RelationshipChanges) > 0 {
			for i := range a.RelationshipChanges {
				a.RelationshipChanges[i].Chapter = a.Chapter
			}
			if err := t.store.World.UpdateRelationships(a.RelationshipChanges); err != nil {
				return nil, fmt.Errorf("update relationships: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		if len(a.StateChanges) > 0 {
			for i := range a.StateChanges {
				a.StateChanges[i].Chapter = a.Chapter
			}
			if err := t.store.World.AppendStateChanges(a.StateChanges); err != nil {
				return nil, fmt.Errorf("append state changes: %w: %w", errs.ErrStoreWrite, err)
			}
		}

		// 4b. Accumulate the supporting-cast roster: non-core characters appearing in this chapter go into cast_ledger, for novel_context to recall.
		// On failure only warn and do not block the commit; the roster is secondary data and can self-heal on the next chapter's commit.
		if len(a.Characters) > 0 {
			coreNames, err := loadCoreCharacterNameSet(t.store)
			if err != nil {
				return nil, fmt.Errorf("load core characters for cast ledger: %w: %w", errs.ErrStoreRead, err)
			}
			if err := t.store.Cast.MergeAppearances(a.Chapter, a.Characters, a.CastIntros, coreNames); err != nil {
				slog.Warn("accumulating minor character list failed, skipped", "module", "commit", "chapter", a.Chapter, "err", err)
			}
		}

		pending.Stage = domain.CommitStageStateApplied
		pending.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := t.store.Signals.SavePendingCommit(pending); err != nil {
			return nil, fmt.Errorf("update pending commit stage: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 5. Update progress
	if !completed {
		if err := t.store.Progress.MarkChapterComplete(a.Chapter, wordCount, a.HookType, a.DominantStrand); err != nil {
			return nil, fmt.Errorf("mark chapter complete: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 6. Decide whether a review is needed
	progress, err = t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	completedCount := 0
	if progress != nil {
		completedCount = len(progress.CompletedChapters)
	}

	// 6b. Long-form mode arc/volume signals: boundary was validated up front at entry, and is guaranteed non-nil in Layered mode
	var arcEnd, volumeEnd, needsExpansion, needsNewVolume bool
	var vol, arc, nextVol, nextArc int
	if progress != nil && progress.Layered && boundary != nil {
		arcEnd = boundary.IsArcEnd
		volumeEnd = boundary.IsVolumeEnd
		vol = boundary.Volume
		arc = boundary.Arc
		needsExpansion = boundary.NeedsExpansion
		needsNewVolume = boundary.NeedsNewVolume
		nextVol = boundary.NextVolume
		nextArc = boundary.NextArc
		if err := t.store.Progress.UpdateVolumeArc(vol, arc); err != nil {
			return nil, fmt.Errorf("update volume/arc: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	var reviewRequired bool
	var reviewReason string
	if progress != nil && progress.Layered {
		reviewRequired, reviewReason = domain.ShouldArcReview(arcEnd, volumeEnd, vol, arc)
	} else {
		reviewRequired, reviewReason = domain.ShouldReview(completedCount)
	}

	// 7. Build the structured signal
	result := domain.CommitResult{
		Chapter:        a.Chapter,
		Committed:      true,
		WordCount:      wordCount,
		NextChapter:    a.Chapter + 1,
		ReviewRequired: reviewRequired,
		ReviewReason:   reviewReason,
		HookType:       a.HookType,
		DominantStrand: a.DominantStrand,
		Feedback:       a.Feedback,
		// (feedback is also persisted to the feedback pool, see persistFeedback below; the return value is only a mirror,
		// what the architect consumes via novel_context is the store fact)
		ArcEnd:         arcEnd,
		VolumeEnd:      volumeEnd,
		Volume:         vol,
		Arc:            arc,
		NeedsExpansion: needsExpansion,
		NeedsNewVolume: needsNewVolume,
		NextVolume:     nextVol,
		NextArc:        nextArc,
	}

	// 8. Completion judgment: last chapter of a non-layered book / last chapter of the final volume of a layered book → MarkComplete
	bookComplete, err := t.applyCompletion(&result, progress)
	if err != nil {
		return nil, err
	}
	if bookComplete {
		result.BookComplete = true
	}
	latestProgress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress after completion: %w: %w", errs.ErrStoreRead, err)
	}
	if latestProgress != nil {
		result.Flow = string(latestProgress.Flow)
	}

	// 8.5 The feedback pool is persistent fact for later planning, consumed by the Architect on its next structural operation.
	if a.Feedback != nil && (strings.TrimSpace(a.Feedback.Deviation) != "" || strings.TrimSpace(a.Feedback.Suggestion) != "") {
		if err := t.store.Outline.AppendOutlineFeedback(store.ChapterFeedback{
			Chapter: a.Chapter, Deviation: a.Feedback.Deviation, Suggestion: a.Feedback.Suggestion,
		}); err != nil {
			return nil, fmt.Errorf("persist outline feedback: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// Mechanical rules are part of the output and must be solidified before ProgressMarked, so recovery returns the same output directly.
	violations := t.checkRules(content, a.Chapter)
	output, err := json.Marshal(commitOutput{CommitResult: result, RuleViolations: violations})
	if err != nil {
		return nil, fmt.Errorf("marshal commit output: %w", err)
	}

	pending.Stage = domain.CommitStageProgressMarked
	pending.Result = &result
	pending.Output = output
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("update pending commit result: %w: %w", errs.ErrStoreWrite, err)
	}

	// 9. Append the checkpoint. This must precede clearing pending_commit, ensuring the pending_commit that stays visible after a restart
	// can always drive a rerun to fill in the missing checkpoint.
	if err := t.appendCommitCheckpoint(a.Chapter); err != nil {
		return nil, fmt.Errorf("checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
	}
	pending.Stage = domain.CommitStageSignalSaved
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("update pending commit checkpoint stage: %w: %w", errs.ErrStoreWrite, err)
	}

	// 10. Clear the intermediate progress state
	if err := t.store.Progress.ClearInProgress(); err != nil {
		return nil, fmt.Errorf("clear in-progress: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Signals.ClearPendingCommit(); err != nil {
		return nil, fmt.Errorf("clear pending commit: %w: %w", errs.ErrStoreWrite, err)
	}

	// Persist the violation facts: the editor review consumes them via novel_context (the return value is only a mirror,
	// since the writer hard-stops immediately after commit and nobody reads the return value). Best-effort.
	if err := t.store.World.SaveRuleViolations(a.Chapter, violations); err != nil {
		slog.Warn("saving mechanical violations failed", "module", "tools", "chapter", a.Chapter, "err", err)
	}
	t.refreshStyleStats(a.Chapter, content)
	return output, nil
}

// finishPendingCommit finishes the ProgressMarked/SignalSaved interruption window. The Checkpoint append is idempotent by
// digest; the recovery record is deleted only after both the checkpoint and the intermediate state cleanup succeed.
func (t *CommitChapterTool) finishPendingCommit(pending domain.PendingCommit, progress *domain.Progress) (json.RawMessage, error) {
	if pending.Stage == domain.CommitStageProgressMarked {
		if err := t.appendCommitCheckpoint(pending.Chapter); err != nil {
			return nil, fmt.Errorf("checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
		}
		pending.Stage = domain.CommitStageSignalSaved
		pending.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := t.store.Signals.SavePendingCommit(pending); err != nil {
			return nil, fmt.Errorf("update pending commit checkpoint stage: %w: %w", errs.ErrStoreWrite, err)
		}
	}
	if err := t.store.Progress.ClearInProgress(); err != nil {
		return nil, fmt.Errorf("clear in-progress: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Signals.ClearPendingCommit(); err != nil {
		return nil, fmt.Errorf("clear pending commit: %w: %w", errs.ErrStoreWrite, err)
	}
	t.refreshStyleStats(pending.Chapter, pending.DraftContent)
	if len(pending.Output) > 0 {
		return append(json.RawMessage(nil), pending.Output...), nil
	}
	if pending.Result != nil {
		return json.Marshal(pending.Result)
	}
	return t.buildSkipResult(pending.Chapter, progress)
}

func (t *CommitChapterTool) validateRewriteDraft(chapter int, title string, progress *domain.Progress) (string, error) {
	content, _, err := t.store.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return "", fmt.Errorf("rewrite: load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return "", fmt.Errorf("no content found for chapter %d: %w", chapter, errs.ErrToolPrecondition)
	}
	changed, err := t.rewriteChanged(chapter, content, title)
	if err != nil {
		return "", err
	}
	if changed {
		return content, nil
	}
	mode := "viết lại"
	if progress != nil && progress.Flow == domain.FlowPolishing {
		mode = "trau chuốt"
	}
	return "", fmt.Errorf("Nội dung và tiêu đề chương %d đều không thay đổi, không phát hiện thay đổi %s: %w",
		chapter, mode, errs.ErrToolPrecondition)
}

func (t *CommitChapterTool) rewriteChanged(chapter int, content, title string) (bool, error) {
	existingFinal, err := t.store.Drafts.LoadChapterText(chapter)
	if err != nil {
		return false, fmt.Errorf("rewrite: load final chapter: %w: %w", errs.ErrStoreRead, err)
	}
	if existingFinal != content {
		return true, nil
	}
	summary, err := t.store.Summaries.LoadSummary(chapter)
	if err != nil {
		return false, fmt.Errorf("rewrite: load chapter summary: %w: %w", errs.ErrStoreRead, err)
	}
	return summary == nil || strings.TrimSpace(summary.Title) != strings.TrimSpace(title), nil
}

func (t *CommitChapterTool) appendCommitCheckpoint(chapter int) error {
	_, err := t.store.Checkpoints.AppendArtifacts(
		domain.ChapterScope(chapter), "commit",
		fmt.Sprintf("chapters/%02d.md", chapter),
		fmt.Sprintf("summaries/%02d.json", chapter),
		store.ChapterRecordPath(chapter),
	)
	return err
}

// checkRules performs mechanical checks on the chapter body: the built-in product-floor Lint (mechanism residue, always run)
// + user-rules Check (reads structured from this book's snapshot; when the snapshot is missing, fall back to the built-in defaults so the mechanical floor is always there).
func (t *CommitChapterTool) checkRules(text string, chapter int) []rules.Violation {
	violations := rules.Lint(text)
	structured := rules.SystemDefaults().Structured
	if snap, err := t.store.UserRules.Load(); err == nil && snap != nil {
		structured = snap.Structured
	}
	violations = append(violations, rules.Check(text, structured)...)
	return append(violations, rules.CheckAgainstPrevious(text, t.previousChapters(chapter))...)
}

// previousChapters reads the final texts of the immediately preceding chapters, for the "does the new chapter copy the previous one" comparison.
// A failed or empty single-chapter read is always skipped: this is a supplementary metric and must not drag down the main commit path.
func (t *CommitChapterTool) previousChapters(chapter int) []string {
	const lookback = 2
	var out []string
	for ch := chapter - 1; ch >= 1 && ch >= chapter-lookback; ch-- {
		text, err := t.store.Drafts.LoadChapterText(ch)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, text)
	}
	return out
}

// executeRewriteCommit handles the commit of a polished/rewritten chapter: overwrite the final text and summary, update the word count, drain the queue.
// It skips all world-state appends (timeline / foreshadow / relationship / state_changes) and arc boundary detection,
// which were already applied at the chapter's original commit.
func (t *CommitChapterTool) executeRewriteCommit(a commitArgs, progress *domain.Progress, pending domain.PendingCommit, recovering bool) (json.RawMessage, error) {
	chapter := a.Chapter
	// 1. Use only the rewrite body frozen at the first commit; crash recovery must not adopt a draft that was overwritten afterwards.
	content := pending.DraftContent
	if content == "" {
		return nil, fmt.Errorf("Bản nộp viết lại của chương %d thiếu draft_content, không thể khôi phục an toàn: %w", chapter, errs.ErrToolConflict)
	}
	content = domain.ApplyChapterHeading(content, a.Title, chapter)
	wordCount := utf8.RuneCountInString(content)

	// 2. At least one of body or title changed; polishing only the title does not require faking a body change.
	if !recovering {
		changed, err := t.rewriteChanged(chapter, content, a.Title)
		if err != nil {
			return nil, err
		}
		if !changed {
			mode := "viết lại"
			if progress != nil && progress.Flow == domain.FlowPolishing {
				mode = "trau chuốt"
			}
			return nil, fmt.Errorf("Nội dung và tiêu đề chương %d đều không thay đổi, không phát hiện thay đổi %s: %w",
				chapter, mode, errs.ErrToolPrecondition)
		}
	}

	if pending.Stage == domain.CommitStageStarted {
		// 3. First build the complete candidate record set and validate it by replay. The old implementation overwrote the records first and then rebuilt the projection;
		// once the fact chain failed to close, the failing payload stayed on disk and later retries always read a bad baseline.
		existing, err := t.store.ChapterRecords.Load(chapter)
		if err != nil {
			return nil, fmt.Errorf("rewrite: load chapter record: %w: %w", errs.ErrStoreRead, err)
		}
		var existingUpdates []domain.ForeshadowUpdate
		var style domain.StyleDelta
		if existing != nil {
			existingUpdates = existing.Facts.ForeshadowUpdates
			style = existing.StyleDelta
		}
		recovered, err := t.restoreRewritePlants(chapter, existingUpdates, &a.ChapterFacts)
		if err != nil {
			return nil, err
		}
		candidate, err := t.store.ChapterRecords.Prepare(
			chapter, domain.ChapterOriginGenerated, content, a.ChapterFacts, style,
		)
		if err != nil {
			return nil, fmt.Errorf("rewrite: prepare chapter record: %w: %w", errs.ErrStoreRead, err)
		}
		chapters := slices.Clone(progress.CompletedChapters)
		slices.Sort(chapters)
		records := make([]domain.ChapterRecord, 0, len(chapters))
		for _, completedChapter := range chapters {
			if completedChapter == chapter {
				records = append(records, *candidate)
				continue
			}
			record, err := t.store.ChapterRecords.Load(completedChapter)
			if err != nil {
				return nil, fmt.Errorf("rewrite: load chapter record %d: %w: %w", completedChapter, errs.ErrStoreRead, err)
			}
			if record == nil {
				return nil, fmt.Errorf("rewrite: chương %d thiếu bản ghi tiếp nhận: %w", completedChapter, errs.ErrToolConflict)
			}
			records = append(records, *record)
		}
		if err := revision.ValidateRecords(records); err != nil {
			if clearErr := t.store.Signals.ClearPendingCommit(); clearErr != nil {
				return nil, fmt.Errorf("rewrite: kiểm tra chuỗi sự thật của chương thất bại (%v), và dọn bản nộp đang đóng băng cũng thất bại: %w: %w", err, errs.ErrStoreWrite, clearErr)
			}
			return nil, fmt.Errorf("rewrite: kiểm tra chuỗi sự thật của chương thất bại, đã gỡ đóng băng và chưa ghi kết quả viết lại: %w: %w", errs.ErrToolPrecondition, err)
		}

		// 4. Overwrite the authoritative records and final text only after validation passes; the same frozen payload can be replayed safely.
		if err := t.store.Drafts.SaveFinalChapter(chapter, content); err != nil {
			return nil, fmt.Errorf("rewrite: save final chapter: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.ChapterRecords.Save(*candidate); err != nil {
			return nil, fmt.Errorf("rewrite: save chapter record: %w: %w", errs.ErrStoreWrite, err)
		}
		if len(recovered) > 0 {
			slog.Warn("recovered foreshadow plant events lost in an older version from the foreshadow ledger", "module", "commit", "chapter", chapter, "foreshadows", recovered)
		}
		if err := revision.NewProjector(t.store).Apply(records); err != nil {
			return nil, fmt.Errorf("rewrite: rebuild chapter projections: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Summaries.SaveSummary(domain.ChapterSummary{
			Chapter: chapter, Title: a.Title, Summary: a.Summary, Characters: a.Characters, KeyEvents: a.KeyEvents,
		}); err != nil {
			return nil, fmt.Errorf("rewrite: save summary: %w: %w", errs.ErrStoreWrite, err)
		}
		pending.Stage = domain.CommitStageStateApplied
		pending.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := t.store.Signals.SavePendingCommit(pending); err != nil {
			return nil, fmt.Errorf("rewrite: update pending state stage: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 5. Update the word count (MarkChapterComplete is idempotent for a completed chapter: it replaces the word count, and slice.Contains prevents duplicate enqueueing)
	if progress.Phase != domain.PhaseComplete {
		if err := t.store.Progress.MarkChapterComplete(chapter, wordCount, a.HookType, a.DominantStrand); err != nil {
			return nil, fmt.Errorf("rewrite: update word count: %w: %w", errs.ErrStoreWrite, err)
		}

		// 6. Drain the pending queue; when the queue is empty, CompleteRewrite automatically switches flow back to writing
		if err := t.store.Progress.CompleteRewrite(chapter); err != nil {
			return nil, fmt.Errorf("rewrite: complete rewrite: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 7. Read the Progress snapshot after the drain and return it as fact
	mode := pending.RewriteMode
	if mode == "" {
		mode = "rewrite"
	}
	latest, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("rewrite: load progress after drain: %w: %w", errs.ErrStoreRead, err)
	}
	remaining := []int{}
	nextChapter := chapter + 1
	flow := string(domain.FlowWriting)
	if latest != nil {
		remaining = append(remaining, latest.PendingRewrites...)
		nextChapter = latest.NextChapter()
		flow = string(latest.Flow)
	}
	drained := len(remaining) == 0

	// Judge completion after the queue is cleared: a rewrite commit does not go through the main path's applyCompletion, so completion can only be triggered here.
	//   - Layered + forward writing: layeredComplete is the overall judgment (finale volume structurally written / quality level when no finale is declared).
	//   - Layered + reopen rework (ReopenedFromComplete): the rework only changes existing chapters and neither adds nor removes structure, so complete again by
	//     structural completeness; if a rework disturbance to some thread left it stuck in writing, the end of the final volume would fall into an out-of-range continuation dead loop.
	//   - Non-layered: complete once TotalChapters are written (rework does not add or remove chapters, so it was already full).
	bookComplete := false
	if drained && latest != nil {
		reComplete := false
		switch {
		case latest.Layered && latest.ReopenedFromComplete:
			reComplete, err = layeredStructurallyComplete(t.store, latest)
		case latest.Layered:
			reComplete, err = layeredComplete(t.store, latest)
		default:
			reComplete = latest.TotalChapters > 0 && len(latest.CompletedChapters) >= latest.TotalChapters
		}
		if err != nil {
			return nil, fmt.Errorf("rewrite: evaluate completion: %w: %w", errs.ErrStoreRead, err)
		}
		if reComplete {
			if err := t.store.Progress.MarkComplete(); err != nil {
				return nil, fmt.Errorf("rewrite: mark complete: %w: %w", errs.ErrStoreWrite, err)
			}
			bookComplete = true
			p, err := t.store.Progress.Load()
			if err != nil {
				return nil, fmt.Errorf("rewrite: reload completed progress: %w: %w", errs.ErrStoreRead, err)
			}
			if p != nil {
				flow = string(p.Flow)
			}
		}
	}

	// Same as the main path: rewrite/polish also run the mechanical checks and persist them (after a rewrite a new record lands, and old violations are treated as cleared)
	violations := t.checkRules(content, a.Chapter)
	output, err := json.Marshal(map[string]any{
		"chapter": chapter, "rewritten": true, "mode": mode, "word_count": wordCount,
		"remaining_queue": remaining, "queue_drained": drained, "next_chapter": nextChapter,
		"flow": flow, "book_complete": bookComplete, "rule_violations": violations,
	})
	if err != nil {
		return nil, fmt.Errorf("rewrite: marshal output: %w", err)
	}
	pending.Stage = domain.CommitStageProgressMarked
	pending.Output = output
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("rewrite: update pending progress stage: %w: %w", errs.ErrStoreWrite, err)
	}

	// 8. Mark signal_saved after the Checkpoint, and finally clean up PendingCommit.
	if err := t.appendCommitCheckpoint(chapter); err != nil {
		return nil, fmt.Errorf("rewrite: checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
	}
	pending.Stage = domain.CommitStageSignalSaved
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("rewrite: update pending checkpoint stage: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Progress.ClearInProgress(); err != nil {
		return nil, fmt.Errorf("rewrite: clear in-progress: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Signals.ClearPendingCommit(); err != nil {
		return nil, fmt.Errorf("rewrite: clear pending commit: %w: %w", errs.ErrStoreWrite, err)
	}

	if err := t.store.World.SaveRuleViolations(chapter, violations); err != nil {
		slog.Warn("saving mechanical violations failed", "module", "tools", "chapter", chapter, "err", err)
	}
	t.refreshStyleStats(chapter, content)
	return output, nil
}

// restoreRewritePlants repairs only the single form of damage already caused by older versions: the foreshadow ledger still records this chapter's
// plant, but this chapter's acceptance record was overwritten by a failed rewrite. The ledger gives the full id, description and planting chapter,
// so it can be restored deterministically; other inconsistencies keep being reported explicitly, and no plot facts are guessed.
func (t *CommitChapterTool) restoreRewritePlants(chapter int, existing []domain.ForeshadowUpdate, facts *domain.ChapterFacts) ([]string, error) {
	planted := make(map[string]struct{}, len(existing)+len(facts.ForeshadowUpdates))
	for _, update := range existing {
		if update.Action == "plant" {
			planted[update.ID] = struct{}{}
		}
	}
	for _, update := range facts.ForeshadowUpdates {
		if update.Action == "plant" {
			planted[update.ID] = struct{}{}
		}
	}

	ledger, err := t.store.World.LoadForeshadowLedger()
	if err != nil {
		return nil, fmt.Errorf("rewrite: load foreshadow ledger for recovery: %w: %w", errs.ErrStoreRead, err)
	}
	var restored []domain.ForeshadowUpdate
	var ids []string
	for _, entry := range ledger {
		if entry.PlantedAt != chapter {
			continue
		}
		if _, ok := planted[entry.ID]; ok {
			continue
		}
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.Description) == "" {
			return nil, fmt.Errorf("rewrite: sổ chi tiết gài trước của chương %d thiếu id hoặc description để khôi phục: %w", chapter, errs.ErrToolConflict)
		}
		planted[entry.ID] = struct{}{}
		restored = append(restored, domain.ForeshadowUpdate{
			ID: entry.ID, Action: "plant", Description: entry.Description,
		})
		ids = append(ids, entry.ID)
	}
	if len(restored) > 0 {
		facts.ForeshadowUpdates = append(restored, facts.ForeshadowUpdates...)
	}
	return ids, nil
}

func (t *CommitChapterTool) refreshStyleStats(chapter int, content string) {
	if content == "" {
		var err error
		content, err = t.store.Drafts.LoadChapterText(chapter)
		if err != nil {
			slog.Error("updating style stats index failed", "module", "tools", "chapter", chapter, "err", err)
			return
		}
		if content == "" {
			slog.Error("updating style stats index failed", "module", "tools", "chapter", chapter, "err", errors.New("Chưa có bản chính thức"))
			return
		}
	}
	t.styleStats.ChapterCommitted(chapter, content)
}

// buildSkipResult builds, for a "duplicate commit of an already completed chapter", a fact return aligned with a normal commit.
// The coordinator makes follow-up decisions from it (writer/editor/architect dispatch) instead of hallucinating from a prose hint.
func (t *CommitChapterTool) buildSkipResult(chapter int, progress *domain.Progress) (json.RawMessage, error) {
	_, wordCount, err := t.store.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return nil, fmt.Errorf("load completed chapter: %w: %w", errs.ErrStoreRead, err)
	}

	result := domain.CommitResult{
		Chapter:     chapter,
		Committed:   true,
		WordCount:   wordCount,
		NextChapter: chapter + 1,
	}

	if progress != nil && progress.Layered {
		boundary, err := t.store.Outline.CheckArcBoundary(chapter)
		if err != nil {
			return nil, fmt.Errorf("check completed chapter boundary: %w: %w", errs.ErrStoreRead, err)
		}
		if boundary != nil {
			result.ArcEnd = boundary.IsArcEnd
			result.VolumeEnd = boundary.IsVolumeEnd
			result.Volume = boundary.Volume
			result.Arc = boundary.Arc
			result.NeedsExpansion = boundary.NeedsExpansion
			result.NeedsNewVolume = boundary.NeedsNewVolume
			result.NextVolume = boundary.NextVolume
			result.NextArc = boundary.NextArc
		}
		result.ReviewRequired, result.ReviewReason = domain.ShouldArcReview(result.ArcEnd, result.VolumeEnd, result.Volume, result.Arc)
	} else if progress != nil {
		result.ReviewRequired, result.ReviewReason = domain.ShouldReview(len(progress.CompletedChapters))
	}

	if progress != nil {
		if progress.Phase == domain.PhaseComplete {
			result.BookComplete = true
		}
		result.Flow = string(progress.Flow)
	}

	return json.Marshal(result)
}

// loadCoreCharacterNameSet loads the set of character names (including aliases) already in characters.json.
// Used as the "known core" filter set for cast_ledger: core characters do not go into the supporting-cast roster.
// On load failure it returns nil (on merge all characters go into the ledger, which is acceptable).
func loadCoreCharacterNameSet(s *store.Store) (map[string]bool, error) {
	chars, err := s.Characters.Load()
	if err != nil {
		return nil, err
	}
	if len(chars) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(chars)*2)
	for _, c := range chars {
		if c.Name != "" {
			set[c.Name] = true
		}
		for _, alias := range c.Aliases {
			if alias != "" {
				set[alias] = true
			}
		}
	}
	return set, nil
}

// applyCompletion decides whether this commit completes the whole book; if so it calls MarkComplete and returns true.
//   - Non-layered: complete once the agreed total chapter count is written.
//   - Layered: the architect explicitly calling save_foundation type=complete_book is the main path; this adds a
//     deterministic fallback (see layeredComplete), preventing the model from neither append_volume nor
//     complete_book at the end point, which causes a livelock of "writer runs bare past the range → out-of-range guard intercepts → retries repeatedly"
//     (root cause of the Mortal Bone ch204..347 case).
func (t *CommitChapterTool) applyCompletion(result *domain.CommitResult, progress *domain.Progress) (bool, error) {
	if progress == nil {
		return false, nil
	}
	if progress.Phase == domain.PhaseComplete {
		return true, nil
	}
	if progress.Layered {
		complete, err := layeredComplete(t.store, progress)
		if err != nil {
			return false, fmt.Errorf("evaluate layered completion: %w: %w", errs.ErrStoreRead, err)
		}
		if complete {
			if err := t.store.Progress.MarkComplete(); err != nil {
				return false, fmt.Errorf("mark book complete: %w: %w", errs.ErrStoreWrite, err)
			}
			return true, nil
		}
		return false, nil
	}
	if progress.TotalChapters > 0 && result.NextChapter > progress.TotalChapters {
		if err := t.store.Progress.MarkComplete(); err != nil {
			return false, fmt.Errorf("mark book complete: %w: %w", errs.ErrStoreWrite, err)
		}
		return true, nil
	}
	return false, nil
}

// ── Layered completion judgment (package level: shared by the two trigger points commit_chapter and save_volume_summary) ──
//
// The completion check always happens in the tool where "the last fact lands":
//   - Finale not declared: the commit of the last chapter (layeredBookComplete, quality level)
//   - Finale declared: the last piece of the forward main path is the end-of-volume trio (review → arc summary → volume summary),
//     so the trigger point is in save_volume_summary; when the trio is already complete after a rewrite drain, commit triggers it.

// layeredStructurallyComplete decides whether a layered long-form book is "structurally fully written": rewrite queue empty + no skeleton arcs awaiting expansion
// + all expanded chapters written. This is a deterministic terminal-state fact with no semantic judgments such as foreshadowing/long threads; it serves as the safety net for
// "preventing terminal-state dead loops" (used to complete again after a rewrite drain).
func layeredStructurallyComplete(st *store.Store, progress *domain.Progress) (bool, error) {
	// 1. The rewrite queue must be empty
	if len(progress.PendingRewrites) > 0 {
		return false, nil
	}
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		return false, fmt.Errorf("load layered outline: %w", err)
	}
	if len(volumes) == 0 {
		return false, nil
	}
	// 2. No skeleton arcs may remain to be expanded (there is still content in the plan to write)
	for i := range volumes {
		for j := range volumes[i].Arcs {
			if !volumes[i].Arcs[j].IsExpanded() {
				return false, nil
			}
		}
	}
	// 3. All expanded chapters must be written
	expanded := len(domain.FlattenOutline(volumes))
	return expanded > 0 && len(progress.CompletedChapters) >= expanded, nil
}

// finaleWrapped reports whether the end-of-volume trio of the finale volume (arc review / arc summary / volume summary) is complete.
// Finale completion does not require foreshadowing/long threads to reach zero, but it must wait for the last arc to pass the editor quality gate: the ending is the most important part of the whole book,
// and completion must not get ahead of the editor review (which may enqueue a rewrite) and the summary landing on disk.
func finaleWrapped(st *store.Store, progress *domain.Progress) (bool, error) {
	last := progress.LatestCompleted()
	if last <= 0 {
		return false, nil
	}
	b, err := st.Outline.CheckArcBoundary(last)
	if err != nil {
		return false, fmt.Errorf("check finale boundary: %w", err)
	}
	if b == nil || !b.IsArcEnd {
		return false, nil
	}
	hasReview, err := st.World.HasArcReview(last)
	if err != nil {
		return false, fmt.Errorf("load finale review: %w", err)
	}
	hasArcSummary, err := st.Summaries.HasArcSummary(b.Volume, b.Arc)
	if err != nil {
		return false, fmt.Errorf("load finale arc summary: %w", err)
	}
	hasVolumeSummary, err := st.Summaries.HasVolumeSummary(b.Volume)
	if err != nil {
		return false, fmt.Errorf("load finale volume summary: %w", err)
	}
	return hasReview && hasArcSummary && hasVolumeSummary, nil
}

// layeredComplete is the overall completion judgment for layered forward writing:
//   - Finale volume declared (the last volume of layered_outline carries final) → structurally written + end-of-volume trio complete
//     means complete, with no further requirement that foreshadowing/long threads reach zero. The whole finale volume aims at tying up threads (the architect already distributed the long threads/
//     foreshadowing across the arcs when planning), and an occasional omission is an editor quality issue that should not trap the whole book outside the terminal state; otherwise
//     a book with an overestimated estimated_scale could never be completed legitimately (the root-cause side of the 140-chapter stop guard circuit-breaker case).
//   - Not declared → quality-level layeredBookComplete, preventing the model, which neither declares a finale nor completes the book, from
//     wrapping up too early where the outline runs out.
func layeredComplete(st *store.Store, progress *domain.Progress) (bool, error) {
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		return false, fmt.Errorf("load layered outline: %w", err)
	}
	if domain.FinaleVolume(volumes) > 0 {
		structurallyComplete, err := layeredStructurallyComplete(st, progress)
		if err != nil || !structurallyComplete {
			return structurallyComplete, err
		}
		return finaleWrapped(st, progress)
	}
	return layeredBookComplete(st, progress)
}

// ReconcileLayeredCompletion fills in the completion state of a layered book from the current persisted facts.
// The normal save_volume_summary path and Engine crash recovery share this entry point, avoiding the permanent loss of the auto-completion trigger when the volume summary has landed but
// Progress has not yet had a chance to MarkComplete.
func ReconcileLayeredCompletion(st *store.Store) (bool, error) {
	progress, err := st.Progress.Load()
	if err != nil {
		return false, fmt.Errorf("load progress: %w", err)
	}
	if progress == nil || !progress.Layered {
		return false, nil
	}
	if progress.Phase == domain.PhaseComplete {
		return true, nil
	}
	if progress.Phase != domain.PhaseWriting {
		return false, nil
	}
	complete, err := layeredComplete(st, progress)
	if err != nil || !complete {
		return complete, err
	}
	if err := st.Progress.MarkComplete(); err != nil {
		return false, fmt.Errorf("mark complete: %w", err)
	}
	return true, nil
}

// layeredBookComplete uses objective facts to decide whether a layered long-form book is truly fully written, checking against the quantifiable items in the architect-long.md completion
// checklist plus structural facts. On top of structural completeness it further requires foreshadowing to reach zero and long threads to be tied up; failing any of them
// yields to the architect continuing expand_arc / append_volume, and it never wraps up before the story is finished. With no compass it conservatively
// judges the book as not complete. This is the "quality level" completion judgment when no finale volume is declared, stricter than layeredStructurallyComplete.
func layeredBookComplete(st *store.Store, progress *domain.Progress) (bool, error) {
	structurallyComplete, err := layeredStructurallyComplete(st, progress)
	if err != nil || !structurallyComplete {
		return structurallyComplete, err
	}
	// 4. Active foreshadowing must reach zero (promises fulfilled)
	active, err := st.World.LoadActiveForeshadow()
	if err != nil {
		return false, fmt.Errorf("load active foreshadow: %w", err)
	}
	if len(active) > 0 {
		return false, nil
	}
	// 5. Active long threads in the compass must be tied up (with no compass, or with unresolved long threads, hand it back to the architect to decide)
	compass, err := st.Outline.LoadCompass()
	if err != nil {
		return false, fmt.Errorf("load compass: %w", err)
	}
	if compass == nil || len(compass.OpenThreads) > 0 {
		return false, nil
	}
	return true, nil
}
