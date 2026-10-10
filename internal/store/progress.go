package store

import (
	"fmt"
	"os"
	"slices"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// ProgressStore manages the writing progress state.
type ProgressStore struct{ io *IO }

func NewProgressStore(io *IO) *ProgressStore { return &ProgressStore{io: io} }

// Load reads meta/progress.json. Returns nil if it does not exist.
func (s *ProgressStore) Load() (*domain.Progress, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadUnlocked()
}

func (s *ProgressStore) loadUnlocked() (*domain.Progress, error) {
	var p domain.Progress
	if err := s.io.ReadJSONUnlocked("meta/progress.json", &p); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// Save saves the progress.
func (s *ProgressStore) Save(p *domain.Progress) error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.saveUnlocked(p)
}

func (s *ProgressStore) saveUnlocked(p *domain.Progress) error {
	return s.io.WriteJSONUnlocked("meta/progress.json", p)
}

// Init creates the initial progress.
func (s *ProgressStore) Init(totalChapters int) error {
	return s.Save(&domain.Progress{
		Phase:         domain.PhaseInit,
		TotalChapters: totalChapters,
	})
}

// SetTotalChapters updates the outline capacity: the number of detailed chapters in non-layered mode, an internal estimate in layered mode.
func (s *ProgressStore) SetTotalChapters(n int) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			p = &domain.Progress{}
		}
		p.TotalChapters = n
		return s.saveUnlocked(p)
	})
}

// UpdatePhase updates the writing phase.
func (s *ProgressStore) UpdatePhase(phase domain.Phase) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			p = &domain.Progress{}
		}
		if err := domain.ValidatePhaseTransition(p.Phase, phase); err != nil {
			return err
		}
		p.Phase = phase
		return s.saveUnlocked(p)
	})
}

// AdvancePhase advances the writing phase to at least phase; if a later phase has already been reached, it stays unchanged.
// Suited to phase artifacts that can be saved repeatedly, so revising an old artifact is not misjudged as a phase regression.
func (s *ProgressStore) AdvancePhase(phase domain.Phase) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			p = &domain.Progress{}
		}
		if domain.CanTransitionPhase(phase, p.Phase) {
			return nil
		}
		if err := domain.ValidatePhaseTransition(p.Phase, phase); err != nil {
			return err
		}
		p.Phase = phase
		return s.saveUnlocked(p)
	})
}

// StartChapter marks a chapter as being written. It cannot take on the job of phase transitions; the caller must first have the
// foundation/import flow explicitly advance Progress to writing, so a faulty dispatch cannot bypass the planning phase.
func (s *ProgressStore) StartChapter(chapter int) error {
	if chapter <= 0 {
		return fmt.Errorf("chapter must be > 0")
	}
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
		}
		if p.Phase != domain.PhaseWriting {
			return fmt.Errorf("Chỉ được viết chương ở giai đoạn writing (hiện phase=%s): %w", p.Phase, errs.ErrToolPrecondition)
		}
		if p.Flow != domain.FlowRewriting && p.Flow != domain.FlowPolishing {
			p.Flow = domain.FlowWriting
		}
		if p.CurrentChapter < chapter {
			p.CurrentChapter = chapter
		}
		p.InProgressChapter = chapter
		p.CompletedScenes = nil
		return s.saveUnlocked(p)
	})
}

// IsChapterCompleted checks whether a chapter has been committed as complete. A read failure is returned explicitly; a corrupt
// progress must not be treated as "not completed" and then go on to overwrite the chapter.
func (s *ProgressStore) IsChapterCompleted(chapter int) (bool, error) {
	p, err := s.Load()
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	return slices.Contains(p.CompletedChapters, chapter), nil
}

// MarkChapterComplete marks a chapter complete, updating the progress atomically.
func (s *ProgressStore) MarkChapterComplete(chapter, wordCount int, hookType, dominantStrand string) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("progress not initialized, call Init first")
		}
		if p.ChapterWordCounts == nil {
			p.ChapterWordCounts = make(map[int]int)
		}
		if oldWC, ok := p.ChapterWordCounts[chapter]; ok {
			p.TotalWordCount -= oldWC
		}
		p.ChapterWordCounts[chapter] = wordCount
		p.TotalWordCount += wordCount
		if !slices.Contains(p.CompletedChapters, chapter) {
			p.CompletedChapters = append(p.CompletedChapters, chapter)
		}
		if chapter+1 > p.CurrentChapter {
			p.CurrentChapter = chapter + 1
		}
		p.InProgressChapter = 0
		p.CompletedScenes = nil
		if err := domain.ValidatePhaseTransition(p.Phase, domain.PhaseWriting); err != nil {
			return err
		}
		p.Phase = domain.PhaseWriting

		if dominantStrand != "" {
			for len(p.StrandHistory) < chapter-1 {
				p.StrandHistory = append(p.StrandHistory, "")
			}
			if len(p.StrandHistory) < chapter {
				p.StrandHistory = append(p.StrandHistory, dominantStrand)
			} else {
				p.StrandHistory[chapter-1] = dominantStrand
			}
		}
		if hookType != "" {
			for len(p.HookHistory) < chapter-1 {
				p.HookHistory = append(p.HookHistory, "")
			}
			if len(p.HookHistory) < chapter {
				p.HookHistory = append(p.HookHistory, hookType)
			} else {
				p.HookHistory[chapter-1] = hookType
			}
		}

		return s.saveUnlocked(p)
	})
}

// MarkComplete marks the whole book as complete and clears the reopen-rework flag (once complete, it is no longer in the rework state).
func (s *ProgressStore) MarkComplete() error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			p = &domain.Progress{}
		}
		if err := domain.ValidatePhaseTransition(p.Phase, domain.PhaseComplete); err != nil {
			return err
		}
		p.Phase = domain.PhaseComplete
		p.ReopenedFromComplete = false
		return s.saveUnlocked(p)
	})
}

// Reopen reopens a completed book into the rework state: phase complete->writing + target chapters enqueued + flow=rewriting,
// done atomically within a single write lock. This is the only exemption exit from the "forward-only" constraint of phaseOrder — it deliberately does not go through
// ValidatePhaseTransition; the legitimacy of the rollback is confined to this method and protected by the phase=complete precondition guard,
// so misuse cannot send the state machine out of control. After the queue is worked off, commit_chapter automatically re-closes and completes the book.
func (s *ProgressStore) Reopen(chapters []int, reason string) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
		}
		if p.Phase != domain.PhaseComplete {
			return fmt.Errorf("reopen chỉ áp dụng cho sách đã kết thúc (hiện phase=%s): %w", p.Phase, errs.ErrToolPrecondition)
		}
		normalized, err := normalizePendingRewrites(chapters, p.CompletedChapters)
		if err != nil {
			return err
		}
		p.Phase = domain.PhaseWriting // The only legitimate rollback, protected by the complete precondition above
		p.PendingRewrites = normalized
		p.RewriteReason = reason
		p.Flow = domain.FlowRewriting
		p.ReopenedFromComplete = true // After draining, complete again per the full structure; see the commit_chapter drain block
		return s.saveUnlocked(p)
	})
}

// ReopenContinue reopens a completed book into the continue-writing state: only phase complete->writing, no rework queue entries,
// ReopenedFromComplete is not set (that is the drain semantic of "after rework drains, automatically complete again with the original structure";
// a continue-writing reopen is precisely meant to extend the structure). Like Reopen, it is an exemption exit from the phaseOrder "forward-only" constraint,
// protected by the same phase=complete precondition guard; after the reopen, end-of-volume routing dispatches the architect to continue the volume.
func (s *ProgressStore) ReopenContinue() error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
		}
		if p.Phase != domain.PhaseComplete {
			return fmt.Errorf("Mở lại chỉ áp dụng cho sách đã kết thúc (hiện phase=%s): %w", p.Phase, errs.ErrToolPrecondition)
		}
		p.Phase = domain.PhaseWriting
		p.ReopenCount++ // Audit + guarantee the progress digest of the re-completion differs from the last one (see the field comment)
		return s.saveUnlocked(p)
	})
}

// ClearInProgress clears the in-progress intermediate state.
func (s *ProgressStore) ClearInProgress() error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		p.InProgressChapter = 0
		p.CompletedScenes = nil
		return s.saveUnlocked(p)
	})
}

// UpdateVolumeArc updates the current volume/arc position.
func (s *ProgressStore) UpdateVolumeArc(volume, arc int) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		p.CurrentVolume = volume
		p.CurrentArc = arc
		return s.saveUnlocked(p)
	})
}

// SetLayered sets the layered-mode flag.
func (s *ProgressStore) SetLayered(layered bool) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		p.Layered = layered
		return s.saveUnlocked(p)
	})
}

// SetFlow updates the current flow state.
func (s *ProgressStore) SetFlow(flow domain.FlowState) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		if err := domain.ValidateFlowTransition(p.Flow, flow); err != nil {
			return err
		}
		p.Flow = flow
		return s.saveUnlocked(p)
	})
}

// SetPendingRewrites sets the pending-rewrite chapter queue and the reason.
// PendingRewrites may contain only completed chapters; an incomplete chapter has no final text yet and cannot enter the rewrite/polish queue.
func (s *ProgressStore) SetPendingRewrites(chapters []int, reason string) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		normalized, err := normalizePendingRewrites(chapters, p.CompletedChapters)
		if err != nil {
			return err
		}
		p.PendingRewrites = normalized
		p.RewriteReason = reason
		return s.saveUnlocked(p)
	})
}

// ApplyReviewOutcome atomically applies the flow state produced by a review. The review semantics are decided by the upper layer; the Store only
// validates the Flow transition and the rework chapters, and guarantees Flow, PendingRewrites and RewriteReason never show an intermediate state.
func (s *ProgressStore) ApplyReviewOutcome(flow domain.FlowState, chapters []int, reason string) (*domain.Progress, error) {
	var latest *domain.Progress
	err := s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
		}
		if len(chapters) > 0 {
			if flow == domain.FlowWriting {
				return fmt.Errorf("Khi còn chương viết lại thì flow không được là writing: %w", errs.ErrToolConflict)
			}
			if err := domain.ValidateFlowTransition(p.Flow, flow); err != nil {
				return err
			}
			normalized, err := normalizePendingRewrites(chapters, p.CompletedChapters)
			if err != nil {
				return err
			}
			p.PendingRewrites = normalized
			p.RewriteReason = reason
			p.Flow = flow
		} else if len(p.PendingRewrites) == 0 {
			if err := domain.ValidateFlowTransition(p.Flow, flow); err != nil {
				return err
			}
			p.Flow = flow
		}
		if err := s.saveUnlocked(p); err != nil {
			return err
		}
		latest = p
		return nil
	})
	return latest, err
}

// ValidatePendingRewrites validates whether the chapter list may enter the rework queue, without modifying state.
func (s *ProgressStore) ValidatePendingRewrites(chapters []int) error {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()

	p, err := s.loadUnlocked()
	if err != nil {
		return err
	}
	if p == nil {
		_, err := normalizePendingRewrites(chapters, nil)
		return err
	}
	_, err = normalizePendingRewrites(chapters, p.CompletedChapters)
	return err
}

// CompleteRewrite removes completed chapters from the pending-rewrite queue.
func (s *ProgressStore) CompleteRewrite(chapter int) error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		var remaining []int
		for _, ch := range p.PendingRewrites {
			if ch != chapter {
				remaining = append(remaining, ch)
			}
		}
		p.PendingRewrites = remaining
		if len(remaining) == 0 {
			if err := domain.ValidateFlowTransition(p.Flow, domain.FlowWriting); err != nil {
				return err
			}
			p.Flow = domain.FlowWriting
			p.RewriteReason = ""
		}
		return s.saveUnlocked(p)
	})
}

// ClearPendingRewrites forcibly empties the rewrite queue.
func (s *ProgressStore) ClearPendingRewrites() error {
	return s.io.WithWriteLock(func() error {
		p, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		p.PendingRewrites = nil
		p.RewriteReason = ""
		if err := domain.ValidateFlowTransition(p.Flow, domain.FlowWriting); err != nil {
			return err
		}
		p.Flow = domain.FlowWriting
		return s.saveUnlocked(p)
	})
}

// ValidateChapterWork validates whether the current chapter may be planned or committed.
// The Writer may work only in the writing phase; under the polish/rewrite flow, only chapters in PendingRewrites may be handled.
// The phase constraint is guarded once more at the Store boundary, so a faulty Arbiter dispatch cannot bypass the Router.
func (s *ProgressStore) ValidateChapterWork(chapter int) error {
	p, err := s.Load()
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
	}
	if p.Phase != domain.PhaseWriting {
		return fmt.Errorf("Chỉ được viết chương ở giai đoạn writing (hiện phase=%s): %w", p.Phase, errs.ErrToolPrecondition)
	}
	if p.Flow != domain.FlowRewriting && p.Flow != domain.FlowPolishing {
		return nil
	}
	if _, err := normalizePendingRewrites(p.PendingRewrites, p.CompletedChapters); err != nil {
		return err
	}
	if slices.Contains(p.PendingRewrites, chapter) {
		return nil
	}

	verb := "viết lại"
	if p.Flow == domain.FlowPolishing {
		verb = "trau chuốt"
	}
	return fmt.Errorf("Chương %d không nằm trong hàng đợi %s, hàng đợi hiện tại: %v. Hãy xử lý các chương trong hàng đợi trước rồi mới viết chương mới: %w", chapter, verb, p.PendingRewrites, errs.ErrToolConflict)
}

func normalizePendingRewrites(chapters, completed []int) ([]int, error) {
	if len(chapters) == 0 {
		return nil, nil
	}
	completedSet := make(map[int]struct{}, len(completed))
	for _, ch := range completed {
		completedSet[ch] = struct{}{}
	}

	seen := make(map[int]struct{}, len(chapters))
	normalized := make([]int, 0, len(chapters))
	var invalid []int
	for _, ch := range chapters {
		if ch <= 0 {
			invalid = append(invalid, ch)
			continue
		}
		if _, ok := completedSet[ch]; !ok {
			invalid = append(invalid, ch)
			continue
		}
		if _, ok := seen[ch]; ok {
			continue
		}
		seen[ch] = struct{}{}
		normalized = append(normalized, ch)
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("pending_rewrites chỉ được chứa chương đã hoàn thành, chương không hợp lệ: %v, completed_chapters=%v: %w", invalid, completed, errs.ErrToolPrecondition)
	}
	return normalized, nil
}
