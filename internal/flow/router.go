// Package flow 实现垂类路由：Host 根据事实决定下一个调哪个子代理做什么。
//
// 设计原则：
//   - Route 是纯函数：输入 State，输出 *Instruction。无 IO、无 Store 调用，可单测。
//   - State 由 LoadState（非纯）从 Store 构造，一次性把路由需要的事实读齐。
//   - 返回 nil 是合法的：表示当前没有可由确定性事实推出的 Worker 指令；
//     Engine 再按终态、启动补裁或等待用户干预处理。
//
// Router 覆盖的是"查表型"决策（每章下一步、弧末后处理、队列驱动），
// 不覆盖"语义理解型"决策（选规划师、处理用户 Steer、输出总结）。
package flow

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// plannerForTier 从已落盘的规划级别推导规划师身份:short 归短篇规划师,
// mid/long 归长篇规划师(与启动 Arbiter 的选型口径一致)。
func plannerForTier(tier domain.PlanningTier) string {
	if tier == domain.PlanningTierShort {
		return "architect_short"
	}
	return "architect_long"
}

// Instruction 指示 Engine 下一步直接运行的 Worker 与任务。
type Instruction struct {
	Agent   string // architect_long / architect_short / writer / editor
	Task    string // 给子代理的任务描述
	Reason  string // 路由理由（用于事件、日志与失败裁定）
	Chapter int    // writer 任务涉及的章节号（续写/重写/打磨）；0 表示不涉及（editor/architect 任务）
}

type AggregateKind string

const (
	AggregateArcReview     AggregateKind = "arc_review"
	AggregateArcSummary    AggregateKind = "arc_summary"
	AggregateVolumeSummary AggregateKind = "volume_summary"
	AggregateGlobalReview  AggregateKind = "global_review"
)

type AggregateRefresh struct {
	Kind         AggregateKind
	Volume       int
	Arc          int
	StartChapter int
	EndChapter   int
}

// State 是 Route 的输入：所有事实必须在此显式声明，禁止 Route 内部读 Store。
type State struct {
	Progress *domain.Progress

	// 已完成章节中的最大章节号；为 0 表示尚未开始写作。
	LastCompleted int

	// 上一章的弧边界信息；IsArcEnd=false 时其他字段无意义。
	// 当 LastCompleted=0 或非 Layered 模式时应为 nil。
	ArcBoundary *storepkg.ArcBoundary

	// 弧末后处理的三个事实：评审 / 弧摘要 / 卷摘要是否已完成。
	HasArcReview     bool
	HasArcSummary    bool
	HasVolumeSummary bool

	// 基础设定缺项（规划阶段的补齐信号）。
	FoundationMissing []string

	// 已落盘的规划级别（save_foundation 落 scale 时写入 RunMeta）。
	// 空 = 首次规划尚未产出任何设定，规划师身份不可判定。
	PlanningTier domain.PlanningTier

	// 非分层书：最近完成章是否已有 scope=global 的全局审阅
	//（仅在 ShouldReview 触发点有意义；分层书恒 false）。
	HasGlobalReview bool

	// 必须在续写前由 Architect 处理的外部修订影响。普通 Writer 反馈留到下一次
	// 自然结构操作统一吸收，不为每章额外派发规划师。
	ImmediateFeedbackCount int

	// 外部修订后最早一个需要由 Editor 重新生成的弧/卷工件。
	AggregateRefresh *AggregateRefresh

	// 返工队首章是否还没有重写指令（chapter_contract）。
	// 只给 writer 一句"重写第 N 章"等于不给方向：实测 Editor 判定队列里最严重的
	// 问题就是 architect_directive_unclear——症状列了一堆，却没说该写成什么样。
	RewriteHeadNeedsDirective bool
}

// Route 根据事实返回下一步确定性指令；返回 nil 由 Engine 按调用上下文处理。
//
// 决策优先级（互斥，自上而下匹配第一个）：
//  1. Phase=Complete        → nil（Host 确定性输出总结）
//  2. 规划期设定缺项且规划师可判定 → 同一规划师补齐；否则 nil（Engine 启动补裁）
//  3. PendingRewrites 非空  → writer 按队列重写/打磨
//  4. Flow=Reviewing        → nil（dormant：当前无写入者，评审期 Flow 实为 writing）
//  5. Flow=Steering         → nil（用户干预处理中）
//  6. 外部修订导致聚合工件失效 → editor 重建
//  7. 外部修订影响后续规划     → architect 处理
//  8. 分层书到达弧末          → 评审、摘要、扩弧或续卷
//  9. 非分层全局审阅到期       → editor(global review)
//
// 10. 非分层大纲已耗尽        → architect(决定完结或续接大纲)
// 11. 其它                   → writer(写 next_chapter)
func Route(s State) *Instruction {
	p := s.Progress
	if p == nil {
		return nil
	}

	// 1. 终态：Host 根据 store 事实生成确定性总结
	if p.Phase == domain.PhaseComplete {
		return nil
	}

	// 2. 规划期补齐：查表型决策——缺什么在 store，规划师身份从已落盘的 scale 推导
	//    （short → architect_short，其余 → architect_long）。tier 为空说明首次规划
	//    尚未落盘任何设定（选型是语义判断），由 Engine 的 planStartFallback 补裁。
	if p.Phase != domain.PhaseWriting {
		if len(s.FoundationMissing) > 0 && s.PlanningTier != "" {
			task := fmt.Sprintf("Bổ sung các mục còn thiếu của thiết lập nền tảng và thông tin tác phẩm: %s; book dùng save_book, các thiết lập nền tảng khác lưu bằng save_foundation", strings.Join(s.FoundationMissing, ", "))
			if len(s.FoundationMissing) == 1 && s.FoundationMissing[0] == "foundation_audit" {
				task = "Thiết lập nền tảng đã đủ: gọi lại novel_context để đọc toàn bộ sản phẩm đã lưu và foundation_status.fingerprint, thẩm tra tính nhất quán ngữ nghĩa giữa các tệp rồi gọi audit_foundation; nếu có vấn đề thì sửa trước và thẩm tra lại"
			}
			return &Instruction{
				Agent:  plannerForTier(s.PlanningTier),
				Task:   task,
				Reason: "Thiết lập nền tảng còn thiếu mục, tiếp tục giao cho cùng kiến trúc sư theo các mục còn thiếu",
			}
		}
		return nil
	}

	// 3. 重写/打磨队列优先（事实已在工具层落盘，Router 只照单派发）
	if len(p.PendingRewrites) > 0 {
		ch := p.PendingRewrites[0]
		verb := "Viết lại"
		if p.Flow == domain.FlowPolishing {
			verb = "Trau chuốt"
		}
		task := fmt.Sprintf("%s chương %d", verb, ch)
		if s.RewriteHeadNeedsDirective {
			// 只说"重写第 N 章"等于没给方向：实测 Editor 把 architect_directive_unclear
			// 判为返工队列里最严重的一条。这里不另派规划师——多插一次派单会让
			// 规划失败时队列永远排不空——而是要求 Writer 自己先把方向落成契约再动笔。
			task += fmt.Sprintf(
				". Chương này chưa có chapter_contract: hãy gọi plan_chapter(chapter=%d) để nêu rõ mục tiêu viết lại"+
					"(goal phải trả lời \"sửa thành như thế nào\", không phải nhắc lại \"sai ở đâu\"; "+
					"nếu cần bổ sung payoff_points / continuity_checks / hook_goal), rồi dựa vào đó viết lại nội dung chương", ch)
		}
		return &Instruction{
			Agent:   "writer",
			Task:    task,
			Reason:  fmt.Sprintf("Hàng đợi viết lại (PendingRewrites) còn %d chương", len(p.PendingRewrites)),
			Chapter: ch,
		}
	}

	// 4. 审阅中 → 交回 LLM。当前为 dormant 分支：save_review 只把 Flow 置为
	//    writing/rewriting/polishing，无任何生产路径置 reviewing（评审期 Flow 实为 writing，
	//    "评审先于续写"由 agentcore steering 优先级保证，不靠此分支）。保留以与 Steering
	//    对称，并在未来 editor 评审期显式置 reviewing 时使路由让位于 LLM。
	if p.Flow == domain.FlowReviewing {
		return nil
	}

	// 5. 用户干预处理中：Arbiter 正在裁定，Engine 不抢占
	if p.Flow == domain.FlowSteering {
		return nil
	}
	if refresh := s.AggregateRefresh; refresh != nil {
		switch refresh.Kind {
		case AggregateArcReview:
			return &Instruction{
				Agent: "editor",
				Task: fmt.Sprintf(
					"Đánh giá tập %d cung %d (chương %d-%d): gọi novel_context(chapter=%d), save_review dùng scope=arc, chapter=%d",
					refresh.Volume, refresh.Arc, refresh.StartChapter, refresh.EndChapter, refresh.EndChapter, refresh.EndChapter,
				),
				Reason: "Thiếu đánh giá cấp cung",
			}
		case AggregateArcSummary:
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Tạo tóm tắt tập %d cung %d, snapshot nhân vật và quy tắc viết (save_arc_summary)", refresh.Volume, refresh.Arc),
				Reason: "Thiếu tóm tắt cấp cung",
			}
		case AggregateVolumeSummary:
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Tạo tóm tắt tập %d (save_volume_summary)", refresh.Volume),
				Reason: "Thiếu tóm tắt tập",
			}
		case AggregateGlobalReview:
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Đánh giá %d chương đầu: gọi novel_context(chapter=%d), save_review dùng scope=global, chapter=%d", refresh.EndChapter, refresh.EndChapter, refresh.EndChapter),
				Reason: "Thiếu đánh giá toàn cục",
			}
		}
	}

	if s.ImmediateFeedbackCount > 0 {
		return &Instruction{
			Agent:  plannerForTier(s.PlanningTier),
			Task:   "Chỉ xử lý writer_feedback là phần sửa đổi bên ngoài trong novel_context: đối chiếu cốt truyện đã xảy ra với kế hoạch phía sau; cần điều chỉnh thì gọi revise_outline hoặc công cụ cấu trúc tương ứng, không cần thì gọi resolve_outline_feedback; không được xử lý foundation_status hay kế hoạch khác; lưu xong thì kết thúc bằng một câu",
			Reason: fmt.Sprintf("Có %d sửa đổi bên ngoài chưa được lan truyền sang kế hoạch phía sau", s.ImmediateFeedbackCount),
		}
	}

	// 8. 分层模式的弧末后处理
	if p.Layered && s.ArcBoundary != nil && s.ArcBoundary.IsArcEnd {
		b := s.ArcBoundary
		switch {
		case !s.HasArcReview:
			return &Instruction{
				Agent: "editor",
				Task: fmt.Sprintf(
					"Đánh giá cấp cung cho tập %d cung %d (chương %d-%d): gọi novel_context(chapter=%d), save_review dùng scope=arc, chapter=%d; issues[].chapters chỉ được nằm trong khoảng này",
					b.Volume, b.Arc, b.StartChapter, b.EndChapter, b.EndChapter, b.EndChapter,
				),
				Reason: "Đánh giá cuối cung chưa hoàn tất",
			}
		case !s.HasArcSummary:
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Tạo tóm tắt tập %d cung %d, snapshot nhân vật và quy tắc viết (save_arc_summary)", b.Volume, b.Arc),
				Reason: "Tóm tắt cung chưa hoàn tất",
			}
		case b.IsVolumeEnd && !s.HasVolumeSummary:
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Tạo tóm tắt tập %d (save_volume_summary)", b.Volume),
				Reason: "Tóm tắt tập chưa hoàn tất",
			}
		case b.NeedsExpansion && b.NextArc > 0:
			return &Instruction{
				Agent:  "architect_long",
				Task:   fmt.Sprintf("Mở rộng tập %d cung %d (save_foundation type=expand_arc)", b.NextVolume, b.NextArc),
				Reason: "Cung phác thảo kế tiếp chờ mở rộng",
			}
		case b.NeedsNewVolume:
			return &Instruction{
				Agent:  "architect_long",
				Task:   "Tạo tập kế tiếp: sau khi đánh giá theo danh sách kiểm tra kết thúc truyện, gọi save_foundation — truyện tiếp tục → type=append_volume; truyện gần đến đích → type=append_volume và JSON tập có \"final\": true ở cấp cao nhất (tập cuối, thu toàn bộ tuyến, viết xong tự kết thúc truyện); mọi điều kiện kết thúc truyện hiện đã thỏa → type=complete_book. Cả ba lựa chọn đều phải kèm tham số reason ghi rõ lý do phán định",
				Reason: "Cuối tập cần quyết định thêm tập mới, tập cuối hay kết thúc cả truyện",
			}
		}
	}

	// 11. 非分层全局审阅：每 ReviewInterval 章一次(事实:该章的 global review 未落盘)。
	//     原为 commit_chapter 返回值里的 review_required 信号,现按事实推导——
	//     返回值只是事实的镜像,Route 从 store 直接看同一事实。
	if !p.Layered && s.LastCompleted > 0 {
		if due, reason := domain.ShouldReview(len(p.CompletedChapters)); due && !s.HasGlobalReview {
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Đánh giá toàn cục %d chương đầu (save_review scope=global, chapter=%d)", s.LastCompleted, s.LastCompleted),
				Reason: reason,
			}
		}
	}

	// 12. 非分层大纲耗尽时不能继续派发越界章节。让 Architect 基于当前故事事实
	// 决定完结，或用 revise_outline 从 next 章续接计划。
	next := p.NextChapter()
	if next <= 0 {
		return nil
	}
	if !p.Layered && p.TotalChapters > 0 && next > p.TotalChapters {
		return &Instruction{
			Agent: plannerForTier(s.PlanningTier),
			Task: fmt.Sprintf(
				"Dàn ý phẳng đã viết xong (đã hoàn thành %d/%d chương): nếu truyện đã khép lại, gọi save_foundation(type=complete_book); nếu cần viết tiếp, dùng revise_outline nối kế hoạch từ chương %d",
				len(p.CompletedChapters), p.TotalChapters, next,
			),
			Reason: "Dàn ý phẳng đã cạn, cần quyết định kết thúc truyện hay nối tiếp",
		}
	}

	// 13. 正常续写
	return &Instruction{
		Agent:   "writer",
		Task:    fmt.Sprintf("Viết chương %d", next),
		Reason:  "Viết tiếp chương kế",
		Chapter: next,
	}
}
