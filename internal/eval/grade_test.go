package eval

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/stylestat"
)

// writerSmokeCase is a typical writer first-chapter smoke case used by the gate tests.
func writerSmokeCase() Case {
	c := Case{
		ID:          "writer_first_chapter",
		Category:    "smoke",
		Role:        "writer",
		Prompt:      "写一本修仙小说",
		MaxChapters: 1,
		Expect: Expect{
			Phase:                "writing",
			MinCompletedChapters: 1,
			RequiredCheckpoints:  []string{"chapter:1:plan", "chapter:1:draft", "chapter:1:commit"},
			NoPending:            []string{"pending_commit", "pending_steer"},
		},
	}
	_ = c.Validate() // fills the default max_severity
	return c
}

// cleanCollected builds a collected result for "one chapter completed normally" (no findings, no
// leftovers, contract satisfied).
func cleanCollected() Collected {
	return Collected{
		Dir:      "/fake",
		Report:   diag.Report{Stats: diag.Stats{CompletedChapters: 1, TotalChapters: 1, Phase: "writing", Flow: "writing"}},
		Progress: &domain.Progress{Phase: domain.PhaseWriting, CompletedChapters: []int{1}},
		Checkpoints: []domain.Checkpoint{
			{Scope: domain.ChapterScope(1), Step: "plan"},
			{Scope: domain.ChapterScope(1), Step: "draft"},
			{Scope: domain.ChapterScope(1), Step: "commit"},
		},
		Pending: map[string]bool{},
	}
}

func TestGradePassesCleanRun(t *testing.T) {
	r := Grade(writerSmokeCase(), cleanCollected())
	if r.Outcome != Pass {
		t.Fatalf("expected PASS, got %s; hard fails=%v", r.Outcome, r.HardFails)
	}
	if len(r.Passed) == 0 {
		t.Fatal("expected passed contract records")
	}
}

// Core assumption: a writer that skips commit must be stopped.
func TestGradeCatchesMissingCommit(t *testing.T) {
	col := cleanCollected()
	col.Checkpoints = col.Checkpoints[:2] // drop commit
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("missing commit should FAIL, got %s", r.Outcome)
	}
	if !hasIssue(r.HardFails, "contract:checkpoint", "chapter:1:commit") {
		t.Fatalf("should report missing chapter:1:commit, got %+v", r.HardFails)
	}
}

// Core assumption: leftover pending signals must be stopped.
func TestGradeCatchesPendingResidual(t *testing.T) {
	col := cleanCollected()
	col.Pending["pending_commit"] = true
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("leftover pending should FAIL, got %s", r.Outcome)
	}
	if !hasIssue(r.HardFails, "contract:no_pending", "pending_commit") {
		t.Fatalf("should report leftover pending_commit, got %+v", r.HardFails)
	}
}

// Core assumption: a phase mismatch must be stopped.
func TestGradeCatchesPhaseMismatch(t *testing.T) {
	col := cleanCollected()
	col.Progress.Phase = domain.PhaseOutline // has not entered writing yet
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("phase mismatch should FAIL, got %s", r.Outcome)
	}
	if !hasIssue(r.HardFails, "contract:phase", "outline") {
		t.Fatalf("should report the phase mismatch, got %+v", r.HardFails)
	}
}

func TestGradeMinChaptersNotMet(t *testing.T) {
	col := cleanCollected()
	col.Report.Stats.CompletedChapters = 0
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("not reaching min_completed_chapters should FAIL, got %s", r.Outcome)
	}
}

// A critical finding triggers a hard fail; a warning finding only WARNs (default
// max_severity=warning).
func TestGradeFindingSeverity(t *testing.T) {
	crit := cleanCollected()
	crit.Report.Findings = []diag.Finding{{Rule: "PhaseFlowMismatch", Severity: diag.SevCritical, Title: "state machine anomaly"}}
	if r := Grade(writerSmokeCase(), crit); r.Outcome != Fail {
		t.Fatalf("critical finding should FAIL, got %s", r.Outcome)
	}

	warn := cleanCollected()
	warn.Report.Findings = []diag.Finding{{Rule: "RewritePendingPressure", Severity: diag.SevWarning, Title: "rewrite backlog"}}
	r := Grade(writerSmokeCase(), warn)
	if r.Outcome != Warn {
		t.Fatalf("warning finding should WARN, got %s", r.Outcome)
	}

	// An info finding is an informational Note and must not push a clean case to WARN.
	info := cleanCollected()
	info.Report.Findings = []diag.Finding{{Rule: "GhostCharacter", Severity: diag.SevInfo, Title: "character absent for a long time"}}
	ri := Grade(writerSmokeCase(), info)
	if ri.Outcome != Pass {
		t.Fatalf("info finding must not change the gate, expected PASS, got %s", ri.Outcome)
	}
	if len(ri.Notes) != 1 {
		t.Fatalf("info finding should go to Notes, got %d", len(ri.Notes))
	}
}

func TestGradeRuntimeErrorFails(t *testing.T) {
	col := cleanCollected()
	col.RuntimeErr = "stream EOF"
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("runtime error should FAIL, got %s", r.Outcome)
	}
}

// A broken artifact the contract depends on must not falsely pass; it must hard fail (fail-loud).
func TestGradeLoadErrorFails(t *testing.T) {
	col := cleanCollected()
	col.LoadErrors = []string{"pending_commit: unexpected end of JSON input"}
	r := Grade(writerSmokeCase(), col)
	if r.Outcome != Fail {
		t.Fatalf("artifact read failure should FAIL, got %s", r.Outcome)
	}
	if !hasIssue(r.HardFails, "load", "pending_commit") {
		t.Fatalf("should report the load failure, got %+v", r.HardFails)
	}
}

func TestGradeDeltaStylestatWarnAndBlock(t *testing.T) {
	base := cleanResult()
	base.Metrics.Stylestat = &stylestat.Stats{
		Patterns: []stylestat.PatternStat{{Name: "p", PerChapter: 1}},
		Ending:   stylestat.EndingStat{ShortRatio: 0.2},
	}
	variant := cleanResult()
	variant.Metrics.Stylestat = &stylestat.Stats{
		Patterns:          []stylestat.PatternStat{{Name: "p", PerChapter: 2}},
		RepeatedSentences: []stylestat.SentenceStat{{Text: "重复句", Chapters: 3, Count: 3}},
		Ending:            stylestat.EndingStat{ShortRatio: 0.5},
	}

	c := writerSmokeCase()
	c.Gate.StylestatRegression = "warn"
	d := GradeDelta(c, base, variant)
	if d.Outcome != Warn {
		t.Fatalf("stylestat regression should WARN by default, got %s", d.Outcome)
	}
	if !hasIssue(d.Warnings, "delta:stylestat", "style metrics regressed") {
		t.Fatalf("should report the stylestat warning, got %+v", d.Warnings)
	}

	c.Gate.StylestatRegression = "block"
	d = GradeDelta(c, base, variant)
	if d.Outcome != Fail {
		t.Fatalf("stylestat block should FAIL, got %s", d.Outcome)
	}
	if !hasIssue(d.HardFails, "delta:stylestat", "style metrics regressed") {
		t.Fatalf("should report the stylestat hard fail, got %+v", d.HardFails)
	}
}

func TestGradeDeltaTitleMixedUsesMinorityCount(t *testing.T) {
	base := cleanResult()
	base.Metrics.Stylestat = &stylestat.Stats{
		TitleFormats: &stylestat.TitleStat{WithPrefix: 2, WithoutPrefix: 3},
	}
	variant := cleanResult()
	variant.Metrics.Stylestat = &stylestat.Stats{
		TitleFormats: &stylestat.TitleStat{WithPrefix: 2, WithoutPrefix: 5},
	}

	d := GradeDelta(writerSmokeCase(), base, variant)
	if d.Metrics.Stylestat == nil {
		t.Fatal("should produce a stylestat delta")
	}
	if d.Metrics.Stylestat.TitleMixedDelta != 0 {
		t.Fatalf("must not misreport a mixed-title regression when the minority format did not grow, got %+d", d.Metrics.Stylestat.TitleMixedDelta)
	}
	if d.Outcome != Pass {
		t.Fatalf("growing only the majority title count must not change the gate, got %s issues=%+v", d.Outcome, d.Warnings)
	}
}

func TestGradeDeltaCostAndToolCallThresholds(t *testing.T) {
	base := cleanResult()
	base.Metrics.ToolCalls = 10
	base.Metrics.Usage = UsageMetrics{UsageRecorded: true, CostUSD: 1, Input: 100, Output: 100}
	variant := cleanResult()
	variant.Metrics.ToolCalls = 14
	variant.Metrics.Usage = UsageMetrics{UsageRecorded: true, CostUSD: 1.4, Input: 150, Output: 140}

	c := writerSmokeCase()
	c.Gate.MaxToolCallDeltaRatio = float64Ptr(0.3)
	c.Gate.MaxCostDeltaRatio = float64Ptr(0.3)
	d := GradeDelta(c, base, variant)
	if d.Outcome != Warn {
		t.Fatalf("cost/tool_calls above threshold should WARN, got %s", d.Outcome)
	}
	if !hasIssue(d.Warnings, "delta:tool_calls", "above the threshold") {
		t.Fatalf("should report the tool_calls regression, got %+v", d.Warnings)
	}
	if !hasIssue(d.Warnings, "delta:cost", "above the threshold") {
		t.Fatalf("should report the cost regression, got %+v", d.Warnings)
	}
}

func TestGradeDeltaInsufficientStylestatIsNote(t *testing.T) {
	d := GradeDelta(writerSmokeCase(), cleanResult(), cleanResult())
	if d.Outcome != Pass {
		t.Fatalf("insufficient sample must not change the gate, got %s", d.Outcome)
	}
	if !hasIssue(d.Notes, "stylestat", "insufficient sample") {
		t.Fatalf("should record the stylestat insufficient-sample note, got %+v", d.Notes)
	}
}

func TestParseCheckpointSpec(t *testing.T) {
	cases := []struct {
		spec  string
		kind  domain.ScopeKind
		step  string
		valid bool
	}{
		{"chapter:1:commit", domain.ScopeChapter, "commit", true},
		{"arc:1:2:arc_summary", domain.ScopeArc, "arc_summary", true},
		{"volume:3:volume_summary", domain.ScopeVolume, "volume_summary", true},
		{"global:layered_outline", domain.ScopeGlobal, "layered_outline", true},
		{"chapter:commit", "", "", false},
		{"bogus:1:x", "", "", false},
	}
	for _, tc := range cases {
		scope, step, err := parseCheckpointSpec(tc.spec)
		if tc.valid && err != nil {
			t.Errorf("%s: expected parse success, got %v", tc.spec, err)
		}
		if !tc.valid {
			if err == nil {
				t.Errorf("%s: expected parse failure", tc.spec)
			}
			continue
		}
		if scope.Kind != tc.kind || step != tc.step {
			t.Errorf("%s: parsed as kind=%s step=%s", tc.spec, scope.Kind, step)
		}
	}
}

func cleanResult() Result {
	r := Grade(writerSmokeCase(), cleanCollected())
	r.Metrics.TotalWords = 1000
	return r
}

// TestCollectReadsCheckpoints verifies the real store read path: after writing a checkpoint,
// Collect can satisfy the contract.
func TestCollectReadsCheckpoints(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if _, err := s.Checkpoints.Append(domain.ChapterScope(1), "commit", "chapters/01.md", "d1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}

	col := Collect(dir, nil)
	ok, err := col.HasCheckpoint("chapter:1:commit")
	if err != nil || !ok {
		t.Fatalf("should match chapter:1:commit, ok=%v err=%v", ok, err)
	}
	if miss, _ := col.HasCheckpoint("chapter:2:commit"); miss {
		t.Fatal("must not match the nonexistent chapter:2:commit")
	}
	if col.Pending["pending_commit"] {
		t.Fatal("a clean directory must not have a leftover pending_commit")
	}
}

func hasIssue(issues []Issue, source, detailContains string) bool {
	for _, it := range issues {
		if it.Source == source && strings.Contains(it.Detail, detailContains) {
			return true
		}
	}
	return false
}

// TestSeverityRanksMatchDiag pins the severity strings eval matches on to the diag constants
// that produce them.
func TestSeverityRanksMatchDiag(t *testing.T) {
	for want, sev := range map[int]diag.Severity{0: diag.SevCritical, 1: diag.SevWarning, 2: diag.SevInfo} {
		if !validSeverity(string(sev)) || severityRank(string(sev)) != want {
			t.Errorf("severity %q: rank=%d, want %d", sev, severityRank(string(sev)), want)
		}
	}
}
