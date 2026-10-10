# Refactor Proposal: Hybrid Coordinator — Host Routing × LLM Ruling

> **Historical document, deprecated.** The Hybrid Coordinator was replaced on 2026-07-12 by the Engine + Arbiter architecture; for the current design see `docs/architecture.md` and `docs/engine-rfc.md`. This document is kept only as a record of how the decisions evolved and must not be used as an implementation basis.
>
> Original status: **adopted and landed** (2026-04-20)
> Research date: 2026-04-20
> Corresponding current documents: `docs/architecture.md` §2 / §3 / §7 / §8 / §13 were updated in sync
>
> **This document is the second draft.** The problems of the first draft's radical plan (removing the Coordinator entirely) are detailed in Appendix A, which is kept so that nobody walks the same detour again.
>
> Landing result:
> - `internal/host/flow/` created (router.go / state.go / dispatcher.go / router_test.go, all 15 branch unit tests pass)
> - `internal/host/reminder/`: `flow.go` / `queue_guard.go` / `book_complete.go` deleted; StopGuard and the subagent Guards kept
> - `assets/prompts/coordinator.md` compressed from 88 lines to ~45 lines (responsibility narrowed to executing Host instructions + ruling + startup selection)
> - `internal/host/resume.go` greatly simplified: it only generates a label and a short prompt, and the concrete next step is dispatched by the Router after the first TurnEnd
> - `internal/store/` gained the helper methods `HasArcReview` / `HasArcSummary` / `HasVolumeSummary` / `CheckConsistency`
> - The bug where the `observer.go` agent state no longer stayed at working was fixed along the way

---

## 1. Background

### 1.1 Project positioning

```
agentcore       — general-purpose agent framework
litellm         — general-purpose LLM gateway
ainovel-cli     — vertical agent for novel writing (this project)
```

The decision space of a vertical agent is **closed**: the flow chart is fixed, the branches are finite, and decisions are fact-driven. Applying the design philosophy of a general-purpose agent ("bet on model capability") to a vertical scenario is suspected of being overly purist.

### 1.2 User goals (in priority order)

1. **Stability** — keep writing without interruption, and do not stop because of a routing error
2. **Ride LLM upgrades** — the architecture does not fight model capability
3. **Make full use of multi-agent capability** — a clear division of duties

This proposal makes a **Pareto improvement** among the three (no goal is sacrificed to gain another).

---

## 2. Current-state research

### 2.1 Classification of the Coordinator's decision points

Extracting the decision points of `coordinator.md` one by one:

| # | Decision point | Nature | Frequency |
|---|---|---|---|
| 1 | Choose architect_long / short at startup | Ruling (semantic understanding) | 1 time per book |
| 2 | Input expansion (auto-supplement for <20 characters) | Ruling (creative) | 0-1 times per book |
| 3 | Planning completion loop | Routing (fact-driven) | 1-3 times |
| 4 | Next step after each chapter commit | **Routing** | **1-2 times per chapter** |
| 5 | Step-by-step execution of the arc-end review | Routing | 3-5 times per arc |
| 6 | Review verdict fork | Routing (already moved into code, see §2.3) | 1 time per arc |
| 7 | Handling user interventions | Ruling (must be an LLM) | Any time |
| 8 | Re-dispatch after a subagent error | Routing | Occasional |
| 9 | Output a summary when the whole book is complete | Routing | 1 time |

**Conclusion**: of the 9 decision points, 6 are pure routing (table lookup) and 3 are rulings that truly need an LLM. **Routing happens far more often than ruling** (1-2 times per chapter vs a few times per book).

### 2.2 The Reminder channel is already a half-finished product of flow-as-code

The generators under `internal/host/reminder/` produce, every round and based on facts, **instructions down to the concrete action**:

- `flow.go` → `"Current flow=writing, next_chapter=37. Please call subagent(writer, \"write chapter 37\") directly..."`
- `queue_guard.go` → `"Current flow=rewriting, pending queue: [3,5]. Please call writer immediately to rewrite chapter by chapter..."`
- `book_complete.go` → `"The whole book is complete. Please output the whole-book summary..."`

**The current architecture has a double dispatch**:
```
Rule layer: coordinator.md defines "if A then B"
  ↓
Reminder layer: every round it makes the rule concrete from the facts → generates "now do B"
  ↓
LLM layer: reads the reminder and generates a tool_call (basically restating the reminder)
  ↓
SubAgent executes
```

**The LLM is in effect only "executing" the instruction the Reminder gave it.** This middle step both consumes tokens and introduces uncertainty (the LLM may not fully obey the reminder, such as the observed mid routing error).

### 2.3 The tool layer used to carry too much semantic judgment

- The old `save_review` implementation used to override the Editor verdict according to a fixed scoring threshold and the contract status; this has been deleted. The literary ruling belongs to the Editor, and the tool only does protocol validation and an atomic state mapping
- `commit_chapter.CheckArcBoundary()`: computes `arc_end / needs_expansion / needs_new_volume` on the spot
- `commit_chapter.applyCompletion()`: decides `book_complete` on the spot
- `CommitResult` returns 17 fact fields

**Conclusion**: deterministic storage and phase invariants stay in the tool layer, while literary and semantic judgment is left to the model.

### 2.4 The actual cost of the current state

Coordinator LLM turns per chapter:
- **1-2 turns per chapter** (read system prompt ~3000 tokens + reminder ~200 tokens + history + CommitResult ~500 tokens → generate tool_call ~50 tokens)
- A 200-chapter long-form novel needs about **200-400 turns** of Coordinator LLM calls
- Of these, **~90% are pure routing** (the LLM restating the reminder) and **~10% are rulings**

**~3500-7000 tokens per chapter are spent on Coordinator decisions, 95% of them redundant** (the Reminder has already computed the answer).

---

## 3. Design: Hybrid Coordinator

### 3.1 Core idea

**Move flow decisions from the LLM to the Host, but keep the Coordinator as the ruling node and the instruction-execution channel**.

```
┌──────────────────────────────────────────────────────────┐
│                   Entry (TUI / headless)                   │
└────────────────────────────────┬─────────────────────────┘
                                 │ Start / Resume / Steer
┌────────────────────────────────▼─────────────────────────┐
│                            Host                            │
│                                                             │
│   ┌──────────────────────────────────────────────────┐     │
│   │  Flow Router (new core)                            │     │
│   │  ───────────                                      │     │
│   │  Subscribes to Coordinator events: on subagent return│    │
│   │  Pure function: route(Progress, Checkpoint, Boundary)│    │
│   │      → NextInstruction                             │     │
│   │  Has instruction → coordinator.FollowUp(instruction)│    │
│   │  No instruction (ruling) → no intervention, LLM free│    │
│   └──────────────────────────────────────────────────┘     │
│                                                             │
│   Kept: lifecycle API / Observer / Usage Tracker            │
│   Kept: resume.go (simplified, core logic unchanged)        │
└────────────────────────────────┬─────────────────────────┘
                                 │
┌────────────────────────────────▼─────────────────────────┐
│                    Coordinator Agent (LLM)                  │
│                                                             │
│   Responsibility narrowed to two kinds:                     │
│   1. Receive a Host FollowUp instruction → emit tool_call   │
│   2. Rule autonomously when a user Steer arrives            │
│      (query / modification assessment)                      │
│                                                             │
│   coordinator.md: 88 lines → ~25 lines                      │
│   MaxTurns: 1000 kept (user steer + Host instructions)      │
└────────────────────────────────┬─────────────────────────┘
                                 │
                                 ▼
         ┌──────────────────────┼───────────────────────┐
         ▼                      ▼                       ▼
    ┌────────┐             ┌────────┐             ┌────────┐
    │Architect│             │ Writer │             │ Editor │
    └────────┘             └────────┘             └────────┘
```

### 3.2 Redistribution of responsibilities

| Layer | What it does | What it does not do |
|---|---|---|
| **Host / Flow Router** | Read facts → pure-function routing → FollowUp instruction | Call a SubAgent itself (still goes through the Coordinator) |
| **Coordinator** | Execute Host instructions + rule on user interventions + choose the planner at startup | Autonomously decide "what to do next" |
| **SubAgent (A/W/E)** | Each does its own job | No change |
| **Tool layer** | Atomic persistence + return facts | No change |

**Key invariants**:
- ✅ The Coordinator is still one continuous agent run, keeping the "continuous awareness" of the whole book
- ✅ User Steer still goes through `coordinator.Inject()`, and the ability to interrupt immediately is kept
- ✅ SubAgentTool is still called by the LLM (via the native agentcore path), and the event stream / ContextManager / model switching are all unchanged
- ✅ Zero modification to agentcore

### 3.3 The concrete logic of the Flow Router

```go
// internal/host/flow/router.go

type NextInstruction struct {
    Agent  string   // architect_long / architect_short / writer / editor
    Task   string   // task description for the subagent
    Reason string   // reason shown to the Coordinator (optional, eases debugging)
}

type RouterState struct {
    Progress        *domain.Progress
    LatestCheckpoint *domain.Checkpoint
    // Arc boundary in layered mode (computed when the previous chapter is complete)
    LastCompleted   int
    ArcBoundary     *store.ArcBoundary
    HasArcReview    bool
    HasArcSummary   bool
    // Missing items of the foundation settings
    FoundationMissing []string
}

// Route returns the next instruction. Returning nil means let the Coordinator rule autonomously (ruling scenario).
func Route(s RouterState) *NextInstruction {
    p := s.Progress

    // 0. Terminal state: let the LLM output a summary, do not route
    if p.Phase == domain.PhaseComplete {
        return nil
    }

    // 1. Planning phase: the ruling (choosing the planner) is done by the LLM, do not route
    if p.Phase != domain.PhaseWriting {
        return nil
    }

    // 2. Writing phase
    // 2a. The rewrite/polish queue comes first
    if len(p.PendingRewrites) > 0 {
        ch := p.PendingRewrites[0]
        verb := "rewrite"
        if p.Flow == domain.FlowPolishing {
            verb = "polish"
        }
        return &NextInstruction{
            Agent:  "writer",
            Task:   fmt.Sprintf("%s chapter %d", verb, ch),
            Reason: fmt.Sprintf("PendingRewrites queue has %d chapters left", len(p.PendingRewrites)),
        }
    }

    // 2b. Under review: do not route, let the Coordinator follow the verdict fork according to the save_review result
    if p.Flow == domain.FlowReviewing {
        return nil
    }

    // 2c. Arc-end post-processing in layered mode
    if p.Layered && s.ArcBoundary != nil && s.ArcBoundary.IsArcEnd {
        b := s.ArcBoundary
        if !s.HasArcReview {
            return &NextInstruction{
                Agent:  "editor",
                Task:   fmt.Sprintf("Run the arc-level review for volume %d arc %d", b.Volume, b.Arc),
                Reason: "arc-end review not done",
            }
        }
        if !s.HasArcSummary {
            return &NextInstruction{
                Agent:  "editor",
                Task:   fmt.Sprintf("Generate the summary of volume %d arc %d", b.Volume, b.Arc),
                Reason: "arc summary not done",
            }
        }
        if b.NeedsExpansion {
            return &NextInstruction{
                Agent:  "architect_long",
                Task:   fmt.Sprintf("Expand volume %d arc %d (save_foundation type=expand_arc)", b.NextVolume, b.NextArc),
                Reason: "next arc skeleton awaiting expansion",
            }
        }
        if b.NeedsNewVolume {
            return &NextInstruction{
                Agent:  "architect_long",
                Task:   "Evaluate and execute save_foundation(type=append_volume) or mark_final",
                Reason: "volume ended, need to decide whether to append a new volume",
            }
        }
    }

    // 2d. Normal continuation
    next := p.NextChapter()
    return &NextInstruction{
        Agent:  "writer",
        Task:   fmt.Sprintf("write chapter %d", next),
        Reason: "continue writing",
    }
}
```

**Function properties**:
- Pure function (input RouterState, output NextInstruction)
- Unit-testable (given a state, assert the routing result)
- **Returning nil is legal** — it means "this is a ruling scenario, let the LLM act on its own"

### 3.4 Trigger timing

The Host subscribes to the `agentcore.EventToolExecEnd` event:

```go
coordinator.Subscribe(func(ev agentcore.Event) {
    if ev.Type == agentcore.EventToolExecEnd && ev.Tool == "subagent" && !ev.IsError {
        // SubAgent just returned → read the latest state → route
        h.flowRouter.Dispatch()
    }
})
```

```go
func (r *FlowRouter) Dispatch() {
    state := r.loadState()
    instruction := Route(state)
    if instruction == nil {
        return // ruling scenario, let the LLM act on its own
    }
    msg := formatInstruction(instruction)
    _ = r.coordinator.FollowUp(agentcore.UserMsg(msg))
}

func formatInstruction(i *NextInstruction) string {
    return fmt.Sprintf(
        "[Host instruction] Next step: call subagent(%s, %q)\n"+
        "Reason: %s\n"+
        "This is an explicit flow-layer instruction. Execute it immediately; do not call novel_context first and do not output reasoning first.",
        i.Agent, i.Task, i.Reason,
    )
}
```

### 3.5 Responsiveness and concurrency

**User Steer path** (unchanged):
```
Steer → coordinator.Inject(UserMsg("[User intervention] xxx"))
```

- Running: the message is inserted into the current run queue
- Idle: resume the run
- Paused: queued

**Concurrency of routing instructions + Steer**:
- Both enter the Coordinator's message queue and are processed in the native agentcore order
- If the Host has just sent `FollowUp("[Host instruction] write chapter 37")` and the user immediately sends the Steer `"Stop for a moment, adjust the style"`
  - Does the Coordinator process the Host instruction first, or the Steer first?
  - **The semantics of `Inject` is to jump to the head of the current queue**, so the Steer is processed first
  - This is the expected behavior: user intervention has a higher priority than routine Host scheduling

**Avoiding conflicts between Host instructions and Steer**:
- After receiving the "Steer injected" signal, the Flow Router **pauses briefly** for a few turns (letting the Coordinator finish processing the Steer before routing)
- The Steer processing result is sensed by subscribing to `agentcore.EventMessageEnd` + checking Progress state changes

### 3.6 coordinator.md simplification example

Cut from 88 lines to about 25 lines:

```markdown
You are the general coordinator of novel writing.

## Your working mode

**Main line**: after each subagent returns, the Host sends a `[Host instruction]` message telling you which subagent to call next and what it should do. On receiving an instruction, generate the corresponding tool_call immediately; do not call novel_context first to reason, and do not restate.

**Ruling**: in the following cases you must judge on your own (the Host will not issue an instruction; you must act proactively):

### At startup: choose the planner

- Default → `architect_long`
- Only when the user explicitly asks for a short story / single volume / sketch and the length is capped within 25 chapters → `architect_short`

If the user input is < 20 characters, first supplement the task description with a differentiated direction, the target readers, and at least one unconventional story hook, then dispatch.

### User Steer

Format: `[User intervention] xxx`

- **Query type** (asking about status / settings): output the text answer directly, no further tool call needed; the Host will continue dispatching.
- **Modification type** (asking to change settings / rewrite / adjust style): assess the scope of impact:
  - Involves a setting change → call architect_* to do `save_foundation(type=...)`
  - Involves already-written chapters → let the tool automatically write the target chapters into `PendingRewrites` (the rewrite intent can be stated when calling writer again)
  - Only affects the later style → describe the requirement briefly, and append it to the writer's task description the next time a Host instruction arrives

## Tools

- `subagent(agent, task)`: call a subagent
- `novel_context`: use only when a user query needs it; do not call it first after a Host instruction arrives

## Subagents

- `architect_long` / `architect_short` / `writer` / `editor`

## Prohibited

- Calling novel_context first and then acting when a Host instruction arrives
- Deciding the next step on your own when there is neither a user Steer nor a Host instruction
```

### 3.7 Major slimming of the Reminder channel

**Delete**:
- `flow.go` (the Host FollowUp already issues the concrete instruction, so the Reminder's routing reminder loses its value)
- `queue_guard.go` (the queue is guaranteed by the Host Router)
- `book_complete.go` (when Phase=Complete the Host FollowUps a summary-output instruction)

**Keep**:
- `subagent_guards.go` (the StopGuard of Writer/Architect/Editor, ensuring a subagent does not finish empty-handed)
- Add a lightweight `foundation_reminder.go`: tells the Coordinator the missing items during the planning phase (this is **information needed for a ruling** rather than a routing instruction)

**StopGuard kept**:
- The Coordinator's StopGuard is kept (blocks end_turn when `Phase != Complete` as a backstop)
- Add a reminder injected when "a Host instruction was received but the corresponding subagent was not called this turn"

### 3.8 resume.go minor simplification

The current `buildResumePrompt` generates natural-language instructions precise down to the step according to the checkpoint (121 lines).

New architecture:
- On Resume, read Progress first, and the Flow Router computes the `NextInstruction`
- The Coordinator receives a **very short** resume prompt, then waits for the Host's FollowUp instruction

```
[Resume] The book "xxx" has completed N chapters and is in phase XX.
Please wait for the Host's next instruction, or handle any user intervention that may have been left during the downtime.
```

Almost all branch logic sinks into the Flow Router (the Router has to route by state anyway, so Resume needs no special path).

---

## 4. Goal attainment assessment

### 4.1 Stability

| Risk | Current | New architecture |
|---|---|---|
| The Coordinator picks the wrong architect | Has happened (mid routing error) | Startup is still a ruling, but the prompt went from three tiers to binary (done), so the error surface is greatly reduced |
| The Coordinator does not obey "just say write chapter N" | Has happened | The Host issues an instruction in a fixed format, so the LLM no longer has to generate the task description |
| The Coordinator misses the queue_drained check | Has happened | The Host Router forces the sequence |
| After the arc-end commit the Coordinator forgets to call editor | Possible | The Host Router detects IsArcEnd && !HasArcReview and dispatches directly |
| A crash-recovery branch is missed | Known gap | The Flow Router's state machine naturally covers all branches |
| StopGuard blocks 5 times in a row and escalates to fatal | Exists | With a clear Host instruction it is hard for the LLM to be blocked repeatedly (unless the prompt fails badly) |

### 4.2 LLM upgrade dividend

| Dimension | Retention |
|---|---|
| Writer model upgrade → writing quality | 100% |
| Editor model upgrade → review accuracy | 100% |
| Architect model upgrade → refined planning | 100% |
| **Coordinator model upgrade → more accurate rulings** | **100%** (ruling scenarios kept) |
| ~~Coordinator model upgrade → more accurate routing~~ | Given up (the routing error rate should be 0 anyway, with no need for the LLM to get smarter) |

**Important retention**: ruling scenarios such as user-intervention assessment, planner selection, and verdict-boundary judgment are still handled by the LLM and benefit directly from model upgrades.

### 4.3 Multi-agent capability

- The number, duties, and assembly of SubAgents are **completely unchanged**
- Heterogeneous models (coordinator/architect/writer/editor configured independently) are **completely unchanged**
- The Coordinator is still a continuous run, keeping the "whole-book view"
- The collaboration medium (artifacts in the Store) is unchanged

### 4.4 Responsiveness

- The ability of a user Steer to interrupt via `coordinator.Inject` is **completely kept**
- The Host Router dispatches instructions when a SubAgent returns, on the same message channel as the user Steer
- Inject has a higher priority than FollowUp (the semantics of `Inject` is to jump the queue), so a Steer will not be crowded out by a Host instruction

### 4.5 Token cost

Currently per chapter: Coordinator ~3500-7000 tokens × 1-2 turns = 3500-14000 tokens

New architecture per chapter:
- The Coordinator prompt is compressed from ~3000 tokens to ~800 tokens
- Each chapter still needs 1 turn (the Coordinator reads the FollowUp instruction + generates a tool_call)
- Total ~1000-1500 tokens

**Saves 60-80%.** For a 200-chapter long-form novel, about 400k-1M tokens saved (not the 100% of the radical plan, but it does not sacrifice responsiveness or the whole-book view).

---

## 5. Impact on docs/architecture.md

### 5.1 §2 Core principles adjustment

**Principle 1** (LLM-driven main loop) → adjusted to:
```
The LLM drives creation and ruling; the Host drives flow routing.

- Creation and ruling (decisions needing semantic understanding, quality judgment, and intent recognition) are still left to the LLM
- Flow routing (read facts → table lookup → issue an instruction) is carried by Host code
- The Host does not bypass the Coordinator to call a SubAgent directly, but issues an explicit instruction through FollowUp,
  keeping the Coordinator as the instruction-execution channel and the ruling node
```

**Principle 2** (bet on model capability, not on hard-coding) → adjusted to:
```
In the creation and ruling dimensions, bet on the model (the ruling capability of Writer/Editor/Architect/Coordinator);
in the flow-routing dimension, express it in code (the decision space of a vertical agent is closed, and a table-lookup task gets no dividend from an LLM).
```

### 5.2 §13 Prohibition list adjustment

- §13.13 "Do not build a deterministic control plane where the Host reads signal files → injects the next-step instruction" →
  **reworded**: "Do not use signal files for IPC (reading Progress + Checkpoint directly is enough); the Host reading facts and then issuing an explicit subagent-call instruction through `coordinator.FollowUp` is reasonable vertical routing"
- §13.14 "Do not hard-code Flow transitions as a state machine" →
  **reworded**: "The Flow label is still updated only by tools (no state machine of 'if A then SetFlow(B)' in the Host), but the Flow Router may decide whom to call next based on Flow and other facts"

### 5.3 §7 Agent assembly adjustment

- Keep the Coordinator assembly
- `coordinator.md` cut from 88 lines to ~25 lines
- Reminder channel reduced (delete flow/queue_guard/book_complete, keep foundation/subagent_guards)
- Add the `internal/host/flow/` package

---

## 6. Known weaknesses (listed honestly)

### 6.1 Long-term evolution of the Flow Router

- As new scenarios are added (new flow states, new arc-end post-processing), the Router's switch-case will grow longer
- It needs strict constraints: **handle routing only, not business logic**; write the decision rules as unit tests
- The warning of the v0.0.1 `handleSubAgentDone` remains valid forever; but this plan avoids sliding into a god object through "pure function + unit tests + calling only pure facts"

### 6.2 Complexity of user intervention

- The current design hands the Steer entirely to the Coordinator's LLM for ruling
- But some Steers span multiple categories (for example "clarify character A in the earlier chapters + add a side plot for him later")
- This relies on the LLM's ability to decompose them, and the prompt needs to give clear guidance
- **This part benefits directly from model upgrades** (compared with a hard-coded InterventionAgent enum classification, flexible LLM ruling fits real scenarios better)

### 6.3 Prerequisite dependency on fact-layer consistency

- The Router makes decisions based on Progress + Checkpoint, so the fact layer must be reliable
- Single-file `withWriteLock` + tmp/rename guarantees atomic replacement; the cross-file steps of `commit_chapter` are recovered through the complete PendingCommit payload, the body-text snapshot, and phased idempotent replay, while structural operations repair derived views with the same parameters; none of them claims a database-style atomic transaction
- But if the fact layer becomes inconsistent (for example Progress says chapter 3 is complete but there is no such file under chapters/), the Router will make a wrong decision
- Suggestion: add a **fact-layer consistency check** at startup (for example, report a warning if Progress.CompletedChapters does not match the chapters/ directory)

### 6.4 The Coordinator still retains the possibility of LLM routing

- Even when the instruction is explicit, the LLM may "creatively" not execute it (for example, generating a passage of thinking text before calling the tool)
- StopGuard as a backstop: if a Host instruction was received but no subagent was called this turn, a reminder is injected
- This is a backstop, not a prohibition — an occasional "extra step of thinking" from a strong model is not a bad thing either

### 6.5 Higher test coverage requirements

- The Flow Router is a pure function and must have complete unit tests (covering all Phase × Flow × Boundary combinations)
- Integration test: simulate the full chain "commit → router → FollowUp → coordinator response → subagent"
- Crash-recovery test: kill the process and resume, and assert that the Router derives the correct next step

---

## 7. Implementation roadmap

### Stage 1: Fact-layer hardening (about 0.5 days)

- Complete the consistency check of §6.3: scan once at startup/Resume and generate a warning
- Make sure the `store.HasArcReview(vol, arc)` and `HasArcSummary(vol, arc)` APIs are available (add them if not)

### Stage 2: Introduce the Flow Router skeleton (about 1 day)

- Create the `internal/host/flow/` package:
  - `route.go` — pure function `Route(state) → *NextInstruction`
  - `dispatcher.go` — subscribe to events + issue FollowUp
  - `route_test.go` — unit tests covering all branches
- Control whether it is active through the config switch `flow_driven: true/false`
- Off by default (false), run a comparison first

### Stage 3: Activate and verify (about 1 day)

- Turn on `flow_driven: true`
- Run a 30-50 chapter novel and compare the metrics:
  - Number of Coordinator LLM calls
  - Number of routing errors (should be 0)
  - Responsiveness (whether steer interruption works normally)
- Fix bugs and tune the Router rules

### Stage 4: Simplify coordinator.md + slim the Reminder (about 0.5 days)

- Change coordinator.md per §3.6
- Delete `reminder/flow.go / queue_guard.go / book_complete.go`
- Keep the necessary foundation reminder
- Update the subagent StopGuard if needed (usually not)

### Stage 5: Simplify resume.go (about 0.5 days)

- Delete most of the branches of `buildResumePrompt`
- Replace them with a short generic "[Resume] Please wait for the Host instruction" message
- After Resume the Router naturally derives the action to continue

### Stage 6: Architecture document update (about 0.5 days)

- Modify `docs/architecture.md` §2 / §13 / §7 per §5
- Change the status of this proposal document to "adopted" and archive it to `docs/history/`

### Stage 7: Observation period (2-4 weeks)

- Run 2-3 long-form novels in a row (100+ chapters each)
- Record all routing errors (if any), responsiveness problems, and unexpected Coordinator behavior
- Fine-tune the Router rules and coordinator.md based on the observations

**About 4 days of implementation in total + the observation period.**

---

## 8. Comparison table

| Dimension | Current architecture | Hybrid (this plan) | Radical plan (Appendix A) |
|---|---|---|---|
| Stability | Medium (the LLM occasionally mis-routes) | **High** | High |
| Responsiveness | High | **High** | **Low** (a Host direct call to a SubAgent cannot be interrupted) |
| LLM dividend | 100% | **100%** | 85% (routing dimension given up) |
| Token savings | 0 | ~70% | ~95% |
| Whole-book view | Yes | **Yes** | None (each SubAgent is independent) |
| Implementation cost | - | Medium (about 4 days) | High (about 1 week + changing agentcore) |
| Documentation updates | - | Small (minor tweaks to §2/§13) | Large (§2 principles rewritten) |
| Needs agentcore change | - | No | Possibly (calling a SubAgent directly) |
| Rollback difficulty | - | Low (config switch) | High |

---

## 9. Decision points

1. **Adopt this proposal (Hybrid Coordinator)?** [ ] Adopt / [ ] Adopt after changes / [ ] Do not adopt
2. Should Stage 3 land first as a separate PR for verification? [ ]
3. Should the `docs/architecture.md` §2 / §13 adjustments be handled together in this round? [ ]
4. Observation period length: [ ] 2 weeks / [ ] 4 weeks / [ ] Longer

---

## Appendix A: The radical plan that was evaluated (removing the Coordinator entirely)

> The first-draft plan. Downgraded to reference because of the responsiveness regression, doubtful technical feasibility, the loss of the Coordinator's whole-book view, and similar problems.

The core of the radical plan: the Host calls `SubAgentTool.Execute` directly, without going through the Coordinator LLM.

**Identified problems**:

1. **Responsiveness regression**: `SubAgentTool.Execute` is a blocking synchronous call, so a user Steer must wait until the current SubAgent returns before it can be handled. The current architecture's `Inject` can interrupt immediately.
2. **Doubtful technical feasibility**:
   - The Host calling SubAgentTool directly violates agentcore usage conventions
   - The event stream (the Events of `Subscribe`) may not bubble up to the observer correctly
   - The `ContextManagerFactory` / `OnMessage` callback paths of the SubAgent are unknown
   - It would require changing agentcore or heavily changing the observer
3. **Loss of the Coordinator's whole-book view**: each SubAgent is an independent run, with no "continuous LLM watcher". Problems such as style drift and character fragmentation in a long run lose one layer of invisible guardianship.
4. **InterventionAgent over-simplified**: the radical plan classifies user intent with an enum (query/modify_setting/rewrite_chapters/adjust_style/noop), but a real Steer may span several categories, and a forced schema would misclassify.
5. **Large workload of rewriting the architecture document**: the §2 core principles are overturned, and 30% of the document's argumentation is affected.
6. **FlowDriver would grow into a god object**: one loop stuffed with all the routing logic, which must be changed for every new scenario, isomorphic to the v0.0.1 `handleSubAgentDone`.

The Hybrid plan avoids the first 4 problems, reduces the 5th to a minor tweak, and controls the 6th through "pure function + unit tests".

---

## Appendix B: Decision-point placement details

| Decision point | Current location | Location in the new architecture | Type |
|---|---|---|---|
| Choose the planner | coordinator.md L26-29 | Coordinator LLM ruling (at startup) | Ruling |
| Input expansion | coordinator.md L31 | Coordinator LLM ruling (at startup) | Ruling |
| Planning completion loop | coordinator.md L36-38 | Host Router Phase=Premise/Outline branch (return nil to leave it to the LLM, or an explicit FollowUp architect) | Hybrid |
| Next step per chapter | coordinator.md L46-51 + reminder/flow | **Host Router branch 2d** (FollowUp writer) | Routing |
| Arc-end review | coordinator.md L78-82 | **Host Router branch 2c** (FollowUp editor/architect) | Routing |
| Verdict fork | coordinator.md L59-61 + save_review tool | Already in code at the tool layer, the Router only reads Flow | Routing (done) |
| User intervention | coordinator.md L67-70 | Coordinator LLM ruling (when an Inject message is received) | Ruling |
| Re-dispatch after a planner error | coordinator.md L40 | The Host Router detects that FoundationMissing is unchanged and counts retries | Routing |
| Whole-book completion summary | coordinator.md L63-65 + reminder/book_complete | The Host Router detects Phase=Complete → FollowUp "output summary" | Routing |

---

## Appendix C: Reference source locations

- `assets/prompts/coordinator.md` — to be simplified
- `internal/host/reminder/flow.go` / `queue_guard.go` / `book_complete.go` — to be deleted
- `internal/host/reminder/subagent_guards.go` — kept
- `internal/host/reminder/stop_guard.go` — kept + add the "a Host instruction must be executed" check
- `internal/host/resume.go` — greatly simplified
- `internal/host/observer.go` — newly subscribes to EventToolExecEnd to trigger the Router
- `internal/host/flow/` — new package
- `internal/tools/commit_chapter.go` L220-280 — the CommitResult 17 fields are already complete
- `internal/tools/save_review.go` — atomic mapping from the Editor verdict to the Flow / rework queue
- `internal/store/outline.go` `CheckArcBoundary` — arc-boundary fact API
