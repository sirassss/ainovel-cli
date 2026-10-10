package imp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ChapterCommitter is the minimal interface needed to publish chapters, satisfied by tools.CommitChapterTool.
// It reuses the PendingCommit saga, checkpoint and completed-chapter idempotency check, without duplicating a second commit logic (RFC §12.3).
type ChapterCommitter interface {
	Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// publishFoundation publishes the Foundation in the formal dependency order, consistent with the Architect's long-form persistence order (RFC §12.2).
// Publishing identical content again is idempotent (Store overwrite of identical content + checkpoint dedup).
func publishFoundation(st *store.Store, f *Foundation) error {
	// Pre-publish conflict reconciliation: an existing formal artifact that differs is refused overwrite (§12.2 / invariant 6).
	// Identical content continues to be written idempotently (Store overwrite of identical content + checkpoint dedup).
	if err := checkFoundationConflicts(st, f); err != nil {
		return err
	}
	if err := st.Book.Save(f.Book); err != nil {
		return fmt.Errorf("book: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "book", "meta/book.json"); err != nil {
		return fmt.Errorf("checkpoint book: %w", err)
	}
	if err := st.RunMeta.SetPlanningTier(f.PlanningTier); err != nil {
		return fmt.Errorf("planning tier: %w", err)
	}
	// premise
	if err := st.Outline.SavePremise(f.Premise); err != nil {
		return fmt.Errorf("premise: %w", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhasePremise); err != nil {
		return fmt.Errorf("phase premise: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "premise", "premise.md"); err != nil {
		return fmt.Errorf("checkpoint premise: %w", err)
	}
	// characters
	if err := st.Characters.Save(f.Characters); err != nil {
		return fmt.Errorf("characters: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "characters", "characters.json"); err != nil {
		return fmt.Errorf("checkpoint characters: %w", err)
	}
	// world rules
	if err := st.World.SaveWorldRules(f.WorldRules); err != nil {
		return fmt.Errorf("world_rules: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "world_rules", "world_rules.json"); err != nil {
		return fmt.Errorf("checkpoint world_rules: %w", err)
	}
	// The layered outline is the only source; the Store rebuilds the flat outline in sync.
	if err := st.Outline.SaveLayeredOutline(f.Volumes); err != nil {
		return fmt.Errorf("layered outline: %w", err)
	}
	// Outline-stage progress is what the engine uses to recompute routing (chapter capacity / layering / current volume and arc); a failed write would leave an inconsistent
	// published state, which must be surfaced rather than swallowed (RFC §12.2).
	if err := st.Progress.UpdatePhase(domain.PhaseOutline); err != nil {
		return fmt.Errorf("phase outline: %w", err)
	}
	if err := st.Progress.SetTotalChapters(domain.EstimatedChapterCapacity(f.Volumes)); err != nil {
		return fmt.Errorf("total chapters: %w", err)
	}
	if err := st.Progress.SetLayered(true); err != nil {
		return fmt.Errorf("set layered: %w", err)
	}
	if len(f.Volumes) > 0 && len(f.Volumes[0].Arcs) > 0 {
		if err := st.Progress.UpdateVolumeArc(f.Volumes[0].Index, f.Volumes[0].Arcs[0].Index); err != nil {
			return fmt.Errorf("volume arc: %w", err)
		}
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "layered_outline", "layered_outline.json"); err != nil {
		return fmt.Errorf("checkpoint layered outline: %w", err)
	}
	// compass
	if err := st.Outline.SaveCompass(f.Compass); err != nil {
		return fmt.Errorf("compass: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "compass", "meta/compass.json"); err != nil {
		return fmt.Errorf("checkpoint compass: %w", err)
	}
	// All formal writes of the imported Foundation succeeded, so it may explicitly enter writing.
	// FoundationMissing of the ordinary creation flow cannot be reused: an import allows world_rules to be empty,
	// and treating a "legitimately empty value" as missing would leave progress stuck at outline forever, after which StartChapter is rejected by the phase gate.
	p, err := st.Progress.Load()
	if err != nil {
		return fmt.Errorf("load progress: %w", err)
	}
	if p == nil {
		return fmt.Errorf("load progress: progress chưa được khởi tạo")
	}
	if p.Phase != domain.PhaseWriting && p.Phase != domain.PhaseComplete {
		if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
			return fmt.Errorf("phase writing: %w", err)
		}
	}
	return nil
}

// checkFoundationConflicts verifies the Foundation to be published against existing formal artifacts for consistency:
// existing empty counts as a first publish; identical counts as idempotent; different reports a conflict and does not overwrite (RFC §12.2 / invariant 6).
// The compass and the flat outline are derived from the layered outline; if the layering is consistent the derivation is consistent, so derived artifacts are not checked separately.
// A read error must not be swallowed into "file does not exist": the store loader returns (zero value, nil) for a missing file, so any non-nil is a real error
// (corrupt / permission / invalid JSON); continuing as if it were empty would overwrite a formal artifact that cannot be read (RFC §12.2).
func checkFoundationConflicts(st *store.Store, f *Foundation) error {
	wantBook := f.Book.Normalized()
	book, err := st.Book.Load()
	if err != nil {
		return fmt.Errorf("Đọc book chính thức: %w", err)
	}
	if book != nil && !jsonEqual(book, wantBook) {
		return fmt.Errorf("book chính thức xung đột với bản tổng hợp khi nhập (đã tồn tại phiên bản khác), từ chối ghi đè")
	}
	cur, err := st.Outline.LoadPremise()
	if err != nil {
		return fmt.Errorf("Đọc premise chính thức: %w", err)
	}
	if cur != "" && cur != f.Premise {
		return fmt.Errorf("premise chính thức xung đột với bản tổng hợp khi nhập (đã tồn tại phiên bản khác), từ chối ghi đè")
	}
	chars, err := st.Characters.Load()
	if err != nil {
		return fmt.Errorf("Đọc characters chính thức: %w", err)
	}
	if len(chars) > 0 && !jsonEqual(chars, f.Characters) {
		return fmt.Errorf("characters chính thức xung đột với bản tổng hợp khi nhập (đã tồn tại phiên bản khác), từ chối ghi đè")
	}
	rules, err := st.World.LoadWorldRules()
	if err != nil {
		return fmt.Errorf("Đọc world_rules chính thức: %w", err)
	}
	if len(rules) > 0 && !jsonEqual(rules, f.WorldRules) {
		return fmt.Errorf("world_rules chính thức xung đột với bản tổng hợp khi nhập (đã tồn tại phiên bản khác), từ chối ghi đè")
	}
	layered, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		return fmt.Errorf("Đọc layered_outline chính thức: %w", err)
	}
	if len(layered) > 0 && !jsonEqual(layered, f.Volumes) {
		return fmt.Errorf("layered_outline chính thức xung đột với bản tổng hợp khi nhập (đã tồn tại phiên bản khác), từ chối ghi đè")
	}
	return nil
}

// jsonEqual compares two values for equivalence by their normalized JSON bytes.
func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

// publishChapter reuses commit_chapter to publish a single chapter; completed chapters are skipped by its idempotency check (RFC §12.3).
func publishChapter(ctx context.Context, st *store.Store, commit ChapterCommitter, chapter int, content string, f ImportedChapterFacts) error {
	completed, err := st.Progress.IsChapterCompleted(chapter)
	if err != nil {
		return fmt.Errorf("load progress ch%d: %w", chapter, err)
	}
	if completed {
		// A crash may land between MarkChapterComplete and ClearPendingCommit: a leftover pending_commit
		// points at this chapter. Skipping directly would bypass the cleanup branch the commit tool prepared for exactly this window (backfill checkpoint + clear the leftover),
		// and the next chapter's Execute would reject with "an unrecovered chapter commit exists"; every rerun of the import would die at the same place and
		// meta/pending_commit.json would have to be deleted by hand to unblock it. When a leftover is hit, still take the tool's idempotent path to finish the cleanup.
		pending, err := st.Signals.LoadPendingCommit()
		if err != nil {
			return fmt.Errorf("load pending commit ch%d: %w", chapter, err)
		}
		if pending != nil && pending.Chapter == chapter {
			raw, err := json.Marshal(commitArgs(chapter, f))
			if err != nil {
				return fmt.Errorf("marshal commit ch%d: %w", chapter, err)
			}
			if _, err := commit.Execute(ctx, raw); err != nil {
				return fmt.Errorf("commit ch%d: %w", chapter, err)
			}
		}
		return nil
	}
	if err := st.Drafts.SaveDraft(chapter, content); err != nil {
		return fmt.Errorf("save draft ch%d: %w", chapter, err)
	}
	if err := st.Progress.StartChapter(chapter); err != nil {
		return fmt.Errorf("start ch%d: %w", chapter, err)
	}
	raw, err := json.Marshal(commitArgs(chapter, f))
	if err != nil {
		return fmt.Errorf("marshal commit ch%d: %w", chapter, err)
	}
	if _, err := commit.Execute(ctx, raw); err != nil {
		return fmt.Errorf("commit ch%d: %w", chapter, err)
	}
	return nil
}

// commitArgs maps per-chapter facts to commit_chapter arguments.
func commitArgs(chapter int, f ImportedChapterFacts) map[string]any {
	keyEvents := f.KeyEvents
	if len(keyEvents) == 0 {
		keyEvents = []string{f.CoreEvent} // core_event already validated non-empty
	}
	args := map[string]any{
		"chapter":         chapter,
		"title":           f.Title,
		"summary":         f.Summary,
		"characters":      f.Characters,
		"key_events":      keyEvents,
		"hook_type":       f.HookType,
		"dominant_strand": f.DominantStrand,
	}
	if len(f.TimelineEvents) > 0 {
		args["timeline_events"] = f.TimelineEvents
	}
	if len(f.ForeshadowUpdates) > 0 {
		args["foreshadow_updates"] = f.ForeshadowUpdates
	}
	if len(f.RelationshipChanges) > 0 {
		args["relationship_changes"] = f.RelationshipChanges
	}
	if len(f.StateChanges) > 0 {
		args["state_changes"] = f.StateChanges
	}
	return args
}

// isPublished decides whether the formal state already reflects a complete import: the Foundation is persisted and the completed chapters reach the expectation.
// It reconciles only the artifacts the import actually produces - book, premise, a flat outline covering all chapters, completed chapters - and does not reuse
// FoundationMissing(): the latter is the ordinary creation flow's "ready to write" gate, which would
// misjudge a legitimately empty world_rules as incomplete, so publish reconciliation would never converge (RFC §12.3).
func isPublished(st *store.Store, expected int) (bool, error) {
	if expected == 0 {
		return false, nil
	}
	book, err := st.Book.Load()
	if err != nil {
		return false, fmt.Errorf("Đọc book chính thức: %w", err)
	}
	if book == nil {
		return false, nil
	}
	p, err := st.Outline.LoadPremise()
	if err != nil {
		return false, fmt.Errorf("Đọc premise chính thức: %w", err)
	}
	if p == "" {
		return false, nil
	}
	o, err := st.Outline.LoadOutline()
	if err != nil {
		return false, fmt.Errorf("Đọc outline chính thức: %w", err)
	}
	if len(o) < expected {
		return false, nil
	}
	prog, err := st.Progress.Load()
	if err != nil {
		return false, fmt.Errorf("Đọc progress chính thức: %w", err)
	}
	return prog != nil && len(prog.CompletedChapters) >= expected, nil
}
