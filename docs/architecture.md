# ainovel-cli Runtime Architecture

> Deterministic fact layer, autonomous semantic layer: one serial deterministic Engine, three autonomous Workers, a few on-demand Arbiter functions, and one filesystem fact layer.
>
> 2026-07-12 control-plane replacement complete: the Coordinator LLM long loop is retired, replaced by the Engine (deterministic loop) + Arbiter (semantic ruling functions). Design decisions and review records are in `docs/engine-arbiter.md`; the RFC is in `docs/engine-rfc.md`.

---

## 1. Goals (in priority order)

1. **Stability**: from a one-sentence input, reliably write a complete novel (200~500 chapters). No self-interruption caused by architecture problems.
2. **Iterable quality**: prompts / reference material / review dimensions / context strategy can be tuned independently without touching the architecture.
3. **Recoverable**: after a crash, network loss, or pause, resume from the latest checkpoint.
4. **Observable**: per-chapter, per-step progress, artifacts, and elapsed time are all queryable.

"Stable" is the premise, "quality" is the layer above. Every architecture decision serves stability first.

---

## 2. Core principles

### 2.1 Three-way split: decisions are placed by their nature

- **Enumerable state transitions → code**. "Who is dispatched after a chapter is written" is a fact lookup: the pure function `flow.Route` + a spec test that exhaustively covers tens of thousands of combinations, with an error rate approaching 0 and zero LLM overhead.
- **Semantic judgments with clear boundaries → LLM functions (Arbiter)**. Choosing the planner, triaging user interventions, deciding the way out of a failure/deadlock: facts in, structured decision out, mechanical validation as a backstop, every ruling persisted to disk and replayable.
- **Open-ended creation → LLM loops (Worker)**. Within one chapter, one review, or one planning pass, architect/writer/editor are fully autonomous.

The symmetry of the two planes is a cross-cutting discipline: any future new decision point follows this shape, and no new patterns are invented:

```
Deterministic plane:  flow.LoadState   → flow.Route     → Instruction   (exhaustive spec test)
Semantic plane:       arbiter.Collect* → arbiter.Decide* → XxxDecision   (decisions.jsonl + eval regression)
                      └── fact collection (IO) ──┘└── core (offline-replayable) ──┘└── Engine executes ──┘
```

### 2.2 Tools are the only interface to the fact layer

All interaction with the filesystem, Progress, and Checkpoint is done by tools. A single file is replaced atomically with `temp + fsync + rename`; cross-file ordered writes do not pretend to be database transactions: chapter commit uses a persistent `PendingCommit` Saga, and structural writes use deterministic idempotent replay with failures explicitly surfaced. Every step must check errors; only flows whose recovery intent has been persisted may promise to resume across restarts with the original payload.

### 2.3 The observation layer only observes

The UI, diagnostics, and event log are passive consumers projected from the event stream / read-only artifacts. They read facts, produce no facts, and do not affect control flow.

Observation data is strictly divided into three layers: `agentcore.ProgressPayload` is the transport layer, whose error text must be complete and must not include UI truncation policy; `host.Event.Summary` is the short display semantics and `Detail` is the full diagnostic; file logs preferentially write the full `Detail`, while the TUI reads only `Summary` and truncates it to the terminal width at final render time. The file logger is owned by `Host`: it first acquires the novel directory lease, then establishes the log session, and only then assembles the Store, models, and Engine; this neither bypasses single-book exclusivity nor misses any assembly or shutdown logging. "Full logging" means that error chains, raw invalid arguments, and lifecycle metadata are not lost; it does not mean re-transcribing successfully generated novel text into `tui.log`; large content is still carried by Store artifacts and `meta/sessions`.

**`internal/diag` is the engine's only observability subsystem**: a first-class supporting facility, but not the product core. It reads across nearly all artifacts + session + log + checkpoint, and takes on two duties: ① **creative-quality diagnostics** (rules → Finding, the on-screen `/diag` report); ② **runtime troubleshooting + redacted export** (behavioral skeleton with body text stripped + loop aggregation → overwrite-style `meta/diag-export.md`).

**Observer discipline (must not be loosened)**: diag may diagnose and may suggest, but **never acts on its own**: no auto-repair, no auto-resume, no changing the flow (for the historical lesson see §10 item 5).

### 2.4 A flat fact layer

There are only three kinds of facts:

- **Progress** — the progress index (which chapter has been written, the list of pending rewrites)
- **Checkpoint** — step-level progress records (plan / draft / commit / review / arc_summary)
- **Artifact** — chapter text, outline, characters, summaries, and other products

No abstractions such as WorkflowInstance / TaskInstance / Command are introduced. Auxiliary facts (the outline feedback pool, mechanical violation records, ruling audit) are likewise flat jsonl, each with exactly one producer and one consumer.

### 2.5 Four iron rules

**Iron rule 1: tools return only facts, never cross-scheduling instructions**. `commit_chapter` returns structured fields such as `arc_end` / `needs_expansion`; it carries no `[系统]` (`[System]`) style instruction strings. The `next_step` field inside a subagent is inline guidance stated as a fact ("I just saved the plan; the next step is draft") and does not count as a violation; see §6.3. <!-- han:keep -->

**Iron rule 2: flow routing is the Flow Router's job, and execution is the Engine's job**. `Route(state) → *Instruction` in `internal/flow/router.go` is a pure function (pinned down by an exhaustive spec test over tens of thousands of combinations); on every round the Engine reads facts from the store, derives the instruction with Route, and **runs the Worker directly and programmatically** (`subagent.Runner.Run`, typed input/result/error chain), with no LLM tool-forwarding layer. Returning nil means a semantic scenario (book-completion wrap-up / waiting for intervention) or a natural stop. **Deadlock has an explicit bound** (RFC §5): if Route still produces the same `Agent+Task` after the previous round, the routing postcondition was not met; 3 repeats consult the Arbiter, and 5 trigger a hard circuit-breaker pause. Intermediate checkpoints inside a Worker do not reset the counter; a deterministic Engine must not spin forever.

**Iron rule 3: semantic rulings go through the Arbiter, and every ruling is persisted**. Choosing the planner at startup, triaging user interventions, and deciding the way out of a failure/deadlock are ruled by the per-scenario Decide functions of `internal/arbiter`: facts in, structured decision out, mechanical validation as a backstop, decisions.jsonl audit (replayable offline for regression). The three Workers each keep their own `CheckpointDeltaGuard` (a fact guardrail: no finishing before the artifact is persisted).

**Iron rule 4: hard-code boundaries, not semantic judgments that cannot be enumerated**. Code fixes only provable invariants (permissions, phases, ordering, idempotency, structural integrity) and gives the model complete facts and enough room to operate; open questions such as creative trade-offs, quality judgments, and how a plan adapts to the text must be left to the Worker / Arbiter. Replacing model understanding with keywords, score thresholds, deviation enumerations, or rule tables is forbidden, as is narrowing the model's legitimate decision space out of fear that it will err. Before adding a new code rule, it must first be shown that the decision space is closed and the result is mechanically verifiable; otherwise, improve the context and tool expressiveness so that the gains from model upgrades are realized without changing the shell.

---

## 3. Architecture overview

```
[Entry: TUI / headless]
        │ prompt / steer
[Host shell]
   ├── observer            Worker progress relay + Engine dispatch events → UI/log projection
   ├── engine              deterministic loop: LoadState → Route → precheck → run Worker → sentinel boundary
   ├── intervention path   Steer/Continue → Arbiter ruling → action execution (immediate / boundary commit)
   └── usage / budget / stop points / model management
        │ programmatic call to subagent.Runner.Run (progress relayed via ctx ToolProgress)
[architect_short/long · writer · editor] (each with its own run + context + model)
        │ tool calls
[Tools]  novel_context · read_chapter · plan_chapter · draft_chapter · edit_chapter
         check_consistency · commit_chapter · save_review · save_arc_summary
         save_volume_summary · save_foundation
        │ single-file atomic + idempotent replay (commit uses a persistent Saga)
[Store: filesystem (tmp + rename)]
   Progress · Checkpoints · Outline · Drafts · Summaries · Characters · World
   · Signals · Decisions (ruling audit) · feedback pool · violation records
```

| Layer | What it does | What it does not do |
|---|---|---|
| Entry | Display, receive input | Business decisions |
| Host/Engine | Lifecycle, Route execution, Worker runs, sentinel boundary, intervention orchestration | Literary judgment; writing creative facts (control-state actions go through the tool kernel) |
| Arbiter | Semantic rulings (structured decisions) | Creating content itself; executing actions |
| Workers | Thinking, writing, reviewing | Reading/writing the Store directly (must go through tools) |
| Tools | Single-file atomic IO + explicit errors + idempotency; commit uses a Saga | Cross-subagent scheduling instructions |
| Store | Filesystem persistence | Business logic |

Dependencies are one-way: `entry → host → agents/arbiter → tools → store → domain`; `flow` is a top-level pure-policy package (above store, below host). Independent laterally: `errs/` may be referenced by any layer, and `diag/` subscribes to the host event stream + reads `store/` read-only.

---

## 4. Data model

### 4.1 BookMetadata and Progress

`BookMetadata` is the single source of truth for the book title and the reader-facing synopsis, persisted to `meta/book.json`; `book.md` is only a human-readable projection. Premise does not store the title again, and Progress does not carry work information.

```go
type BookMetadata struct {
    Title    string
    Synopsis string
}
```

Progress (`internal/domain/runtime.go`) records runtime state only:

```go
type Progress struct {
    Phase             Phase           // init / premise / outline / writing / complete
    CurrentChapter    int
    TotalChapters     int
    CompletedChapters []int
    TotalWordCount    int
    ChapterWordCounts map[int]int
    InProgressChapter int             // chapter currently being written
    Flow              FlowState       // writing / reviewing / rewriting / polishing / steering
    PendingRewrites   []int
    StrandHistory     []string        // dominant_strand sequence
    HookHistory       []string        // hook_type sequence
    CurrentVolume, CurrentArc int     // long-form layering
    Layered           bool
}
```

Control logic reads only the fact fields above and does not depend on any "updated timestamp"; time information is carried by the checkpoint's `OccurredAt`.

RunMeta (`meta/run.json`) carries the **user's run intent** (not creative facts): PlanningTier, PlanStart (the frozen startup ruling, the sole basis for crash recovery during planning), PendingSteer (intervention crash protection, a single in-flight slot), AdvanceMode / AdvancePermitChapter (the per-chapter approval policy and the exact chapter permit), AdvanceHold (a one-shot pause signed by an intervention). `RunMeta.Init` preserves all intent fields across restarts.

### 4.2 Checkpoint (`internal/domain/checkpoint.go`)

```go
type Scope      struct { Kind ScopeKind; Chapter, Volume, Arc int }
type Checkpoint struct {
    Seq        int64       // monotonically increasing
    Scope      Scope       // chapter / arc / volume / global
    Step       string      // plan / draft / commit / review / arc_summary / ...
    Artifact   string
    Digest     string
    OccurredAt time.Time
}
```

Storage: `meta/checkpoints.jsonl`, append-only. Repeated writes of the same `Scope+Step+Digest` are treated as idempotent and produce no new line.

### 4.3 Artifacts and auxiliary facts

Artifacts live in `store/outline.go` `drafts.go` `summaries.go` `characters.go` `world.go`.

- **Signals**: `PendingCommit` (commit interruption recovery). Read at startup/resume, not read at runtime.
- **Decisions** (`meta/decisions.jsonl`): the audit record of every Arbiter ruling (facts+input+decision), replayable offline; **not a recovery data source** (recovery depends only on Progress/Checkpoint/RunMeta).
- **Growing world facts**: the timeline and character state changes are appended to `timeline.jsonl` and `meta/state_changes.jsonl` respectively; a dedup index is maintained in-process, and a normal commit writes only this chapter's delta. The old JSON array is migrated on the next append by an idempotent protocol of "atomically write the new log first, then delete the old file"; `timeline.md` is a rebuildable human-readable projection.
- **Outline feedback pool** (`meta/outline_feedback.jsonl`): the writer's ordinary feedback is consumed at the next structural operation; if an external text revision affects the plot, it goes to the architect first before writing continues, and the pool is cleared after handling.
- **Mechanical violation records** (`meta/rule_violations.jsonl`): the result of checking against user_rules at commit time, consumed by the editor review through `novel_context(chapter=N)`; best-effort quality metadata, not strongly consistent at the same level as the commit.

### 4.4 Layered outline and book-completion convergence (the finale volume)

Rolling planning (compass anchor + volume skeleton + arcs expanded on demand) solves "opening and rolling", but turns "when to end" from a number into an open ruling at the end of each volume. Book-completion convergence must be explicitly designed, otherwise two kinds of deadlock appear: the ledger says it is written but the ending cannot be wrapped up (an out-of-bounds continuation infinite loop, already fixed by a structural backstop), and the narrative is finished but the ledger will not let it stop (estimated_scale overestimated + the completion threshold hard-vetoing → padding or circuit break).

**The finale volume is a first-class concept of convergence**, and book completion = one directional ruling + one deterministic glide:

- **Declaration (LLM semantic ruling)**: at the end of a volume the architect picks one of three: append_volume (continue) / append_volume with `"final": true` (finale volume) / complete_book (all conditions currently met). estimated_scale is **evidence, not a veto** in the completion decision.
- **Execution (code fact lookup)**: the finale fact = `domain.FinaleVolume`. Once the final volume's structure is fully written (`layeredStructurallyComplete`) **and the volume-end wrap-up trio is in place (arc review / arc summary / volume summary)**, MarkComplete is applied automatically, so completion never jumps ahead of the editor's quality gate. Books with no declaration still follow the quality-level `layeredBookComplete` (foreshadowing + long threads at zero).
- **Release (data-derived, no undo tool)**: if an unmarked new volume is appended after the declaration, the wrap-up state is released naturally. The state can always be derived from layered_outline.
- **Dispatch of the completion decision**: at the end of a volume, Route branch 10 dispatches architect_long to run the completion-decision checklist; the authority over the completion ruling lies with the architect (a Worker), not with the control plane.

---

## 5. Tool specification

Tools are the only point of interaction between the fact layer and the Agent.

### 5.1 Read tools

`novel_context(scope)` / `read_chapter(n)` can be called at any time, do not depend on prior state, and return enough data for the LLM to decide independently. `novel_context(chapter=N)` additionally injects that chapter's mechanical violations (if any); the architect path injects completed-volume/current-volume arc summaries, character snapshots, the outline feedback pool, and foundation status. When expanding an arc, what has already happened is fact and the skeleton is only a plan; the Architect may revise the target arc's title/goal in `expand_arc` while expanding its chapters.

### 5.2 Write tools (single-file atomic + graded recovery semantics)

Single-file writes are atomic; cross-file steps do not promise database-style atomicity. The ordinary commit and the rewrite commit of `commit_chapter` share `PendingCommit` and advance in the order "complete intent → artifact/state → Progress → checkpoint → clear intent"; recovery uses only the canonicalized payload and text snapshot written to disk the first time, and it is forbidden to adopt parameters the model regenerated after a restart or a draft that has been overwritten. Structural operations such as `expand_arc` / `append_volume` have no persistent intent, and promise only idempotent replay with the same parameters, repair of derived views, and explicit error return.

| Tool | Artifact | Step |
|---|---|---|
| `save_book` | meta/book.json + book.md | book |
| `plan_chapter` | drafts/chXX.plan.json | plan |
| `draft_chapter` | drafts/chXX.draft.md | draft |
| `edit_chapter` | drafts/chXX.draft.md | edit |
| `check_consistency` | none (read-only, returned inline) | consistency_check |
| `commit_chapter` | chapters/chXX.md + Progress (+ feedback pool / violation records best-effort) | commit |
| `save_review` | reviews/chXX.json (chXX-global.json for global) | review |
| `save_arc_summary` | summaries/arc-vNNaNN.json | arc_summary |
| `save_volume_summary` | summaries/vol-vNN.json | volume_summary |
| `save_foundation` | foundation/*.json (expand_arc/append_volume/update_compass consume the feedback pool on success) | premise / outline / layered_outline / characters / world_rules / expand_arc / append_volume / update_compass / complete_book |

`commit_chapter` carries out arc/volume/whole-book completion detection and returns structured facts; `save_review` makes no literary threshold rulings, it only validates the review facts and atomically maps the verdict given by the Editor to Flow and the rewrite queue.

`edit_chapter` is a thin wrapper over `agentcore.EditTool` and only allows editing chapters that are completed and in `PendingRewrites`; a new chapter's first draft must overwrite the whole chapter through `draft_chapter(mode="write")`.

### 5.3 Error layering

| Error type | Handling layer | Action |
|---|---|---|
| Network timeout / stream EOF | Tools | Retry 3 times |
| provider 429/503 | litellm | failover to a backup provider |
| Auth / model not found | Tools | terminal, raised upward |
| Missing prerequisite artifact | Tools | conflict raised upward; the LLM calls `novel_context` and retries |
| Illegal tool arguments | Tools | validation raised upward; the LLM fixes the arguments |
| retryable (stream-idle, etc.) | subagent layer | MaxRetries=7 retries locally, never leaving the Worker |
| Worker failure (guard escalation / hard_stop, etc.) | Engine | Deterministic errors pause directly; otherwise retry the same instruction once → Arbiter rules retry/reroute/abort |
| Deadlock (the same routing instruction keeps recurring) | Engine | 3 repeats consult the Arbiter, 5 trigger a hard circuit-breaker pause |
| Empty stream response / long thinking | litellm (`StreamIdleTimeout=5min`) | watchdog triggers a retry |

### 5.4 Idempotency

Before executing, every write tool first checks the checkpoint: if the `Step+Digest` of the latest checkpoint in the current scope equals this call's, it returns the existing artifact directly. Retries and repeated dispatch after crash recovery are both safe; this is also the foundation on which the Engine recovery model (read the store and carry on) stands.

---

## 6. Worker assembly

> A single huge prompt + a single Agent running a whole book is feasible in theory, but three things block stability: **context explosion** (even strong compression degrades at 200 chapters), **role interference** (rigorous planning / imaginative writing / critical review dilute one another in one prompt), and **loss of the model-heterogeneity dividend** (choosing models independently for planning/writing/review is a significant cost/quality optimization space). A multi-Worker topology is therefore necessary.

### 6.1 Assembly and running

`agents.BuildWorkers` (`internal/agents/build.go`) assembles the three kinds of Worker into one `subagent.Runner`: the Engine calls `Run(agent, task)` directly, and each call is one complete `agentcore.AgentLoop` (independent context, independent model, independent retries). The whole assembly takes effect at once: role models + failover, prompt cache key (#seq increments on each spawn), ThinkingLevel, UsageRecorder/SessionLogger (OnMessage), Writer ContextManagerFactory (the window is rebuilt automatically when /model switches), RestorePack, StopGuardFactory, StopAfterTools.

Worker progress relay goes through the **ToolProgress callback in ctx**: the Engine calls `Runner.Run` with `agentcore.WithToolProgress(ctx, relay)`, and the subagent's tool calls / streamed body / thinking / retry / context events enter the observer through the relay, in the same ProgressPayload shape as in the Coordinator era, so the observation layer is reused.

```
Engine ── Runner.Run(agent, task) ──▶ architect_short/long · writer · editor
                                          │ tool calls
                                        Store (collaboration medium; Workers do not communicate directly)
```

`bootstrap.ModelSet` supports role-level models: architect/writer/editor are each configured independently + provider failover. Running the Writer on Sonnet rather than Opus saves an order of magnitude in cost on a 200-chapter novel. The Arbiter uniformly uses the Default model (billed through usageTrackedModel) and currently does not expose an independent role configuration.

### 6.2 Three collaboration patterns

Workers do not communicate directly; all information flows through structured artifacts in the Store:

**Pattern A · Serial handoff (the trunk)**: Route dispatches the Architect to plan → Writer chapters 1..N → Editor arc-end review → Writer rewrite. At each step, "who is next" is derived by Route from the facts.

**Pattern B · Feedback loop**: the Writer reports an outline deviation in commit → the feedback pool is persisted (layered books only) → the Architect consults it via novel_context at its next structural operation → on success the operation consumes and clears it. The Writer never calls the Architect directly; feedback flows through the fact layer.

**Pattern C · Skeleton expansion (rolling planning)**: after a commit the facts show the next arc is still a skeleton → Route (or the Engine precheck) dispatches architect_long to expand it → the Writer continues. The long-form "rolling planning" capability is exactly this closed loop.

### 6.3 Code constraints on the Worker flow (no prompt crutches)

> The early writer flow relied on the "advance strictly in the following order" constraint in `writer.md`. LLMs often violated it: skipping plan and going straight to draft, or writing the body only into the chat without persisting it. **Constraining the flow with prompts is unstable**, and a model upgrade may even make it "creatively disobey".

Four layers of code constraints (all in effect at once):

| Layer | Where | Effect |
|---|---|---|
| `StopAfterTools` / `StopAfterToolResult` | `agents/build.go` SubAgentConfig | A Worker run exits as soon as a key tool succeeds (terminal exits still consult StopGuard, see the contract tests). The Writer stops once `commit_chapter` hits; the Editor's `save_review`/`save_arc_summary`/`save_volume_summary` and the Architect's arc/volume wrap-up go through `StopAfterToolResult` |
| `CheckpointDeltaGuard` | `agents/guard/subagent_guards.go` | With the baseline checkpoint as the dividing line, a new checkpoint for the corresponding step must be seen before the round ends, otherwise `end_turn` is rejected; 3 consecutive blocks escalate to terminate (a backstop against weak-model deadlock). The Editor's guard is task-aware: when dispatched to generate a summary, a re-check alone does not count as completion |
| Inline tool `next_step` | return-value fields of each tool | Every fact carries a "suggested next step", so the LLM knows the next step upon seeing the fact |
| In-tool ownership / precondition checks | `edit_chapter` `commit_chapter`, etc. | Physical interception at the data layer: point edits on a first draft, edits to completed chapters not in the queue, and empty commits are all rejected, and `ConcurrencySafe=false` prevents concurrency races |

writer.md carries only: the execution protocol, the mental model for resuming from a breakpoint, and interpretation of the chapter contract; writing standards live in the voice layer (the `{{VOICE}}` placeholder is backfilled and users may override it, see `docs/voice-layer.md`). **This is exactly the precondition for opening the voice layer to users: invariants live in the tool layer, so no matter how the prompt is broken, it cannot damage the state machine.**

### 6.4 agentcore dependency

`../agentcore` is this project's own general-purpose Agent library (linked via go.work). The primitives the Engine uses: `subagent.Runner.Run` (direct programmatic call, typed results and error chains; classification such as `errors.Is(err, subagent.ErrUnknownAgent)` does not depend on error wording), the ctx `ToolProgress` (event relay), `subagent.Config`, `StopGuard`/`StopAfterTools`. `subagent.Tool` is only for hosts that need to expose the Runner to the model via `Runner.AsTool()`; AINovel does not go through this layer.

**Modification boundary**: allowed into agentcore: new ContextManager strategies, new provider adapters, new event types; not allowed into agentcore: business models and business tools. The criterion: suppose agentcore is later adopted by a coding agent / customer-service agent; a new capability is allowed in only if it still makes sense in that scenario. **Writing backstop patches at the application layer is forbidden**; if a capability is missing, change upstream directly.

**Contract tests** (`internal/agents/agentcore_contract_test.go`, 6 of them, all driven through `Runner.Run`): they pin the framework behaviors this project depends on into executable assertions (a terminal exit consults StopGuard, Error/Aborted do not reach the guard, an Escalate error chain can be matched with `errors.Is`, `Run`'s typed `ErrUnknownAgent`, tool error progress is complete and plain text). **They must all be green before bumping agentcore**: comments go stale, tests do not (this discipline has already caught a failed assumption once and saved a workaround).

### 6.5 Prompt caching

The second lever of long-run cost (the first is model selection). The full explanatory version is in `docs/prompt-cache-design.md`. A three-layer division of labor: **litellm does only protocol translation**, **agentcore decides cache placement and identity**, **ainovel plugs in with one line of configuration**.

The premise of cache benefit is **a byte-stable request prefix**, guaranteed by three disciplines (all in agentcore):

1. **Byte-deterministic tools** — Description/Schema are rebuilt every time, and any map iteration is sorted first
2. **Append-only history** — messages are only appended, never rewritten; context compression is an explicit trade of "pay one full miss to get the window back", and the projection must `CommitOnProject`
3. **Dynamic content goes to the tail** — envelopes/instructions are all appended at the tail, never written back into earlier messages

The configuration is "one base per book, one name per role, one key per session": for the OpenAI family `PromptCacheKey = nvl-<book hash>-<role>#<spawn sequence>` provides routing affinity (sent only to the official endpoint by default; relays may enable it explicitly); for the Claude family `CacheLastMessage: "ephemeral"` is a rolling breakpoint + a system floor breakpoint. **Latch red line**: every quantity that goes into the cache key is frozen at first computation within a session, preferring staleness over breaking the cache. Break detection (`host/usage.go noteCacheBreak`) is pure observation and does no repair; its count goes into `usage.json cache_breaks` and the TUI cache panel.

---

## 7. Engine and Arbiter

### 7.1 Engine loop (`internal/host/engine.go`)

```
for {
    apply intervention control-state actions (drain; hold+dispatch first establish the rewrite facts)
    advanceGate.HandleBoundary() // hold consumption + review permit reconciliation
    inst := intervention dispatch ?? Route(LoadState) ?? planStartFallback
    inst == nil → return          // book complete / semantic stop, wait for Continue
    precheck(inst)                // the deterministic successor of the old ToolGate: discard dispatch once complete;
                                  // writer target chapter not expanded → reroute to architect to expand
    advanceGate.Allow(inst)       // blocks only forward new chapters without a permit
    trackDeadlock(inst)           // same Agent+Task keeps recurring: 3 times ask the Arbiter, 5 times circuit-break
    runWorker(inst)               // subagent.Runner.Run + progress relay + DISPATCH event
    error classification: deterministic error → pause; retry once on first failure; fail again → Arbiter (retry/reroute/abort)
    policy boundary: budget → advanceGate
}
```

Single-goroutine serial; `ctx` cancel = pause (checkpoints guarantee nothing is lost). **Control state changes only at loop boundaries**: an intervention's hold/reopen/dispatch are queued and committed at the boundary (for a hold+dispatch combination the dispatch runs first to build the queue, then the Gate is allowed to consume the hold); answer/rules execute immediately. `review` mode constrains only forward new chapters and does not block rewrites, reviews, structural maintenance, or commit recovery. Before executing an Arbiter dispatch, an Expect reconciliation is done (the semantic fields Phase/Flow/QueueHead; CheckpointSeq is audited only, not reconciled, because workers are mostly still running at intervention time so seq always changes); on mismatch the dispatch is discarded and the original intervention is **synchronously** sent back to the full ruling path for re-consultation.

### 7.2 Arbiter (`internal/arbiter/`)

Four scenarios, each with a `Collect*Facts` (IO boundary) / `Decide*` (no IO other than the model request managed by the unified executor, replayable offline) pair + a dedicated Decision type (actions that do not match the scenario are inexpressible in the type):

| Scenario | Trigger | Decision type |
|---|---|---|
| `plan_start` | New book startup | Choose the short/long planner + expand an overly short request |
| `intervention` | User intervention | A combination of answer/rules/hold/reopen/dispatch (execution order fixed by the Engine) |
| `worker_failure` | Worker errors and the deterministic classification has no way out | retry / reroute / abort |
| `deadlock` | The same instruction repeats with no progress | retry / reroute / abort |

Failure path: the unified structured executor chooses between native JSON Schema and a prompt contract depending on capability; format/Schema errors in prompt mode and business-validation errors in both modes feed the exact reason back to the model to keep correcting, until success or `context` ends, with no cap on attempts. Native-contract violations, refusals, truncation, erroneous termination, and non-retryable request errors are returned explicitly and immediately; an intervention produces no writes, startup reports the error explicitly, and failure/deadlock pause conservatively. **Arbiter output, like all LLM output, is untrusted**: after JSON Schema validation, `Validate` continues with mechanical validation against the facts (phase constraints, reopen only after completion, chapter out-of-bounds). Usage enters the budget and usage systems through `usageTrackedModel`.

### 7.3 Host shell (`internal/host/host.go`)

Lifecycle (`StartPrepared`/`Resume`/`Continue`/`Steer`/`Abort`/`Close`), intervention orchestration (FIFO serial + PendingSteer crash protection), event projection, and model management. Observation channels `Events`/`Stream`/`Done`, the UI aggregate `Snapshot()`, and extension entry points (import/export/co-creation/imitation/model switching).

`runEnded` (the engine.onDone callback) decides the terminal state from store facts: Phase=Complete → completed + a deterministic completion summary (no LLM call spent); otherwise → idle/paused. **No "auto-resume" logic may appear here** (historical lesson, §10 item 5).

---

## 8. Startup, recovery, and intervention

### 8.1 New book

```
User: "one-line request"
  → StartPrepared(raw)
    → Progress.Init / Checkpoints.Reset
    → StartPrompt frozen into RunMeta (input facts are persisted before the ruling)
    → Arbiter plan_start ruling (choose planner + expand request) → failure is reported explicitly (the audit carries the error)
    → PlanStartRecord frozen into RunMeta (the ruling is persisted as fact first, then execution starts)
    → engine.start (first dispatch instruction)
```

A failed ruling is not a dead end: StartPrompt is already there, and any later resume/continue will have the engine make up the ruling (see §8.2).

### 8.2 Recovery (restart after a crash)

```
process start → resumeLabel (pure UI label) → consistency warning → AdvanceGate reconciliation
  → PendingSteer exists → go through the intervention ruling path synchronously (the intervention takes effect before resuming), then bring up the engine
  → otherwise engine.start(nil): restore facts only, Route recomputes from the store and carries on
```

There is no session to recover. A crash during planning (ruling persisted, first foundation not persisted) is handled by `planStartFallback`, which continues dispatching per the PlanStartRecord without redoing the existing ruling. If the startup ruling was **never completed** (model failure at startup), `planStartFallback` makes up the ruling on the spot from StartPrompt; this is a retry of the first ruling and does not violate "recovery does not re-rule"; if the make-up ruling fails, it pauses explicitly and notifies, and a silent stop is not allowed. Safety of repeated dispatch is guaranteed by tool idempotency (§5.4).

### 8.3 User intervention

`Steer`/`Continue` both go through the Arbiter ruling path (`doIntervention`):

```
persist PendingSteer (crash protection) → Collect facts → Decide (seconds)
  → write decisions.jsonl → answer echoed / rules persisted immediately
  → hold/reopen/dispatch queued for boundary commit (when the engine is stopped, execute immediately and bring up the engine according to intent)
  → all actions succeed → atomically clear PendingSteer (ClearHandledSteer)
```

Crash protection is **best-effort single in-flight persistence**: if the first `SetPendingSteer` fails, it reports an error explicitly and stops ruling, and never continues executing without a recovery record; the ruling phase, action failure (kept for replay), and normal exit/Abort (defer saves back the leftover dispatch) are protected. Two windows are still explicitly not guaranteed: a hard kill after the dispatch has moved into the in-memory execution queue (milliseconds), and concurrent input waiting on interMu. A present user can notice, and the cost of resending is seconds.

**Persistence layer for long-lived interventions**: writing-style/quality rules are normalized into this book's rule snapshot by the ruling's `rules` action through `userrules.Service`, and `novel_context` injects `working_memory.user_rules`, taking effect across compression and across restarts (see [User rules snapshot](user-rules-runtime.md) for details). The other outlets already land in the store (length/plot → architect dispatch, revising old chapters → editor enqueues PendingRewrites, rework after completion → reopen).

### 8.4 Chapter advance control

`ChapterAdvanceGate` uniformly enforces two kinds of user intent at different time scales:

| Intent | Source | Semantics |
|---|---|---|
| `AdvanceMode=review` + exact permit | `/review on`, `/next` | Persistent policy: every forward new chapter must be released individually |
| `AdvanceHold` | Arbiter intervention | One-shot intent: pause at the current boundary, after the rewrite drains, or after the target chapter commits stably |

A permit is bound to a chapter number. It is consumed only when the target chapter enters CompletedChapters, PendingCommit is cleared, and the commit checkpoint exists, so a crash in any window of the commit saga will never reuse the same permit for the next chapter. Detailed invariants are in [Chapter Advance Gate](chapter-advance-gate.md).

---

## 9. Directory structure

```
internal/
  domain/         pure data: Phase / FlowState / Progress / Checkpoint / Scope / Story / Plan /
                  Review / StateChange / Phase-Flow transition rules
  store/          filesystem persistence (tmp+rename + idempotent reconciliation; commit has Saga phase facts): progress / checkpoints / outline /
                  drafts / summaries / characters / world / signals / run_meta / runtime /
                  session / decisions (ruling audit)
  tools/          11 Agent tools, write tools are single-file atomic + explicit errors + idempotent; commit additionally uses a persistent Saga
  flow/           routing policy (pure functions + IO boundary): router.go (Route decision table) + state.go (LoadState)
                  + pause.go (stop-point ruling)
  arbiter/        semantic ruling layer (LLM-as-function): plan_start / intervention / failure(deadlock)
                  per-scenario Collect/Decide function pairs + per-scenario Decision types + mechanical validation
  agents/         build.go assembles the three Workers (subagent.Runner, called directly and programmatically by the Engine); ctxpack/ Writer context compression strategy
    guard/        subagent_guards.go (CheckpointDeltaGuard ×3, Worker fact guardrails)
  host/           host.go (lifecycle / intervention orchestration) + engine.go (deterministic execution loop) + observer*.go
                  + events.go + usage*.go + budget.go + advance_gate.go + resume.go + cocreate.go
    imp/          external-novel semantic-compilation import: ingest → segment → analyze → synthesize → publish (pure state derivation + LLM as a function)
    exp/          export of completed chapters: TXT / EPUB 3; purely read-only
  entry/          tui (Bubble Tea) / headless / startup
  bootstrap/      config + ModelSet + provider failover + setup wizard
  eval/           offline evaluation (prompt/voice A/B, regression)
  diag/ errs/ models/ notify/ rules/ userrules/ stylestat/ ...

assets/
  prompts/        arbiter-plan-start / arbiter-intervention / arbiter-failure / architect-short|long
                  / writer (protocol template, {{VOICE}} placeholder) / editor / import-* / simulation-*
  voice.md        writing standards (built-in default of the voice layer; three-layer override see docs/voice-layer.md)
  references/     writing techniques + anti-ai-tone + genre templates, etc.
  styles/         default/fantasy/romance/suspense (users can override/add)

../agentcore     general-purpose Agent framework (go.work sibling directory; may gain general capabilities, never business logic)
../litellm       LLM gateway
```

### 9.1 Evolution milestones

| Date | Refactor | Net effect |
|---|---|---|
| 2026-04-10 | `internal/orchestrator/` (6342 lines) → `host/` + `agents/` | Runtime core -74% |
| 2026-04-20 | Hybrid Coordinator: new `host/flow/`, routing consolidated into pure functions | Routing error rate approaches 0 |
| 2026-05-02 | agentcore slow-thinking/streaming fixes; removed the `idleResumeCount` auto-resume patch | mimo / slow-thinking streaming runs through |
| 2026-06-05 | Rolling-planning closed loop + `/import` reverse-engineering continuation | 200+ chapters run through for the first time |
| 2026-07-12 | **Engine + Arbiter control-plane replacement**: the Coordinator long loop and its seven-patch ecosystem retired; three-layer override for the voice layer; hardened by five rounds of adversarial review | One fewer LLM forward per boundary; control plane 100% testable offline; semantic rulings replayable |
| 2026-07-15 | **`/import` semantic compilation pipeline**: hard-coded splitting rules retired, replaced by staged compilation ingest→segment→analyze→synthesize→publish; pure state derivation (`NextAction(Facts)`) + input-fingerprint-bound artifacts, fully recoverable and idempotent throughout | Splitting improves naturally with model capability; no drifting stage enumeration; interruptions can resume, control plane testable offline |

Measured: hy3-preview free 12 chapters / 73 minutes and mimo-v2.5-pro 10 chapters / 84,000 characters, both finished in a single run; the long-form gpt-5.4 novel 《凡骨》 (Fan Gu) 235 chapters / 1.27 million characters ran through the rolling-planning closed loop (Coordinator-era data; the first Engine-era run is yet to be added). <!-- han:keep -->

---

## 10. What is explicitly not done

A violation means the architecture has deviated.

1. **Do not introduce the Task / Job / WorkItem concept**. The "current task" shown by the UI is an event-stream projection, not a fact.
2. **Do not invent a second scheduler outside Route**. Every "who is next" must go through the Route decision table (pinned by the exhaustive spec) or an Arbiter ruling (persisted audit); scattered if-else dispatching is not allowed.
3. **Do not build an "idle resume" mechanism**. The Engine loop ending = Host enters a terminal state; the only ways to get moving again are the user's `Continue` or a restart `Resume`.
4. **Do not add behavioral discipline to prompts**. Needing a behavioral guardrail means the layering is wrong: invariants go into tool preconditions, judgment goes to the Arbiter, flow goes into Route.
5. **Do not add auto-resume patches in the Host for abnormal stops**. The former `idleResumeCount` failed to save the day 100% of the time in the only long run where it actually fired, and instead masked the real cause at the agentcore layer (see `feedback_no_host_resilience.md`).
6. **Do not infer task completion from "tool exec end"**. The only evidence of completion is a checkpoint write.
7. **Do not build four-layer models such as WorkflowInstance / Command + Apply**. The fact layer is only Progress + Checkpoint + Artifact.
8. **Do not support parallel Workers**. A single active Engine loop, one book advances serially. For multiple novels, use multiple processes.
9. **Do not make LLM calls in the tool layer** (except the Agent tool itself). Pure IO + validation + idempotency.
10. **Do not let the UI read the Store directly**. It may only subscribe to events or read the Host `Snapshot()`.
11. **Do not write a Host-side Flow state machine**. The Flow label is updated only by tools; Route only reads and never writes.
12. **Do not write hard-coded backstops for "LLM hallucination"**. Optimize the prompt, improve tool return values, and make novel_context present the facts more clearly.
13. **Do not let diag / the observation layer intervene in control flow**. Diagnostics are read-only; auto-repair / auto-resume / changing the flow are never done.
14. **Budget and chapter-advance policy do not go into Route / the tool layer**. `BudgetSentinel` / `ChapterAdvanceGate` are policy components at the Engine boundary (executing instructions the user signed in advance, not evaluating literary behavior); `notify` is pure observation.
15. **Control-plane changes must change the exhaustive spec first and the implementation second**; **the contract tests must pass before bumping agentcore**.
16. **Do not build a general workflow DSL, event sourcing, or a global State Digest**. Route is one table per domain; generalizing it is over-design.

---

## 11. Verification strategy

### 11.1 Test asset inventory

| Layer | Asset | Coverage |
|---|---|---|
| Control-plane spec | `flow/router_exhaustive_test.go` | Exhaustive 120k combinations of the Route decision table + pure-function / determinism / conservation properties |
| Framework contract | `agents/agentcore_contract_test.go` | 5 agentcore behavior assumptions, driven through `Runner.Run` (must be run before an upgrade) |
| Engine end-to-end | `host/engine_test.go` | fake model + real tools: write a complete book / failure ruling / deadlock ruling / rewrite-approval timing / boundary hold stops immediately / exit-race preservation / one permit per chapter |
| Ruling | `arbiter/arbiter_test.go` | parsing / feedback retry / per-scenario validation matrix / fact collection |
| Fact pipeline contract | store/tools tests | feedback pool across restarts, violation records latest-wins / cleared on rewrite / novel_context injection, PlanStart preserved across Init |
| Voice layer | `assets/load_test.go` | splitting is byte-for-byte identical / three-layer override semantics / eval uses the same assembly path |
| Semantic quality | `internal/eval` + decisions.jsonl | prompt/voice A/B, offline replay of rulings (regression set under construction) |

### 11.2 Stability scenarios

- **A Long-run**: 80~200 chapters finished in one run, Phase=complete. Provider failover and retries are allowed; any auto-resume is forbidden.
- **B Crash recovery**: kill the process after any step → Resume → Route continues from the facts, does not rewrite persisted artifacts, and checkpoints have no duplicate steps. A crash during planning goes through PlanStartRecord.
- **C Provider jitter**: intermittent 503 → litellm failover, the Worker is unaware.
- **D User intervention**: Steer while running → ruling echoed in seconds, actions committed at the boundary; Steer while stopped → after the ruling, bring up the engine according to intent; crash → PendingSteer replay.

### 11.3 Compliance (can be written as a linter / test)

- `flow.Route` must be a pure function: reading the Store / any IO is forbidden
- No call that starts the engine may appear in the body of `runEnded`
- A new ruling scenario must add a Collect/Decide pair + a Decision type + persistence together
- Recovery-related code may appear only in `host/resume.go` and `engine.planStartFallback`

### 11.4 Quality iteration

Changing the voice → edit `<book directory>/style/` (user level) or assets/voice.md (built-in), verified by A/B on the voice evaluation set; adding review dimensions → edit editor.md (save_review receives them in structured form); adding reference material → wire it explicitly in three places (`tools.References` + `loadReferences` + the novel_context injection mapping).

**Whole-book style statistics (`internal/stylestat`)**: the Host creates a unique `StyleStatsIndex` per book and explicitly injects it into `novel_context` and `commit_chapter`. On first startup it rebuilds the index from all completed chapters, then updates it incrementally for newly added/rewritten chapters (sentence patterns / high-frequency phrases / cross-chapter repeated sentences / chapter-ending shapes); under the same book state the snapshot is reused and injected as `episodic_memory.style_stats`: the editor rules by the numbers, and the writer uses them to avoid repeating itself. Offline eval can still call the pure function `Compute` directly. **Statistics belong to code, rulings belong to the LLM**.

---

## 12. Summary

> **Deterministic fact layer, autonomous semantic layer.** The model is free where verification is impossible (what to write, how to write it, how to judge), and constrained where verification is possible (ordering, idempotency, phases).

There is no task queue, no policy engine, no resident session. What there is:

- One serial deterministic Engine loop (~500 lines, six end-to-end paths pinned down)
- One Route decision table (a pure function, a 120k-combination exhaustive spec)
- Four Arbiter ruling functions (facts in, structured decision out, persisted and replayable)
- Three kinds of functional Worker (independent context and model, fact guardrails with zero disturbance)
- 11 tools that are single-file atomic with explicit cross-file failure / idempotent recovery; of these, commit uses a persistent Saga + one jsonl checkpoint file

Where the gains from model upgrades flow is clear at a glance: better creation (all output of Writer/Architect/Editor), more accurate rulings (the Arbiter's four scenarios), better summaries (ctxpack), all obtained just by swapping the model, with not one line of the shell changed. The control plane does not take the model dividend, because **a table lookup needs no intelligence**; what it needs is to be proven correct, and it has been proven.

The rigidity of the flow is intentional, priced, and left with a door: to loosen the writer's tool order → relax one section of the protocol prompt (invariants are backstopped at the tool layer); to dispatch by arc → add one branch to Route; to extend ruling capability → add a Collect/Decide pair. Every loosening has a referee (the exhaustive spec, voice evaluation, decisions replay): **use evidence, not faith, to decide how much rope to give the model**.

The only discipline: **when someone wants to add a decision point, first run it through the three-way split: enumerable ones go into Route, those with clear boundaries go into the Arbiter, open-ended ones go into the Worker**. For a decision that is none of the three, think again about whether it really exists.
