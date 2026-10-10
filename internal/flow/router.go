// Package flow implements vertical routing: the Host decides from facts which sub-agent to call next and what it should do.
//
// Design principles:
//   - Route is a pure function: input State, output *Instruction. No IO, no Store calls, unit-testable.
//   - State is built from the Store by LoadState (impure), reading in one pass all the facts routing needs.
//   - Returning nil is legitimate: it means there is currently no Worker instruction that can be derived from deterministic facts;
//     the Engine then handles it by terminal state, startup arbitration, or waiting for user intervention.
//
// The Router covers "lookup-table" decisions (next step of each chapter, post-arc-end processing, queue-driven),
// and does not cover "semantic-understanding" decisions (choosing the planner, handling user Steer, producing the summary).
package flow

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// plannerForTier derives the planner identity from the persisted planning tier: short maps to the short-story planner,
// mid/long map to the long-form planner (consistent with how the startup Arbiter picks).
func plannerForTier(tier domain.PlanningTier) string {
	if tier == domain.PlanningTierShort {
		return "architect_short"
	}
	return "architect_long"
}

// Instruction tells the Engine which Worker to run directly next and with what task.
type Instruction struct {
	Agent   string // architect_long / architect_short / writer / editor
	Task    string // task description for the sub-agent
	Reason  string // routing reason (used for events, logs, and failure arbitration)
	Chapter int    // chapter number the writer task concerns (continue/rewrite/polish); 0 means none (editor/architect tasks)
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

// State is the input of Route: all facts must be declared explicitly here, and Route must not read the Store internally.
type State struct {
	Progress *domain.Progress

	// The largest chapter number among completed chapters; 0 means writing has not started.
	LastCompleted int

	// Arc boundary info of the previous chapter; other fields are meaningless when IsArcEnd=false.
	// Should be nil when LastCompleted=0 or not in Layered mode.
	ArcBoundary *storepkg.ArcBoundary

	// Three facts for post-arc-end processing: whether the review / arc summary / volume summary are done.
	HasArcReview     bool
	HasArcSummary    bool
	HasVolumeSummary bool

	// Missing foundation items (the completion signal of the planning stage).
	FoundationMissing []string

	// The persisted planning tier (save_foundation writes it into RunMeta when it persists scale).
	// Empty = the first planning has not produced any foundation yet, so the planner identity cannot be determined.
	PlanningTier domain.PlanningTier

	// Non-layered book: whether the most recently completed chapter already has a global review (scope=global)
	// (meaningful only at the ShouldReview trigger point; always false for layered books).
	HasGlobalReview bool

	// External revision impact that the Architect must handle before continuing to write. Ordinary Writer feedback is left for the next
	// natural structural operation to absorb together, without dispatching the planner extra for every chapter.
	ImmediateFeedbackCount int

	// The earliest arc/volume artifact that the Editor must regenerate after an external revision.
	AggregateRefresh *AggregateRefresh

	// Whether the chapter at the head of the rewrite queue still has no rewrite instruction (chapter_contract).
	// Giving the writer only "rewrite chapter N" amounts to giving no direction: measured, the most severe issue in the Editor's verdict queue
	// was architect_directive_unclear - a pile of symptoms listed, but no statement of what it should be rewritten into.
	RewriteHeadNeedsDirective bool
}

// Route returns the next deterministic instruction from the facts; a nil return is handled by the Engine according to the calling context.
//
// Decision priority (mutually exclusive, matched top-down, first match wins):
//  1. Phase=Complete        -> nil (the Host deterministically produces the summary)
//  2. planning-stage foundation items missing and planner determinable -> the same planner completes them; otherwise nil (the Engine arbitrates at startup)
//  3. PendingRewrites non-empty  -> writer rewrites/polishes per the queue
//  4. Flow=Reviewing        -> nil (dormant: currently no writer; during review Flow is actually writing)
//  5. Flow=Steering         -> nil (user intervention in progress)
//  6. external revision invalidated an aggregate artifact -> editor rebuilds
//  7. external revision affects later planning     -> architect handles it
//  8. layered book reaches an arc end          -> review, summary, arc expansion, or next volume
//  9. non-layered global review due       -> editor(global review)
//
// 10. non-layered outline exhausted        -> architect(decide on ending, or continue the outline)
// 11. otherwise                   -> writer(write next_chapter)
func Route(s State) *Instruction {
	p := s.Progress
	if p == nil {
		return nil
	}

	// 1. Terminal state: the Host generates a deterministic summary from store facts
	if p.Phase == domain.PhaseComplete {
		return nil
	}

	// 2. Planning-stage completion: a lookup-table decision - what is missing is in the store, and the planner identity is derived from the persisted scale
	//    (short -> architect_short, otherwise -> architect_long). An empty tier means the first planning
	//    has not persisted any foundation yet (choosing is a semantic judgment), left to the Engine's planStartFallback to arbitrate.
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

	// 3. The rewrite/polish queue takes priority (the facts are already persisted at the tool layer; the Router just dispatches per the order)
	if len(p.PendingRewrites) > 0 {
		ch := p.PendingRewrites[0]
		verb := "Viết lại"
		if p.Flow == domain.FlowPolishing {
			verb = "Trau chuốt"
		}
		task := fmt.Sprintf("%s chương %d", verb, ch)
		if s.RewriteHeadNeedsDirective {
			// Saying only "rewrite chapter N" amounts to giving no direction: measured, the Editor ranked architect_directive_unclear
			// as the most severe item in the rewrite queue. No planner is dispatched here - an extra dispatch would make the queue
			// never drain when planning fails - instead the Writer is required to first turn the direction into a contract before writing.
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

	// 4. Under review -> hand back to the LLM. Currently a dormant branch: save_review only sets Flow to
	//    writing/rewriting/polishing, and no production path sets reviewing (during review Flow is actually writing;
	//    "review before continuing" is guaranteed by the agentcore steering priority, not by this branch). Kept for symmetry
	//    with Steering, and so that routing yields to the LLM if the editor review phase explicitly sets reviewing in the future.
	if p.Flow == domain.FlowReviewing {
		return nil
	}

	// 5. User intervention in progress: the Arbiter is arbitrating, and the Engine does not preempt
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

	// 8. Post-arc-end processing in layered mode
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

	// 11. Non-layered global review: once every ReviewInterval chapters (fact: the global review of that chapter has not been persisted).
	//     Originally the review_required signal in the commit_chapter return value, now derived from facts -
	//     the return value is merely a mirror of the fact, and Route looks at the same fact directly from the store.
	if !p.Layered && s.LastCompleted > 0 {
		if due, reason := domain.ShouldReview(len(p.CompletedChapters)); due && !s.HasGlobalReview {
			return &Instruction{
				Agent:  "editor",
				Task:   fmt.Sprintf("Đánh giá toàn cục %d chương đầu (save_review scope=global, chapter=%d)", s.LastCompleted, s.LastCompleted),
				Reason: reason,
			}
		}
	}

	// 12. When a non-layered outline is exhausted, out-of-range chapters must not keep being dispatched. Let the Architect decide, from the current story facts,
	// whether to end the story, or to continue the plan from the next chapter with revise_outline.
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

	// 13. Normal continuation
	return &Instruction{
		Agent:   "writer",
		Task:    fmt.Sprintf("Viết chương %d", next),
		Reason:  "Viết tiếp chương kế",
		Chapter: next,
	}
}
