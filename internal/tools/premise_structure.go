package tools

import (
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// premiseHeadingAliases ánh xạ tiêu đề premise (Việt, và Hán cho chế độ zh) về khóa
// chuẩn tiếng Việt. Khóa chuẩn là thứ LLM nhìn thấy trong premise_sections/missing.
var premiseHeadingAliases = map[string]string{
	"Thể loại và giọng điệu":       "Thể loại và giọng điệu",
	"Định vị thể loại":             "Định vị thể loại",
	"Xung đột cốt lõi":             "Xung đột cốt lõi",
	"Mục tiêu nhân vật chính":      "Mục tiêu nhân vật chính",
	"Hướng kết cục":                "Hướng kết cục",
	"Vùng cấm sáng tác":            "Vùng cấm sáng tác",
	"Điểm bán hàng khác biệt":      "Điểm bán hàng khác biệt",
	"Hook khác biệt":               "Hook khác biệt",
	"Móc câu khác biệt":            "Hook khác biệt", // term:keep legacy heading so premises saved before the rename still parse
	"Cam kết cốt lõi":              "Cam kết cốt lõi",
	"Động cơ câu chuyện":           "Động cơ câu chuyện",
	"Tuyến quan hệ/trưởng thành":   "Tuyến quan hệ/trưởng thành",
	"Lộ trình nâng cấp":            "Lộ trình nâng cấp",
	"Chuyển hướng trung kỳ":        "Chuyển hướng trung kỳ",
	"Mệnh đề kết cục":              "Mệnh đề kết cục",
	"Tính phù hợp với truyện ngắn": "Tính phù hợp với truyện ngắn",

	// han:keep alias tiêu đề premise của chế độ zh
	"题材定位": "Định vị thể loại", "题材和基调": "Thể loại và giọng điệu", "核心冲突": "Xung đột cốt lõi", // han:keep
	"主角目标": "Mục tiêu nhân vật chính", "结局方向": "Hướng kết cục", "终局方向": "Hướng kết cục", // han:keep
	"写作禁区": "Vùng cấm sáng tác", "差异化卖点": "Điểm bán hàng khác biệt", "差异化钩子": "Hook khác biệt", // han:keep
	"核心兑现承诺": "Cam kết cốt lõi", "故事引擎": "Động cơ câu chuyện", "关系/成长主线": "Tuyến quan hệ/trưởng thành", // han:keep
	"升级路径": "Lộ trình nâng cấp", "中段转折": "Chuyển hướng trung kỳ", "中期转向": "Chuyển hướng trung kỳ", // han:keep
	"终局命题": "Mệnh đề kết cục", "短篇适配性": "Tính phù hợp với truyện ngắn", // han:keep
	"本作为什么适合短篇/单卷收束": "Tính phù hợp với truyện ngắn", // han:keep
}

// premiseHeadingLookup là premiseHeadingAliases với khóa viết thường, để tra không phân biệt hoa thường.
var premiseHeadingLookup = func() map[string]string {
	m := make(map[string]string, len(premiseHeadingAliases))
	for k, v := range premiseHeadingAliases {
		m[strings.ToLower(k)] = v
	}
	return m
}()

func parsePremiseSections(premise string) map[string]string {
	lines := strings.Split(premise, "\n")
	sections := make(map[string]string)
	var current string
	var body []string

	flush := func() {
		if current == "" {
			return
		}
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if text != "" {
			sections[current] = text
		}
		body = body[:0]
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if heading, ok := canonicalPremiseHeading(trimmed); ok {
			flush()
			current = heading
			continue
		}
		if current != "" {
			body = append(body, line)
		}
	}
	flush()
	return sections
}

func canonicalPremiseHeading(line string) (string, bool) {
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	title := strings.TrimSpace(strings.TrimLeft(line, "#"))
	if title == "" {
		return "", false
	}
	// Model hay chép cả phần mô tả trong prompt: "Hướng kết cục (…)", "Hook khác biệt: …".
	if i := strings.IndexAny(title, "(（:："); i >= 0 { // han:keep dấu ngăn cách full-width của tiêu đề zh
		title = strings.TrimSpace(title[:i])
	}
	canonical, ok := premiseHeadingLookup[strings.ToLower(title)]
	return canonical, ok
}

func premiseStructure(premise string, tier domain.PlanningTier) map[string]any {
	sections := parsePremiseSections(premise)
	required := requiredPremiseHeadings(tier)
	found := make([]string, 0, len(required))
	var missing []string
	for _, heading := range required {
		if _, ok := sections[heading]; ok {
			found = append(found, heading)
			continue
		}
		missing = append(missing, heading)
	}

	structure := map[string]any{
		"template_ready": len(missing) == 0,
		"found":          found,
		"missing":        missing,
	}
	if len(sections) > 0 {
		structure["section_count"] = len(sections)
	}
	return structure
}

func requiredPremiseHeadings(tier domain.PlanningTier) []string {
	common := []string{
		"Thể loại và giọng điệu",
		"Định vị thể loại",
		"Xung đột cốt lõi",
		"Mục tiêu nhân vật chính",
		"Hướng kết cục",
		"Vùng cấm sáng tác",
		"Điểm bán hàng khác biệt",
		"Hook khác biệt",
		"Cam kết cốt lõi",
	}

	switch tier {
	case domain.PlanningTierLong:
		return append(common,
			"Động cơ câu chuyện",
			"Tuyến quan hệ/trưởng thành",
			"Lộ trình nâng cấp",
			"Chuyển hướng trung kỳ",
			"Mệnh đề kết cục",
		)
	case domain.PlanningTierMid:
		return append(common,
			"Động cơ câu chuyện",
			"Chuyển hướng trung kỳ",
		)
	case domain.PlanningTierShort:
		return append(common,
			"Tính phù hợp với truyện ngắn",
		)
	default:
		return common
	}
}
