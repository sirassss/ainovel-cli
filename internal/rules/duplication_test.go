package rules

import (
	"strings"
	"testing"
)

func para(seed string) string {
	return seed + " Gió từ đỉnh núi phả vào mặt lạnh như kim loại, hắn siết chặt cây gậy gỗ mục."
}

// Real chapter 21: the same passage appears 5 times, total duplication 43.5%.
func TestSelfDuplicationCatchesRepeatedParagraph(t *testing.T) {
	rep := para("Họ đi xuống dốc núi theo con đường Thầy Mộc Lâu chọn.")
	body := strings.Join([]string{rep, para("A."), rep, para("B."), rep, rep, rep}, "\n\n")
	vs := appendSelfDuplication(nil, body)
	if len(vs) != 1 || vs[0].Rule != "self_duplication" {
		t.Fatalf("mong đợi self_duplication, nhận: %+v", vs)
	}
	if !strings.Contains(vs[0].Target, "×5") {
		t.Errorf("phải nêu số lần lặp, nhận: %v", vs[0].Target)
	}
}

// 19 of 22 real chapters measure exactly 0.0% - normal prose must not be flagged.
func TestSelfDuplicationSilentOnCleanChapter(t *testing.T) {
	body := strings.Join([]string{para("Một."), para("Hai."), para("Ba."), para("Bốn.")}, "\n\n")
	if vs := appendSelfDuplication(nil, body); len(vs) != 0 {
		t.Fatalf("không được báo trên văn sạch, nhận: %+v", vs)
	}
}

// Real chapter 14: 32.2% of characters are taken verbatim from chapter 13.
func TestCheckAgainstPreviousCatchesCopiedChapter(t *testing.T) {
	shared := []string{para("Hơi ẩm rừng buổi sáng hòa lẫn mùi đất."), para("Hắn mở mắt ra.")}
	prev := strings.Join(append(shared, para("Riêng của chương trước.")), "\n\n")
	cur := strings.Join(append(shared, para("Một câu mới.")), "\n\n")
	vs := CheckAgainstPrevious(cur, []string{prev})
	if len(vs) != 1 || vs[0].Rule != "copied_previous_chapter" {
		t.Fatalf("mong đợi copied_previous_chapter, nhận: %+v", vs)
	}
}

func TestCheckAgainstPreviousSilentOnFreshChapter(t *testing.T) {
	prev := strings.Join([]string{para("Một."), para("Hai.")}, "\n\n")
	cur := strings.Join([]string{para("Ba."), para("Bốn.")}, "\n\n")
	if vs := CheckAgainstPrevious(cur, []string{prev}); len(vs) != 0 {
		t.Fatalf("không được báo khi chương mới hoàn toàn, nhận: %+v", vs)
	}
	if vs := CheckAgainstPrevious(cur, nil); len(vs) != 0 {
		t.Fatalf("không có chương trước thì phải im, nhận: %+v", vs)
	}
}

// Real chapter 18: "not" 27 times, "the" 9 times, "from" 3 times in the middle of Vietnamese prose.
func TestEnglishResidueCatchesLeakInVietnamese(t *testing.T) {
	text := "Lá cây bắt đầu chuyển động—not nhanh chóng mà nhẹ nhàng nghiêng mình. " +
		"Lê An nắm chặt cây gậy gỗ mục—the flow từ lòng đất vẫn thì thầm điều gì đó khó hiểu."
	vs := appendEnglishResidue(nil, text)
	if len(vs) != 1 || vs[0].Rule != "english_residue" {
		t.Fatalf("mong đợi english_residue, nhận: %+v", vs)
	}
}

func TestEnglishResidueSilentOnCleanVietnamese(t *testing.T) {
	text := "Ngọc Lâm cúi xuống bên luống thuốc, ngón tay lần theo sống lá còn đọng sương sớm. " +
		"Hắn biết cây này ưa nắng sớm, chịu được đất cằn, rễ bám sâu hơn vẻ ngoài của nó."
	if vs := appendEnglishResidue(nil, text); len(vs) != 0 {
		t.Fatalf("không được báo trên tiếng Việt sạch, nhận: %+v", vs)
	}
}

// English/Chinese novels must stay completely silent - the same lesson as non_cjk_fragments.
func TestEnglishResidueSilentOnNonVietnameseNovel(t *testing.T) {
	text := "He walked into the forest and the flow of qi from the roots was not what he expected."
	if vs := appendEnglishResidue(nil, text); len(vs) != 0 {
		t.Fatalf("văn tiếng Anh không được báo lỗi này, nhận: %+v", vs)
	}
}
