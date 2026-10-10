package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// Store is the composition root of state management and holds all sub-stores.
type Store struct {
	dir  string
	lang string
	ios  []*IO

	Progress       *ProgressStore
	Book           *BookStore
	Outline        *OutlineStore
	Drafts         *DraftStore
	Summaries      *SummaryStore
	RunMeta        *RunMetaStore
	UserRules      *UserRulesStore
	Signals        *SignalStore
	Runtime        *RuntimeStore
	Characters     *CharacterStore
	Cast           *CastStore
	World          *WorldStore
	Checkpoints    *CheckpointStore
	Sessions       *SessionStore
	Usage          *UsageStore
	Simulation     *SimulationStore
	Decisions      *DecisionStore
	ChapterRecords *ChapterRecordStore
	Revisions      *RevisionStore

	crossMu sync.Mutex // Serializes cross-domain coordination; does not mean multiple files have transactional atomicity
}

const (
	LegacyProjectFormatVersion  = 1
	CurrentProjectFormatVersion = 2
	projectFormatPath           = "meta/format.json"
)

type projectFormat struct {
	Version int `json:"version"`
}

// NewStore creates the state manager; dir is the novel output root directory.
func NewStore(dir string) *Store {
	var ios []*IO
	// mk records the IO of each sub-store: each holds its own lock (not blocking one another), but the language is uniform across the whole book,
	// and registering centrally lets it be set in one go, without missing any store added in the future.
	mk := func() *IO { x := newIO(dir); ios = append(ios, x); return x }
	io := mk()
	outline := NewOutlineStore(io)
	s := &Store{
		dir:            dir,
		Progress:       NewProgressStore(mk()),
		Book:           NewBookStore(mk()),
		Outline:        outline,
		Drafts:         NewDraftStore(mk()),
		Summaries:      NewSummaryStore(mk(), outline),
		RunMeta:        NewRunMetaStore(mk()),
		UserRules:      NewUserRulesStore(mk()),
		Signals:        NewSignalStore(mk()),
		Runtime:        NewRuntimeStore(mk()),
		Characters:     NewCharacterStore(mk(), outline),
		Cast:           NewCastStore(mk()),
		World:          NewWorldStore(mk()),
		Checkpoints:    NewCheckpointStore(io),
		Sessions:       NewSessionStore(mk()),
		Usage:          NewUsageStore(mk()),
		Simulation:     NewSimulationStore(mk()),
		Decisions:      NewDecisionStore(mk()),
		ChapterRecords: NewChapterRecordStore(mk()),
		Revisions:      NewRevisionStore(mk()),
	}
	s.ios = ios
	return s
}

// SetLanguage sets the work language ("vi" / "zh"), which affects the labels of all derived Markdown views.
// Setting it once at startup is enough; if never called, the upstream default (Chinese) applies.
func (s *Store) SetLanguage(lang string) {
	s.lang = lang
	for _, x := range s.ios {
		x.SetLanguage(lang)
	}
}

// Language returns the work language set via SetLanguage ("" if not yet set).
func (s *Store) Language() string { return s.lang }

// Dir returns the output root directory.
func (s *Store) Dir() string { return s.dir }

// LoadProjectFormatVersion returns the data format version of the work directory. Legacy works have no version file and
// are treated as v1, upgraded uniformly by the startup migration, so business code needs no legacy-format branches.
func (s *Store) LoadProjectFormatVersion() (int, error) {
	var format projectFormat
	if err := s.Progress.io.ReadJSON(projectFormatPath, &format); err != nil {
		if os.IsNotExist(err) {
			return LegacyProjectFormatVersion, nil
		}
		return 0, err
	}
	if format.Version <= 0 {
		return 0, fmt.Errorf("Phiên bản định dạng dự án không hợp lệ: %d", format.Version)
	}
	return format.Version, nil
}

// SaveProjectFormatVersion atomically updates the project format version after a migration has fully completed.
func (s *Store) SaveProjectFormatVersion(version int) error {
	if version <= 0 {
		return fmt.Errorf("Phiên bản định dạng dự án phải lớn hơn 0: %d", version)
	}
	return s.Progress.io.WriteJSON(projectFormatPath, projectFormat{Version: version})
}

// CheckConsistency does a shallow validation of the fact layer, used to generate warnings at startup/recovery.
// Purely read-only: it does not fix data, it only returns readable problem descriptions. The caller decides how to display them (log / UI).
// To avoid the IO cost of scanning the whole directory, it validates only the key points of Progress:
//   - The last completed chapter must have its final text under chapters/
//   - In Layered mode, the current Volume/Arc must be findable in layered_outline
func (s *Store) CheckConsistency() []string {
	var warnings []string
	progress, err := s.Progress.Load()
	if err != nil {
		return append(warnings, fmt.Sprintf("Đọc progress thất bại: %v", err))
	}
	if progress == nil {
		return warnings
	}
	if n := len(progress.CompletedChapters); n > 0 {
		lastCh := progress.CompletedChapters[n-1]
		if text, err := s.Drafts.LoadChapterText(lastCh); err != nil {
			warnings = append(warnings, fmt.Sprintf("Đọc bản chính thức chương %d thất bại: %v", lastCh, err))
		} else if text == "" {
			warnings = append(warnings, fmt.Sprintf("progress đánh dấu chương %d đã hoàn thành, nhưng chapters/%02d.md không tồn tại hoặc rỗng", lastCh, lastCh))
		}
	}
	if progress.Layered && progress.CurrentVolume > 0 && progress.CurrentArc > 0 {
		volumes, err := s.Outline.LoadLayeredOutline()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Đọc dàn ý phân tầng thất bại: %v", err))
		} else if len(volumes) > 0 {
			found := false
			for _, v := range volumes {
				if v.Index != progress.CurrentVolume {
					continue
				}
				for _, a := range v.Arcs {
					if a.Index == progress.CurrentArc {
						found = true
						break
					}
				}
				break
			}
			if !found {
				warnings = append(warnings, fmt.Sprintf("V%d A%d hiện tại của progress không có mục tương ứng trong dàn ý phân tầng", progress.CurrentVolume, progress.CurrentArc))
			}
		}
	}
	return warnings
}

// FoundationMissing returns the work info and foundation settings still missing from the initial planning, in a stable order.
// Long-form mode (layered_outline already exists) additionally requires the compass. A read failure must be returned as is; an artifact that is
// corrupt or unreadable due to permissions must not be misjudged as "not yet created", or the caller might overwrite real data.
func (s *Store) FoundationMissing() ([]string, error) {
	var missing []string
	book, err := s.Book.Load()
	if err != nil {
		return nil, fmt.Errorf("load book metadata: %w", err)
	}
	if book == nil {
		missing = append(missing, "book")
	}
	premise, err := s.Outline.LoadPremise()
	if err != nil {
		return nil, fmt.Errorf("load premise: %w", err)
	}
	if premise == "" {
		missing = append(missing, "premise")
	}
	outline, err := s.Outline.LoadOutline()
	if err != nil {
		return nil, fmt.Errorf("load outline: %w", err)
	}
	if len(outline) == 0 {
		missing = append(missing, "outline")
	}
	characters, err := s.Characters.Load()
	if err != nil {
		return nil, fmt.Errorf("load characters: %w", err)
	}
	if len(characters) == 0 {
		missing = append(missing, "characters")
	}
	rules, err := s.World.LoadWorldRules()
	if err != nil {
		return nil, fmt.Errorf("load world rules: %w", err)
	}
	if len(rules) == 0 {
		missing = append(missing, "world_rules")
	}
	layered, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		return nil, fmt.Errorf("load layered outline: %w", err)
	}
	if len(layered) > 0 {
		compass, err := s.Outline.LoadCompass()
		if err != nil {
			return nil, fmt.Errorf("load compass: %w", err)
		}
		if compass == nil {
			missing = append(missing, "compass")
		}
	}
	// A new book may move from planning into writing only after the model has made an explicit semantic review of the artifacts on disk.
	// PhaseWriting/Complete represent a legacy book or an already-reviewed new book, kept compatible with historical projects; the review itself
	// is an action rather than a missing file, so it is appended only when the other artifacts are complete.
	if len(missing) == 0 {
		progress, err := s.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("load progress: %w", err)
		}
		if progress == nil || (progress.Phase != domain.PhaseWriting && progress.Phase != domain.PhaseComplete) {
			missing = append(missing, "foundation_audit")
		}
	}
	return missing, nil
}

// FoundationFingerprint returns the content fingerprint of the current foundation artifacts. The Architect must hand the value
// it read from novel_context back to the review tool as is, ensuring the conclusion targets the version actually on disk,
// rather than content in the session that is not yet saved or already stale.
func (s *Store) FoundationFingerprint() (string, error) {
	files := []string{"meta/book.json", "premise.md", "outline.json", "characters.json", "world_rules.json"}
	layered, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		return "", fmt.Errorf("load layered outline: %w", err)
	}
	if len(layered) > 0 {
		files = append(files, "layered_outline.json", "meta/compass.json")
	}

	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Init creates the required subdirectory structure.
func (s *Store) Init() error {
	if err := s.Checkpoints.InitError(); err != nil {
		return fmt.Errorf("load checkpoints: %w", err)
	}
	return s.Progress.io.EnsureDirs([]string{
		"chapters", "summaries", "drafts", "reviews", "meta", "meta/chapter_records", "meta/runtime", "meta/runtime/tasks", "meta/sessions", "meta/sessions/agents",
	})
}

// ── Cross-domain coordination methods ──

// ExpandArc calibrates a skeleton arc and expands it into detailed chapters (Outline + Progress linked).
func (s *Store) ExpandArc(volumeIdx, arcIdx int, expansion domain.ArcExpansion) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	volumes, err := s.Outline.expandArcUnlocked(volumeIdx, arcIdx, expansion)
	if err != nil {
		return err
	}

	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return err
	}
	if p == nil {
		p = &domain.Progress{}
	}
	p.TotalChapters = domain.EstimatedChapterCapacity(volumes)
	return s.Progress.saveUnlocked(p)
}

// AppendVolume appends a new volume to the end of the layered outline (Outline + Progress linked).
func (s *Store) AppendVolume(vol domain.VolumeOutline) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	volumes, err := s.Outline.appendVolumeUnlocked(vol)
	if err != nil {
		return err
	}

	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return err
	}
	if p == nil {
		p = &domain.Progress{}
	}
	p.TotalChapters = domain.EstimatedChapterCapacity(volumes)
	return s.Progress.saveUnlocked(p)
}

// ReviseOutline replaces the not-yet-happened planned tail starting from fromChapter.
// A flat outline replaces the tail of the whole book; a layered outline replaces only the tail of the arc containing the target chapter. This definition lets the same payload
// replay to the same result, while avoiding an enumeration of operations such as JSON Patch and insert/delete.
func (s *Store) ReviseOutline(fromChapter int, replacement []domain.OutlineEntry) (int, error) {
	if fromChapter <= 0 {
		return 0, fmt.Errorf("from_chapter must be > 0: %w", errs.ErrToolArgs)
	}

	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()
	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return 0, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if p == nil {
		return 0, fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
	}
	if p.Phase == domain.PhaseComplete {
		return 0, fmt.Errorf("Cả truyện đã kết thúc, không cho phép sửa dàn ý: %w", errs.ErrToolPrecondition)
	}
	protected := p.InProgressChapter
	if latest := p.LatestCompleted(); latest > protected {
		protected = latest
	}
	if fromChapter <= protected {
		// Only reporting "not allowed to change" would drive the caller into a dead end: in testing the architect tried 4 times here (from=21/22/13,
		// then fell back to save_foundation(outline)) and was rejected every time, spinning until the circuit breaker tripped. The error must also say who is responsible for
		// the rework, otherwise the architect keeps looking in its own toolset for an exit that does not exist.
		return 0, fmt.Errorf(
			"Chương %d đã hoàn thành hoặc đang viết; revise_outline chỉ sửa được các chương chưa xảy ra, phải bắt đầu sau chương %d."+
				"Các chương đã viết trong pending_rewrites không thuộc phạm vi sửa dàn ý: việc viết lại do writer thực hiện theo hàng đợi; "+
				"nếu kiến trúc sư không có chương tương lai nào cần viết lại, hãy gọi resolve_outline_feedback xác nhận kế hoạch hiện có vẫn phù hợp rồi kết thúc: %w",
			fromChapter, protected, errs.ErrToolPrecondition)
	}

	if p.Layered {
		volumes, err := s.Outline.reviseLayeredTailUnlocked(fromChapter, replacement)
		if err != nil {
			return 0, err
		}
		p.TotalChapters = domain.EstimatedChapterCapacity(volumes)
		if err := s.Progress.saveUnlocked(p); err != nil {
			return 0, fmt.Errorf("save progress: %w: %w", errs.ErrStoreWrite, err)
		}
		return p.TotalChapters, nil
	}

	outline, err := s.Outline.reviseFlatTailUnlocked(fromChapter, replacement)
	if err != nil {
		return 0, err
	}
	p.TotalChapters = len(outline)
	if err := s.Progress.saveUnlocked(p); err != nil {
		return 0, fmt.Errorf("save progress: %w: %w", errs.ErrStoreWrite, err)
	}
	return p.TotalChapters, nil
}

// ClearHandledSteer clears PendingSteer and resets the legacy FlowSteering state.
// Two files cannot form a file-system transaction, so it writes the repeatable Progress first and deletes the recovery intent last;
// if any step fails, at least PendingSteer is kept, and the next Resume can safely replay.
func (s *Store) ClearHandledSteer() error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.RunMeta.io.mu.Lock()
	defer s.RunMeta.io.mu.Unlock()
	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	meta, err := s.RunMeta.loadUnlocked()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	p, err := s.Progress.loadUnlocked()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if p != nil && p.Flow == domain.FlowSteering {
		if err := domain.ValidateFlowTransition(p.Flow, domain.FlowWriting); err != nil {
			return err
		}
		p.Flow = domain.FlowWriting
		if err := s.Progress.saveUnlocked(p); err != nil {
			return err
		}
	}
	if meta != nil && meta.PendingSteer != "" {
		meta.PendingSteer = ""
		if err := s.RunMeta.saveUnlocked(*meta); err != nil {
			return err
		}
	}
	return nil
}
