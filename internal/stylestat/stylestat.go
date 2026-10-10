// Package stylestat computes book-wide style statistics over the written chapters and
// produces plain facts.
//
// Motivation: the in-arc review window (~10 chapters) is blind to book-wide pattern
// fossilization. Sentence-pattern tics run dozens of times per chapter, chapter endings
// share one shape and sentences repeat across chapters, yet every single spot looks
// "normal" in isolation; only book-wide statistics expose it. The code owns the counting
// (deterministic, no hallucination) and the LLM owns the verdict (the editor scores
// dimensions from the numbers, the writer avoids the patterns accordingly).
// Compute is for one-shot full offline evaluation; at runtime a Tracker maintains the
// statistics incrementally per chapter.
package stylestat

import (
	"regexp"
	"sort"
	"strings"
)

// minChapters is the chapter count below which no stats are produced: the sample is
// too small for frequencies to mean anything.
const minChapters = 5

// phraseWindow limits dynamic phrase mining to the last N chapters: what the writer
// must avoid is the current verbal tic.
const phraseWindow = 20

// Input is the statistics input. Chapters are in ascending chapter order; Stopwords are
// proper nouns such as character names, skipped during dynamic phrase mining (names that
// appear on stage are naturally frequent and not a style problem).
type Input struct {
	Chapters  []string
	Titles    []string
	Stopwords []string
}

// Stats is the book-wide style statistics result. Every field is a factual count with no
// verdict or instruction.
type Stats struct {
	Chapters          int            `json:"chapters"`
	Patterns          []PatternStat  `json:"patterns,omitempty"`
	TopPhrases        []PhraseStat   `json:"top_phrases,omitempty"`
	RepeatedSentences []SentenceStat `json:"repeated_sentences,omitempty"`
	Ending            EndingStat     `json:"ending"`
	OpeningTimeRate   float64        `json:"opening_time_rate"`
	TitleFormats      *TitleStat     `json:"title_formats,omitempty"`
}

// PatternStat is the book-wide count of one fixed sentence-pattern class (generic
// AI-style tics).
type PatternStat struct {
	Name       string  `json:"name"`
	Total      int     `json:"total"`
	PerChapter float64 `json:"per_chapter"`
}

// PhraseStat is a high-frequency phrase mined from the last phraseWindow chapters.
type PhraseStat struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// SentenceStat is a long sentence repeated verbatim across chapters (direct evidence of
// repeated exposition).
type SentenceStat struct {
	Text     string `json:"text"`
	Chapters int    `json:"chapters"`
	Count    int    `json:"count"`
}

// EndingStat is the distribution of final-line shapes. A short ending is legitimate on
// its own; the problem is the whole book sharing one shape.
type EndingStat struct {
	ShortRatio  float64 `json:"short_ratio"`
	MedianRunes int     `json:"median_runes"`
}

// TitleStat counts chapter titles with and without the "Chapter N" prefix. Mixing them
// means mechanism traces leak into the output.
type TitleStat struct {
	WithPrefix    int `json:"with_prefix"`
	WithoutPrefix int `json:"without_prefix"`
}

// patternDefs lists generic AI-style sentence patterns. The regexes match Chinese prose
// and are matching data that must stay in Chinese (each carries han:keep); only the
// Vietnamese labels are shown to the writer. Counts are approximate (the regexes do no
// grammar analysis); they serve as a longitudinal baseline for this book, so absolute
// precision does not matter.
var patternDefs = []struct {
	name string
	re   *regexp.Regexp
}{
	{"câu chỉnh lý『không phải…mà là…』", regexp.MustCompile(`不是[^。！？\n]{1,24}?[，、]?(?:而)?是`)},                                            // han:keep (regex)
	{"lượng từ đếm thời gian『X hơi thở/X chớp mắt』", regexp.MustCompile(`[一两二三四五六七八九十几数半][息瞬]`)},                                        // han:keep (regex)
	{"so sánh ví von『như một/phảng phất/như thể/tựa như』", regexp.MustCompile(`像一|仿佛|如同|宛如`)},                                           // han:keep (regex)
	{"nhịp im lặng『đã im lặng/không nói gì/không ngoảnh lại』", regexp.MustCompile(`沉默了|没有说话|没有回头`)},                                     // han:keep (regex)
	{"khuôn thần thái『trong mắt lóe lên/khóe môi nhếch/cắn môi』", regexp.MustCompile(`眼[中底]闪过|目光一凝|瞳孔一缩|眼眶微红|嘴角[微轻一]?[勾扬翘]|咬了咬唇|不可置信`)}, // han:keep (regex)
	{"phản ứng cơ thể『lòng thắt lại/người run lên/hít một hơi lạnh』", regexp.MustCompile(`心头一[紧沉颤]|身子一[颤震僵]|倒吸(?:了)?一口凉气`)},             // han:keep (regex)
	{"dấu hiệu suy nghĩ『nghĩ thầm/nhận ra/cảm thấy/thấy rằng』", regexp.MustCompile(`心想|意识到|感到|觉得`)},                                     // han:keep (regex)
	{"câu sáo rỗng trừu tượng『một thứ không nói nên lời/ý nghĩa nằm ở』", regexp.MustCompile(`一种说不出的|说不清[的道]|的意义在于|真正的[^。！？\n]{1,10}是`)}, // han:keep (regex)
}

var (
	sentenceSplit = regexp.MustCompile(`[。！？\n]+`)                         // han:keep (sentence terminators)
	openingTimeRe = regexp.MustCompile(`夜|清晨|黎明|天亮|醒来|晨光|一整夜`)             // han:keep (time-of-day opening words)
	titlePrefixRe = regexp.MustCompile(`^#{0,2}\s*第[零〇一二三四五六七八九十百千万\d]+章`) // han:keep (chapter title prefix)
)

// shortEndingRunes is the final-line length (in runes) up to which an ending counts as
// a "short ending".
const shortEndingRunes = 30

// Compute computes the book-wide style statistics; it returns nil when there are too
// few chapters.
func Compute(in Input) *Stats {
	n := len(in.Chapters)
	if n < minChapters {
		return nil
	}
	all := strings.Join(in.Chapters, "\n")

	s := &Stats{Chapters: n}
	for _, def := range patternDefs {
		total := len(def.re.FindAllStringIndex(all, -1))
		if total == 0 {
			continue
		}
		s.Patterns = append(s.Patterns, PatternStat{
			Name:       def.name,
			Total:      total,
			PerChapter: round1(float64(total) / float64(n)),
		})
	}
	s.TopPhrases = minePhrases(recentWindow(in.Chapters), in.Stopwords)
	s.RepeatedSentences = repeatedSentences(in.Chapters)
	s.Ending = endingShape(in.Chapters)
	s.OpeningTimeRate = openingTimeRate(in.Chapters)
	s.TitleFormats = titleFormats(in.Titles)
	return s
}

func recentWindow(chapters []string) []string {
	if len(chapters) <= phraseWindow {
		return chapters
	}
	return chapters[len(chapters)-phraseWindow:]
}

// minePhrases mines high-frequency 3-6 character phrases inside the window.
// Filters: anything with punctuation or whitespace, edge function words and proper-noun
// hits. Dedup: a candidate that is a substring of, or contains, an already picked phrase
// is dropped.
func minePhrases(chapters []string, stopwords []string) []PhraseStat {
	text := strings.Join(chapters, "\n")
	runes := []rune(text)
	threshold := max(8, len(chapters)/2)

	counts := make(map[string]int)
	for size := 3; size <= 6; size++ {
		for i := 0; i+size <= len(runes); i++ {
			gram := runes[i : i+size]
			if !validGram(gram) {
				continue
			}
			counts[string(gram)]++
		}
	}

	stopGrams := stopwordBigrams(stopwords)
	type cand struct {
		text  string
		count int
	}
	var cands []cand
	for g, c := range counts {
		if c < threshold || hitStopword(g, stopGrams) {
			continue
		}
		cands = append(cands, cand{g, c})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].count != cands[j].count {
			return cands[i].count > cands[j].count
		}
		// On equal counts prefer the longer phrase (more information), then sort
		// lexicographically for a stable order
		if len(cands[i].text) != len(cands[j].text) {
			return len(cands[i].text) > len(cands[j].text)
		}
		return cands[i].text < cands[j].text
	})

	var out []PhraseStat
	for _, c := range cands {
		if len(out) >= 8 {
			break
		}
		dup := false
		for _, picked := range out {
			if strings.Contains(picked.Text, c.text) || strings.Contains(c.text, picked.Text) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, PhraseStat{Text: c.text, Count: c.count})
		}
	}
	return out
}

// gramEdgeStop: an n-gram starting or ending with one of these function words or
// pronouns is not a style phrase and is skipped. han:keep (Chinese function words)
const gramEdgeStop = "的了着是在和与就也都还又把被他她它我你这那"

func validGram(gram []rune) bool {
	for _, r := range gram {
		if r < 0x4E00 || r > 0x9FFF { // pure Han-character fragments only
			return false
		}
	}
	if strings.ContainsRune(gramEdgeStop, gram[0]) || strings.ContainsRune(gramEdgeStop, gram[len(gram)-1]) {
		return false
	}
	return true
}

// stopwordBigrams splits proper nouns into 2-character fragments: a character name often
// appears in text only in part (a short form of the full name), so matching the whole
// name would let it slip through. Erring on the strict side is fine: one phrase fact
// fewer is harmless, while a name leaking into the verbal-tic list is noise.
func stopwordBigrams(stopwords []string) []string {
	var grams []string
	for _, w := range stopwords {
		runes := []rune(strings.TrimSpace(w))
		if len(runes) < 2 {
			continue
		}
		for i := 0; i+2 <= len(runes); i++ {
			grams = append(grams, string(runes[i:i+2]))
		}
	}
	return grams
}

func hitStopword(gram string, stopGrams []string) bool {
	for _, g := range stopGrams {
		if strings.Contains(gram, g) {
			return true
		}
	}
	return false
}

// repeatedSentences finds sentences of at least 12 characters repeated verbatim across at
// least 3 chapters and returns the top 5 by count.
func repeatedSentences(chapters []string) []SentenceStat {
	type rec struct {
		count    int
		chapters map[int]struct{}
	}
	seen := make(map[string]*rec)
	for ci, text := range chapters {
		for sent, count := range chapterSentenceCounts(text) {
			r := seen[sent]
			if r == nil {
				r = &rec{chapters: make(map[int]struct{})}
				seen[sent] = r
			}
			r.count += count
			r.chapters[ci] = struct{}{}
		}
	}

	var out []SentenceStat
	for sent, r := range seen {
		if len(r.chapters) < 3 {
			continue
		}
		out = append(out, SentenceStat{Text: truncateRunes(sent, 40), Chapters: len(r.chapters), Count: r.count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// trimWrappedQuotes strips wrapping quotes so the same line of dialogue with or without
// an opening quote is not counted as two sentences.
func trimWrappedQuotes(sentence string) string {
	// han:keep (CJK quote marks)
	return strings.Trim(strings.TrimSpace(sentence), `"“”‘’「」『』`)
}

func endingShape(chapters []string) EndingStat {
	var lengths []int
	short := 0
	for _, text := range chapters {
		line := lastNonEmptyLine(text)
		if line == "" {
			continue
		}
		n := len([]rune(line))
		lengths = append(lengths, n)
		if n <= shortEndingRunes {
			short++
		}
	}
	if len(lengths) == 0 {
		return EndingStat{}
	}
	sort.Ints(lengths)
	return EndingStat{
		ShortRatio:  round2(float64(short) / float64(len(lengths))),
		MedianRunes: lengths[len(lengths)/2],
	}
}

func openingTimeRate(chapters []string) float64 {
	hit := 0
	for _, text := range chapters {
		if openingTimeRe.MatchString(firstParagraph(text)) {
			hit++
		}
	}
	return round2(float64(hit) / float64(len(chapters)))
}

func titleFormats(titles []string) *TitleStat {
	if len(titles) == 0 {
		return nil
	}
	t := &TitleStat{}
	for _, title := range titles {
		if strings.TrimSpace(title) == "" {
			continue
		}
		if titlePrefixRe.MatchString(title) {
			t.WithPrefix++
		} else {
			t.WithoutPrefix++
		}
	}
	// Only mixing is worth reporting; a uniform format is not a factual problem
	if t.WithPrefix == 0 || t.WithoutPrefix == 0 {
		return nil
	}
	return t
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// firstParagraph returns the first non-empty line that is not a Markdown heading (the
// first line of a chapter file is usually a # title).
func firstParagraph(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
