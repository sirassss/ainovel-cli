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

// PlanChapterTool saves the chapter plan; the Agent decides the planning granularity itself.
type PlanChapterTool struct {
	store *store.Store
}

func NewPlanChapterTool(store *store.Store) *PlanChapterTool {
	return &PlanChapterTool{store: store}
}

func (t *PlanChapterTool) Name() string { return "plan_chapter" }
func (t *PlanChapterTool) Description() string {
	return "Lưu ý tưởng viết chương. Agent tự quyết mức độ chi tiết của kế hoạch, không bắt buộc chia cảnh"
}
func (t *PlanChapterTool) Label() string { return "Lập kế hoạch chương" }

// Write tool; concurrency is forbidden.
func (t *PlanChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *PlanChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *PlanChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("title", schema.String("Tiêu đề chương tạm thời; có thể điều chỉnh theo nội dung chương sau khi viết")).Required(),
		schema.Property("goal", schema.String("Mục tiêu chương")).Required(),
		schema.Property("conflict", schema.String("Xung đột cốt lõi")).Required(),
		schema.Property("hook", schema.String("Hook cuối chương")).Required(),
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
	// Chapters in the rewrite queue are the one exception: when a completed chapter must be rewritten, there has to be somewhere on disk for "what it should be rewritten into",
	// and the Contract (goal / payoff_points / continuity_checks / hook_goal) is exactly that instruction.
	// This used to reject unconditionally, which blocked all three paths: revise_outline (only unwritten chapters may change), save_foundation(outline)
	// (full overwrite forbidden while writing) and this tool; the Architect had nowhere to write the rewrite direction, and the circuit breaker tripped after 4 idle turns in practice.
	queuedForRewrite := progress != nil && slices.Contains(progress.PendingRewrites, plan.Chapter)
	if completed && !queuedForRewrite {
		return json.Marshal(map[string]any{
			"chapter":   plan.Chapter,
			"skipped":   true,
			"completed": true,
			"reason":    fmt.Sprintf("Chương %d đã nộp xong, không thể lập kế hoạch lại", plan.Chapter),
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
	// Recording a rewrite instruction is not the same as starting to write this chapter: the Engine already pre-marks it in progress when dispatching the writer
	// (engine.go), and draft_chapter marks it again when it starts writing, so for a rewrite chapter this is redundant here.
	// Moreover StartChapter unconditionally rewrites InProgressChapter and clears CompletedScenes;
	// if the planner writes an instruction for chapter 20 in the queue while the writer is on chapter 13, the pointer would be yanked away.
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
		"next_step": "Gọi ngay draft_chapter(chapter=số chương này, content=chuỗi nội dung chương đầy đủ) để ghi nội dung chương, đừng lập kế hoạch lặp lại cùng một chương",
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
