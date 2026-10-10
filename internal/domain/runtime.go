package domain

import (
	"fmt"
	"strings"
)

// Phase denotes the novel-writing phase.
type Phase string

const (
	PhaseInit     Phase = "init"
	PhasePremise  Phase = "premise"
	PhaseOutline  Phase = "outline"
	PhaseWriting  Phase = "writing"
	PhaseComplete Phase = "complete"
)

// FlowState is the type of the currently active flow, used for checkpoint recovery.
type FlowState string

const (
	FlowWriting   FlowState = "writing"
	FlowReviewing FlowState = "reviewing"
	FlowRewriting FlowState = "rewriting"
	FlowPolishing FlowState = "polishing"
	FlowSteering  FlowState = "steering"
)

// PlanningTier denotes the length tier of the planned work.
type PlanningTier string

const (
	PlanningTierShort PlanningTier = "short"
	PlanningTierMid   PlanningTier = "mid"
	PlanningTierLong  PlanningTier = "long"
)

// Progress tracks progress and is persisted to meta/progress.json.
type Progress struct {
	Phase          Phase `json:"phase"`
	CurrentChapter int   `json:"current_chapter"`
	// TotalChapters is the number of chapters in the detailed outline in non-layered mode; in layered mode it is only an
	// internal capacity value that includes skeleton estimates, used for context strategy, and does not represent a fixed total chapter count for the book.
	TotalChapters     int         `json:"total_chapters"`
	CompletedChapters []int       `json:"completed_chapters"`
	TotalWordCount    int         `json:"total_word_count"`
	ChapterWordCounts map[int]int `json:"chapter_word_counts,omitempty"` // Word count per chapter, supports correcting the total word count on rewrite
	InProgressChapter int         `json:"in_progress_chapter,omitempty"` // Chapter currently being written (scene-level recovery)
	CompletedScenes   []int       `json:"completed_scenes,omitempty"`    // Scene numbers already completed in the current chapter
	Flow              FlowState   `json:"flow,omitempty"`                // Current flow
	PendingRewrites   []int       `json:"pending_rewrites,omitempty"`    // Queue of chapters pending rewrite
	RewriteReason     string      `json:"rewrite_reason,omitempty"`      // Rewrite reason
	StrandHistory     []string    `json:"strand_history,omitempty"`      // Records dominant_strand in chapter order
	HookHistory       []string    `json:"hook_history,omitempty"`        // Records hook_type in chapter order
	// Layered tracking for long-form (used only in long-form mode; zero values for short / medium-length)
	CurrentVolume int  `json:"current_volume,omitempty"`
	CurrentArc    int  `json:"current_arc,omitempty"`
	Layered       bool `json:"layered,omitempty"`
	// ReopenedFromComplete marks that this book entered rework by being reopened from the completed state via reopen. Rework only changes existing chapters
	// and neither adds nor removes structure, so once the queue drains it should be let through as "structure complete, so complete again" (avoiding a final-volume closing foreshadowing, disturbed by rework, getting stuck in a
	// writing -> out-of-bounds continuation dead loop); forward writing does not set this mark, and the completion check keeps its conservative semantics of threads being wrapped up.
	ReopenedFromComplete bool `json:"reopened_from_complete,omitempty"`
	// ReopenCount records the cumulative number of times this book was reopened from the completed state (an audit fact of /reopen). It also guarantees that
	// a re-completion after reopening differs in progress.json content from the previous completion: checkpoints are idempotent and deduplicated on the same digest,
	// so a byte-identical re-completion would produce no new checkpoint, and StopGuard would misjudge a successful complete_book
	// as idling and escalate to termination.
	ReopenCount int `json:"reopen_count,omitempty"`
}

// IsResumable reports whether the run can be resumed from a breakpoint.
func (p *Progress) IsResumable() bool {
	return p.Phase == PhaseWriting && p.CurrentChapter > 0
}

// NextChapter returns the number of the next chapter to write.
func (p *Progress) NextChapter() int {
	return p.LatestCompleted() + 1
}

// LatestCompleted returns the largest completed chapter number; it returns 0 when no chapter is completed.
func (p *Progress) LatestCompleted() int {
	max := 0
	for _, ch := range p.CompletedChapters {
		if ch > max {
			max = ch
		}
	}
	return max
}

// ContextProfile is the context loading strategy, adapted to the total chapter count.
type ContextProfile struct {
	SummaryWindow  int  // Load the last N chapter summaries
	TimelineWindow int  // Load the last N chapter timelines
	Layered        bool // true = enable layered summary loading (volume summary + arc summary + chapter summary)
}

// MemoryPolicy denotes the memory usage policy shared across the runtime.
// It is used both for context output and for the host layer's handoff / reminder decisions.
type MemoryPolicy struct {
	Mode                string `json:"mode,omitempty"`
	SummaryWindow       int    `json:"summary_window,omitempty"`
	TimelineWindow      int    `json:"timeline_window,omitempty"`
	LayeredSummaries    bool   `json:"layered_summaries,omitempty"`
	SummaryStrategy     string `json:"summary_strategy,omitempty"`
	WorkingRefresh      string `json:"working_refresh,omitempty"`
	EpisodicRefresh     string `json:"episodic_refresh,omitempty"`
	PlanningRefresh     string `json:"planning_refresh,omitempty"`
	FoundationRefresh   string `json:"foundation_refresh,omitempty"`
	PlanningFocus       string `json:"planning_focus,omitempty"`
	FoundationFocus     string `json:"foundation_focus,omitempty"`
	PreviousTailChars   int    `json:"previous_tail_chars,omitempty"`
	ChapterPlanEnabled  bool   `json:"chapter_plan_enabled,omitempty"`
	RelatedLookup       bool   `json:"related_chapter_lookup,omitempty"`
	CurrentOutlineBound bool   `json:"current_outline_bound,omitempty"`
	HandoffPreferred    bool   `json:"handoff_preferred,omitempty"`
	ReadOnlyThreshold   int    `json:"read_only_threshold,omitempty"`
}

// NewContextProfile computes the context strategy from the total chapter count.
func NewContextProfile(totalChapters int) ContextProfile {
	switch {
	case totalChapters <= 15:
		return ContextProfile{SummaryWindow: 10, TimelineWindow: 10}
	case totalChapters <= 50:
		return ContextProfile{SummaryWindow: 5, TimelineWindow: 8}
	default:
		return ContextProfile{SummaryWindow: 3, TimelineWindow: 5, Layered: true}
	}
}

// NewChapterMemoryPolicy generates the chapter runtime memory policy from progress and the context profile.
func NewChapterMemoryPolicy(progress *Progress, profile ContextProfile, currentOutlineBound bool) MemoryPolicy {
	policy := MemoryPolicy{
		Mode:                "chapter",
		SummaryWindow:       profile.SummaryWindow,
		TimelineWindow:      profile.TimelineWindow,
		LayeredSummaries:    profile.Layered,
		WorkingRefresh:      "Làm mới mỗi khi nạp theo chương",
		EpisodicRefresh:     "Làm mới khi nộp chương, đánh giá và thay đổi trạng thái truyện dài",
		PreviousTailChars:   800,
		ChapterPlanEnabled:  true,
		CurrentOutlineBound: currentOutlineBound,
		ReadOnlyThreshold:   5,
	}
	if profile.Layered {
		policy.SummaryStrategy = "Tóm tắt tập + tóm tắt cung + tóm tắt các chương gần nhất"
	} else {
		policy.SummaryStrategy = "Tóm tắt các chương gần nhất"
	}
	if progress != nil {
		if progress.TotalChapters > 30 {
			policy.RelatedLookup = true
		}
		if progress.Flow == FlowReviewing || progress.Flow == FlowRewriting || progress.Flow == FlowPolishing {
			policy.HandoffPreferred = true
		}
		if progress.Layered && len(progress.CompletedChapters) >= 6 {
			policy.HandoffPreferred = true
		}
		if len(progress.CompletedChapters) >= 12 {
			policy.HandoffPreferred = true
		}
		if progress.Layered && len(progress.CompletedChapters) >= 6 {
			policy.ReadOnlyThreshold = 4
		}
		if len(progress.CompletedChapters) >= 12 {
			policy.ReadOnlyThreshold = 4
		}
	}
	return policy
}

// NewArchitectMemoryPolicy returns the memory policy used in the planning phase.
func NewArchitectMemoryPolicy() MemoryPolicy {
	return MemoryPolicy{
		Mode:               "architect",
		PlanningRefresh:    "Làm mới khi cấu trúc tập/cung, compass hoặc tóm tắt được cập nhật",
		FoundationRefresh:  "Làm mới khi nhân vật, chi tiết gài trước, thiết lập thay đổi",
		PlanningFocus:      "Dàn ý phân tầng, compass, tóm tắt tập",
		FoundationFocus:    "Thiết lập nhân vật, snapshot nhân vật, sổ chi tiết gài trước",
		HandoffPreferred:   true,
		ChapterPlanEnabled: false,
		ReadOnlyThreshold:  4,
	}
}

// RunMeta is run metadata, persisted to meta/run.json.
type RunMeta struct {
	StartedAt            string             `json:"started_at"`
	Provider             string             `json:"provider,omitempty"`
	Style                string             `json:"style"`
	Model                string             `json:"model"`
	PlanningTier         PlanningTier       `json:"planning_tier,omitempty"`
	StartPrompt          string             `json:"start_prompt,omitempty"`           // The user's original writing request (an input fact, persisted before the start ruling; used to redo the ruling if it fails)
	PlanStart            *PlanStartRecord   `json:"plan_start,omitempty"`             // The start ruling fact, the sole basis for crash recovery during planning
	PendingSteer         string             `json:"pending_steer,omitempty"`          // Unfinished Steer instructions, re-injected on interrupt recovery
	AdvanceMode          ChapterAdvanceMode `json:"advance_mode"`                     // Chapter advance mode: auto / review
	AdvancePermitChapter int                `json:"advance_permit_chapter,omitempty"` // One-time permitted forward chapter in review mode
	AdvanceHold          *AdvanceHold       `json:"advance_hold,omitempty"`           // One-time pause intent signed by the current intervention
}

// ChapterAdvanceMode decides whether new chapters need per-chapter approval.
type ChapterAdvanceMode string

const (
	ChapterAdvanceAuto   ChapterAdvanceMode = "auto"
	ChapterAdvanceReview ChapterAdvanceMode = "review"
)

// Valid reports whether the chapter advance mode is supported by the current version.
func (m ChapterAdvanceMode) Valid() bool {
	return m == ChapterAdvanceAuto || m == ChapterAdvanceReview
}

// UnsupportedAdvanceModeError means the book's control mode is not supported by the current binary.
// The caller must stop constructing a writable Host and tell the user to use a matching version; guessing a downgrade is forbidden.
type UnsupportedAdvanceModeError struct {
	Mode ChapterAdvanceMode
}

func (e *UnsupportedAdvanceModeError) Error() string {
	return fmt.Sprintf("Chế độ tiến chương %q không được hỗ trợ, hãy dùng bản ainovel mới hơn đã tạo dự án này", e.Mode)
}

// AdvanceHoldAfter is the deterministic trigger condition of a one-time pause.
type AdvanceHoldAfter string

const (
	AdvanceHoldAtBoundary           AdvanceHoldAfter = "boundary"
	AdvanceHoldAfterRewritesDrained AdvanceHoldAfter = "rewrites_drained"
	AdvanceHoldAtChapter            AdvanceHoldAfter = "chapter"
)

// Valid reports whether the pause condition is supported by the current version.
func (a AdvanceHoldAfter) Valid() bool {
	return a == AdvanceHoldAtBoundary || a == AdvanceHoldAfterRewritesDrained || a == AdvanceHoldAtChapter
}

// AdvanceHold is the one-time pause intent signed by the current intervention, consumed at the Host boundary.
type AdvanceHold struct {
	After         AdvanceHoldAfter `json:"after"`
	TargetChapter int              `json:"target_chapter,omitempty"`
	Reason        string           `json:"reason"`
}

// Validate checks the structural constraints of the one-time pause intent itself.
func (h AdvanceHold) Validate() error {
	if !h.After.Valid() {
		return fmt.Errorf("Điều kiện tạm dừng một lần không được hỗ trợ %q", h.After)
	}
	if h.After == AdvanceHoldAtChapter {
		if h.TargetChapter <= 0 {
			return fmt.Errorf("Chương mục tiêu phải lớn hơn 0")
		}
	} else if h.TargetChapter != 0 {
		return fmt.Errorf("Điều kiện tạm dừng %q không được đặt chương mục tiêu", h.After)
	}
	if strings.TrimSpace(h.Reason) == "" {
		return fmt.Errorf("Lý do tạm dừng một lần không được để trống")
	}
	return nil
}

// PlanStartRecord is the persisted fact of the start ruling (the ruling is persisted as a fact first, then execution starts; recovery does not rule again).
// After the first save_foundation persists scale, planning-phase recovery switches to deriving from PlanningTier, and this record
// covers only the window "between the ruling completing and the first persist". DecisionID links to the decisions.jsonl audit.
type PlanStartRecord struct {
	RawPrompt   string `json:"raw_prompt"`
	Planner     string `json:"planner"`
	PlannerTask string `json:"planner_task"`
	DecisionID  string `json:"decision_id,omitempty"`
}
