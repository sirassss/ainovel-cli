package diag

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// TestRuntimeFindings_Classify proves repeat signatures are classified by shape, thresholds
// escalate and de-escalate correctly, and every runtime Finding is AutoNone (observer
// discipline: diagnose only, never produce an Action).
func TestRuntimeFindings_Classify(t *testing.T) {
	rc := RuntimeCapture{
		Repeats: []RepeatStat{
			{Sig: "writer-ch07 · err: InputValidationError", Count: 14}, // error loop: critical
			{Sig: "writer-ch07 · novel_context", Count: 45},             // normal high-frequency tool: no Finding
			{Sig: "writer · save_plan (args invalid)", Count: 4},        // invalid arguments: warning
		},
		StuckStep:  "writing.commit_ch07",
		StuckCount: 9, // stuck: critical
		LogKinds:   map[string]int{"stream_idle": 4},
		LogErrors:  270, // accumulated over a long run, must not produce a Finding on its own
	}

	fs := runtimeFindings(&rc)
	sev := map[string]Severity{}
	for _, f := range fs {
		sev[f.Rule] = f.Severity
		if f.AutoLevel != AutoNone {
			t.Errorf("%s should be AutoNone (observer discipline), got %s", f.Rule, f.AutoLevel)
		}
	}

	want := map[string]Severity{
		"RepeatedToolError": SevCritical,
		"ArgsInvalidLoop":   SevWarning,
		"StuckStep":         SevCritical,
		"StreamIdleStorm":   SevWarning,
	}
	for rule, w := range want {
		if sev[rule] != w {
			t.Errorf("%s: got %q want %q", rule, sev[rule], w)
		}
	}
	// Normal high-frequency tools and accumulated log errors must not produce a Finding (avoids false alarms in long runs).
	if _, ok := sev["RepeatedToolCall"]; ok {
		t.Error("Ordinary tool repeats should not produce a Finding")
	}
	if _, ok := sev["LogErrorBurst"]; ok {
		t.Error("Accumulated log errors should not produce a Finding on their own")
	}
}

// TestRuntimeFindings_Quiet proves no runtime Finding is produced when there is no anomaly signal (zero false positives).
func TestRuntimeFindings_Quiet(t *testing.T) {
	rc := RuntimeCapture{
		LogKinds:  map[string]int{"stream_idle": 1}, // below the threshold
		LogErrors: 2,
	}
	if fs := runtimeFindings(&rc); len(fs) != 0 {
		t.Errorf("A quiet state should produce no Finding, got %d: %+v", len(fs), fs)
	}
}

// TestRuntimeRulesFireOnCurrentStrings pins the runtime rules to the strings their
// producers emit today: signatures come from aggregateRepeats, the stuck step from
// analyzeCheckpoints, and the stream_idle count from captureLog parsing a slog
// text-handler line (level=WARN ... kind=stream_idle).
func TestRuntimeRulesFireOnCurrentStrings(t *testing.T) {
	var evs []SkelEvent
	for i := 0; i < repeatCritical; i++ {
		evs = append(evs,
			SkelEvent{Agent: "writer", Role: "tool", ErrClass: "InputValidationError: chapter"},
			SkelEvent{Agent: "writer", Role: "assistant", Tools: []SkelTool{{Name: "save_plan", Invalid: true}}},
		)
	}
	repeats := map[string]int{}
	aggregateRepeats("writer", evs, repeats, map[string]int{})

	var cps []domain.Checkpoint
	for i := 0; i < repeatMin; i++ {
		cps = append(cps, domain.Checkpoint{Scope: domain.ChapterScope(7), Step: "commit"})
	}
	cur, stuck, n := analyzeCheckpoints(cps)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `time=2026-10-10T12:00:00.000+07:00 level=WARN msg="stream idle" module=event kind=stream_idle` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "logs", "tui.log"), []byte(line+line+line), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := RuntimeCapture{LogKinds: map[string]int{}, CurrentStep: cur, StuckStep: stuck, StuckCount: n, Repeats: topRepeats(repeats)}
	captureLog(dir, &rc)

	got := map[string]Severity{}
	for _, rule := range runtimeRules {
		for _, f := range rule(&rc) {
			got[f.Rule] = f.Severity
		}
	}
	want := map[string]Severity{
		"RepeatedToolError": SevCritical,
		"ArgsInvalidLoop":   SevCritical,
		"StuckStep":         SevWarning,
		"StreamIdleStorm":   SevWarning,
	}
	for rule, sev := range want {
		if got[rule] != sev {
			t.Errorf("%s: got %q want %q (all: %v, repeats: %v)", rule, got[rule], sev, got, rc.Repeats)
		}
	}
}
