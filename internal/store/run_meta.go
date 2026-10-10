package store

import (
	"fmt"
	"os"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// RunMetaStore manages run metadata (model, intervention history, planning tier, etc.).
type RunMetaStore struct{ io *IO }

func NewRunMetaStore(io *IO) *RunMetaStore { return &RunMetaStore{io: io} }

// Save saves the run metadata to meta/run.json.
func (s *RunMetaStore) Save(meta domain.RunMeta) error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.saveUnlocked(meta)
}

// Load reads the run metadata.
func (s *RunMetaStore) Load() (*domain.RunMeta, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	return s.loadUnlocked()
}

func (s *RunMetaStore) loadUnlocked() (*domain.RunMeta, error) {
	var meta domain.RunMeta
	if err := s.io.ReadJSONUnlocked("meta/run.json", &meta); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &meta, nil
}

func (s *RunMetaStore) saveUnlocked(meta domain.RunMeta) error {
	return s.io.WriteJSONUnlocked("meta/run.json", meta)
}

// Init initializes or updates the run metadata; all run-intent facts are preserved across restarts —
// PlanStart is especially critical: after a crash during planning (startup ruling persisted, first foundation not yet persisted),
// it is the only basis for recovering the planner identity, and an Init overwrite would make recovery halt outright.
func (s *RunMetaStore) Init(style, provider, model string) error {
	return s.io.WithWriteLock(func() error {
		existing, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		meta := domain.RunMeta{
			StartedAt: time.Now().Format(time.RFC3339),
			Provider:  provider,
			Style:     style,
			Model:     model,
		}
		if existing != nil {
			meta.PendingSteer = existing.PendingSteer
			meta.PlanningTier = existing.PlanningTier
			meta.PlanStart = existing.PlanStart
			meta.StartPrompt = existing.StartPrompt
			meta.AdvanceMode = existing.AdvanceMode
			meta.AdvancePermitChapter = existing.AdvancePermitChapter
			meta.AdvanceHold = existing.AdvanceHold
		}
		if meta.AdvanceMode == "" {
			meta.AdvanceMode = domain.ChapterAdvanceAuto
		}
		if err := validateAdvanceControl(meta); err != nil {
			return err
		}
		return s.saveUnlocked(meta)
	})
}

func validateAdvanceControl(meta domain.RunMeta) error {
	if !meta.AdvanceMode.Valid() {
		return &domain.UnsupportedAdvanceModeError{Mode: meta.AdvanceMode}
	}
	if meta.AdvancePermitChapter < 0 {
		return fmt.Errorf("Giấy phép chương không được âm: %d", meta.AdvancePermitChapter)
	}
	if meta.AdvanceMode == domain.ChapterAdvanceAuto && meta.AdvancePermitChapter != 0 {
		return fmt.Errorf("Chế độ auto không được giữ giấy phép chương: %d", meta.AdvancePermitChapter)
	}
	if meta.AdvanceHold != nil {
		if err := meta.AdvanceHold.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// SetStartPrompt persists the user's original writing request — an input fact, written to disk **before** the startup ruling.
// If the ruling fails (e.g. a model fault) it is still there, and recovery/continue lets the engine make up the ruling from it (engine.planStartFallback),
// so a startup failure is no longer a dead end.
func (s *RunMetaStore) SetStartPrompt(prompt string) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			meta = &domain.RunMeta{}
		}
		meta.StartPrompt = prompt
		return s.saveUnlocked(*meta)
	})
}

// SetPendingSteer records an unfinished Steer instruction.
func (s *RunMetaStore) SetPendingSteer(input string) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			meta = &domain.RunMeta{}
		}
		meta.PendingSteer = input
		return s.saveUnlocked(*meta)
	})
}

// ClearPendingSteer clears a handled Steer instruction.
func (s *RunMetaStore) ClearPendingSteer() error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil || meta.PendingSteer == "" {
			return nil
		}
		meta.PendingSteer = ""
		return s.saveUnlocked(*meta)
	})
}

// SetAdvanceMode switches the chapter advance mode. Switching back to auto clears the chapter permit within the same write lock.
func (s *RunMetaStore) SetAdvanceMode(mode domain.ChapterAdvanceMode) error {
	if !mode.Valid() {
		return &domain.UnsupportedAdvanceModeError{Mode: mode}
	}
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			return fmt.Errorf("run meta chưa được khởi tạo")
		}
		meta.AdvanceMode = mode
		if mode == domain.ChapterAdvanceAuto {
			meta.AdvancePermitChapter = 0
		}
		return s.saveUnlocked(*meta)
	})
}

// GrantAdvancePermit persists one exact chapter permit for review mode.
func (s *RunMetaStore) GrantAdvancePermit(chapter int) error {
	if chapter <= 0 {
		return fmt.Errorf("Giấy phép chương phải lớn hơn 0: %d", chapter)
	}
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			return fmt.Errorf("run meta chưa được khởi tạo")
		}
		if meta.AdvanceMode != domain.ChapterAdvanceReview {
			return fmt.Errorf("Chỉ chế độ duyệt từng chương mới được cấp phép chương kế (hiện %s)", meta.AdvanceMode)
		}
		if meta.AdvancePermitChapter == chapter {
			return nil
		}
		if meta.AdvancePermitChapter != 0 {
			return fmt.Errorf("Đã có giấy phép cho chương %d, từ chối ghi đè thành chương %d", meta.AdvancePermitChapter, chapter)
		}
		meta.AdvancePermitChapter = chapter
		return s.saveUnlocked(*meta)
	})
}

// ClearAdvancePermit consumes only the matching chapter permit; idempotent when the target no longer exists.
func (s *RunMetaStore) ClearAdvancePermit(chapter int) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil || meta.AdvancePermitChapter == 0 {
			return nil
		}
		if meta.AdvancePermitChapter != chapter {
			return fmt.Errorf("Giấy phép chương đã thay đổi: mong đợi chương %d, thực tế chương %d", chapter, meta.AdvancePermitChapter)
		}
		meta.AdvancePermitChapter = 0
		return s.saveUnlocked(*meta)
	})
}

// SetAdvanceHold registers a one-shot pause intent; an in-flight intent must not be silently overwritten by another.
func (s *RunMetaStore) SetAdvanceHold(hold domain.AdvanceHold) error {
	if err := hold.Validate(); err != nil {
		return err
	}
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			return fmt.Errorf("run meta chưa được khởi tạo")
		}
		if meta.AdvanceHold != nil {
			if *meta.AdvanceHold == hold {
				return nil
			}
			return fmt.Errorf("Đã có ý định tạm dừng một lần (%s: %s), từ chối ghi đè", meta.AdvanceHold.After, meta.AdvanceHold.Reason)
		}
		meta.AdvanceHold = &hold
		return s.saveUnlocked(*meta)
	})
}

// ClearAdvanceHold consumes only the same intent the caller just read; idempotent when the target no longer exists.
func (s *RunMetaStore) ClearAdvanceHold(expected domain.AdvanceHold) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil || meta.AdvanceHold == nil {
			return nil
		}
		if *meta.AdvanceHold != expected {
			return fmt.Errorf("Ý định tạm dừng một lần đã thay đổi, từ chối xóa nhầm")
		}
		meta.AdvanceHold = nil
		return s.saveUnlocked(*meta)
	})
}

// SetPlanningTier records the planning tier of the current work.
func (s *RunMetaStore) SetPlanningTier(tier domain.PlanningTier) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			meta = &domain.RunMeta{}
		}
		meta.PlanningTier = tier
		return s.saveUnlocked(*meta)
	})
}

// SetPlanStart persists the startup-ruling fact (the ruling lands as a fact before execution starts; recovery from a planning-phase crash continues from it).
func (s *RunMetaStore) SetPlanStart(rec domain.PlanStartRecord) error {
	return s.io.WithWriteLock(func() error {
		meta, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if meta == nil {
			meta = &domain.RunMeta{}
		}
		meta.PlanStart = &rec
		return s.saveUnlocked(*meta)
	})
}
