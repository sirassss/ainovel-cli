package exp

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func hasHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

func TestRenderTXTVietnameseLabels(t *testing.T) {
	got := renderTXT(
		labelsFor("vi"),
		"Hào Quang",
		[]int{1, 2},
		chapterTitleIndex{1: "Đêm mưa"},
		map[int]chapterLocation{1: {VolumeIdx: 1, VolumeTitle: "Khởi đầu", IsFirstOfVolume: true}},
		map[int]string{1: "# Chương 1 Đêm mưa\n\nAnh nhìn ra cửa sổ.", 2: "Cô đẩy cửa."},
	)
	for _, want := range []string{"Hào Quang\n\n", "Tập 1  Khởi đầu", "Chương 1  Đêm mưa", "Chương 2\n\n", "Anh nhìn ra cửa sổ."} {
		if !strings.Contains(got, want) {
			t.Errorf("thiếu %q trong:\n%s", want, got)
		}
	}
	if hasHan(got) || strings.Contains(got, "# Chương 1") {
		t.Errorf("không được còn chữ Hán hay dòng tiêu đề chương trùng lặp:\n%s", got)
	}
}

func TestRenderEPUBVietnameseLabels(t *testing.T) {
	data, err := renderEPUB(
		labelsFor("vi"),
		domain.BookMetadata{Title: "Hào Quang", Synopsis: "Một thiếu niên truy tìm sự thật."},
		[]int{1},
		chapterTitleIndex{1: "Đêm mưa"},
		map[int]chapterLocation{1: {VolumeIdx: 1, VolumeTitle: "Khởi đầu", IsFirstOfVolume: true}},
		map[int]string{1: "Anh nhìn ra cửa sổ."},
	)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		all += string(b)
	}
	for _, want := range []string{"Chương 1 Đêm mưa", "Tập 1 Khởi đầu", "Mục lục", "Bìa", `xml:lang="vi"`, "<dc:language>vi</dc:language>"} {
		if !strings.Contains(all, want) {
			t.Errorf("EPUB thiếu %q", want)
		}
	}
	if hasHan(all) || strings.Contains(all, "zh-CN") {
		t.Errorf("EPUB tiếng Việt không được còn chữ Hán hay zh-CN")
	}
}

func TestChapterHeaderReMatchesBothLanguages(t *testing.T) {
	for _, line := range []string{"# 第 12 章 雨夜", "## Chương 12 Đêm mưa", "# chương 3"} {
		if !chapterHeaderRe.MatchString(line) {
			t.Errorf("chapterHeaderRe phải khớp %q", line)
		}
	}
}
