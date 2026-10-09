package store

// mdLabels 是派生 Markdown 视图里的固定标签。
//
// 这些 .md 会被 novel_context 读回模型上下文，因此标签语种不只是显示问题：
// 越南语正文里每章顶着「第 N 章」「核心事件」，等于持续暗示模型当前是中文语境，
// 实测导致正文夹带汉字（quan sát草木 / Bạo虐 / 顿号）。标签跟随作品语种，
// 才不会和模型的输出语种互相拉扯。
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
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	bookTitleFmt: "《%s》", synopsis: "简介", charProfiles: "角色档案", charArc: "角色弧线", traits: "特征",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	listSep: "、", openParen: "（", closeParen: "）", colon: "：",

	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	outline: "大纲", layeredOutline: "分层大纲",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	volumeFmt: "第 %d 卷", arcFmt: "第 %d 弧", chapterFmt: "第 %d 章",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	theme: "主题", goal: "目标", pendingArcFmt: "（待展开，预估 %d 章）",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	coreEvent: "核心事件", hook: "钩子", scenes: "场景",

	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	timeline: "时间线", foreshadow: "伏笔账本",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	resolvedAtFmt: "已回收（第 %d 章）", plantedAtFmt: "埋设于第 %d 章，状态：%s",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	relationships: "人物关系", atChapterFmt: "（第 %d 章）",
	// han:keep bảng nhãn Markdown của chế độ zh (labelsZH)
	worldRules: "世界观规则", rule: "规则", boundary: "边界",
}

// labelsVI 用项目既定译名：tập / cung / chương / đề cương / điểm móc / phục bút。
var labelsVI = mdLabels{
	bookTitleFmt: "%s", synopsis: "Giới thiệu", charProfiles: "Hồ sơ nhân vật", charArc: "Cung nhân vật",
	traits: "Đặc điểm", listSep: ", ", openParen: " (", closeParen: ")", colon: ": ",

	outline: "Đề cương", layeredOutline: "Đề cương phân tầng",
	volumeFmt: "Tập %d", arcFmt: "Cung %d", chapterFmt: "Chương %d",
	theme: "Chủ đề", goal: "Mục tiêu", pendingArcFmt: "*(chưa khai triển, ước %d chương)*",
	coreEvent: "Sự kiện chính", hook: "Điểm móc", scenes: "Cảnh",

	timeline: "Dòng thời gian", foreshadow: "Sổ phục bút",
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
