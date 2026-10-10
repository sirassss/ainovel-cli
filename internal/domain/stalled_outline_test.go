package domain

import "strings"
import "testing"

// Reproduces the real outline that produced a story of "rewrite, then add a little more":
// 11 of 12 chapters share one hook.
func TestStalledOutlineCatchesRepeatedHook(t *testing.T) {
	same := "Một tu sĩ trẻ xuất hiện với lời mời gia nhập tông phái"
	var entries []OutlineEntry
	for i := 1; i <= 12; i++ {
		e := OutlineEntry{Chapter: i, Title: "Chương", CoreEvent: "Giả Mộc học được điều thứ " + string(rune('a'+i))}
		if i > 1 {
			e.Hook = same
		}
		entries = append(entries, e)
	}
	got := StalledOutline(entries)
	if !strings.Contains(got, "hook") {
		t.Fatalf("phải bắt được hook lặp, nhận %q", got)
	}
}

func TestStalledOutlineAcceptsHealthyOutline(t *testing.T) {
	entries := []OutlineEntry{
		{Chapter: 1, CoreEvent: "Mất mùa, phải nộp giống", Hook: "Cây mai rung lên trong đêm"},
		{Chapter: 2, CoreEvent: "Gặp người lạ ngoài chợ", Hook: "Người lạ biết tên mẹ hắn"},
		{Chapter: 3, CoreEvent: "Bị đuổi khỏi thôn", Hook: "Hắn mang theo một hạt giống không rõ nguồn"},
	}
	if got := StalledOutline(entries); got != "" {
		t.Fatalf("dàn ý lành mạnh không được báo lỗi, nhận %q", got)
	}
}

// Reproduces a real incident: Volume 1 still had two skeleton arcs (38 chapters), yet the book
// was declared complete after the 15 chapters of Volume 2.
func TestSkeletonArcsSeesUnexpandedVolume(t *testing.T) {
	volumes := []VolumeOutline{
		{Index: 1, Arcs: []ArcOutline{
			{Index: 1, Title: "Nguyên Bản Dược Lâm", EstimatedChapters: 25},
			{Index: 2, Title: "Thảm Họa Của Linh Căn", EstimatedChapters: 13},
		}},
		{Index: 2, Arcs: []ArcOutline{
			{Index: 1, Title: "Linh Căn Giới Hạn", Chapters: []OutlineEntry{{Chapter: 1}}},
		}},
	}
	got := SkeletonArcs(volumes)
	if len(got) != 2 {
		t.Fatalf("phải thấy 2 cung phác thảo, nhận %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "Nguyên Bản Dược Lâm") {
		t.Fatalf("phải nêu tên cung, nhận %q", got[0])
	}
}

func TestSkeletonArcsSilentWhenAllExpanded(t *testing.T) {
	volumes := []VolumeOutline{{Index: 1, Arcs: []ArcOutline{
		{Index: 1, Chapters: []OutlineEntry{{Chapter: 1}}},
	}}}
	if got := SkeletonArcs(volumes); len(got) != 0 {
		t.Fatalf("không được báo khi mọi cung đã khai triển, nhận %v", got)
	}
}

// Reproduces a real incident: Arc 2 was expanded to 20 chapters (113792 characters ≈ 37k tokens),
// so no model could read the arc-boundary review and the whole pipeline hung.
func TestOversizedArcCatchesTwentyChapterArc(t *testing.T) {
	got := OversizedArc("Tập 1 cung 2", 20)
	if got == "" {
		t.Fatal("cung 20 chương phải bị bác")
	}
	for _, want := range []string{"20", "8", "tách"} {
		if !strings.Contains(got, want) && !strings.Contains(got, "拆") {
			t.Errorf("chẩn đoán thiếu thông tin %q: %s", want, got)
		}
	}
}

func TestOversizedArcAcceptsNormalArc(t *testing.T) {
	for _, n := range []int{1, 5, 8} {
		if got := OversizedArc("cung", n); got != "" {
			t.Errorf("cung %d chương phải hợp lệ, nhận: %s", n, got)
		}
	}
	if OversizedArc("cung", 9) == "" {
		t.Error("cung 9 chương phải bị bác")
	}
}
