package rules

import (
	"fmt"
	"maps"
	"strings"
)

// Snapshot is the normalized user-rules snapshot of this book (meta/user_rules.json).
//
// It is the single source of truth at runtime: built by merging the normalized sources when a book is opened/imported/refreshed; afterwards novel_context
// injection and commit_chapter checks both read only this one copy and no longer re-read the rules files (avoiding drift and divergence between two readers).
//
// Only Structured + Preferences are injected into the model (see Payload); Version / Status / Sources /
// Uncertain are operational and diagnostic metadata and do not enter working_memory.user_rules.
type Snapshot struct {
	Version     int        `json:"version"`
	Status      Status     `json:"status"`
	Structured  Structured `json:"structured"`
	Preferences string     `json:"preferences"`
	Sources     []string   `json:"sources"`
	Uncertain   []string   `json:"uncertain"`
}

// Status marks whether snapshot normalization fully succeeded.
type Status string

const (
	// StatusReady means all sources were normalized successfully.
	StatusReady Status = "ready"
	// StatusDegraded means at least one source failed normalization and was downgraded to raw preferences (see Uncertain / the log).
	StatusDegraded Status = "degraded"
)

// SnapshotVersion is the current snapshot schema version, to ease future migration.
// v2: chapter_words left structured (word count is a soft semantic constraint and goes through preferences).
// A v1 snapshot loads compatibly as-is: unknown fields are ignored on deserialization and naturally converge to v2 on the next overlay save;
// "rebuild on version mismatch" is deliberately not done, since that would drop the non-regenerable rules appended at runtime by AddRuntimeRule.
const SnapshotVersion = 2

// Candidate is the candidate result of normalizing a single source.
//
// Sources are handed to BuildSnapshot for deterministic merging after being sorted by priority low -> high. The LLM is only responsible for turning the
// natural language of a single source into candidate Structured/Preferences; priority and field overriding are adjudicated by BuildSnapshot (Go).
type Candidate struct {
	Source      string     // readable source label, enters Snapshot.Sources (e.g. system_defaults / startup_prompt / global:my.md)
	Structured  Structured // structured fields proposed by this source
	Preferences string     // natural-language preference text of this source
	Uncertain   []string   // items this source deliberately did not promote to structured + the reason (diagnostics)
	Degraded    bool       // normalization of this source failed and was downgraded to raw preferences
}

// Payload returns the shape injected into working_memory.user_rules: only structured + preferences are exposed.
// A stable structure is returned even when both are empty, so the LLM does not see user_rules=null and take an abnormal branch.
func (s Snapshot) Payload() map[string]any {
	return map[string]any{
		"structured":  s.Structured,
		"preferences": s.Preferences,
	}
}

// BuildSnapshot deterministically merges candidates, already sorted by priority (low -> high), into a snapshot.
//
// Merge rules (all deterministic on the Go side, not left to the LLM):
//   - structured: overridden per field, higher-priority sources override lower-priority ones; fatigue_words are accumulated per word
//   - preferences: not overridden, concatenated in source order (higher priority last), with source headings
//   - empty/zero values are treated as a missing field and do not override existing values (sanitizeStructured)
//   - any source Degraded -> snapshot status=degraded
func BuildSnapshot(cands []Candidate) Snapshot {
	snap := Snapshot{
		Version: SnapshotVersion,
		Status:  StatusReady,
		Sources: make([]string, 0, len(cands)),
	}
	var prefs []string
	for _, c := range cands {
		s := sanitizeStructured(c.Structured)
		if s.Genre != "" {
			snap.Structured.Genre = s.Genre
		}
		if len(s.ForbiddenChars) > 0 {
			snap.Structured.ForbiddenChars = s.ForbiddenChars
		}
		if len(s.ForbiddenPhrases) > 0 {
			snap.Structured.ForbiddenPhrases = s.ForbiddenPhrases
		}
		if len(s.FatigueWords) > 0 {
			snap.Structured.FatigueWords = mergeFatigueWords(snap.Structured.FatigueWords, s.FatigueWords)
		}

		if p := strings.TrimSpace(c.Preferences); p != "" {
			if src := strings.TrimSpace(c.Source); src != "" {
				prefs = append(prefs, fmt.Sprintf("## [%s]\n\n%s", src, p))
			} else {
				prefs = append(prefs, p)
			}
		}
		if src := strings.TrimSpace(c.Source); src != "" {
			snap.Sources = append(snap.Sources, src)
		}
		snap.Uncertain = append(snap.Uncertain, c.Uncertain...)
		if c.Degraded {
			snap.Status = StatusDegraded
		}
	}
	snap.Preferences = strings.Join(prefs, "\n\n")
	return snap
}

// OverlaySnapshot overlays a higher-priority candidate onto an existing snapshot (the candidate wins).
//
// Used by the runtime Arbiter rules action: it does not re-normalize all sources, it only overlays the new rule onto the current snapshot -
// structured is overridden per field, preferences get one more section appended, sources/uncertain accumulate, and degradation propagates.
func OverlaySnapshot(base Snapshot, cand Candidate) Snapshot {
	out := base
	out.Version = SnapshotVersion
	s := sanitizeStructured(cand.Structured)
	if s.Genre != "" {
		out.Structured.Genre = s.Genre
	}
	if len(s.ForbiddenChars) > 0 {
		out.Structured.ForbiddenChars = s.ForbiddenChars
	}
	if len(s.ForbiddenPhrases) > 0 {
		out.Structured.ForbiddenPhrases = s.ForbiddenPhrases
	}
	if len(s.FatigueWords) > 0 {
		out.Structured.FatigueWords = mergeFatigueWords(cloneFatigue(out.Structured.FatigueWords), s.FatigueWords)
	}
	if p := strings.TrimSpace(cand.Preferences); p != "" {
		section := p
		if src := strings.TrimSpace(cand.Source); src != "" {
			section = fmt.Sprintf("## [%s]\n\n%s", src, p)
		}
		if strings.TrimSpace(out.Preferences) == "" {
			out.Preferences = section
		} else {
			out.Preferences = out.Preferences + "\n\n" + section
		}
	}
	if src := strings.TrimSpace(cand.Source); src != "" {
		out.Sources = append(append([]string{}, out.Sources...), src)
	}
	if len(cand.Uncertain) > 0 {
		out.Uncertain = append(append([]string{}, out.Uncertain...), cand.Uncertain...)
	}
	if cand.Degraded {
		out.Status = StatusDegraded
	}
	return out
}

// mergeFatigueWords accumulates fatigue-word thresholds per word; src overrides the threshold of the same word in dst (nearest wins).
// So the user only needs to add a few fatigue words without relisting the built-in baseline.
func mergeFatigueWords(dst, src map[string]int) map[string]int {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]int, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

func cloneFatigue(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	maps.Copy(out, m)
	return out
}

// SystemDefaults is the mechanical baseline built into the code (the lowest-priority source); it does not go through LLM normalization.
//
// The values were migrated from the front matter of the old assets/rules/default.md. The threshold rationale is preserved as well:
// the late-stage fatigue words ("like one" / "fell silent" / "did not speak" / "X breaths" and the like) come from measurement on 196-chapter long-run output - after the traditional AI cliches were
// wiped out by the early-stage table, the model switched to using these "beat words" 5-7 times per chapter on average, so the thresholds were relaxed to tolerate normal use.
func SystemDefaults() Candidate {
	return Candidate{
		Source: "system_defaults",
		Structured: Structured{
			// Fixed-length AI cliche phrases; the checker does literal substring matching, and patterns with variables ("not X but Y") belong to the semantic layer.
			// Baseline fatigue words / forbidden phrases for the Chinese (zh) mode. han:keep
			ForbiddenPhrases: []string{"某种程度上", "值得注意的是", "不知为何", "五味杂陈"},
			FatigueWords: map[string]int{
				// Baseline fatigue words / forbidden phrases for the Chinese (zh) mode. han:keep
				"不禁": 1, "竟然": 1, "仿佛": 2, "此外": 1, "然而": 2,
				// Baseline fatigue words / forbidden phrases for the Chinese (zh) mode. han:keep
				"一丝": 2, "一抹": 2, "一缕": 2, "宛如": 1, "不由得": 1,
				// Baseline fatigue words / forbidden phrases for the Chinese (zh) mode. han:keep
				"像一": 3, "沉默了": 2, "没有说话": 2, "几息": 3, "一息": 3, "数息": 2,
			},
		},
	}
}

// sanitizeStructured enforces "empty/zero value = field missing": the normalizer may emit placeholders such as genre:"",
// (observed in the prototype), which must be treated as undeclared to avoid polluting merging and mechanical checks.
func sanitizeStructured(s Structured) Structured {
	out := Structured{}
	if g := strings.TrimSpace(s.Genre); g != "" {
		out.Genre = g
	}
	out.ForbiddenChars = nonEmptyStrings(s.ForbiddenChars)
	out.ForbiddenPhrases = nonEmptyStrings(s.ForbiddenPhrases)
	out.FatigueWords = sanitizeFatigueWords(s.FatigueWords)
	return out
}

func nonEmptyStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func sanitizeFatigueWords(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for w, n := range m {
		if w = strings.TrimSpace(w); w == "" || n <= 0 {
			continue
		}
		out[w] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
