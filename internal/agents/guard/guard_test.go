package guard

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	return s
}

// TestSubAgentGuard_HardStopReasonEscalatesImmediately verifies: when the model returns an unrecoverable provider-side refusal
// such as safety / content_filter, the sub-agent StopGuard
// must Escalate immediately instead of injecting a nudge message.
//
// Historical background: measured with hy3-preview:free writing chapter 2, it refused 8 times in a row with stop_reason='safety';
// the old logic repeatedly injected "must commit", the model kept returning safety, and only after 3 blocks did it escalate,
// after which the Engine re-ran the writer 3 times in total. Each time was a fresh SubAgent -> the cache
// prefix went completely cold. After the fix, the first safety escalates immediately and the Engine can pause directly on an unrecoverable error.
//
// Note that only safety / content_filter are tested: StopReasonError / StopReasonAborted take the branch in
// agentcore loop.go that terminates the run directly and never call StopGuard; listing them would
// introduce dead code.
func TestSubAgentGuard_HardStopReasonEscalatesImmediately(t *testing.T) {
	cases := []agentcore.StopReason{
		agentcore.StopReason("safety"),
		agentcore.StopReason("content_filter"),
	}
	for _, sr := range cases {
		t.Run(string(sr), func(t *testing.T) {
			s := newTestStore(t)
			guard := NewWriterStopGuard(s, nil)
			info := agentcore.StopInfo{
				TurnIndex: 1,
				Message:   agentcore.Message{StopReason: sr},
			}
			d := guard(context.Background(), info)
			if !d.Escalate {
				t.Fatalf("stop_reason=%q must escalate immediately, got %#v", sr, d)
			}
			if d.InjectMessage != "" {
				t.Fatalf("stop_reason=%q must not inject any message, got %q", sr, d.InjectMessage)
			}
		})
	}
}

// TestSubAgentGuard_NormalStopStillBlocks ensures the blocking behavior for a normal stop_reason
// is unaffected by the hard-error bypass - when the LLM stops by itself without a commit, it still needs a nudge.
func TestSubAgentGuard_NormalStopStillBlocks(t *testing.T) {
	s := newTestStore(t)
	guard := NewWriterStopGuard(s, nil)
	info := agentcore.StopInfo{
		TurnIndex: 1,
		Message:   agentcore.Message{StopReason: agentcore.StopReasonStop},
	}
	d := guard(context.Background(), info)
	if d.Escalate {
		t.Fatal("normal stop must not escalate on first block")
	}
	if d.Allow {
		t.Fatal("normal stop must be blocked when no commit checkpoint exists")
	}
	if d.InjectMessage == "" {
		t.Fatal("normal stop must inject a follow-up message")
	}
}

// TestSubAgentGuard_ProgressBetweenBlocksResetsCounter verifies: when a new checkpoint appears between two blocks
// (the model re-drafts after being nudged, etc.) consecutive is reset - escalation only punishes consecutive idle spinning
// with no artifacts, following the "reset on progress" semantics (issue #75).
func TestSubAgentGuard_ProgressBetweenBlocksResetsCounter(t *testing.T) {
	s := newTestStore(t)
	guard := NewWriterStopGuard(s, nil)
	normalStop := agentcore.StopInfo{TurnIndex: 1, Message: agentcore.Message{StopReason: agentcore.StopReasonStop}}

	// Block -> persist a new draft (progress) -> block again: going back and forth past the threshold must not escalate.
	for i := 0; i < subagentMaxConsecutiveBlocks+2; i++ {
		if d := guard(context.Background(), normalStop); d.Escalate {
			t.Fatalf("escalated at block %d despite progress between blocks", i)
		}
		if _, err := s.Checkpoints.Append(domain.ChapterScope(1), "draft", "drafts/01.draft.md", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatalf("append draft: %v", err)
		}
	}
	// Progress stops: escalate only once consecutive idle blocks reach the threshold.
	for i := 0; i < subagentMaxConsecutiveBlocks; i++ {
		if d := guard(context.Background(), normalStop); d.Escalate {
			t.Fatalf("escalated too early at idle block %d", i)
		}
	}
	if d := guard(context.Background(), normalStop); !d.Escalate {
		t.Fatal("expected escalate after consecutive no-progress blocks")
	}
}

// TestWriterStopGuard_StageAwareBlockMessage verifies that the nudge message is assembled from the steps already persisted:
// the static "must call commit_chapter" misleads the model when a prerequisite step is missing or the commit reports an error (issue #75).
func TestWriterStopGuard_StageAwareBlockMessage(t *testing.T) {
	s := newTestStore(t)
	guard := NewWriterStopGuard(s, nil)
	normalStop := agentcore.StopInfo{TurnIndex: 1, Message: agentcore.Message{StopReason: agentcore.StopReasonStop}}

	// No artifacts at all: should guide the full flow rather than nudging commit directly.
	d := guard(context.Background(), normalStop)
	if !strings.Contains(d.InjectMessage, "draft_chapter") || !strings.Contains(d.InjectMessage, "plan_chapter") {
		t.Fatalf("no-draft message should walk through the protocol, got %q", d.InjectMessage)
	}

	// Draft persisted: should nudge check_consistency to wrap up.
	if _, err := s.Checkpoints.Append(domain.ChapterScope(1), "draft", "drafts/01.draft.md", "d1"); err != nil {
		t.Fatalf("append draft: %v", err)
	}
	d = guard(context.Background(), normalStop)
	if !strings.Contains(d.InjectMessage, "check_consistency") {
		t.Fatalf("draft-only message should point to check_consistency, got %q", d.InjectMessage)
	}

	// Draft + consistency check done: only the commit is left, and room must be left for the commit-error scenario.
	if _, err := s.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "meta/checks/01.json", "c1"); err != nil {
		t.Fatalf("append consistency_check: %v", err)
	}
	d = guard(context.Background(), normalStop)
	if !strings.Contains(d.InjectMessage, "commit_chapter") || !strings.Contains(d.InjectMessage, "lỗi") {
		t.Fatalf("ready-to-commit message should mention commit and error handling, got %q", d.InjectMessage)
	}
}

// TestSubAgentGuard_BlockHookReceivesAgentAndReason verifies the audit callback receives the correct
// agent name and reason sequence - the Host relies on it to surface blocks to the TUI.
func TestSubAgentGuard_BlockHookReceivesAgentAndReason(t *testing.T) {
	s := newTestStore(t)
	var agents, reasons []string
	guard := NewWriterStopGuard(s, func(agent, reason string, _ int32) {
		agents = append(agents, agent)
		reasons = append(reasons, reason)
	})
	normalStop := agentcore.StopInfo{TurnIndex: 1, Message: agentcore.Message{StopReason: agentcore.StopReasonStop}}

	for i := 0; i < subagentMaxConsecutiveBlocks+1; i++ {
		guard(context.Background(), normalStop)
	}
	if len(reasons) != subagentMaxConsecutiveBlocks+1 {
		t.Fatalf("hook called %d times, want %d", len(reasons), subagentMaxConsecutiveBlocks+1)
	}
	for i, agent := range agents {
		if agent != "writer" {
			t.Fatalf("hook call %d: agent = %q, want writer", i, agent)
		}
	}
	for i := 0; i < subagentMaxConsecutiveBlocks; i++ {
		if reasons[i] != "blocked" {
			t.Fatalf("reason[%d] = %q, want blocked", i, reasons[i])
		}
	}
	if last := reasons[len(reasons)-1]; last != "escalated" {
		t.Fatalf("last reason = %q, want escalated", last)
	}

	// hard_stop must also be reported.
	var hardReasons []string
	hardGuard := NewWriterStopGuard(s, func(_, reason string, _ int32) {
		hardReasons = append(hardReasons, reason)
	})
	hardGuard(context.Background(), agentcore.StopInfo{
		TurnIndex: 1,
		Message:   agentcore.Message{StopReason: agentcore.StopReason("safety")},
	})
	if len(hardReasons) != 1 || hardReasons[0] != "hard_stop" {
		t.Fatalf("hard stop hook reasons = %v, want [hard_stop]", hardReasons)
	}
}

// TestEditorStopGuard_TaskAware verifies task awareness: when dispatched to produce an arc summary, save_review (re-check) alone
// does not count as done; arc_summary must be produced before release - closing the starting point of the mid-volume skeleton-arc dead loop, Defect C.
func TestEditorStopGuard_TaskAware(t *testing.T) {
	normalStop := agentcore.StopInfo{TurnIndex: 1, Message: agentcore.Message{StopReason: agentcore.StopReasonStop}}

	// Summary task + only a review saved -> must block (a review does not satisfy the arc_summary requirement).
	t.Run("summary task blocks on review only", func(t *testing.T) {
		s := newTestStore(t)
		guard := NewEditorStopGuard(s, "生成第 5 卷第 1 弧摘要（save_arc_summary）", nil)
		if _, err := s.Checkpoints.Append(domain.ArcScope(5, 1), "review", "reviews/v05a01.json", "d1"); err != nil {
			t.Fatalf("append review: %v", err)
		}
		if d := guard(context.Background(), normalStop); d.Allow {
			t.Fatal("summary task must NOT be satisfied by a review checkpoint")
		}
	})

	// Summary task + arc_summary already saved -> release.
	t.Run("summary task allows on arc_summary", func(t *testing.T) {
		s := newTestStore(t)
		guard := NewEditorStopGuard(s, "生成第 5 卷第 1 弧摘要（save_arc_summary）", nil)
		if _, err := s.Checkpoints.Append(domain.ArcScope(5, 1), "arc_summary", "summaries/arc-v05a01.json", "d1"); err != nil {
			t.Fatalf("append arc_summary: %v", err)
		}
		if d := guard(context.Background(), normalStop); !d.Allow {
			t.Fatal("summary task must be satisfied by an arc_summary checkpoint")
		}
	})

	// Review task + a review saved -> release (the default lenient behavior is unchanged).
	t.Run("review task allows on review", func(t *testing.T) {
		s := newTestStore(t)
		guard := NewEditorStopGuard(s, "对第 5 卷第 1 弧做弧级评审（scope=arc）", nil)
		if _, err := s.Checkpoints.Append(domain.ArcScope(5, 1), "review", "reviews/v05a01.json", "d1"); err != nil {
			t.Fatalf("append review: %v", err)
		}
		if d := guard(context.Background(), normalStop); !d.Allow {
			t.Fatal("review task must be satisfied by a review checkpoint")
		}
	})
}

// An arc-summary task produced by the router that reads "summarize volume N arc M" in Vietnamese: the guard must recognize it as an
// ARC summary (save_arc_summary), not a volume summary.
func TestEditorStopGuardArcSummaryTaskFromRouterFormat(t *testing.T) {
	normalStop := agentcore.StopInfo{TurnIndex: 1, Message: agentcore.Message{StopReason: agentcore.StopReasonStop}}
	arcTask := fmt.Sprintf("Tạo tóm tắt tập %d cung %d, snapshot nhân vật và quy tắc viết (save_arc_summary)", 5, 1)
	volTask := fmt.Sprintf("Tạo tóm tắt tập %d (save_volume_summary)", 5)

	s := newTestStore(t)
	arcGuard := NewEditorStopGuard(s, arcTask, nil)
	if _, err := s.Checkpoints.Append(domain.ArcScope(5, 1), "arc_summary", "summaries/arc-v05a01.json", "d1"); err != nil {
		t.Fatal(err)
	}
	if d := arcGuard(context.Background(), normalStop); !d.Allow {
		t.Fatalf("task tóm tắt cung đã có arc_summary phải được thả: %q", d.InjectMessage)
	}

	s2 := newTestStore(t)
	d := NewEditorStopGuard(s2, volTask, nil)(context.Background(), normalStop)
	if d.Allow || !strings.Contains(d.InjectMessage, "save_volume_summary") {
		t.Fatalf("task tóm tắt tập chưa có volume_summary phải bị chặn và nhắc save_volume_summary: %+v", d)
	}
	d = NewEditorStopGuard(s2, arcTask, nil)(context.Background(), normalStop)
	if d.Allow || !strings.Contains(d.InjectMessage, "save_arc_summary") {
		t.Fatalf("task tóm tắt cung chưa có arc_summary phải bị chặn và nhắc save_arc_summary: %+v", d)
	}
}
