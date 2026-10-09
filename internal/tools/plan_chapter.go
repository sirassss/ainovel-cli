package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// PlanChapterTool 保存章节构思，Agent 自主决定规划粒度。
type PlanChapterTool struct {
	store *store.Store
}

func NewPlanChapterTool(store *store.Store) *PlanChapterTool {
	return &PlanChapterTool{store: store}
}

func (t *PlanChapterTool) Name() string { return "plan_chapter" }
func (t *PlanChapterTool) Description() string {
	return "Lưu ý tưởng viết chương. Agent tự quyết mức độ chi tiết của quy hoạch, không bắt buộc chia cảnh"
}
func (t *PlanChapterTool) Label() string { return "Quy hoạch chương" }

// 写工具，禁止并发。
func (t *PlanChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *PlanChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *PlanChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("title", schema.String("Tiêu đề chương tạm thời; có thể điều chỉnh theo chính văn sau khi viết")).Required(),
		schema.Property("goal", schema.String("Mục tiêu chương")).Required(),
		schema.Property("conflict", schema.String("Xung đột cốt lõi")).Required(),
		schema.Property("hook", schema.String("Móc câu cuối chương")).Required(),
		schema.Property("emotion_arc", schema.String("Đường cong cảm xúc")),
		schema.Property("notes", schema.String("Ghi chú tự do (bất cứ điều gì bạn thấy cần nhớ khi viết)")),
		schema.Property("required_beats", schema.Array("Các hạng mục tiến triển chương này bắt buộc hoàn thành", schema.String(""))),
		schema.Property("forbidden_moves", schema.Array("Các tiến triển chương này tuyệt đối không được xảy ra", schema.String(""))),
		schema.Property("continuity_checks", schema.Array("Các điểm liên tục cần đối chiếu đặc biệt trong chương này", schema.String(""))),
		schema.Property("evaluation_focus", schema.Array("Hạng mục Editor cần kiểm tra trọng điểm", schema.String(""))),
		schema.Property("emotion_target", schema.String("Tùy chọn: cảm xúc chính chương này muốn độc giả cảm nhận")),
		schema.Property("payoff_points", schema.Array("Tùy chọn: điểm tình tiết hoặc điểm hồi đáp mà chương then chốt muốn đáp lại", schema.String(""))),
		schema.Property("hook_goal", schema.String("Tùy chọn: ham muốn đọc tiếp hoặc mục tiêu hồi hộp mà cuối chương muốn thúc đẩy")),
	)
}

func (t *PlanChapterTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	plan, err := decodeChapterPlanArgs(args)
	if err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if plan.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	completed := progress != nil && slices.Contains(progress.CompletedChapters, plan.Chapter)
	// 返工队列里的章节是唯一例外：已完成却要重写时，"该重写成什么样"必须有地方落盘，
	// 而 Contract（goal / payoff_points / continuity_checks / hook_goal）正是那份指令。
	// 此前这里一律拒绝，导致 revise_outline（只许改未写章节）、save_foundation(outline)
	// （写作期禁止全量覆盖）与本工具三路皆堵——架构师无处可写重写方向，实测空转 4 次后熔断。
	queuedForRewrite := progress != nil && slices.Contains(progress.PendingRewrites, plan.Chapter)
	if completed && !queuedForRewrite {
		return json.Marshal(map[string]any{
			"chapter":   plan.Chapter,
			"skipped":   true,
			"completed": true,
			"reason":    fmt.Sprintf("Chương %d đã nộp xong, không thể quy hoạch lại", plan.Chapter),
		})
	}
	if err := t.store.Progress.ValidateChapterWork(plan.Chapter); err != nil {
		return nil, err
	}
	if err := EnsureChapterExpanded(t.store, plan.Chapter); err != nil {
		return nil, err
	}

	if err := t.store.Drafts.SaveChapterPlan(plan); err != nil {
		return nil, fmt.Errorf("save chapter plan: %w", err)
	}
	// 记录返工指令不等于开始写这一章：Engine 在派发 writer 时已预标进行中
	// (engine.go)，draft_chapter 落笔时也会再标一次，所以这里对返工章是多余的。
	// 而 StartChapter 会无条件改写 InProgressChapter 并清空 CompletedScenes——
	// 若规划者为队列里的第 20 章写指令、而 writer 正在第 13 章，指针会被拽走。
	if !queuedForRewrite || progress.InProgressChapter == plan.Chapter {
		if err := t.store.Progress.StartChapter(plan.Chapter); err != nil {
			return nil, fmt.Errorf("mark chapter in progress: %w", err)
		}
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(plan.Chapter), "plan",
		fmt.Sprintf("drafts/%02d.plan.json", plan.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint chapter plan: %w", err)
	}

	return json.Marshal(map[string]any{
		"planned":   true,
		"chapter":   plan.Chapter,
		"next_step": "Gọi ngay draft_chapter(chapter=số chương này, content=chuỗi chính văn đầy đủ) để ghi chính văn, đừng quy hoạch lặp lại cùng một chương",
	})
}

func decodeChapterPlanArgs(args json.RawMessage) (domain.ChapterPlan, error) {
	var a struct {
		Chapter          int      `json:"chapter"`
		Title            string   `json:"title"`
		Goal             string   `json:"goal"`
		Conflict         string   `json:"conflict"`
		Hook             string   `json:"hook"`
		EmotionArc       string   `json:"emotion_arc"`
		Notes            string   `json:"notes"`
		RequiredBeats    []string `json:"required_beats"`
		ForbiddenMoves   []string `json:"forbidden_moves"`
		ContinuityChecks []string `json:"continuity_checks"`
		EvaluationFocus  []string `json:"evaluation_focus"`
		EmotionTarget    string   `json:"emotion_target"`
		PayoffPoints     []string `json:"payoff_points"`
		HookGoal         string   `json:"hook_goal"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return domain.ChapterPlan{}, err
	}

	return domain.ChapterPlan{
		Chapter:    a.Chapter,
		Title:      a.Title,
		Goal:       a.Goal,
		Conflict:   a.Conflict,
		Hook:       a.Hook,
		EmotionArc: a.EmotionArc,
		Notes:      a.Notes,
		Contract: domain.ChapterContract{
			RequiredBeats:    a.RequiredBeats,
			ForbiddenMoves:   a.ForbiddenMoves,
			ContinuityChecks: a.ContinuityChecks,
			EvaluationFocus:  a.EvaluationFocus,
			EmotionTarget:    a.EmotionTarget,
			PayoffPoints:     a.PayoffPoints,
			HookGoal:         a.HookGoal,
		},
	}, nil
}
