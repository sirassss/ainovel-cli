package exp

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestStripChapterTitleHeader(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		title string
		want  string
	}{
		{"plain body untouched", "他望着窗外。", "雨夜归人", "他望着窗外。"},
		{"strip h1 chinese title", "# 第 1 章  雨夜归人\n\n他望着窗外。", "雨夜归人", "他望着窗外。"},
		{"strip h2 with chapter token", "## 第二章\n\n他望着窗外。", "", "他望着窗外。"},
		{"keep body even if no header", "正文第一句。\n第二句。", "", "正文第一句。\n第二句。"},
		{"do not strip non-chapter heading", "# 序章\n他望着窗外。", "边村浮生", "# 序章\n他望着窗外。"},
		{"single line header only", "# 第 1 章", "", ""},
		// the writer wrote the bare chapter name as the first-line heading → duplicates the exporter's unified heading, should be stripped
		{"strip h1 matching chapter title", "# 边村浮生\n\n天还没亮。", "边村浮生", "天还没亮。"},
		// first-line h1 but the text does not equal this chapter's title → treated as body, kept
		{"keep h1 not matching title", "# 别的小标题\n正文。", "边村浮生", "# 别的小标题\n正文。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripChapterTitleHeader(c.in, c.title)
			if got != c.want {
				t.Fatalf("stripChapterTitleHeader\nin   = %q\ntitle= %q\nwant = %q\ngot  = %q", c.in, c.title, c.want, got)
			}
		})
	}
}

func TestBuildTitleIndex(t *testing.T) {
	outline := []domain.OutlineEntry{
		{Chapter: 1, Title: "雨夜归人"},
		{Chapter: 2, Title: ""}, // an empty title should be filtered out
		{Chapter: 3, Title: "破晓"},
	}
	idx := buildTitleIndex(outline)
	if got := idx[1]; got != "雨夜归人" {
		t.Errorf("ch1 title: got %q want 雨夜归人", got)
	}
	if _, ok := idx[2]; ok {
		t.Errorf("ch2 should be absent (empty title)")
	}
	if got := idx[3]; got != "破晓" {
		t.Errorf("ch3 title: got %q want 破晓", got)
	}
}

func TestBuildLocations(t *testing.T) {
	volumes := []domain.VolumeOutline{
		{Index: 1, Title: "起源", Arcs: []domain.ArcOutline{
			{Index: 1, Title: "少年初登场", Chapters: []domain.OutlineEntry{{}, {}}}, // 2 chapters
			{Index: 2, Title: "宗门试炼", Chapters: []domain.OutlineEntry{{}}},      // 1 chapter
		}},
		{Index: 2, Title: "崛起", Arcs: []domain.ArcOutline{
			{Index: 1, Title: "初战", Chapters: []domain.OutlineEntry{{}}},
		}},
	}
	locs := buildLocations(volumes)

	// Verify only volume membership: arcs no longer enter location, but the arc level still participates in accumulating global chapter numbers.
	if loc := locs[1]; !loc.IsFirstOfVolume || loc.VolumeIdx != 1 {
		t.Errorf("ch1 should be first of volume 1: %+v", loc)
	}
	if loc := locs[2]; loc.IsFirstOfVolume || loc.VolumeIdx != 1 {
		t.Errorf("ch2 should be volume 1, not first: %+v", loc)
	}
	// ch3 is the first chapter of arc 2 but is still inside volume 1 → not the start of a volume.
	if loc := locs[3]; loc.IsFirstOfVolume || loc.VolumeIdx != 1 {
		t.Errorf("ch3 (arc 2, same volume) should not be first of volume: %+v", loc)
	}
	if loc := locs[4]; !loc.IsFirstOfVolume || loc.VolumeIdx != 2 {
		t.Errorf("ch4 should start volume 2: %+v", loc)
	}
}

func TestRenderTXT_TitleAndChapter(t *testing.T) {
	got := renderTXT(
		labelsFor("zh"),
		"光斑",
		[]int{1, 2},
		chapterTitleIndex{1: "雨夜归人", 2: "破晓"},
		nil,
		map[int]string{
			1: "# 第 1 章 雨夜归人\n\n他望着窗外。",
			2: "她推开门。",
		},
	)
	if !strings.HasPrefix(got, "《光斑》\n\n") {
		t.Errorf("missing book title at start:\n%s", got)
	}
	// premise is not exported: right after the book title comes the chapters, with no premise summary in between
	if !strings.Contains(got, "第 1 章  雨夜归人") {
		t.Errorf("missing ch1 header")
	}
	if !strings.Contains(got, "他望着窗外。") {
		t.Errorf("missing ch1 body")
	}
	if strings.Contains(got, "# 第 1 章") {
		t.Errorf("body markdown header not stripped:\n%s", got)
	}
	if !strings.Contains(got, "第 2 章  破晓") {
		t.Errorf("missing ch2 header")
	}
}

func TestRenderTXT_EmptyBookTitleNoTitleLine(t *testing.T) {
	got := renderTXT(
		labelsFor("zh"),
		"",
		[]int{1},
		chapterTitleIndex{1: "雨夜归人"},
		nil,
		map[int]string{1: "正文。"},
	)
	if strings.Contains(got, "《") {
		t.Errorf("should not contain book title brackets: %s", got)
	}
	if !strings.HasPrefix(got, "第 1 章  雨夜归人") {
		t.Errorf("expect chapter header at very start: %s", got)
	}
}

// TestRenderTXT_LayeredVolume verifies that a layered outline inserts a volume divider only at the start of a volume, and an arc divider never appears
// (issue #27: the layout is fixed as book title → volume divider → chapter body).
func TestRenderTXT_LayeredVolume(t *testing.T) {
	locs := map[int]chapterLocation{
		1: {VolumeIdx: 1, VolumeTitle: "起源", IsFirstOfVolume: true},
		2: {VolumeIdx: 1, VolumeTitle: "起源"},
	}
	got := renderTXT(
		labelsFor("zh"),
		"X", []int{1, 2},
		chapterTitleIndex{1: "A", 2: "B"},
		locs,
		map[int]string{1: "正文一。", 2: "正文二。"},
	)
	if !strings.Contains(got, "第 1 卷  起源") {
		t.Errorf("missing volume header: %s", got)
	}
	if strings.Contains(got, "弧") {
		t.Errorf("arc divider should never appear: %s", got)
	}
	// The volume title appears only once, before the first chapter
	if strings.Count(got, "第 1 卷") != 1 {
		t.Errorf("volume header should appear exactly once: %s", got)
	}
}

func TestRenderTXT_ChapterWithoutTitleFallsBackToNumberOnly(t *testing.T) {
	got := renderTXT(
		labelsFor("zh"),
		"", []int{5},
		chapterTitleIndex{}, // no titles
		nil,
		map[int]string{5: "正文。"},
	)
	if !strings.Contains(got, "第 5 章\n\n") {
		t.Errorf("expect 'first 5 章' fallback header: %s", got)
	}
}
