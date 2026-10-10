package store

// mdLabels are the fixed labels in the derived Markdown views.
//
// These .md files are read back into the model context by novel_context, so the label language is not just a display issue:
// a Vietnamese manuscript whose every chapter carries the Chinese labels 「第 N 章」「核心事件」 (han:keep) constantly hints that the current context is Chinese,
// and in testing this led to Han characters leaking into the prose (quan sát草木 / Bạo虐 / 顿号). Labels follow the work language,
// so they do not pull against the model's output language.
type mdLabels struct {
	bookTitleFmt string
	synopsis     string
	charProfiles string
	charArc      string
	traits       string
	listSep      string
	openParen    string
	closeParen   string
	colon        string

	outline        string
	layeredOutline string
	volumeFmt      string
	arcFmt         string
	chapterFmt     string
	theme          string
	goal           string
	pendingArcFmt  string
	coreEvent      string
	hook           string
	scenes         string

	timeline      string
	foreshadow    string
	resolvedAtFmt string
	plantedAtFmt  string
	relationships string
	atChapterFmt  string
	worldRules    string
	rule          string
	boundary      string
}

var labelsZH = mdLabels{
	// Markdown label table of zh mode (labelsZH). han:keep
	bookTitleFmt: "《%s》", synopsis: "简介", charProfiles: "角色档案", charArc: "角色弧线", traits: "特征",
	// Markdown label table of zh mode (labelsZH). han:keep
	listSep: "、", openParen: "（", closeParen: "）", colon: "：",

	// Markdown label table of zh mode (labelsZH). han:keep
	outline: "大纲", layeredOutline: "分层大纲",
	// Markdown label table of zh mode (labelsZH). han:keep
	volumeFmt: "第 %d 卷", arcFmt: "第 %d 弧", chapterFmt: "第 %d 章",
	// Markdown label table of zh mode (labelsZH). han:keep
	theme: "主题", goal: "目标", pendingArcFmt: "（待展开，预估 %d 章）",
	// Markdown label table of zh mode (labelsZH). han:keep
	coreEvent: "核心事件", hook: "钩子", scenes: "场景",

	// Markdown label table of zh mode (labelsZH). han:keep
	timeline: "时间线", foreshadow: "伏笔账本",
	// Markdown label table of zh mode (labelsZH). han:keep
	resolvedAtFmt: "已回收（第 %d 章）", plantedAtFmt: "埋设于第 %d 章，状态：%s",
	// Markdown label table of zh mode (labelsZH). han:keep
	relationships: "人物关系", atChapterFmt: "（第 %d 章）",
	// Markdown label table of zh mode (labelsZH). han:keep
	worldRules: "世界观规则", rule: "规则", boundary: "边界",
}

// labelsVI uses the project's established Vietnamese terms: tập (volume) / cung (arc) / chương (chapter) / dàn ý (outline) / hook / chi tiết gài trước (foreshadowing detail). han:keep
var labelsVI = mdLabels{
	bookTitleFmt: "%s", synopsis: "Giới thiệu", charProfiles: "Hồ sơ nhân vật", charArc: "Cung nhân vật",
	traits: "Đặc điểm", listSep: ", ", openParen: " (", closeParen: ")", colon: ": ",

	outline: "Dàn ý", layeredOutline: "Dàn ý phân tầng",
	volumeFmt: "Tập %d", arcFmt: "Cung %d", chapterFmt: "Chương %d",
	theme: "Chủ đề", goal: "Mục tiêu", pendingArcFmt: "*(chưa khai triển, ước %d chương)*",
	coreEvent: "Sự kiện chính", hook: "Hook", scenes: "Cảnh",

	timeline: "Dòng thời gian", foreshadow: "Sổ chi tiết gài trước",
	resolvedAtFmt: "đã thu ở chương %d", plantedAtFmt: "gieo ở chương %d, trạng thái: %s",
	relationships: "Quan hệ nhân vật", atChapterFmt: " (chương %d)",
	worldRules: "Luật thế giới", rule: "Luật", boundary: "Ranh giới",
}

func labelsFor(lang string) mdLabels {
	if lang == "vi" {
		return labelsVI
	}
	return labelsZH
}
