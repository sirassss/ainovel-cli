package stylestat

import (
	"strings"
	"testing"
)

func chapterWith(body string) string {
	return "# 标题\n" + body
}

func TestComputeBelowMinChapters(t *testing.T) {
	in := Input{Chapters: []string{"a", "b", "c", "d"}}
	if Compute(in) != nil {
		t.Fatal("below minChapters should return nil")
	}
}

func TestComputePatterns(t *testing.T) {
	body := "他不是愤怒，而是恐惧。沉默了几息。像一盏灯。她眼中闪过慌乱，心头一紧。他觉得这是一种说不出的寒意。\n正文。\n"
	chapters := make([]string, 6)
	for i := range chapters {
		chapters[i] = chapterWith(body)
	}
	s := Compute(Input{Chapters: chapters})
	if s == nil {
		t.Fatal("expected stats")
	}
	want := map[string]int{
		"câu chỉnh lý『không phải…mà là…』":                                  6,
		"lượng từ đếm thời gian『X hơi thở/X chớp mắt』":                     6,
		"so sánh ví von『như một/phảng phất/như thể/tựa như』":               6,
		"nhịp im lặng『đã im lặng/không nói gì/không ngoảnh lại』":           6,
		"khuôn thần thái『trong mắt lóe lên/khóe môi nhếch/cắn môi』":        6,
		"phản ứng cơ thể『lòng thắt lại/người run lên/hít một hơi lạnh』":    6,
		"dấu hiệu suy nghĩ『nghĩ thầm/nhận ra/cảm thấy/thấy rằng』":          6,
		"câu sáo rỗng trừu tượng『một thứ không nói nên lời/ý nghĩa nằm ở』": 6,
	}
	for _, p := range s.Patterns {
		if w, ok := want[p.Name]; ok && p.Total != w {
			t.Errorf("%s total: got %d want %d", p.Name, p.Total, w)
		}
		if p.PerChapter != 1.0 {
			t.Errorf("%s per_chapter: got %v want 1.0", p.Name, p.PerChapter)
		}
	}
	if len(s.Patterns) != len(want) {
		t.Errorf("want %d pattern classes, got %d: %+v", len(want), len(s.Patterns), s.Patterns)
	}
}

func TestComputeTopPhrasesWithStopwords(t *testing.T) {
	// The mountain-peak phrase appears often; the character name must be filtered out
	line := "众人望向青云山巅，陆九渊负手而立。\n"
	chapters := make([]string, 10)
	for i := range chapters {
		chapters[i] = chapterWith(strings.Repeat(line, 3))
	}
	s := Compute(Input{Chapters: chapters, Stopwords: []string{"陆九渊"}})
	if s == nil {
		t.Fatal("expected stats")
	}
	var hasMountain, hasName bool
	for _, p := range s.TopPhrases {
		if strings.Contains(p.Text, "青云山") {
			hasMountain = true
		}
		if strings.Contains(p.Text, "九渊") || strings.Contains(p.Text, "陆九") {
			hasName = true
		}
	}
	if !hasMountain {
		t.Errorf("expected 青云山 phrase mined, got %+v", s.TopPhrases)
	}
	if hasName {
		t.Errorf("character name should be filtered, got %+v", s.TopPhrases)
	}
}

func TestComputeRepeatedSentences(t *testing.T) {
	motto := "此生未能远行，望你替我看看远方的山海。"
	chapters := make([]string, 6)
	for i := range chapters {
		body := "平常正文，没有什么重复。\n"
		if i%2 == 0 {
			body += motto + "\n"
		}
		chapters[i] = chapterWith(body)
	}
	s := Compute(Input{Chapters: chapters})
	if s == nil {
		t.Fatal("expected stats")
	}
	if len(s.RepeatedSentences) == 0 {
		t.Fatalf("expected repeated sentence, got none")
	}
	got := s.RepeatedSentences[0]
	if got.Chapters != 3 || got.Count != 3 {
		t.Errorf("repeated sentence: %+v", got)
	}
	if !strings.HasPrefix(got.Text, "此生未能远行") {
		t.Errorf("text: %q", got.Text)
	}
}

func TestComputeEndingAndOpening(t *testing.T) {
	short := chapterWith("一整夜没有睡。\n正文很长很长很长。\n他走了。")
	long := chapterWith("白天的事。\n正文。\n这是一个非常非常非常长的结尾句子，远远超过三十个字符的阈值长度，用来测试中位数。")
	chapters := []string{short, short, short, long, long}
	s := Compute(Input{Chapters: chapters})
	if s == nil {
		t.Fatal("expected stats")
	}
	if s.Ending.ShortRatio != 0.6 {
		t.Errorf("short_ratio: got %v want 0.6", s.Ending.ShortRatio)
	}
	if s.OpeningTimeRate != 0.6 {
		t.Errorf("opening_time_rate: got %v want 0.6", s.OpeningTimeRate)
	}
}

func TestComputeTitleFormats(t *testing.T) {
	chapters := make([]string, 5)
	for i := range chapters {
		chapters[i] = chapterWith("正文。")
	}
	// Mixed -> reported
	s := Compute(Input{Chapters: chapters, Titles: []string{"第一章 风起", "云涌", "第3章 雷动"}})
	if s.TitleFormats == nil || s.TitleFormats.WithPrefix != 2 || s.TitleFormats.WithoutPrefix != 1 {
		t.Errorf("title formats: %+v", s.TitleFormats)
	}
	// Uniform -> not reported
	s = Compute(Input{Chapters: chapters, Titles: []string{"风起", "云涌"}})
	if s.TitleFormats != nil {
		t.Errorf("uniform titles should not report: %+v", s.TitleFormats)
	}
}

// TestZhPatternsStillMatch locks the Chinese matching data: translating the pattern
// labels must never change which text the regexes hit.
func TestZhPatternsStillMatch(t *testing.T) {
	samples := []string{
		"他不是愤怒，而是恐惧", // corrective sentence
		"沉默了几息",      // time-counting measure word
		"像一盏灯",       // simile
		"她沉默了",       // silence beat
		"她眼中闪过慌乱",    // facial-expression template
		"他心头一紧",      // bodily reaction
		"他心想这不对",     // thought marker
		"这是一种说不出的寒意", // abstract filler
	}
	if len(samples) != len(patternDefs) {
		t.Fatalf("want one sample per pattern: %d samples, %d patterns", len(samples), len(patternDefs))
	}
	for i, def := range patternDefs {
		if !def.re.MatchString(samples[i]) {
			t.Errorf("pattern %d (%s) does not match %q", i, def.name, samples[i])
		}
	}

	if !openingTimeRe.MatchString("清晨的风很冷") || openingTimeRe.MatchString("白天的事") {
		t.Error("openingTimeRe behavior changed")
	}
	for title, want := range map[string]bool{"第一章 风起": true, "# 第3章 雷动": true, "云涌": false} {
		if titlePrefixRe.MatchString(title) != want {
			t.Errorf("titlePrefixRe(%q) want %v", title, want)
		}
	}
	if got := sentenceSplit.Split("甲。乙！丙？\n丁", -1); len(got) != 4 {
		t.Errorf("sentenceSplit: got %q", got)
	}
}
