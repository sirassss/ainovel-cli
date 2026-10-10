package diag

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/store"
)

// Runtime detection thresholds.
const (
	repeatCritical = 8 // recent repeats at or above this count escalate to critical
	streamIdleWarn = 3 // cumulative stream_idle warning threshold
)

// RuntimeRuleFunc is the common signature of runtime diagnostic rules (the counterpart of the authoring-side RuleFunc).
// Its input is the redacted, aggregated RuntimeCapture and it yields report-only Findings, all AutoNone:
// diagnose only, never produce an Action (observer discipline, see architecture.md §2.3).
type RuntimeRuleFunc func(rc *RuntimeCapture) []Finding

var runtimeRules = []RuntimeRuleFunc{
	repeatedErrors,
	stuckStep,
	streamIdleStorm,
}

// runtimeFindings runs all runtime rules.
func runtimeFindings(rc *RuntimeCapture) []Finding {
	var out []Finding
	for _, rule := range runtimeRules {
		out = append(out, rule(rc)...)
	}
	return out
}

// Diagnose is the full entry point of /diag: authoring diagnosis + runtime signals + runtime detection.
// It returns the merged Report and the raw RuntimeCapture (reused by the export to avoid capturing twice).
// Runtime Findings are only merged into Findings for display and never touch Actions, keeping it observe-only.
func Diagnose(s *store.Store) (Report, RuntimeCapture) {
	rep := Analyze(s)
	rc := CaptureRuntime(s)
	rep.Findings = append(rep.Findings, runtimeFindings(&rc)...)
	sortFindings(rep.Findings)
	return rep, rc
}

// repeatedErrors only turns "errors / invalid arguments that recur recently" into Findings.
// It ignores ordinary tool repeats: subagent/novel_context/read_chapter and friends are naturally
// frequent in a long run, so a running total is not a loop signal; real "repeating without
// progress" is caught by stuckStep.
func repeatedErrors(rc *RuntimeCapture) []Finding {
	var out []Finding
	for _, r := range rc.Repeats {
		var rule, title, sugg string
		switch {
		case strings.Contains(r.Sig, " · err: "):
			rule = "RepeatedToolError"
			title = "Tool repeatedly reports the same error"
			sugg = "The same tool keeps returning the same error recently, usually because the model's arguments do not conform or the tool contract mismatches; check agentcore tool validation and the prompt argument conventions (see #34)."
		case strings.Contains(r.Sig, "(args invalid)"):
			rule = "ArgsInvalidLoop"
			title = "Arguments repeatedly fail to parse"
			sugg = "The model keeps retrying with arguments that cannot be parsed; check whether agentcore applies lenient coercion for that type (see #34)."
		default:
			continue // ordinary tool repeats do not produce a Finding
		}
		sev := SevWarning
		if r.Count >= repeatCritical {
			sev = SevCritical
		}
		out = append(out, Finding{
			Rule:       rule,
			Category:   CatFlow,
			Severity:   sev,
			Confidence: ConfHigh,
			AutoLevel:  AutoNone,
			Target:     "runtime.flow",
			Title:      title,
			Evidence:   fmt.Sprintf("`%s` ×%d", r.Sig, r.Count),
			Suggestion: sugg,
		})
	}
	return out
}

// stuckStep detects checkpoints stuck on the same step.
func stuckStep(rc *RuntimeCapture) []Finding {
	if rc.StuckStep == "" {
		return nil
	}
	sev := SevWarning
	if rc.StuckCount >= repeatCritical {
		sev = SevCritical
	}
	return []Finding{{
		Rule:       "StuckStep",
		Category:   CatFlow,
		Severity:   sev,
		Confidence: ConfHigh,
		AutoLevel:  AutoNone,
		Target:     "runtime.flow",
		Title:      "Checkpoint stalled on the same step",
		Evidence:   fmt.Sprintf("Stayed on `%s` ×%d in a row", rc.StuckStep, rc.StuckCount),
		Suggestion: "The same step is written repeatedly without progress; combine with the repeat signatures above to locate which sub-agent is stuck.",
	}}
}

// streamIdleStorm detects frequent stream interruptions (#32).
func streamIdleStorm(rc *RuntimeCapture) []Finding {
	n := rc.LogKinds["stream_idle"]
	if n < streamIdleWarn {
		return nil
	}
	return []Finding{{
		Rule:       "StreamIdleStorm",
		Category:   CatFlow,
		Severity:   SevWarning,
		Confidence: ConfHigh,
		AutoLevel:  AutoNone,
		Target:     "runtime.provider",
		Title:      "Frequent stream interruptions (stream_idle)",
		Evidence:   fmt.Sprintf("stream_idle ×%d", n),
		Suggestion: "The upstream stays silent for a long time and the watchdog kills the stream by mistake; raise streamIdleTimeout for slow-thinking models, or check provider connection stability (see #32).",
	}}
}
