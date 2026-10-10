// Package imp implements the staged semantic import pipeline for external novels (docs/import-pipeline.md).
//
// The model handles open-ended semantics; code handles coordinates, coverage, types, hashes, ordering and idempotency. All semantic artifacts are
// validated in an isolated workspace (meta/import/) before being published to the real book state. The next action is derived only from artifacts
// (NextAction); there is no stage enum that could drift, and resuming does not depend on from=N.
package imp

import "time"

// Options controls a single import. On resume the fields may be empty; they are derived from the active workspace and the saved Intent.
type Options struct {
	SourcePath      string // required for a new import; may be empty on resume
	AutoConfirm     bool   // --yes: accept the segmentation automatically once coverage validation passes
	StoryResolution string // --story=open|closed: preselected only when synthesis returns uncertain
	ContinueAfter   bool   // --continue: do not create the import-complete Hold
	Guidance        string // --guide: natural-language segmentation guidance; persisted in the workspace so an old segmentation naturally mismatches and is re-identified
	// AcceptSegmentation: explicit manual confirmation (y) after the TUI preview. Lets the current segmentation through once; does not write the intent.
	// Difference from --yes: --yes is blind authorization without seeing the preview and does not let through a segmentation carrying tolerance notes (Notes); y is a ruling made after viewing the preview.
	AcceptSegmentation bool
}

// intent extracts the user authorizations that must be persisted from Options.
func (o Options) intent() Intent {
	return Intent{
		Version:             workspaceSchemaVersion,
		AutoConfirm:         o.AutoConfirm,
		StoryResolution:     o.StoryResolution,
		ContinueAfterImport: o.ContinueAfter,
	}
}

// Stage is the current stage of the import flow. It is for UI display only and is not the recovery source of truth (RFC §14.1).
type Stage string

const (
	StageIngesting            Stage = "ingesting"
	StageSegmenting           Stage = "segmenting"
	StageAwaitingConfirmation Stage = "awaiting_confirmation"
	StageAnalyzing            Stage = "analyzing"
	StageSynthesizing         Stage = "synthesizing"
	StageAwaitingStoryStatus  Stage = "awaiting_story_status"
	StageValidating           Stage = "validating"
	StagePublishing           Stage = "publishing"
	StageDone                 Stage = "done"
	StageError                Stage = "error"
)

// Event is a progress event the import flow emits outward. Event is a projection and takes no part in recovery.
type Event struct {
	Time      time.Time
	Stage     Stage
	Current   int       // chapter/range progress
	Total     int       // total count
	Message   string    // human-readable description
	Level     string    // ""=normal progress; "warn"=warning states such as backoff retry or validation re-ask
	Key       string    // when non-empty, the UI updates consecutive events with the same Key in place (e.g. 7 backoffs change on one line); aligned with the event panel ID mechanism
	RetryAt   time.Time // non-zero = deadline of the next retry; the UI counts down per second from it and clears at the deadline (the request is already in flight)
	Err       error     // carried for StageError
	Continued bool      // set by Host on StageDone: whether the Engine was already started automatically as a relay (--continue × auto)
}
