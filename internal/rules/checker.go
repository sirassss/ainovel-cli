package rules

import (
	"strings"
)

// Check mechanically checks chapter content against the structured rules and returns a list of violation facts.
//
// Design contract:
//   - returns facts only, never issues instructions (Iron Rule 1)
//   - never blocks any caller's flow
//   - severity is fixed per rule type (see the table in the types.go comments)
//
// Parameters:
//   - text: chapter content (either final or draft)
//   - s: the merged structured rules; returns nil immediately when IsEmpty.
func Check(text string, s Structured) []Violation {
	if s.IsEmpty() {
		return nil
	}

	var violations []Violation
	violations = appendForbiddenChars(violations, text, s.ForbiddenChars)
	violations = appendForbiddenPhrases(violations, text, s.ForbiddenPhrases)
	violations = appendFatigueWords(violations, text, s.FatigueWords)
	return violations
}

// forbidden_chars: error as soon as it appears at least once.
// A single rule produces only one violation; actual is the number of occurrences.
func appendForbiddenChars(vs []Violation, text string, list []string) []Violation {
	for _, ch := range list {
		if ch == "" {
			continue
		}
		n := strings.Count(text, ch)
		if n == 0 {
			continue
		}
		vs = append(vs, Violation{
			Rule:     "forbidden_chars",
			Target:   ch,
			Actual:   n,
			Severity: SeverityError,
		})
	}
	return vs
}

// forbidden_phrases: error as soon as it appears at least once; behaves like forbidden_chars, only the rule name differs.
func appendForbiddenPhrases(vs []Violation, text string, list []string) []Violation {
	for _, ph := range list {
		if ph == "" {
			continue
		}
		n := strings.Count(text, ph)
		if n == 0 {
			continue
		}
		vs = append(vs, Violation{
			Rule:     "forbidden_phrases",
			Target:   ph,
			Actual:   n,
			Severity: SeverityError,
		})
	}
	return vs
}

// fatigue_words: a violation only when the occurrence count in this chapter exceeds the threshold; warning level.
// Not accumulated across chapters; cross-chapter issues are left to later diagnostics.
func appendFatigueWords(vs []Violation, text string, m map[string]int) []Violation {
	for word, limit := range m {
		if word == "" || limit <= 0 {
			continue
		}
		n := strings.Count(text, word)
		if n <= limit {
			continue
		}
		vs = append(vs, Violation{
			Rule:     "fatigue_words",
			Target:   word,
			Limit:    limit,
			Actual:   n,
			Severity: SeverityWarning,
		})
	}
	return vs
}
