# Control Plane Evolution: Engine + Arbiter (Removing the Coordinator Long Loop)

> Status (2026-07-14 v6): **code implementation complete**. Engine/Arbiter are implemented, and the Coordinator and everything around it are deleted (§10 list). End-to-end validation covers writing a complete book, failure rulings, deadlock rulings, rewrite acceptance (hold + editor sequencing), immediate stop on boundary hold, intervention preservation under exit races, and one permit per single chapter. All blocking items from the three external review rounds are resolved (including the feedback fact closed loop, PendingSteer crash protection, and lifecycle races).
> **Documentation migration complete (2026-07-12)**: the body of architecture.md has been fully rewritten for the current Engine+Arbiter architecture (including the new validation strategy / directory layout / discipline); the old architecture narratives in README, context-management, evaluation-system, observability and user-rules-runtime have been cleaned up (only labeled historical comparisons remain). The Coordinator configuration and session-compatibility paths are both deleted; the Arbiter deliberately uses the Default model uniformly and does not expose an independent role configuration.
> **Design-semantics clarifications (review rounds 4/5)**: ① The consumption point of writer feedback **is** the next structural operation (expand_arc/append_volume/update_compass clear it after consulting it via novel_context). It is "advice for the later outline" (the original wording of the commit schema), not an immediate dispatch signal; a severe mid-arc deviation goes through editor review and the user intervention channel. A non-layered book has no structural operations, so **commit does not persist its feedback** (to avoid permanently consumer-less junk facts; the return-value mirror is kept for diagnostics). ② rule_violations is now a closed loop: commit persists them on both paths (**best-effort quality metadata**, not at the same strong-consistency level as the chapter commit; a crash right after pending_commit is cleared would lose one record, which is acceptable) → injected into novel_context(chapter=N) → the editor consumes them per the mechanical-check mapping section of its prompt. ③ PendingSteer crash protection is **best-effort single-in-flight persistence**: if the first persistence fails, the ruling is explicitly stopped; it is protected during the ruling, when applying an action fails, and on normal exit/Abort; two windows are explicitly not guaranteed: (a) a hard kill of the process after a dispatch has moved into the in-memory execution queue (e.next) but before the worker starts (a millisecond-scale window; defer does not run); (b) a concurrent intervention waiting on interMu (not yet written into the slot). The user is present and can notice, and resending costs seconds, so no persistent intent/FIFO is built for this. ④ A failed startup ruling is not a dead end (a real-world failure patched on 2026-07-12: a provider account became invalid so plan_start failed, and every recovery path was blocked): StartPrompt (input fact) is now persisted **before** the ruling; when plan_start never completed, the engine's planStartFallback re-rules on the spot based on it. Retrying the first ruling does not violate "recovery does not redo an existing ruling"; a failed re-ruling pauses explicitly and echoes the failure, and the audit record of a failed ruling carries an error field (DecisionRecord.Error).
> This document is kept as a design decision record; for the current architecture see the architecture section of the README and docs/engine-rfc.md. Related: docs/voice-layer.md (implemented).

## 1. Motivation: outdated assumptions surrounded by patches

The project's founding assumption, "one Prompt and one resident LLM long loop drives the whole book", is outdated. After the April Hybrid refactor, decision power actually lies in `flow.Route`, and in 90% of its calls in the main loop the Coordinator only did **verbatim forwarding**. The patch ecosystem paid for "keeping an LLM session alive that should not stop":

1. StopGuard + blockMessage dynamic-copy engineering
2. The Dispatcher repeated-instruction protocol ("issued for the Nth time", i.e. 第 N 次下达) <!-- han:keep -->
3. Behavioral discipline in coordinator.md (recovery special cases / query-type requests must dispatch in the same turn / must not express a stance by stopping)
4. completePhaseGate / writerExpandedChapterGate
5. MaxTurns=100_000
6. FlowBoundaryHook
7. Completion-dispute special cases

**The project's new internal subsystems (import/simulation/cocreate/userrules) all bypassed the Coordinator and already use the "Host orchestrates directly + LLM as a function" paradigm.** This plan unifies the main flow onto a pattern the project has already validated.

## 2. Target shape

```
Entry
  ↓
Host (package name kept; an EngineLoop is added internally, not a purely mechanical rename)
  ├─ read Store → flow.Route → run Worker directly
  ├─ clear semantic scenario → call Arbiter function
  └─ event projection / budget / stop points / notifications (existing responsibilities kept)
  ↓
Workers (architect / writer / editor, autonomous internally, checkpoint-delta guards kept)
  ↓
Tools → Store (the sole source of facts)
```

Responsibilities: **Route owns every next step that can be looked up in a table; the Arbiter owns semantic judgments with clear boundaries; Workers own open-ended creation; the Engine executes decisions and takes no part in literary judgment; Observer/Diag only observe.**

One-sentence summary of the end state: **one serial deterministic Engine, three autonomous Workers, a few on-demand Arbiter functions, and one filesystem fact layer.**

### Two symmetric planes (to be written into architecture.md as a new iron rule)

```
Deterministic plane:  flow.LoadState   → flow.Route     → Instruction   (exhaustive spec tests)
Semantic plane:       arbiter.Collect* → arbiter.Decide* → XxxDecision   (decisions persisted + eval regression)
                      └── fact collection (IO) ──┘└── decision core (offline replayable) ──┘└── Engine execution ──┘
```

## 3. Arbiter scenarios (final set, kept minimal)

| Scenario | Trigger | Notes |
|------|------|------|
| `plan_start` | New book startup | Choose the short/long planner + expand overly short requirements |
| `intervention` | User intervention | Query / long-lived rules / plot-structure adjustment / rewrite of written chapters / rewrite or refusal after the book is complete |
| `worker_failure` | Worker error **and deterministic classification has no way out** | Network / arguments / missing prerequisite artifacts etc. are classified first by deterministic code and are not sent to the Arbiter |
| `deadlock` | The same routing instruction is still produced after the previous round | For counting and termination semantics see §8 mandatory question 5 |
| `completion_dispute` | **Reserve; add when there is evidence** | End-of-volume completion judgment is already handled by Route dispatching the architect (branch 10); only a mid-book dispute where "the structure has not reached a boundary but the story should wrap up" would need it, its real incidence is unknown, so it is not pre-built |

The completed-book summary is not a ruling but a generation task: the Engine dispatches the editor directly or makes one ordinary LLM call, and it takes no Arbiter scenario.

## 4. Arbiter design

### 4.1 Per-scenario Decision types (v3 revision: abandon the universal structure)

```go
// Shared subtype, to prevent drift between scenarios
type DispatchDecision struct {
    Instruction flow.Instruction
    Expect      DispatchExpect // see §5
}

type PlanStartDecision struct {
    Planner string // architect_long | architect_short
    Task    string // includes the expanded requirements
    Reason  string
}

type InterventionDecision struct {
    Answer   string
    Rules    string
    Hold     *AdvanceHoldOp
    Reopen   *ReopenOp
    Dispatch *DispatchDecision
    Reason   string
}

type FailureDecision struct {
    Action   string // retry | reroute | abort
    Dispatch *DispatchDecision
    Reason   string
}
```

Evolution record: action list (order validation is redundant, polymorphic arrays are error-prone) → universal flat structure (illegal orderings cannot be expressed, but illegal combinations still need a scenario × action matrix validation) → **per-scenario types (actions that do not match the scenario cannot be expressed, the matrix validation disappears, each per-scenario schema is smaller, the LLM output is more stable, and eval can be scored per scenario)**. Validate shrinks to per-type fact validation (phase constraints etc.).

### 4.2 API: one explicit pair of functions per scenario

```go
func CollectInterventionFacts(st *store.Store) InterventionFacts        // IO boundary, same discipline as flow.LoadState
func DecideIntervention(ctx, model, facts, text) (InterventionDecision, error) // no IO beyond the model request managed by the unified executor; replayable offline
// The other scenarios have the same pair; the Collect/Decide shape is uniform, and no generic Question/Decision framework is built
```

- **Failure path**: the unified structured executor chooses native JSON Schema or a prompt contract based on model capability; in prompt mode, format/schema errors, and in both modes business-validation errors, are handed back to the model with the exact reason for correction, and the lifecycle is controlled only by `context`. Native-contract violations, refusals, truncation, erroneous termination and non-retryable request errors are returned explicitly and immediately; an intervention produces no writes, startup reports an explicit error, and failure/deadlock pause conservatively
- **Intervention memory**: decisions.jsonl doubles as the intervention history, and `CollectInterventionFacts` includes summaries of the most recent N rulings
- **Model**: the Arbiter uniformly uses Default and exposes no independent role; the configuration contract is extended only when a clear capability or cost need appears

### 4.3 Audit (small and stable; audit is not a recovery source)

```json
{"schema_version":1,"id":"...","kind":"intervention","checkpoint_seq":123,
 "input":"...","facts":{...},"decision":{...},"reason":"...","duration_ms":1200}
```

(Tokens/cost are not in the record: the ruling model is wrapped by usageTrackedModel, and usage goes uniformly into the UsageTracker/budget, on the same ledger as each Worker.)

- facts stores only structured facts + summaries + artifact/checkpoint references, **without copying body text or storing the complete context pack**; each record has a size cap, and anything over it is truncated and marked
- **input is kept in the record** (required for offline replay of `Decide*(facts, input)`: an audit without input cannot be regression-tested); redaction happens at the **diag export boundary**, not when writing to disk
- The audit log is neither event sourcing nor a recovery data source

## 5. State commit protocol (serial Engine loop)

```
read facts → Route / Arbiter produces a decision → check preconditions → execute action
           → Worker runs → recompute Route postconditions → next round
```

- **Invariant: control state changes serially, only at Engine boundaries.** Interventions may be consulted in parallel while a Worker runs (read-only safe, and the user sees the Answer/Reason echo within seconds), but **actions that change control state (hold/reopen/dispatch) go into the Engine queue and are committed after the boundary check**; answer (stateless) and rules (content plane; the semantics are that the old rules apply to this chapter and the new ones take effect next chapter) execute immediately
- Each Dispatch carries a snapshot from the Collect moment, reconciled at the boundary; on mismatch → discard, record `decision_stale`, and re-consult with the new facts:

```go
type DispatchExpect struct {
    CheckpointSeq int64
    Phase         domain.Phase
    Flow          domain.FlowState
    QueueHead     int
}
```

- Explicit preconditions are preferable to a global Store hash (readable, diagnosable); no global digest is built

## 6. Recovery model (recover facts only, not the session)

```
startup → read Progress → read the latest Checkpoint → check PendingSteer/AdvanceHold/chapter permit → Gate reconciliation → Route → continue running Worker
```

The recovery of plan_start depends on a single persisted fact (inside RunMeta); **the ruling is persisted as a fact first, and execution starts afterward**:

```go
type PlanStartRecord struct {
    RawPrompt   string
    Planner     string
    PlannerTask string
    DecisionID  string // links to the audit record
    Status      string // decided | dispatched | done — makes the intermediate states of the startup transaction explicit
}
```

A crash at any point: if the Record exists, continue according to Status without re-consulting; if the Record is missing, treat it as a new book and consult again (re-consulting is acceptable, and the audit keeps two records).

## 7. Migration path (v3 reordered: Engine first, Arbiter wired afterward)

Rationale for the reordering: the original "Arbiter first" needed a transitional pipeline (rulings disguised as Host instructions via steering and fed to the Coordinator); **if the Engine lands first, that pipeline never needs to be built at all**, and the Arbiter is wired directly to the Engine executor. Every step deletes something, and no temporary bridge is built; the "dual-brain option value" concern is structurally dissolved by the ordering.

| # | Step | Status |
|---|------|------|
| 0 | Unconditional items: planning completion enters the Router (exhaustive spec first); decisions.jsonl audit. Implementation improvement: the planner identity is derived from the existing `RunMeta.PlanningTier`, so no new recording mechanism is needed | ✅ 2026-07-12 |
| 1 | Voice layer delivered (docs/voice-layer.md) | ✅ 2026-07-12 |
| 2 | Step 2 RFC finalized (docs/engine-rfc.md, seven mandatory questions) | ✅ 2026-07-12 |
| 3 | WorkerRunner: call subagent.Runner directly and programmatically, with events relayed through ctx ToolProgress | ✅ 2026-07-22 |
| 4-5 | The Engine takes over all dispatch + the four Arbiter scenarios are wired (plan_start/intervention/failure/deadlock), connected directly to the Engine executor (during implementation it turned out that landing the Engine first meant the steering transitional pipeline never needed to be built, so 4/5 landed together) | ✅ 2026-07-12 |
| 6 | Delete the Coordinator and everything around it (the whole §10 list executed); end-to-end integration tests (real tools writing a complete book / failure ruling / deadlock ruling) | ✅ 2026-07-12 |

## 8. Step 2 RFC mandatory questions (do not enter Step 3 until finalized)

1. **Worker extraction surface**: the WorkerRunner API; ownership and lifecycle of the full set of build.go assembly parts: role models/failover, prompt cache key, ThinkingLevel, UsageRecorder, SessionLogger, Writer ContextManagerFactory, RestorePack, StopGuardFactory, StopAfterTools, nested Observer event projection
2. **Engine lifecycle**: start/pause/abort/resume; the single-Worker serial guarantee; runtime switching of /model and thinking
3. **Completing the state commit protocol**: make the Expect reconciliation of §5 cover every scenario; the Engine precondition list after the Gate is dismantled
4. **Error taxonomy**: deterministic classification (retry/reroute/terminal) first, with only the ones that have no way out sent to `worker_failure`; layering against agentcore-level retries
5. **Deadlock protocol**: the same `Agent+Task` recurring consecutively means the routing postcondition is not met; intermediate checkpoints inside a Worker do not reset the counter; an Arbiter decision to retry does not reset the counter; consult at 3 repeats, hard circuit breaker at 5.
6. **Crash semantics**: how to determine whether the previous Worker already produced valid facts
7. **Prototype acceptance**: Observer/Usage/Context/model switching/recovery, five items compared point by point with the current state

## 9. Value ledger

| Dimension | Current | End state |
|------|------|------|
| LLM cost per chapter | One forwarding call per boundary | Eliminated; the class of forwarding-failure problems disappears |
| Ruling testability | ~zero (mixed into a long session) | Per-scenario offline replay + eval regression |
| Intervention response | Waits for the chapter boundary (minutes) | Consultation is immediate, control state committed at the boundary |
| Complexity | Seven-item patch ecosystem | Net reduction of 1500+ lines, three problem classes retired |
| Crash recovery | Session replay + recovery protocol | Read the store and continue |
| Transition risk | — | Concentrated in steps 3/4 (Worker extraction), controlled by the RFC + prototype gate; steps 0/1 hold unconditionally |

## 10. End-state deletion list

The Coordinator and its session-recovery logic, the Coordinator StopGuard, the Dispatcher steering protocol and the `[Host 下达指令]` ("Host issues instruction") text protocol, FlowBoundaryHook, completePhaseGate / writerExpandedChapterGate (their validations are moved over as Engine preconditions), MaxTurns=100_000, and all behavioral discipline in coordinator.md. <!-- han:keep -->

## 11. Objections and review record

1. *"Ruling correctness will not improve"*: true; the real difference is focus/curation/precondition validation vs session memory, a slightly better net value that is measurable for the first time
2. *"The current state works; touching the control plane is risky"*: acknowledged; the foundation was built for this, and the steps can be stopped or rolled back one at a time
3. *"Architecture is not the bottleneck, content quality is"*: partly true, hence the voice layer goes first
4. **Review 1 (2026-07-12)**: missing state commit protocol → §5; thin Step 2 → §8 mandatory questions + prototype gate; startup sequencing → §6; overclaim of "illegal states are unrepresentable" → the 4.1 evolution record; factual error in the arbiter role whitelist → 4.2; audit hygiene → 4.3
5. **Review 2 (2026-07-12)**: per-scenario Decision types (adopted, 4.1); migration order rearranged to Engine first (adopted, §7); unified boundary commit (adopted, §5); PlanStartRecord (adopted, §6); do not rename host (adopted); keep the word-count advice in the protocol file (adopted, see voice-layer). **Reservations**: the audit must keep input or replay is impossible (4.3); completion_dispute downgraded to a reserve scenario (§3)

## 12. Discipline and non-goals

**Discipline**: ① a new decision point first goes through the §2 three-way split, and defaulting to "add a rule to the prompt" is forbidden; ② every LLM decision point must have a fact list / structured output / degradation path / persisted audit; ③ write only fact guardrails, not behavioral guardrails; ④ declarative invariants are preferable to procedural scripts; ⑤ for control-plane changes, change the exhaustive spec first and the implementation second.

**Non-goals**: an event-sourcing rewrite; abstracting the Store for a hypothetical multi-tenancy; a generic workflow DSL; a global State Digest; renaming the host package; pre-building completion_dispute.
