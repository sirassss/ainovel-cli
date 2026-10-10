package diag

// Severity is how serious a finding is.
type Severity string

const (
	SevCritical Severity = "critical" // blocks progress or corrupts data
	SevWarning  Severity = "warning"  // may lower quality or waste tokens
	SevInfo     Severity = "info"     // optimization opportunity
)

// Category groups findings by dimension.
type Category string

const (
	CatFlow     Category = "flow"     // stalled flow, abnormal state, recovery problems
	CatQuality  Category = "quality"  // review scores, chapter requirement fulfilment, consistency
	CatPlanning Category = "planning" // outline gaps, foreshadow drift, stale compass
	CatContext  Category = "context"  // character / timeline / relationship anomalies
)

// Confidence is how much a rule trusts its own verdict.
type Confidence string

const (
	ConfHigh   Confidence = "high"   // deterministic, trustworthy
	ConfMedium Confidence = "medium" // heuristic, may misfire
	ConfLow    Confidence = "low"    // rough signal, for reference only
)

// AutoLevel says whether a Finding may be turned into an automated action.
type AutoLevel string

const (
	AutoNone    AutoLevel = "none"    // report only, never automate
	AutoSuggest AutoLevel = "suggest" // suggest an action that needs human confirmation
	AutoSafe    AutoLevel = "safe"    // safe to execute automatically
)

// Finding is one actionable diagnostic result.
type Finding struct {
	Rule       string     // rule name, e.g. "StaleForeshadow"
	Category   Category   // category
	Severity   Severity   // severity
	Confidence Confidence // verdict confidence
	AutoLevel  AutoLevel  // automation level
	Target     string     // suggested target surface, e.g. "runtime.flow"
	Title      string     // one-line summary
	Evidence   string     // concrete data evidence
	Suggestion string     // improvement suggestion (pointing at prompt/flow/config)
}

// RuleFunc is the common signature of diagnostic rules.
type RuleFunc func(snap *Snapshot) []Finding

// ActionKind is the type of a diagnostic action.
type ActionKind string

const (
	ActionEmitNotice      ActionKind = "emit_notice"       // emit a system notice
	ActionEnqueueFollowUp ActionKind = "enqueue_follow_up" // enqueue a follow-up suggestion
)

// Action is an executable action the Planner derives from a high-confidence Finding.
type Action struct {
	SourceRule  string     // source rule name
	Kind        ActionKind // action type
	Severity    Severity   // inherited from the Finding
	Summary     string     // short description
	Message     string     // message handed to the control flow
	Fingerprint string     // stable fingerprint of the source Finding, used for runtime dedup
}

// Stats are the overview metrics shown alongside the findings.
type Stats struct {
	CompletedChapters int
	TotalChapters     int
	TotalWords        int
	AvgWordsPerCh     int
	Phase             string
	Flow              string
	PlanningTier      string
	ReviewCount       int
	RewriteCount      int
	AvgReviewScore    float64
	ForeshadowOpen    int
	ForeshadowStale   int
}

// Report is the complete output of one diagnostic run.
type Report struct {
	Stats    Stats
	Findings []Finding
	Actions  []Action
}
