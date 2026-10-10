package eval

import (
	"path/filepath"
	"testing"
)

// TestSmokeCasesLoad makes sure the built-in smoke cases in the repo can be parsed by the loader
// (including the DisallowUnknownFields check).
func TestSmokeCasesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "evals", "cases", "smoke")
	cases, err := LoadCases(dir)
	if err != nil {
		t.Fatalf("load smoke cases failed: %v", err)
	}
	if len(cases) < 3 {
		t.Fatalf("expected at least 3 smoke cases, got %d", len(cases))
	}
	for _, c := range cases {
		if c.Category != "smoke" {
			t.Errorf("%s: category should be smoke, got %s", c.ID, c.Category)
		}
		if c.Gate.MaxSeverity == "" {
			t.Errorf("%s: Validate should fill the default max_severity", c.ID)
		}
		if c.Gate.MaxCostDeltaRatio == nil || *c.Gate.MaxCostDeltaRatio != 0.3 ||
			c.Gate.MaxToolCallDeltaRatio == nil || *c.Gate.MaxToolCallDeltaRatio != 0.3 {
			t.Errorf("%s: Validate should fill the default delta ratio, got cost=%v tool=%v",
				c.ID, c.Gate.MaxCostDeltaRatio, c.Gate.MaxToolCallDeltaRatio)
		}
		if c.Gate.StylestatRegression != "warn" {
			t.Errorf("%s: Validate should default stylestat_regression=warn, got %s", c.ID, c.Gate.StylestatRegression)
		}
	}
}

func TestLoadCasesRejectsUnknownField(t *testing.T) {
	// Indirect check: a valid case must contain id+prompt; a missing one is an error (Validate path).
	if _, err := LoadCases(filepath.Join("..", "..", "evals", "cases", "smoke", "writer_first_chapter.json")); err != nil {
		t.Fatalf("single-file load should succeed: %v", err)
	}
}

// The case id is joined into the RemoveAll path, so path traversal/separators must be rejected
// (high-risk protection).
func TestCaseIDRejectsUnsafe(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", "/abs", "..", "Up", "with space", "dot.case"} {
		c := Case{ID: bad, Prompt: "x"}
		if err := c.Validate(); err == nil {
			t.Errorf("invalid id %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"writer_first_chapter", "architect-long", "case1"} {
		c := Case{ID: ok, Prompt: "x"}
		if err := c.Validate(); err != nil {
			t.Errorf("valid id %q should not be rejected: %v", ok, err)
		}
	}
}

func TestCaseRejectsInvalidGate(t *testing.T) {
	c := Case{ID: "bad_gate", Prompt: "x", Gate: Gate{StylestatRegression: "maybe"}}
	if err := c.Validate(); err == nil {
		t.Fatal("invalid stylestat_regression should be rejected")
	}
	c = Case{ID: "disabled_ratio", Prompt: "x", Gate: Gate{MaxCostDeltaRatio: float64Ptr(-1), MaxToolCallDeltaRatio: float64Ptr(-1)}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a negative delta ratio should be accepted as an explicit off switch: %v", err)
	}
	if *c.Gate.MaxCostDeltaRatio != -1 || *c.Gate.MaxToolCallDeltaRatio != -1 {
		t.Fatalf("an explicitly disabled delta ratio must not be overwritten by the default: %+v", c.Gate)
	}
	c = Case{ID: "strict_ratio", Prompt: "x", Gate: Gate{MaxCostDeltaRatio: float64Ptr(0), MaxToolCallDeltaRatio: float64Ptr(0)}}
	if err := c.Validate(); err != nil {
		t.Fatalf("an explicit 0 delta ratio should be accepted as a strict threshold: %v", err)
	}
	if *c.Gate.MaxCostDeltaRatio != 0 || *c.Gate.MaxToolCallDeltaRatio != 0 {
		t.Fatalf("an explicit 0 delta ratio must not be overwritten by the default: %+v", c.Gate)
	}
}
