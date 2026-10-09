package tools

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestParsePremiseSections(t *testing.T) {
	premise := `# Premise

## 题材和基调
东方玄幻，冷硬成长。

## 题材定位
东方玄幻升级流，面向追求爽点和关系推进的读者。

## 核心冲突
主角必须在宗门规则与个人良知之间做选择。

## 中期转向
旧有修炼路线失效，必须转向禁术体系。
`

	sections := parsePremiseSections(premise)
	if sections["Thể loại và giọng điệu"] == "" {
		t.Fatalf("expected 题材和基调 section, got %+v", sections)
	}
	if sections["Định vị thể loại"] == "" {
		t.Fatalf("expected 题材定位 section, got %+v", sections)
	}
	if sections["Xung đột cốt lõi"] == "" {
		t.Fatalf("expected 核心冲突 section, got %+v", sections)
	}
	if sections["Chuyển hướng trung kỳ"] == "" {
		t.Fatalf("expected 中期转向 alias normalized to 中段转折, got %+v", sections)
	}
}

func TestPremiseStructure(t *testing.T) {
	premise := `## 题材和基调
升级流，偏冷硬。

## 题材定位
升级流

## 核心冲突
冲突

## 主角目标
目标

## 终局方向
终局

## 写作禁区
禁区

## 差异化卖点
卖点

## 差异化钩子
钩子

## 核心兑现承诺
兑现

## 故事引擎
引擎

## 中段转折
转折
`

	structure := premiseStructure(premise, domain.PlanningTierMid)
	if ready, _ := structure["template_ready"].(bool); !ready {
		t.Fatalf("expected template_ready, got %+v", structure)
	}
	missing, _ := structure["missing"].([]string)
	if len(missing) != 0 {
		t.Fatalf("expected no missing headings, got %+v", missing)
	}
}

func TestPremiseStructureShortAcceptsLegacyHeadingAlias(t *testing.T) {
	premise := `## 题材和基调
单卷高压营救。

## 题材定位
短篇高密度冒险。

## 核心冲突
主角必须在一夜内救出人质。

## 主角目标
救出人质并活着离开。

## 结局方向
完成任务但付出代价。

## 写作禁区
不扩展成长期连载。

## 差异化卖点
时限压力与连续反转。

## 差异化钩子
每次选择都缩短救援时间。

## 核心兑现承诺
紧迫感、抉择与反转。

## 本作为什么适合短篇/单卷收束
核心矛盾和人物弧线都能在单次任务中完成。
`

	structure := premiseStructure(premise, domain.PlanningTierShort)
	if ready, _ := structure["template_ready"].(bool); !ready {
		t.Fatalf("expected short template_ready, got %+v", structure)
	}
}

const viLongPremise = `# Tiền đề cốt truyện

## Thể loại và giọng điệu
x
## Định vị thể loại
x
## Xung đột cốt lõi
x
## Mục tiêu nhân vật chính
x
## Hướng kết cục
x
## Vùng cấm sáng tác
x
## Điểm bán hàng khác biệt
x
## Móc câu khác biệt
x
## Cam kết cốt lõi
x
## Động cơ câu chuyện
x
## Tuyến quan hệ/trưởng thành
x
## Lộ trình nâng cấp
x
## Chuyển hướng trung kỳ
x
## Mệnh đề kết cục
x
`

func TestParsePremiseSectionsVietnamese(t *testing.T) {
	structure := premiseStructure(viLongPremise, domain.PlanningTierLong)
	if ready, _ := structure["template_ready"].(bool); !ready {
		t.Fatalf("premise tiếng Việt đủ tiêu đề phải template_ready: %+v", structure)
	}
	if missing, _ := structure["missing"].([]string); len(missing) != 0 {
		t.Fatalf("không được thiếu tiêu đề: %+v", missing)
	}
	if parsePremiseSections(viLongPremise)["Xung đột cốt lõi"] == "" {
		t.Fatal("thiếu khóa Xung đột cốt lõi")
	}
}

func TestParsePremiseSectionsTolerateTrailingNote(t *testing.T) {
	sections := parsePremiseSections("## Hướng kết cục (Định hướng chủ đề)\na\n## MÓC CÂU khác biệt: điểm độc đáo\nb\n")
	if sections["Hướng kết cục"] != "a" || sections["Móc câu khác biệt"] != "b" {
		t.Fatalf("đuôi mô tả phải được bỏ qua: %+v", sections)
	}
}

func TestParsePremiseSectionsChineseStillWorks(t *testing.T) {
	zh := "## 题材和基调\nx\n## 题材定位\nx\n## 核心冲突\nx\n## 主角目标\nx\n## 终局方向\nx\n## 写作禁区\nx\n## 差异化卖点\nx\n## 差异化钩子\nx\n## 核心兑现承诺\nx\n## 短篇适配性\nx\n"
	structure := premiseStructure(zh, domain.PlanningTierShort)
	if ready, _ := structure["template_ready"].(bool); !ready {
		t.Fatalf("premise zh phải vẫn parse được: %+v", structure)
	}
	if parsePremiseSections(zh)["Xung đột cốt lõi"] == "" {
		t.Fatal("khóa chuẩn phải là tiếng Việt")
	}
}

func TestPremiseStructureMissingIsVietnamese(t *testing.T) {
	structure := premiseStructure("## Xung đột cốt lõi\nx\n", domain.PlanningTierLong)
	missing, _ := structure["missing"].([]string)
	if len(missing) == 0 || strings.IndexFunc(strings.Join(missing, ""), func(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }) >= 0 {
		t.Fatalf("missing phải khác rỗng và không chứa chữ Hán: %+v", missing)
	}
}
