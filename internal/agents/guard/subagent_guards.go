package guard

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/store"
)

// subagentMaxConsecutiveBlocks: after N consecutive blocks, escalate to termination, avoiding a dead loop with weak models.
const subagentMaxConsecutiveBlocks = 3

// BlockHook is the audit callback of StopGuard: called synchronously on every block/escalation. The Host uses it to surface the block
// facts to the TUI event stream and off-screen notifications - otherwise blocks only go to the log and the user sees only
// "stalling + tokens flying" in the UI, with no way to tell whether the system is self-healing or spinning idle (issue #75).
// The callback does not take part in guard decisions. reason values:
//   - "blocked"    a nudge message was injected and the model will keep going
//   - "escalated"  consecutive idle spinning exceeded the limit, this run is terminated and handed back to the upper layer
//   - "hard_stop"  provider refusal (safety/content_filter), terminate immediately
type BlockHook func(agent, reason string, consecutive int32)

// hardStopReasons are provider-side refusal reasons that cannot be recovered with a nudge message. Injecting
// "must commit" has no effect on them and instead burns the tokens of a full LLM call each time,
// and after the eventual escalation the Engine re-runs the whole Worker task, multiplying the waste
// (measured: when ch02 hit safety, writing one chapter produced 3 re-dispatches and 17 LLM calls, with the hit rate
// falling from 50% to 2.8%).
//
// Note that StopReasonError / StopReasonAborted need not be listed: when agentcore receives these two stop reasons in
// loop.go it terminates the run directly and never calls StopGuard.
// Only the provider refusal semantics that actually reach StopGuard are listed here.
var hardStopReasons = map[agentcore.StopReason]struct{}{
	"safety":         {},
	"content_filter": {},
}

// newCheckpointDeltaGuard builds a StopGuard:
// if no checkpoint of the specified step appears after the baseline, end_turn is refused.
// The baseline is captured by the caller at factory time, ensuring correct per-run semantics.
//
// blockMsg receives the set of checkpoint steps observed after the baseline and assembles the nudge
// message from the actual progress - a static message is misleading when "the required tool itself keeps failing" (it nudges the model to call a
// tool that is failing, see #75).
//
// The counting semantics are "reset on progress": if any new checkpoint (re-draft / check etc.) appears
// between two blocks, the model is considered to be advancing and consecutive goes back to zero;
// only consecutive idle spinning with no artifacts accumulates and escalates to termination.
func newCheckpointDeltaGuard(st *store.Store, agentName string, requiredSteps []string, blockMsg func(seen map[string]struct{}) string, onBlock BlockHook) agentcore.StopGuard {
	var baseline int64
	if cp := st.Checkpoints.LatestGlobal(); cp != nil {
		baseline = cp.Seq
	}
	need := make(map[string]struct{}, len(requiredSteps))
	for _, s := range requiredSteps {
		need[s] = struct{}{}
	}
	var consecutive atomic.Int32
	var lastBlockSeq atomic.Int64 // latest checkpoint Seq observed at the last block; -1 means never blocked
	lastBlockSeq.Store(-1)
	return func(_ context.Context, info agentcore.StopInfo) agentcore.StopDecision {
		// Unrecoverable error: escalate directly, without wasting a nudge.
		if _, hard := hardStopReasons[info.Message.StopReason]; hard {
			slog.Error("subagent stop_guard detected unrecoverable stop, escalating immediately",
				"module", "agent.guard", "agent", agentName,
				"turn", info.TurnIndex, "stop_reason", info.Message.StopReason)
			if onBlock != nil {
				onBlock(agentName, "hard_stop", consecutive.Load())
			}
			return agentcore.StopDecision{Allow: false, Escalate: true}
		}
		// Scan the checkpoints after the baseline in reverse order and collect the steps that have appeared (shared by the release decision and the progress message).
		// New checkpoints are at the tail, so break as soon as one <= baseline is met.
		all := st.Checkpoints.All()
		latestSeq := baseline
		seen := make(map[string]struct{})
		for i := len(all) - 1; i >= 0; i-- {
			cp := all[i]
			if cp.Seq <= baseline {
				break
			}
			if cp.Seq > latestSeq {
				latestSeq = cp.Seq
			}
			seen[cp.Step] = struct{}{}
		}
		for s := range need {
			if _, ok := seen[s]; ok {
				consecutive.Store(0)
				return agentcore.StopDecision{Allow: true}
			}
		}
		// A new artifact persisted since the last block = the model is advancing (e.g. after being nudged it re-drafts and then probes for a wrap-up),
		// reset the count; escalation should only punish idle spinning with no progress, not accumulate the blocks of the whole run and write it off.
		if prev := lastBlockSeq.Load(); prev >= 0 && latestSeq > prev {
			consecutive.Store(0)
		}
		lastBlockSeq.Store(latestSeq)
		n := consecutive.Add(1)
		if n > subagentMaxConsecutiveBlocks {
			slog.Error("subagent stop_guard blocked consecutively past the limit, escalating to termination",
				"module", "agent.guard", "agent", agentName, "turn", info.TurnIndex, "consecutive", n)
			if onBlock != nil {
				onBlock(agentName, "escalated", n)
			}
			return agentcore.StopDecision{Allow: false, Escalate: true}
		}
		slog.Warn("subagent stop_guard blocked end_turn",
			"module", "agent.guard", "agent", agentName, "turn", info.TurnIndex, "consecutive", n)
		if onBlock != nil {
			onBlock(agentName, "blocked", n)
		}
		return agentcore.StopDecision{Allow: false, InjectMessage: blockMsg(seen)}
	}
}

// staticBlockMsg adapts a fixed text to the blockMsg signature (the artifact of the architect/editor is a single-tool persist,
// there is no multi-step progress, so a static nudge is enough).
func staticBlockMsg(msg string) func(map[string]struct{}) string {
	return func(map[string]struct{}) string { return msg }
}

// NewWriterStopGuard requires the writer to produce at least one successful commit_chapter this turn.
// The nudge message is assembled from the step progress already persisted: the writer is the only sub-agent with a multi-step tool chain,
// and the static "must call commit_chapter" misleads when a prerequisite step is missing or the commit itself errors.
func NewWriterStopGuard(st *store.Store, onBlock BlockHook) agentcore.StopGuard {
	return newCheckpointDeltaGuard(st, "writer", []string{"commit"}, writerBlockMsg, onBlock)
}

// writerBlockMsg decides from the checkpoint steps seen this turn which step the writer is stuck on.
// Step names correspond to the values each tool persists: plan / draft / edit / consistency_check / commit.
func writerBlockMsg(seen map[string]struct{}) string {
	_, hasDraft := seen["draft"]
	_, hasEdit := seen["edit"]
	_, hasCheck := seen["consistency_check"]
	switch {
	case !hasDraft && !hasEdit:
		return "Cấm kết thúc: lượt này chưa lưu nội dung chương nào. Hãy hoàn thành chương theo thứ tự plan_chapter → draft_chapter → check_consistency → commit_chapter; nội dung chương chỉ xuất trong chat là coi như mất, phải lưu và nộp qua công cụ."
	case !hasCheck:
		return "Cấm kết thúc: nội dung chương đã lưu nhưng chưa hoàn tất. Hãy gọi check_consistency để đối chiếu tính nhất quán, rồi gọi commit_chapter để nộp chương. draft_chapter / edit_chapter chỉ lưu bản nháp, chưa tính là hoàn thành."
	default:
		return "Cấm kết thúc: chương này chỉ còn thiếu bước nộp commit_chapter. Hãy gọi commit_chapter ngay; nếu nó trả về lỗi thì xử lý theo thông báo lỗi (kiểm tra số chương, bổ sung thao tác tiên quyết theo gợi ý) rồi nộp lại, không được kết thúc khi chưa nộp."
	}
}

// NewArchitectStopGuard requires the architect to persist at least one planning artifact this turn.
func NewArchitectStopGuard(st *store.Store, onBlock BlockHook) agentcore.StopGuard {
	return newCheckpointDeltaGuard(st, "architect",
		[]string{
			"book", "premise", "outline", "layered_outline", "characters", "world_rules",
			"foundation_audit", "expand_arc", "append_volume", "update_compass", "complete_book", "revise_outline", "resolve_outline_feedback", "plan",
		},
		staticBlockMsg("Bạn phải gọi save_book, save_foundation, revise_outline, resolve_outline_feedback, audit_foundation hoặc plan_chapter (viết chỉ thị viết lại cho chương trong hàng đợi viết lại) để lưu sản phẩm rồi mới được kết thúc. Chỉ xuất văn bản Markdown/JSON là coi như mất."),
		onBlock,
	)
}

// NewEditorStopGuard requires the editor to persist an artifact matching the "task" this turn before it may finish.
//
// Task awareness: when dispatched to produce a summary, save_review (re-check) alone does not count as done - the corresponding summary must be produced.
// Otherwise an editor that was "dispatched to produce an arc summary but re-checked first" would satisfy the old lenient criterion and finish early, and the arc summary would never be persisted
// (together with the dispatcher dedup going mute this once caused the mid-volume skeleton-arc dead loop, see outline-exhaustion-livelock).
// A terminal tool exit also consults the StopGuard (contract test TestContract_TerminalToolExitConsultsStopGuard),
// so the save_review hard stop in build.go is safe: when the editor re-checks first in a summary task, this guard
// vetoes that exit and nudges until the corresponding summary is persisted.
func NewEditorStopGuard(st *store.Store, task string, onBlock BlockHook) agentcore.StopGuard {
	switch summaryTaskKind(task) {
	case "volume":
		return newCheckpointDeltaGuard(st, "editor", []string{"volume_summary"},
			staticBlockMsg("Nhiệm vụ lần này là tạo tóm tắt tập: bạn phải gọi save_volume_summary để lưu rồi mới được kết thúc; save_review (kiểm tra lại) không tính là hoàn thành."), onBlock)
	case "arc":
		return newCheckpointDeltaGuard(st, "editor", []string{"arc_summary"},
			staticBlockMsg("Nhiệm vụ lần này là tạo tóm tắt cung: bạn phải gọi save_arc_summary để lưu rồi mới được kết thúc; save_review (kiểm tra lại) không tính là hoàn thành."), onBlock)
	default:
		// Review or ad-hoc task: persisting any review/summary is enough (keeps the existing lenient behavior).
		return newCheckpointDeltaGuard(st, "editor",
			[]string{"review", "arc_summary", "volume_summary"},
			staticBlockMsg("Bạn phải gọi một trong save_review / save_arc_summary / save_volume_summary để lưu kết quả rồi mới được kết thúc."), onBlock)
	}
}

// arcSummaryPhraseRe recognizes an ARC summary task in natural language, including the Vietnamese form
// "summarize volume N arc M" (which contains the volume-summary phrase but is an arc summary).
var arcSummaryPhraseRe = regexp.MustCompile(`tóm tắt cung|tóm tắt tập\s*\d+\s*cung`)

// summaryTaskKind classifies the editor's task: "volume", "arc" or "" (not a summary).
// It prefers tool names because the task text generated by flow always has them; natural language is only a fallback
// for tasks written by the Arbiter.
func summaryTaskKind(task string) string {
	hasVol, hasArc := strings.Contains(task, "save_volume_summary"), strings.Contains(task, "save_arc_summary")
	switch {
	case hasVol:
		return "volume"
	case hasArc:
		return "arc"
	}
	lower := strings.ToLower(task)
	// Matches the task text of the zh mode. han:keep
	if strings.Contains(task, "弧摘要") || arcSummaryPhraseRe.MatchString(lower) {
		return "arc"
	}
	// Matches the task text of the zh mode. han:keep
	if strings.Contains(task, "卷摘要") || strings.Contains(lower, "tóm tắt tập") {
		return "volume"
	}
	return ""
}
