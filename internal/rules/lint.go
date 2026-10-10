package rules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Lint is the built-in product-baseline check: it scans chapter content for mechanical residue, independent of user rules, and always runs at commit time.
// Same contract as Check: facts only (Iron Rule 1), never blocks the flow; the editor/user decides.
//
// Three categories at present (all backed by evidence from real long-run output):
//   - markdown_residue: leftover ** bold and # heading lines other than the first line in the content (an exported txt would expose the raw symbols)
//   - non_cjk_fragments / cjk_leak: mixed-script fragments; the direction is determined automatically by the dominant script of the content
//     (Chinese content reports Latin fragments; content in Latin script such as Vietnamese reports Han fragments)
func Lint(text string) []Violation {
	var vs []Violation
	vs = appendMarkdownResidue(vs, text)
	vs = appendScriptMixing(vs, text)
	vs = appendBrokenWords(vs, text)
	vs = appendSelfDuplication(vs, text)
	vs = appendEnglishResidue(vs, text)
	return vs
}

func appendMarkdownResidue(vs []Violation, text string) []Violation {
	if n := strings.Count(text, "**"); n > 0 {
		vs = append(vs, Violation{
			Rule:     "markdown_residue",
			Target:   "**",
			Actual:   n,
			Severity: SeverityWarning,
		})
	}
	headings := 0
	seenContent := false
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// A # heading on the first non-empty line is the legal format of a chapter file (not hard-coded by line number, so leading blank lines are tolerated)
		first := !seenContent
		seenContent = true
		if !first && strings.HasPrefix(t, "#") {
			headings++
		}
	}
	if headings > 0 {
		vs = append(vs, Violation{
			Rule:     "markdown_residue",
			Target:   "#",
			Actual:   headings,
			Severity: SeverityWarning,
		})
	}
	return vs
}

var (
	latinFragmentRe = regexp.MustCompile(`[A-Za-z]{2,}`)
	// Includes CJK punctuation: what leaks into Vietnamese content is not only Han characters but also full-width punctuation.
	// Measured: a lone full-width period in the content of chapter 4 - a Han-only rule could not see it at all.
	cjkFragmentRe = regexp.MustCompile(`[\p{Han}\x{3000}-\x{303f}\x{ff01}-\x{ff5e}]+`)
)

// appendScriptMixing reports fragments of "another script" mixed into the chapter content.
//
// Which script counts as the intruder is decided by the content itself, not read from config: a bare "pattern" mixed into Chinese content is a defect,
// whereas every word of Vietnamese content is in Latin letters, so the same rule would raise false alarms over a thousand times per chapter and drown the review
// context - measured: one chapter hit 1021 times though it contained not a single Han character. Conversely, a leaked Han phrase
// in Vietnamese content is a real defect that the old rule could not see at all.
//
// So first determine the dominant script by character share, then report only the minority. Both genres hold on their own, and a misconfigured
// setting cannot make it stop working. Legitimate loanwords (brand names/abbreviations) still hit - a warning-level fact, adjudicated by the review.
func appendScriptMixing(vs []Violation, text string) []Violation {
	latin := latinFragmentRe.FindAllString(text, -1)
	han := cjkFragmentRe.FindAllString(text, -1)

	// What is compared is the "number of Han characters" against the "number of Latin words", not the character counts on each side: a Han character is about one word,
	// while a Latin word has several letters. Comparing by character count, a few "pattern"/"DNA" mixed into Chinese content
	// would push the Latin character count above the Han count and misjudge the language of the content.
	rule, matches := "non_cjk_fragments", latin
	if runeCount(han) <= len(latin) {
		// The content is in Latin script (Vietnamese etc.): Han characters are the intruders.
		rule, matches = "cjk_leak", han
	}
	if len(matches) == 0 {
		return vs
	}

	seen := make(map[string]struct{})
	var examples []string
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		if len(examples) < 3 {
			examples = append(examples, m)
		}
	}
	return append(vs, Violation{
		Rule:     rule,
		Target:   strings.Join(examples, ", "),
		Actual:   len(matches),
		Severity: SeverityWarning,
	})
}

func runeCount(ss []string) int {
	n := 0
	for _, s := range ss {
		n += utf8.RuneCountInString(s)
	}
	return n
}

// brokenWordRe matches a word cut apart by a paragraph break: a line ends with a letter and, after a blank line, continues with a lowercase letter.
// Measured on Vietnamese content: a person's name was split in two - the first half ended one paragraph and the rest started the next after a blank line.
// A word in Latin script never spans paragraphs, so this shape has no legitimate form and can be judged a defect directly.
var brokenWordRe = regexp.MustCompile(`(?m)[\p{L}]\n\s*\n[[:space:]]*[\p{Ll}]`)

func appendBrokenWords(vs []Violation, text string) []Violation {
	matches := brokenWordRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return vs
	}
	return append(vs, Violation{
		Rule:     "broken_word",
		Target:   strings.Join(strings.Fields(matches[0]), "⏎"),
		Actual:   len(matches),
		Severity: SeverityWarning,
	})
}

// paraMinRunes is the minimum paragraph length that takes part in duplicate comparison. Short paragraphs naturally repeat
// (a short action beat, a one-word exclamation); only verbatim recurrence of full paragraphs of text is a generation incident.
const paraMinRunes = 20

// paragraphs splits out content paragraphs long enough to be worth comparing, skipping heading lines.
func paragraphs(text string) []string {
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "#") {
			continue
		}
		if utf8.RuneCountInString(p) >= paraMinRunes {
			out = append(out, p)
		}
	}
	return out
}

// selfDupThreshold / crossDupThreshold are the upper limits of the proportion of duplicated characters in this chapter.
//
// The thresholds come from measurement: in a 22-chapter book, 19 chapters were 0.0% on both, and the three problem chapters were
// self-duplication 43.5% (chapter 21, the same paragraph appearing 5 times), cross-chapter 32.2% (chapter 14 copied chapter 13)
// and 12.9% (chapter 22 copied chapter 21). The signal is binary with no gray zone, so 10% is chosen
// to leave ample room for "deliberately repeated refrains/incantations".
const (
	selfDupThreshold  = 0.10
	crossDupThreshold = 0.10
)

func appendSelfDuplication(vs []Violation, text string) []Violation {
	paras := paragraphs(text)
	if len(paras) == 0 {
		return vs
	}
	count := make(map[string]int, len(paras))
	total := 0
	for _, p := range paras {
		count[p]++
		total += utf8.RuneCountInString(p)
	}
	dup, worst, worstN := 0, "", 0
	for p, n := range count {
		if n > 1 {
			dup += utf8.RuneCountInString(p) * (n - 1)
			if n > worstN {
				worst, worstN = p, n
			}
		}
	}
	if total == 0 || float64(dup)/float64(total) < selfDupThreshold {
		return vs
	}
	return append(vs, Violation{
		Rule:     "self_duplication",
		Target:   fmt.Sprintf("×%d %s", worstN, truncateRunes(worst, 50)),
		Limit:    fmt.Sprintf("<%.0f%%", selfDupThreshold*100),
		Actual:   fmt.Sprintf("%.0f%%", 100*float64(dup)/float64(total)),
		Severity: SeverityError,
	})
}

// CheckAgainstPrevious detects "the new chapter is actually a copy of the previous one".
//
// The Writer has read_chapter and first looks at what the previous chapter wrote, then copies it down as the continuation - measured:
// 32.2% of the characters of chapter 14 came verbatim from chapter 13, and 12.9% of chapter 22 from chapter 21.
//
// self_duplication cannot see this kind of incident: it only counts within a single chapter, and the copied chapter is
// perfectly clean internally. So it must be compared separately against the "preceding text". Skipped when previous is empty.
func CheckAgainstPrevious(text string, previous []string) []Violation {
	cur := paragraphs(text)
	if len(cur) == 0 || len(previous) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, prev := range previous {
		for _, p := range paragraphs(prev) {
			seen[p] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}

	dup, total, sample := 0, 0, ""
	for _, p := range unique(cur) {
		n := utf8.RuneCountInString(p)
		total += n
		if _, ok := seen[p]; ok {
			dup += n
			if sample == "" {
				sample = p
			}
		}
	}
	if total == 0 || float64(dup)/float64(total) < crossDupThreshold {
		return nil
	}
	return []Violation{{
		Rule:     "copied_previous_chapter",
		Target:   truncateRunes(sample, 60),
		Limit:    fmt.Sprintf("<%.0f%%", crossDupThreshold*100),
		Actual:   fmt.Sprintf("%.0f%%", 100*float64(dup)/float64(total)),
		Severity: SeverityError,
	}}
}

func unique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, x := range in {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// englishFunctionWordRe collects English function words only - they never enter Vietnamese content as loanwords.
// Content words (flow / lesson / footsteps and the like) are deliberately excluded: in modern-setting genres they may be legitimate loanwords,
// and a phrase such as "the flow" is already hit by the "the" in it, so there is no need to risk a false alarm.
var englishFunctionWordRe = regexp.MustCompile(
	`(?i)\b(not|the|and|but|for|from|with|they|this|that|just|one|was|were|have|when|which|while|into|over)\b`)

// vietnameseMarkRe matches letters specific to Vietnamese: the 7 base letters (a-breve, a-circumflex, d-stroke, e-circumflex, o-circumflex, o-horn, u-horn, including uppercase) plus
// the Latin Extended Additional block U+1EA0-U+1EF9 - that block is used almost exclusively for Vietnamese tone letters.
// Listing only a few precomposed characters is not enough: most tone letters of an ordinary Vietnamese sentence fall within that range.
var vietnameseMarkRe = regexp.MustCompile(`[ăâđêôơưĂÂĐÊÔƠƯ\x{1ea0}-\x{1ef9}]`)

// vietnameseMarkFloor is the number of Vietnamese-specific letters required to judge "the content is Vietnamese". A real chapter has hundreds or thousands;
// the floor exists only to keep this rule completely silent in English/Chinese works.
const vietnameseMarkFloor = 12

// appendEnglishResidue reports English function words mixed into Vietnamese content.
//
// cjk_leak is helpless here: Vietnamese and English share the Latin alphabet, so an intruding "not"/"the" is indistinguishable from the content
// at the character level. And this is no small matter - measured: chapter 18 had 27 "not", 9 "the" and 3 "from",
// for example an English "not" in the middle of a Vietnamese sentence after a dash.
func appendEnglishResidue(vs []Violation, text string) []Violation {
	if len(vietnameseMarkRe.FindAllString(text, vietnameseMarkFloor)) < vietnameseMarkFloor {
		return vs
	}
	matches := englishFunctionWordRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return vs
	}
	seen := make(map[string]struct{})
	var examples []string
	for _, m := range matches {
		low := strings.ToLower(m)
		if _, ok := seen[low]; ok {
			continue
		}
		seen[low] = struct{}{}
		if len(examples) < 4 {
			examples = append(examples, low)
		}
	}
	return append(vs, Violation{
		Rule:     "english_residue",
		Target:   strings.Join(examples, ", "),
		Actual:   len(matches),
		Severity: SeverityError,
	})
}
