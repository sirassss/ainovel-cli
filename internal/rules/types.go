// Package rules implements the input layer (Policy) of user preferences: it normalizes and merges the writing rules from each source into
// the snapshot of this book (see snapshot.go); at runtime novel_context injects it and commit_chapter runs the mechanical checks.
//
// Rule is the fourth kind of fact, alongside Progress / Checkpoint / Artifact but of the opposite nature:
// the first three are system outputs, while Rule is the persisted input of user intent.
//
// Design constraints (non-negotiable):
//   - tools return facts only, never instructions (a Violation is a fact; the editor decides whether to trigger a rewrite)
//   - no new verdict path is introduced (reuse PendingRewrites)
//   - no strictness field is introduced (severity is fixed by rule type; the editor adjudicates semantics on its own)
//   - the Flow Router is untouched (rules do not take part in routing)
package rules

// SourceKind marks the origin of a rules file, used only to generate the source label (e.g. global:my-style.md).
type SourceKind int

const (
	// SourceGlobal - the user's global preferences (all .md files under ~/.ainovel/rules/, merged in filename lexicographic order), reused across books.
	SourceGlobal SourceKind = iota
	// SourceProject - this book's rules (all .md files under ./.ainovel/rules/, merged in filename lexicographic order), highest priority.
	SourceProject
)

// String returns the readable name of the source, used as the source-label prefix.
func (k SourceKind) String() string {
	switch k {
	case SourceGlobal:
		return "global"
	case SourceProject:
		return "project"
	default:
		return "unknown"
	}
}

// Structured holds the mechanically checkable structured rule fields (the candidate/merged result after normalizing each source).
// Chapter word count is deliberately not listed here: how long a chapter should be is a question of narrative completeness, a matter of semantic discretion (writer/editor),
// and digitizing it into a mechanical hard line would tempt the model to pad text to cross the line - the word-count wish goes through the natural-language preferences channel.
type Structured struct {
	Genre            string         `json:"genre,omitempty"`
	ForbiddenChars   []string       `json:"forbidden_chars,omitempty"`
	ForbiddenPhrases []string       `json:"forbidden_phrases,omitempty"`
	FatigueWords     map[string]int `json:"fatigue_words,omitempty"`
}

// IsEmpty determines whether there are no structured rules at all; the checker can skip on that basis.
func (s Structured) IsEmpty() bool {
	return s.Genre == "" &&
		len(s.ForbiddenChars) == 0 &&
		len(s.ForbiddenPhrases) == 0 &&
		len(s.FatigueWords) == 0
}

// Severity marks the severity level of a Violation.
// Fixed mapping (not user-configurable):
//
//	forbidden_chars present          -> Error
//	forbidden_phrases present        -> Error
//	fatigue_words over threshold     -> Warning
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Violation is the checker's output: a statement of fact that this chapter violated some mechanical rule.
//
// Note: commit_chapter passes violations through into the returned JSON and does not block the commit;
// when reviewing, the editor maps these facts onto the existing seven dimensions (aesthetic/pacing/character/consistency),
// and the LLM decides on its own whether to escalate the verdict to trigger polish/rewrite.
type Violation struct {
	Rule     string   `json:"rule"`             // forbidden_chars / forbidden_phrases / fatigue_words
	Target   string   `json:"target,omitempty"` // the specific offending object (which word/character)
	Limit    any      `json:"limit,omitempty"`  // threshold; fatigue_words=int / forbidden_*=empty
	Actual   any      `json:"actual"`           // actual value: number of occurrences
	Severity Severity `json:"severity"`         // error / warning
}
