package flow

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// LoadState reads from the Store all the facts that Route needs.
// This is the routing "IO boundary": all reads are concentrated here so that Route stays pure.
// Any read failure returns an error; a corrupted artifact and "not generated yet" are two different facts, and the Router must not
// keep dispatching on an incomplete snapshot.
func LoadState(store *storepkg.Store) (State, error) {
	var s State
	missing, err := store.FoundationMissing()
	if err != nil {
		return s, fmt.Errorf("load foundation state: %w", err)
	}
	s.FoundationMissing = missing
	// Planning tier: save_foundation writes it into RunMeta when it persists scale; the completion branch derives the planner from it.
	// A read failure is treated as unknown (empty tier -> completion is left to the LLM to adjudicate), consistent with the conservative defaults of the other facts.
	meta, err := store.RunMeta.Load()
	if err != nil {
		return s, fmt.Errorf("load run meta: %w", err)
	}
	if meta != nil {
		s.PlanningTier = meta.PlanningTier
	}
	progress, err := store.Progress.Load()
	if err != nil {
		return s, fmt.Errorf("load progress: %w", err)
	}
	if progress == nil {
		return s, nil
	}
	s.Progress = progress
	feedback, err := store.Outline.LoadPendingOutlineFeedback()
	if err != nil {
		return s, fmt.Errorf("load outline feedback: %w", err)
	}
	for _, item := range feedback {
		if item.RequiresImmediateReview() {
			s.ImmediateFeedbackCount++
		}
	}

	s.LastCompleted = progress.LatestCompleted()

	// If the head of the rewrite queue still has no chapter_contract, let the planner add one first and then dispatch the writer.
	// A read failure is treated as "instruction already present": this is a guidance branch and must not let one failed disk read stall the rewrite.
	if len(progress.PendingRewrites) > 0 {
		head := progress.PendingRewrites[0]
		plan, err := store.Drafts.LoadChapterPlan(head)
		if err == nil {
			s.RewriteHeadNeedsDirective = plan == nil || !plan.HasDirective()
		}
	}

	// The arc boundary is computed only in layered mode when there are completed chapters
	if progress.Layered && s.LastCompleted > 0 {
		boundaries, err := store.Outline.CompletedArcBoundaries(s.LastCompleted)
		if err != nil {
			return s, fmt.Errorf("load completed arc boundaries: %w", err)
		}
		for i := range boundaries {
			boundary := &boundaries[i]
			hasReview, err := store.World.HasArcReview(boundary.EndChapter)
			if err != nil {
				return s, fmt.Errorf("load arc review: %w", err)
			}
			if !hasReview {
				s.AggregateRefresh = aggregateRefresh(AggregateArcReview, boundary)
				break
			}
			hasArcSummary, err := store.Summaries.HasArcSummary(boundary.Volume, boundary.Arc)
			if err != nil {
				return s, fmt.Errorf("load arc summary: %w", err)
			}
			if !hasArcSummary {
				s.AggregateRefresh = aggregateRefresh(AggregateArcSummary, boundary)
				break
			}
			if boundary.IsVolumeEnd {
				hasVolumeSummary, err := store.Summaries.HasVolumeSummary(boundary.Volume)
				if err != nil {
					return s, fmt.Errorf("load volume summary: %w", err)
				}
				if !hasVolumeSummary {
					s.AggregateRefresh = aggregateRefresh(AggregateVolumeSummary, boundary)
					break
				}
			}
		}

		boundary, err := store.Outline.CheckArcBoundary(s.LastCompleted)
		if err != nil {
			return s, fmt.Errorf("check arc boundary: %w", err)
		}
		if boundary != nil {
			s.ArcBoundary = boundary
			if boundary.IsArcEnd {
				s.HasArcReview, err = store.World.HasArcReview(s.LastCompleted)
				if err != nil {
					return s, fmt.Errorf("load arc review: %w", err)
				}
				s.HasArcSummary, err = store.Summaries.HasArcSummary(boundary.Volume, boundary.Arc)
				if err != nil {
					return s, fmt.Errorf("load arc summary: %w", err)
				}
				if boundary.IsVolumeEnd {
					s.HasVolumeSummary, err = store.Summaries.HasVolumeSummary(boundary.Volume)
					if err != nil {
						return s, fmt.Errorf("load volume summary: %w", err)
					}
				}
			}
		}
	}

	// Non-layered global review fact: read from disk only at the trigger point (in other combinations Route does not consume this field).
	if !progress.Layered && s.LastCompleted > 0 {
		for completed := domain.ReviewInterval; completed <= len(progress.CompletedChapters); completed += domain.ReviewInterval {
			chapter := progress.CompletedChapters[completed-1]
			hasReview, err := store.World.HasGlobalReview(chapter)
			if err != nil {
				return s, fmt.Errorf("load global review: %w", err)
			}
			if !hasReview {
				s.AggregateRefresh = &AggregateRefresh{Kind: AggregateGlobalReview, EndChapter: chapter}
				break
			}
		}
		if due, _ := domain.ShouldReview(len(progress.CompletedChapters)); due {
			s.HasGlobalReview, err = store.World.HasGlobalReview(s.LastCompleted)
			if err != nil {
				return s, fmt.Errorf("load global review: %w", err)
			}
		}
	}

	return s, nil
}

func aggregateRefresh(kind AggregateKind, boundary *storepkg.ArcBoundary) *AggregateRefresh {
	return &AggregateRefresh{
		Kind: kind, Volume: boundary.Volume, Arc: boundary.Arc,
		StartChapter: boundary.StartChapter, EndChapter: boundary.EndChapter,
	}
}
