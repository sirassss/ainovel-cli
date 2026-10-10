# Step 2 RFC: Engine Runs Workers Directly (Answers to the Seven Required Questions)

> Status: final (2026-07-12). Based on a code survey of host/observer/subagent/usage/cocreate.
> Conclusion: all seven questions have low-risk answers; proceeding to implementation. Related: docs/engine-arbiter.md.

## 1. Worker execution surface: calling subagent.Runner programmatically

> Postscript (2026-07-22): agentcore has split typed execution from the model tool protocol. `Runner.Run` is the host entry point;
> `Runner.AsTool()` is only for hosts that need the LLM to dispatch subagents autonomously. AINovel depends only on Runner.

`Runner.Run(agent, task)` starts one complete `agentcore.AgentLoop` per call. The Engine calls it directly, and build.go's
**entire assembly takes effect unchanged**: role models + failover, prompt cache key (#seq increments on every call), ThinkingLevel,
UsageRecorder/SessionLogger (OnMessage), Writer ContextManagerFactory, RestorePack, StopGuardFactory,
StopAfterTools. Typed results and error chains are returned directly, with no JSON encoding/decoding or tool-result sniffing.

**Event projection**: the subagent progress relay reads the **ToolProgress callback in ctx** (`agentcore.ReportToolProgress(ctx,...)`).
The Engine calls `Runner.Run` with `agentcore.WithToolProgress(ctx, relay)`, so the relay works as usual; the relay synthesizes ProgressPayload into
`EventToolExecUpdate` and feeds it to the existing `observer.handleToolUpdate`. The observer's worker-side handling (TOOL lines / streamed body /
thinking / retry / context) is **~95% reusable**. DISPATCH lines are now started and finished directly by the Engine (two new observer entry points).
The Coordinator's left-pane narration stream disappears, replaced by Engine narration events.

**/model and reasoning effort**: model switching goes through a ModelSet swap (configs hold the failover wrapper, as before); reasoning effort goes through
`runner.SetThinkingLevel` (applyThinking is kept, the coordinator branch is deleted).

## 2. Engine lifecycle

A single-goroutine serial loop; `ctx` cancel = pause/abort (propagates into the worker loop, checkpoints guarantee nothing is lost);
Resume/Continue = start a new loop. A single serial Worker is guaranteed naturally by the loop structure. Budget/stop-point sentinels are called
directly by the Engine at each round boundary (replacing event subscriptions and FlowBoundaryHook).

## 3. State commit protocol → serial execution makes it nearly disappear

The Engine runs `LoadState+Route` only right before each spawn, so instructions are always based on the latest facts: Route instructions have no TOCTOU and need no Expect reconciliation.
The Expect snapshot is used only for **dispatches from Arbiter decisions** (a worker run sits between consultation and execution): before executing at the boundary, compare
{Phase, QueueHead}; on mismatch → discard + re-consult with the new facts. Precondition checks (the former Gate's duty) become ordinary Engine code:
phase=complete dispatches nothing; writer target chapter not expanded → reroute to architect_long expand (deterministic, no teaching copy needed).
Control-state actions from interventions (hold/reopen/dispatch) are committed at the Engine queue boundary; answer/rules take effect immediately.

## 4. Error taxonomy (deterministic first)

- retryable (network/rate limit/stream-idle): subagent's internal MaxRetries=7 absorbs these locally, never leaving the loop
- worker returns error (escalate/hard_stop/hard tool error): the Engine retries the same instruction once → still failing → Arbiter
  `worker_failure` consultation (retry/reroute/abort) → abort or Arbiter itself fails → pause + notify
- deterministic errors such as bad arguments / unknown agent: pause + notify directly (a code bug; retrying is pointless)

## 5. Deadlock protocol

Each round records the instruction key `Agent+Task`. If Route still produces the same key after the previous round executed, the task's postcondition was not met, and `repeat++`; if the instruction changes, the counter resets to zero. Intermediate checkpoints inside a Worker such as `plan/draft/edit` do not count as Engine-level progress.
repeat==3 → Arbiter `deadlock` consultation; if the Arbiter suggests retry, the counter is **not reset**; repeat==5 → hard circuit breaker: pause + notify.
(In the Coordinator era, "no threshold" relied on its autonomy; a deterministic Engine must be bounded.)

## 6. Crash semantics → free

There is no need to judge "whether the previous Worker produced valid facts": tool-layer checkpoint+digest is idempotent, Route is recomputed from the store,
and repeated dispatch is safe. agentcore's model-stream retries never cross a tool execution boundary. Recovery = enter the loop directly. PendingSteer is handled before the loop starts, as an
intervention going through the Arbiter.

## 7. Prototype acceptance

End-to-end integration tests (fake ChatModel): the full path of planning → backfill → write chapters → arc-end review/summary → expansion → book completion; intervention triage persisted to the store;
pause/resume; deadlock circuit breaker; usage recording; observer event shapes (DISPATCH/TOOL lines, streamed deltas). Plus the existing
60k Route spec, agentcore contract, and editor stream tests as the regression net.

## Completion summary (design decision)

The completion summary is now **generated deterministically**: the store already holds all the facts (chapter summaries / characters / foreshadowing ledger / word counts), and the Engine renders the report directly,
no longer spending an LLM call to produce ceremonial text. The original coordinator's LLM summary is dropped (engine-arbiter.md §3: a summary is not a ruling).
