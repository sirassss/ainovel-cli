// Package eval is the offline evaluation harness of ainovel-cli.
//
// Design stance: the evaluators (deterministic diagnosis diag, whole-book style stylestat, the
// seven-dimension rubric) already exist in the project, so eval is only a thin layer: it drives
// cases in batch, collects the output, maps diag Findings and the case contract to a gate, and
// aggregates the report. There is one definition of each fact; the evaluation layer does not
// re-implement the judgment. See docs/evaluation-system.md.
//
// The deterministic main line is covered: single-arm gate, baseline/variant A/B delta, repeat
// aggregation and stylestat regression. The LLM Judge remains an optional later layer and must
// not pollute the deterministic gate.
package eval

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// caseIDPattern restricts the case id to safe characters: the id is joined into the output
// directory and cleaned by RunCase's RemoveAll, so path characters such as . and / are
// forbidden, which rules out "../" path traversal deleting outside the workspace.
var caseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const defaultDeltaRatio = 0.3

// Case is an evaluation sample: a writing request plus a set of fact-layer assertions.
type Case struct {
	ID            string   `json:"id"`
	Category      string   `json:"category"`       // evaluation layer: smoke/workflow/quality/longform/recovery/steering
	Role          string   `json:"role,omitempty"` // role under test: writer/architect/editor (orthogonal to Category)
	Description   string   `json:"description,omitempty"`
	Prompt        string   `json:"prompt"`                   // the user's writing request
	Style         string   `json:"style,omitempty"`          // overrides the configured style
	MaxChapters   int      `json:"max_chapters"`             // chapter cap; 0 means run only until planning completes (enter writing)
	TargetPrompts []string `json:"target_prompts,omitempty"` // prompt files this case mainly verifies (informational)
	Rubric        string   `json:"rubric,omitempty"`         // LLM Judge scoring sheet (enabled in Phase 3)
	Expect        Expect   `json:"expect"`
	Gate          Gate     `json:"gate"`
}

// Expect holds the case-level contract assertions: only expectations that the generic diag rules
// do not cover and that are strongly tied to this case.
type Expect struct {
	Phase                string   `json:"phase,omitempty"`                  // expected final phase
	MinCompletedChapters int      `json:"min_completed_chapters,omitempty"` // minimum number of completed chapters
	RequiredCheckpoints  []string `json:"required_checkpoints,omitempty"`   // like "chapter:1:commit" / "arc:1:1:arc_summary" / "global:layered_outline"
	NoPending            []string `json:"no_pending,omitempty"`             // signals that must be cleared at the end: pending_commit/pending_steer/last_commit/last_review
}

// Gate holds the gate thresholds of this case. Only MaxSeverity is used for now; the other fields
// are reserved for the A/B (regression) stage, parsed but not part of the gate. They are kept so
// case files can follow the full schema in docs/evaluation-system.md.
type Gate struct {
	MaxSeverity string `json:"max_severity,omitempty"` // highest diag Finding severity allowed (default warning): anything above is a hard fail

	MaxCostDeltaRatio     *float64 `json:"max_cost_delta_ratio,omitempty"`
	MaxToolCallDeltaRatio *float64 `json:"max_tool_call_delta_ratio,omitempty"`
	StylestatRegression   string   `json:"stylestat_regression,omitempty"`
}

// Validate checks the required case fields.
func (c *Case) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("case is missing id")
	}
	if !caseIDPattern.MatchString(c.ID) {
		return fmt.Errorf("invalid case id %q: only lowercase letters, digits, underscores and hyphens are allowed, with no path characters", c.ID)
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return fmt.Errorf("case %q is missing prompt", c.ID)
	}
	if c.Gate.MaxSeverity == "" {
		c.Gate.MaxSeverity = "warning"
	}
	if !validSeverity(c.Gate.MaxSeverity) {
		return fmt.Errorf("case %q has invalid gate.max_severity: %s", c.ID, c.Gate.MaxSeverity)
	}
	if c.Gate.MaxCostDeltaRatio == nil {
		c.Gate.MaxCostDeltaRatio = float64Ptr(defaultDeltaRatio)
	}
	if c.Gate.MaxToolCallDeltaRatio == nil {
		c.Gate.MaxToolCallDeltaRatio = float64Ptr(defaultDeltaRatio)
	}
	if c.Gate.StylestatRegression == "" {
		c.Gate.StylestatRegression = "warn"
	}
	if !validStylestatGate(c.Gate.StylestatRegression) {
		return fmt.Errorf("case %q has invalid gate.stylestat_regression: %s", c.ID, c.Gate.StylestatRegression)
	}
	return nil
}

func float64Ptr(v float64) *float64 { return &v }

func validStylestatGate(s string) bool {
	switch s {
	case "warn", "block", "off":
		return true
	default:
		return false
	}
}

// LoadCases loads cases from a single .json file or a directory. All *.json files under a
// directory are loaded recursively and sorted by id.
func LoadCases(path string) ([]Case, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	var files []string
	if info.IsDir() {
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".json") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		files = []string{path}
	}

	var cases []Case
	seen := map[string]string{}
	for _, f := range files {
		c, err := loadCaseFile(f)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("duplicate case id: %q (%s and %s)", c.ID, prev, f)
		}
		seen[c.ID] = f
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no case found: %s", path)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

func loadCaseFile(path string) (Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Case{}, err
	}
	var c Case
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // a misspelled field is an error, never silently ignored
	if err := dec.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("parse case %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Case{}, err
	}
	return c, nil
}
