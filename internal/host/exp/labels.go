package exp

// labels is the table of display labels in the exported file, by work language.
type labels struct {
	lang            string // BCP-47 language tag for EPUB (xml:lang, dc:language)
	bookTitleFmt    string // book-title line at the top of a TXT file
	chapterFmt      string // "Chương %d" // han:keep
	chapterTitleFmt string // EPUB: "Chương %d %s" // han:keep
	txtChapterFmt   string // TXT: "Chương %d  %s" // han:keep
	volumeFmt       string // EPUB: "Tập %d %s" // han:keep
	txtVolumeFmt    string // TXT: volume heading with alignment padding
	cover           string
	toc             string
}

var labelsVI = labels{
	lang: "vi", bookTitleFmt: "%s", chapterFmt: "Chương %d", chapterTitleFmt: "Chương %d %s",
	txtChapterFmt: "Chương %d  %s", volumeFmt: "Tập %d %s", txtVolumeFmt: "Tập %d  %s",
	cover: "Bìa", toc: "Mục lục",
}

var labelsZH = labels{ // han:keep export labels for zh works
	lang: "zh-CN", bookTitleFmt: "《%s》", chapterFmt: "第 %d 章", chapterTitleFmt: "第 %d 章 %s", // han:keep
	txtChapterFmt: "第 %d 章  %s", volumeFmt: "第 %d 卷 %s", txtVolumeFmt: "第 %d 卷  %s", // han:keep
	cover: "封面", toc: "目录", // han:keep
}

// labelsFor picks labels by work language; same convention as store: only "vi" uses the Vietnamese labels.
func labelsFor(lang string) labels {
	if lang == "vi" {
		return labelsVI
	}
	return labelsZH
}
