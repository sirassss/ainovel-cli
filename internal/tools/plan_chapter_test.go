package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func planArgs(chapter int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"chapter":     chapter,
		"title":       "测试章",
		"goal":        "推进剧情",
		"conflict":    "外部阻力",
		"hook":        "留下悬念",
		"emotion_arc": "紧张到期待",
	})
	return b
}

func TestPlanChapterRejectsUnexpandedLayeredChapter(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init(5); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1,
		Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1,
			Title: "第一弧",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "一"},
				{Chapter: 2, Title: "二"},
			},
		}, {
			Index:             2,
			Title:             "第二弧",
			EstimatedChapters: 3,
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewPlanChapterTool(st)
	if _, err := tool.Execute(context.Background(), planArgs(3)); err == nil || !strings.Contains(err.Error(), "expand_arc") {
		t.Fatalf("expected unexpanded chapter rejection, got %v", err)
	}
	if p, _ := st.Progress.Load(); p != nil && p.InProgressChapter == 3 {
		t.Fatal("unexpanded chapter should not become in-progress")
	}
}

func TestPlanChapterAllowsExpandedLayeredChapter(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init(2); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1,
		Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1,
			Title: "第一弧",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "一"},
				{Chapter: 2, Title: "二"},
			},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewPlanChapterTool(st)
	if _, err := tool.Execute(context.Background(), planArgs(2)); err != nil {
		t.Fatalf("expected expanded chapter to plan, got %v", err)
	}
}

// A chapter that is already written but sits in the rewrite queue must be plannable:
// this is the only place that records "what to rewrite it into". Previously it was rejected, so
// revise_outline / save_foundation(outline) / plan_chapter blocked all three paths, and the architect
// spun for 4 turns before the circuit breaker tripped.
func TestPlanChapterAllowsChapterQueuedForRewrite(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init(3); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.StartChapter(1); err != nil {
		t.Fatalf("StartChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "đoạn mở đầu trùng chương trước"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}

	raw, err := NewPlanChapterTool(st).Execute(context.Background(), planArgs(1))
	if err != nil {
		t.Fatalf("chương trong hàng viết lại phải lập kế hoạch được, nhận: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if skipped, _ := got["skipped"].(bool); skipped {
		t.Fatalf("không được bỏ qua, nhận: %v", got)
	}
	if plan, err := st.Drafts.LoadChapterPlan(1); err != nil || plan == nil {
		t.Fatalf("kế hoạch phải được ghi xuống, err=%v plan=%v", err, plan)
	}
}

// A chapter that is done and NOT waiting for rewrite must still be blocked; the old behavior is unchanged.
func TestPlanChapterStillSkipsSettledCompletedChapter(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init(3); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.StartChapter(1); err != nil {
		t.Fatalf("StartChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}

	raw, err := NewPlanChapterTool(st).Execute(context.Background(), planArgs(1))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if skipped, _ := got["skipped"].(bool); !skipped {
		t.Fatalf("chương đã chốt phải bị bỏ qua, nhận: %v", got)
	}
}
