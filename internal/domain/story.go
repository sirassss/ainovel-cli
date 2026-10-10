package domain

import (
	"fmt"
	"strings"
)

// BookMetadata 是面向读者和出版物的作品信息。
// 创作设定属于 Foundation，运行进度属于 Progress，二者都不承载这份数据。
type BookMetadata struct {
	Title    string `json:"title"`
	Synopsis string `json:"synopsis"`
}

// Normalized 返回可持久化、可比较的规范值。
func (b BookMetadata) Normalized() BookMetadata {
	b.Title = strings.TrimSpace(b.Title)
	b.Synopsis = strings.TrimSpace(b.Synopsis)
	return b
}

// Validate 检查作品信息的必填字段。
func (b BookMetadata) Validate() error {
	b = b.Normalized()
	if b.Title == "" {
		return fmt.Errorf("book title is required")
	}
	if b.Synopsis == "" {
		return fmt.Errorf("book synopsis is required")
	}
	return nil
}

// OutlineEntry 大纲条目，对应一章。
type OutlineEntry struct {
	Chapter   int      `json:"chapter"`
	Title     string   `json:"title"`
	CoreEvent string   `json:"core_event"`
	Hook      string   `json:"hook"`
	Scenes    []string `json:"scenes"`
}

// Character 角色档案。
type Character struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"` // 别名/称号/绰号（如"废物少年"、"炎哥"）
	Role        string   `json:"role"`
	Description string   `json:"description"`
	Arc         string   `json:"arc"`
	Traits      []string `json:"traits"`
	Tier        string   `json:"tier,omitempty"` // core / important / secondary / decorative（默认 important）
}

// VolumeOutline 卷级大纲（长篇分层模式）。
type VolumeOutline struct {
	Index int          `json:"index"`
	Title string       `json:"title"`
	Theme string       `json:"theme"`           // 本卷核心冲突/主题
	Final bool         `json:"final,omitempty"` // 收官卷：全书在本卷收束（架构师 append_volume 时宣告）
	Arcs  []ArcOutline `json:"arcs"`
}

// IsExpanded 判断卷是否已展开（有弧级结构）。
func (v *VolumeOutline) IsExpanded() bool { return len(v.Arcs) > 0 }

// FinaleVolume 返回已宣告的收官卷序号，未宣告返回 0。
// 收官事实 = "最后一卷带 Final 标记"：宣告后全书进入收束态（规划收线、终卷结构
// 写完即完结）；若此后又追加了未标记的新卷，新卷成为最后一卷，收束态自然解除——
// 因此无需撤销工具，状态永远可从大纲数据推导。
func FinaleVolume(volumes []VolumeOutline) int {
	if n := len(volumes); n > 0 && volumes[n-1].Final {
		return volumes[n-1].Index
	}
	return 0
}

// StoryCompass 终局方向指南针，替代固定的骨架卷列表。
// Architect 在每次卷边界时可更新，允许故事方向随创作演化。
type StoryCompass struct {
	EndingDirection string   `json:"ending_direction"`          // 终局方向（主题性描述）
	OpenThreads     []string `json:"open_threads,omitempty"`    // 活跃长线（需收束才能结局）
	EstimatedScale  string   `json:"estimated_scale,omitempty"` // 模糊规模（如"预计 4-6 卷"）
	LastUpdated     int      `json:"last_updated,omitempty"`    // 更新时的已完成章节数
}

// ArcOutline 弧级大纲。
type ArcOutline struct {
	Index             int            `json:"index"` // 卷内弧序号
	Title             string         `json:"title"`
	Goal              string         `json:"goal"`                         // 弧目标（起承转合）
	EstimatedChapters int            `json:"estimated_chapters,omitempty"` // 骨架弧的预估章数（展开后清零）
	Chapters          []OutlineEntry `json:"chapters"`
}

// IsExpanded 判断弧是否已展开（有详细章节）。
func (a *ArcOutline) IsExpanded() bool { return len(a.Chapters) > 0 }

// ArcExpansion 是 Architect 在结构边界对一个未写弧作出的完整规划。
// Title/Goal 不是骨架的机械副本：模型可依据已完成正文修订尚未发生的计划。
type ArcExpansion struct {
	Title    string         `json:"title"`
	Goal     string         `json:"goal"`
	Chapters []OutlineEntry `json:"chapters"`
}

// EstimatedChapterCapacity 计算分层大纲的内部容量估算：已展开弧按真实章节数，
// 骨架弧按 EstimatedChapters。它只用于上下文策略，不是全书总章数；真正已细化、
// 可写的章节始终来自 FlattenOutline，禁止把本值暴露给用户或模型。
func EstimatedChapterCapacity(volumes []VolumeOutline) int {
	n := 0
	for _, v := range volumes {
		for _, a := range v.Arcs {
			if a.IsExpanded() {
				n += len(a.Chapters)
			} else {
				n += a.EstimatedChapters
			}
		}
	}
	return n
}

// FlattenOutline 将分层大纲展开为扁平章节列表，保持全局章节号连续。
func FlattenOutline(volumes []VolumeOutline) []OutlineEntry {
	var result []OutlineEntry
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for _, e := range a.Chapters {
				e.Chapter = ch
				result = append(result, e)
				ch++
			}
		}
	}
	return result
}

// WorldRule 世界观规则条目。
type WorldRule struct {
	Category string `json:"category"` // magic / technology / geography / society / other
	Rule     string `json:"rule"`     // 规则描述
	Boundary string `json:"boundary"` // 不可违反的边界
}

// RenumberVolumes 按位置重排卷与弧的序号，从 1 开始。
//
// 规划模型写 index 时并不可靠：常见从 0 起算，甚至给同一卷内每个弧都写 0。
// 而 ExpandArc / ArcScope 是按 index 值查找的，重复或 0 值会让弧永远无法寻址——
// 症状要到几步之后 expand_arc 报「参数无效」才浮现，且报错完全指错方向。
//
// 数组顺序才是事实，index 只是它的名字，因此在落盘前统一以位置改写。
func RenumberVolumes(volumes []VolumeOutline) {
	for vi := range volumes {
		volumes[vi].Index = vi + 1
		for ai := range volumes[vi].Arcs {
			volumes[vi].Arcs[ai].Index = ai + 1
		}
	}
}

// 同一钩子/核心事件重复到此次数即判为大纲空转。2 次可能是有意的两段式，
// 3 次起没有正当写法：读者被同一个悬念挂三章而无人兑现。
const (
	maxHookRepeat      = 3
	maxCoreEventRepeat = 2
)

// StalledOutline 检出「原地打转」的大纲：章节标题各异，但钩子或核心事件是同一句
// 复制多份。此时 Writer 会忠实执行——每章重述上章、再添一点，读起来像改写而非续写。
//
// 这类缺陷 Writer 与 Editor 都察觉不到：两者都只看单章，而单章本身自洽。
// 必须在大纲落盘处按整本比对才拦得住。
//
// 返回空串表示通过；否则为可直接回给规划师的中文诊断。
func StalledOutline(entries []OutlineEntry) string {
	hooks := map[string]int{}
	events := map[string]int{}
	for _, e := range entries {
		if h := strings.TrimSpace(e.Hook); h != "" {
			hooks[h]++
		}
		if c := strings.TrimSpace(e.CoreEvent); c != "" {
			events[c]++
		}
	}
	if worst, n := mostRepeated(hooks); n >= maxHookRepeat {
		return fmt.Sprintf("Dàn ý quay vòng: cùng một hook lặp ở %d/%d chương — %q. "+
			"hook của mỗi chương phải là hệ quả mới phát sinh trong chương đó và được chương kế tiếp đáp lại; hãy viết lại từng chương", n, len(entries), truncateRunes(worst, 40))
	}
	if worst, n := mostRepeated(events); n >= maxCoreEventRepeat {
		return fmt.Sprintf("Dàn ý quay vòng: cùng một core_event lặp ở %d chương — %q. "+
			"Mỗi chương phải xảy ra việc khác nhau và làm thay đổi tình thế; hãy viết lại từng chương", n, truncateRunes(worst, 40))
	}
	return ""
}

func mostRepeated(m map[string]int) (string, int) {
	var key string
	best := 0
	for k, n := range m {
		if n > best {
			key, best = k, n
		}
	}
	return key, best
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// SkeletonArcs 统计尚未展开的骨架弧，用于完本前置校验。
//
// 完本校验只比对扁平大纲，而扁平大纲由 FlattenOutline 从「已展开的弧」派生——
// 骨架弧贡献 0 章，对该校验完全隐形。实测事故：第 1 卷两个骨架弧共 38 章从未展开，
// 架构师直接跳到第 2 卷写完 15 章后宣告完本，校验因 next(16) > len(flat)(15) 而放行。
//
// 想提前收束仍有正当出口：append_volume 带 "final": true 宣告收官卷。
func SkeletonArcs(volumes []VolumeOutline) []string {
	var out []string
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			if a := &volumes[vi].Arcs[ai]; !a.IsExpanded() {
				out = append(out, fmt.Sprintf("Tập %d cung %d \"%s\"", volumes[vi].Index, a.Index, a.Title))
			}
		}
	}
	return out
}

// maxArcChapters 是单弧详细章节数的上限。
//
// 约束来自弧末评审，不是叙事口味：Editor 在弧边界必须读完整弧才能出审阅意见。
// 实测一弧 20 章 = 113792 字 ≈ 37k token 的正文，叠加大纲/快照/提示后没有任何
// 可用模型吃得下——本地 32k 窗口装不进，云端免费档在 8 tok/s 下反复断流 14 次，
// 最终整条流水线卡死在弧边界。8 章 ≈ 45k 字 ≈ 15k token，两侧都留有余量。
//
// 这也是结构上的好事：把 20 章塞进一弧，本身就说明弧目标没有收敛。
const maxArcChapters = 8

// OversizedArc 检查一个弧的规模是否超限，超限返回可直接回给规划师的诊断。
// 返回空串表示通过。
//
// chapters 取"详细章节数"与"骨架预估章数"的较大者：骨架阶段就写下 estimated=20 的弧，
// 到 expand_arc 时必然撞上同一道墙，而那已是二十章之后——结构问题要在结构落盘时就报。
func OversizedArc(label string, chapters int) string {
	if chapters <= maxArcChapters {
		return ""
	}
	return fmt.Sprintf("%s có %d chương chi tiết, vượt giới hạn %d chương mỗi cung. "+
		"Đánh giá cuối cung phải đọc trọn cả cung, cung quá dài thì model nào cũng không đánh giá nổi (thực tế 20 chương đã làm kẹt cả pipeline). "+
		"Hãy tách thành nhiều cung, mỗi cung có mục tiêu độc lập: lần gọi này chỉ mở rộng cung đầu tiên trong phạm vi %d chương, "+
		"các cung còn lại để dạng phác thảo, tới biên thì mở rộng tiếp",
		label, chapters, maxArcChapters, maxArcChapters)
}
