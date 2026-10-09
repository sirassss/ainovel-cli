package domain

import "strings"
import "testing"

// Bốn dạng đầu chương đã gặp thật trong 15 chương của lần chạy vừa rồi.
func TestApplyChapterHeadingNormalisesRealCases(t *testing.T) {
	const title = "Chương 4: Bí Mật Về Linh Thảo"
	cases := map[string]string{
		"khong co tieu de": "Nửa canh giờ trước khi mặt trời lặn, sương mù còn dính trên lá.",
		"heading dung":     "# Chương 4: Bí Mật Về Linh Thảo\n\nNửa canh giờ trước khi mặt trời lặn.",
		"heading cap hai":  "## Chương 4: Bí Mật Về Linh Thảo\n\nNửa canh giờ trước khi mặt trời lặn.",
		"in dam":           "**Chương 4: Bí Mật Về Linh Thảo**\n\nNửa canh giờ trước khi mặt trời lặn.",
	}
	for name, content := range cases {
		got := ApplyChapterHeading(content, title, 4)
		if !strings.HasPrefix(got, "# "+title+"\n\n") {
			t.Errorf("%s: đầu ra sai\n%q", name, got)
		}
		if strings.Count(got, title) != 1 {
			t.Errorf("%s: tiêu đề bị lặp\n%q", name, got)
		}
	}
}

// Số chương trong tiêu đề phải theo số thật, không theo trí nhớ của model.
func TestFixChapterNumber(t *testing.T) {
	cases := []struct {
		in   string
		ch   int
		want string
	}{
		{"Chương 20: Gặp Linh Vân", 2, "Chương 2: Gặp Linh Vân"},
		{"Chương 7: Linh Thảo Bí Mật", 7, "Chương 7: Linh Thảo Bí Mật"},
		{"第 20 章 灵草秘密", 2, "第 2 章 灵草秘密"},
		{"Gốc cây thông cổ thụ", 5, "Gốc cây thông cổ thụ"},
	}
	for _, c := range cases {
		if got := FixChapterNumber(c.in, c.ch); got != c.want {
			t.Errorf("FixChapterNumber(%q,%d) = %q, mong đợi %q", c.in, c.ch, got, c.want)
		}
	}
}

// Câu văn mở đầu bằng chữ in đậm không được nuốt nhầm thành tiêu đề.
func TestApplyChapterHeadingKeepsBoldProse(t *testing.T) {
	content := "**Không ai** ngờ rằng đêm ấy cả thôn đều thức trắng chờ tin dữ."
	got := ApplyChapterHeading(content, "Chương 9: Đêm Trắng", 9)
	if !strings.Contains(got, "**Không ai** ngờ rằng") {
		t.Fatalf("câu văn bị nuốt mất:\n%q", got)
	}
}
