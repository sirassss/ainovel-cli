package exp

// labels là bảng nhãn hiển thị trong file xuất theo ngôn ngữ tác phẩm.
type labels struct {
	lang            string // thẻ ngôn ngữ BCP-47 cho EPUB (xml:lang, dc:language)
	bookTitleFmt    string // dòng tên sách đầu file TXT
	chapterFmt      string // "Chương %d"
	chapterTitleFmt string // EPUB: "Chương %d %s"
	txtChapterFmt   string // TXT: "Chương %d  %s"
	volumeFmt       string // EPUB: "Tập %d %s"
	txtVolumeFmt    string // TXT: tiêu đề tập có căn lề
	cover           string
	toc             string
}

var labelsVI = labels{
	lang: "vi", bookTitleFmt: "%s", chapterFmt: "Chương %d", chapterTitleFmt: "Chương %d %s",
	txtChapterFmt: "Chương %d  %s", volumeFmt: "Tập %d %s", txtVolumeFmt: "Tập %d  %s",
	cover: "Bìa", toc: "Mục lục",
}

var labelsZH = labels{ // han:keep nhãn xuất file cho tác phẩm zh
	lang: "zh-CN", bookTitleFmt: "《%s》", chapterFmt: "第 %d 章", chapterTitleFmt: "第 %d 章 %s", // han:keep
	txtChapterFmt: "第 %d 章  %s", volumeFmt: "第 %d 卷 %s", txtVolumeFmt: "第 %d 卷  %s", // han:keep
	cover: "封面", toc: "目录", // han:keep
}

// labelsFor chọn nhãn theo ngôn ngữ tác phẩm; cùng quy ước với store: chỉ "vi" mới dùng nhãn Việt.
func labelsFor(lang string) labels {
	if lang == "vi" {
		return labelsVI
	}
	return labelsZH
}
