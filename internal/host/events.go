package host

import (
	"time"
)

// Event is the structured event consumed by the TUI.
//
// For the three call-type events TOOL / DISPATCH / DECISION, the start and end of the same call share one ID:
// at the start an event with a zero FinishedAt is emitted (the TUI renders it in an "in progress" style);
// at the end another event with the same ID is emitted, filling in FinishedAt + Duration (+ Failed),
// and the TUI locates the original row by ID and updates it in place, avoiding the redundancy of "one row for start, another for completion".
//
// Non-call events such as SYSTEM / ERROR / CONTEXT have an empty ID and are each appended independently.
type Event struct {
	ID         string    // shared by the start/end of the same call; empty for non-call events
	Time       time.Time // first emission time (the start moment)
	FinishedAt time.Time // zero value = in progress; non-zero = finished
	Failed     bool      // finished but failed (meaningful only in the finished state)
	Category   string    // DISPATCH / TOOL / DECISION / SYSTEM / REVIEW / CHECK / ERROR / CONTEXT
	Agent      string    // the agent that produced the event
	Summary    string
	Detail     string        // full text, written to the log untruncated for diagnosis; falls back to Summary when empty. The UI reads only Summary
	Kind       string        // error category (e.g. stream_idle), emitted with the log for filtering/alerting; not emitted when empty
	Level      string        // info / warn / error / success
	Depth      int           // 0 = Engine layer, 1 = Worker layer
	Duration   time.Duration // execution time at completion
	RetryAt    time.Time     // retry-type events: deadline of the next retry; the UI counts down per second from it and clears at the deadline (the request is already in flight)
}

// Running reports whether the event is in progress.
// Only call-type events (TOOL / DISPATCH / DECISION with an ID) can be in progress; other types always return false.
func (e Event) Running() bool {
	return e.hasLifecycle() && e.FinishedAt.IsZero()
}

func (e Event) hasLifecycle() bool {
	if e.ID == "" {
		return false
	}
	switch e.Category {
	case "TOOL", "DISPATCH", "DECISION":
		return true
	default:
		return false
	}
}

// UISnapshot is the aggregate state snapshot needed for TUI rendering.
type UISnapshot struct {
	Provider             string
	BookTitle            string
	ModelName            string
	ModelContextWindow   int // context window of the current default model (resolved live as /model switches)
	ThinkingLevel        string
	Style                string
	RuntimeState         string // idle / running / pausing / paused / completed
	StatusLabel          string
	Phase                string
	Flow                 string
	CurrentChapter       int
	TotalChapters        int
	CompletedCount       int
	TotalWordCount       int
	InProgressChapter    int
	PendingRewrites      []int
	RewriteReason        string
	PendingSteer         string
	AdvanceMode          string
	AdvancePermitChapter int
	HasAdvanceHold       bool
	AdvanceHoldReason    string
	RecoveryLabel        string
	IsRunning            bool
	Agents               []AgentSnapshot

	// Cumulative usage (whole session, across all agents and model switches)
	TotalInputTokens      int
	TotalOutputTokens     int
	TotalCacheReadTokens  int
	TotalCacheWriteTokens int
	TotalCostUSD          float64
	TotalSavedUSD         float64 // dollars saved by CacheRead hits (relative to billing everything at the non-cached input price)
	BudgetLimitUSD        float64 // budget cap (config budget.book_usd); 0 = not enabled

	// Cache diagnostics
	OverallCacheCapable    bool // at least one role has run a model that supports prompt cache (distinguishes "not enabled" from "0% hit")
	OverallRecentCacheRead int  // sum of cacheRead over the last N calls in the sliding window
	OverallRecentInput     int  // sum of input over the last N calls in the sliding window
	OverallRecentSamples   int  // number of samples in the sliding window (<= recentSampleCap)
	TotalCacheBreaks       int  // number of cache-chain breaks detected live (prefix did not shrink but hits dropped sharply); see noteCacheBreak in usage.go

	// MissingAssistantUsage > 0 usually means the upstream streaming did not send the final usage chunk per the OpenAI
	// stream_options.include_usage protocol (common with self-hosted proxies),
	// so UsageTracker receives no cumulative data at all. The UI uses this to tell the user explicitly to check the backend,
	// so they do not mistake the cache module itself for being broken.
	MissingAssistantUsage int

	// Per-role cache dimension, sorted by CacheRead descending, with roles that consumed no tokens filtered out
	CachePerAgent []AgentCacheStat
	CachePerModel []AgentCacheStat

	// Basic settings
	Synopsis         string
	Premise          string
	Outline          []OutlineSnapshot
	Characters       []string
	SupportingCount  int      // total number of secondary characters in the supporting cast roster
	RecentSupporting []string // recently active secondary characters (at most 5, in descending order of LastSeenChapter)
	Layered          bool
	CurrentVolumeArc string
	NextVolumeTitle  string
	CompassDirection string
	CompassScale     string

	// Details
	LastCommitSummary  string
	LastReviewSummary  string
	LastCheckpointName string
	RecentSummaries    []string
}

// OutlineSnapshot is the display summary of an outline entry.
type OutlineSnapshot struct {
	Chapter   int
	Title     string
	CoreEvent string
}

// AgentSnapshot is the display projection of Agent state.
type AgentSnapshot struct {
	Name      string
	State     string
	TaskID    string
	TaskKind  string
	Summary   string
	Tool      string
	Turn      int
	Context   AgentContextSnapshot
	UpdatedAt time.Time
}

// AgentCacheStat is the cumulative cache-hit figure of a single agent (projected to the left column).
// HitRate = CacheRead / Input; Input has been unified at the litellm layer to the "includes CacheRead" semantics.
//
// CacheCapable distinguishes two kinds of 0% hit:
//   - true  → the model supports prompt cache; 0% means poor prompt design or an unstable prefix and needs optimizing
//   - false → the model/provider does not support prompt cache; 0% is expected and needs no investigation
//
// Recent* is hit data for the sliding window (the last N calls); compared with the cumulative figures it tells "early drag" apart from "steady-state low hits".
type AgentCacheStat struct {
	Role            string
	Model           string
	Input           int
	Output          int
	CacheRead       int
	CacheWrite      int
	Cost            float64
	Saved           float64
	CacheCapable    bool
	RecentCacheRead int
	RecentInput     int
	RecentSamples   int
}

// AgentContextSnapshot is the Agent's context usage.
type AgentContextSnapshot struct {
	Tokens          int
	ContextWindow   int
	Percent         float64
	Scope           string
	Strategy        string
	ActiveMessages  int
	SummaryMessages int
	CompactedCount  int
	KeptCount       int
}

// CoCreateMessage is a message in the co-creation conversation.
type CoCreateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CoCreateReply is the LLM reply in the co-creation conversation. Raw keeps the model's full four-part original text,
// which is written back to history so the next round's model sees its own [DRAFT] from the previous round and thus truly
// accumulates updates on the existing draft (with only Message and no [DRAFT], the model would re-summarize from the conversation every round).
// Suggestions are the AI's proactive "what you might want to say next"; when the user is stuck, pressing a number key fills one into the input box.
type CoCreateReply struct {
	Message     string
	Prompt      string
	Ready       bool
	Suggestions []string
	Raw         string
}
