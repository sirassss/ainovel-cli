# Chapter Advance Gate

> Status: implemented  
> Date: 2026-07-14  
> Solves: per-chapter approval, safe pause after an intervention, and exact chapter permits under crash recovery

## 1. Why it is needed

The core risk of long-form automatic creation is not spending one extra call, but that the system keeps writing new chapters while the user is reviewing, folding summaries, character states and outline feedback built on the old plot into later sources of fact. Deleting the extra chapter does not automatically undo these derived states, and the user loses trust in the creation process as a result.

The project still defaults to "keep working autonomously until done once the goal is given", so per-chapter confirmation is not made the global default. The system offers two explicit policies:

- `auto`: the default mode, advancing autonomously and continuously;
- `review`: the per-chapter approval mode that the user opts into, where every forward new chapter needs one exact permit.

This does not hand the workflow back to the Coordinator LLM. When user confirmation is needed is a user policy; the deterministic next step is still derived by Route; only whether to stop once to approve the result of a given intervention is a semantic judgment made by the Arbiter.

## 2. Division of responsibilities

| Question | Owner | Reason |
|---|---|---|
| Whether the current mode is per-chapter approval | RunMeta / Host | Persistent user run intent |
| Which chapter has been permitted | RunMeta / Gate | A verifiable, recoverable mechanical fact |
| Which Worker runs next | `flow.Route` | Derived as a pure function of the creation facts |
| Whether an instruction starts a forward new chapter | `flow.StartsForwardChapter` | Typed mechanical judgment |
| Whether "show me when you're done editing" requires a pause | Arbiter | Natural-language semantic judgment |
| When the pause triggers | `ChapterAdvanceGate` | Deterministic execution of a one-shot intent |
| Whether the budget allows continuing | `BudgetSentinel` | An independent Host policy |

`AdvanceMode`, the chapter permit and the one-shot hold do not enter the Route decision table, and the model is not allowed to modify them. Route's creation state machine and the per-chapter approval policy stay orthogonal.

## 3. Minimal state model

Only three run-intent items are added to `meta/run.json`:

```go
type RunMeta struct {
	AdvanceMode          ChapterAdvanceMode `json:"advance_mode"`
	AdvancePermitChapter int                `json:"advance_permit_chapter,omitempty"`
	AdvanceHold          *AdvanceHold       `json:"advance_hold,omitempty"`
}

const (
	ChapterAdvanceAuto   ChapterAdvanceMode = "auto"
	ChapterAdvanceReview ChapterAdvanceMode = "review"
)

const (
	AdvanceHoldAtBoundary           AdvanceHoldAfter = "boundary"
	AdvanceHoldAfterRewritesDrained AdvanceHoldAfter = "rewrites_drained"
	AdvanceHoldAtChapter            AdvanceHoldAfter = "chapter"
)

type AdvanceHold struct {
	After         AdvanceHoldAfter `json:"after"`
	TargetChapter int              `json:"target_chapter,omitempty"`
	Reason        string           `json:"reason"`
}
```

There is no generic PolicyEngine, condition array, permit queue, expiry time or policy version. Chapter advancement keeps only one persistent mode, one exact permit and one typed one-shot hold.

### 3.1 Invariants

1. `AdvanceMode` can only be `auto` or `review`; an unknown value returns `UnsupportedAdvanceModeError`.
2. An unknown mode must neither start the Host nor rewrite RunMeta.
3. Under `auto` the permit must be `0`.
4. Under `review` the permit can only be `0` or a positive integer chapter number.
5. Granting the same target again is idempotent, and a different target must not overwrite an in-flight permit.
6. The permit constrains only "starting a forward new chapter that is not yet completed"; planning, review, rewrite, polish and commit recovery are not blocked.
7. The permit is bound to the chapter number, not to a particular process run or a particular Worker call.
8. The permit counts as stably consumed only when the target chapter has entered `CompletedChapters`, the corresponding `PendingCommit` has been cleared, and the `commit` checkpoint for that chapter exists.
9. A target chapter that is completed but lacks its commit checkpoint is state corruption: report an explicit error and pause, without guessing a repair.
10. An uncompleted permit must equal `Progress.NextChapter()`. `PendingRewrites` does not change `NextChapter()`, so rewrites and an in-flight forward permit can coexist mechanically.
11. `AdvanceHold` can only use `boundary`, `rewrites_drained` or `chapter`, and must carry a non-empty reason; `chapter` must carry a positive target chapter, and the other conditions must not carry one.
12. hold and permit use compare-and-clear; when the state has been replaced by a new action it must not be cleared by mistake.
13. A target-chapter hold under `review` mode constitutes a one-shot range authorization; after the pause the original per-chapter approval policy is unchanged.

## 4. Store API

RunMetaStore provides narrow, typed atomic operations:

```go
SetAdvanceMode(mode domain.ChapterAdvanceMode) error
GrantAdvancePermit(chapter int) error
ClearAdvancePermit(chapter int) error
SetAdvanceHold(hold domain.AdvanceHold) error
ClearAdvanceHold(expected domain.AdvanceHold) error
```

- Switching back to `auto` clears the chapter permit under the same write lock, but does not clear a hold produced by another user intervention;
- Granting is legal only under `review`;
- A clear operation consumes only the same target the caller just read;
- When RunMeta is initialized the default mode is `auto`, and the persisted mode, permit and hold are preserved.

The project currently has no historical data to migrate, so the implementation contains no old-field reads, dual writes or downgrade branches.

## 5. Pure-function semantics

### 5.1 Forward new chapter detection

```go
func StartsForwardChapter(
	inst *Instruction,
	progress *domain.Progress,
	pending *domain.PendingCommit,
) bool
```

It returns true only when all of the following hold:

- the Worker is `writer`;
- phase is `writing`;
- there is no `PendingCommit`;
- there is no rewrite queue;
- there is no `InProgressChapter`;
- the target chapter equals `NextChapter()`.

The judgment reads only typed fields and does not parse the Task or Reason text.

### 5.2 One-shot hold

`ResolveAdvanceHold` returns, based on the hold and Progress:

- `keep`: the condition is not yet met;
- `consume`: in the completed state only the intent needs to be cleared;
- `consume-and-stop`: clear the intent and pause.

`boundary` triggers at the current Worker boundary; `rewrites_drained` triggers once the rewrite queue has drained; `chapter` triggers after the target chapter enters the completed list, `PendingCommit` is cleared and the commit checkpoint exists. Unknown conditions and missing facts are reported as errors directly.

## 6. ChapterAdvanceGate

The Gate is the only creation-advancement policy component besides the budget, and it has just two duties:

1. Resolve and consume the one-shot hold at the loop boundary;
2. Check the per-chapter permit before a writer dispatch, and reconcile at the boundary whether the permit was stably consumed.

The Engine order is:

```text
commit pending interventions
→ Gate boundary check
→ Route / take the Arbiter dispatch
→ precheck
→ Gate dispatch permit check
→ Worker
→ Budget boundary check
→ Gate boundary check
→ next round
```

When `auto && hold == nil`, the boundary check returns immediately after reading RunMeta, without reading Progress, PendingCommit or checkpoints.

### 6.1 hold + dispatch

The Arbiter can rule "rewrite chapter 3 and show me when it's done" as:

```json
{
  "hold": {
    "after": "rewrites_drained",
    "reason": "wait for user approval after the rewrite completes"
  },
  "dispatch": {
    "agent": "editor",
    "task": "re-review chapter 3 and build the rewrite queue from the result"
  }
}
```

This set of actions must first execute the paired dispatch so the Editor establishes the rewrite facts, and only then does the Gate judge whether the queue has drained. The Engine binds "this dispatch defers the Gate" to that in-memory instruction and clears it when the instruction is taken; an ordinary Arbiter dispatch cannot bypass the Gate.

### 6.2 permit and rewrites

A completed-book `reopen` can only happen in `complete`, while `/next` can only happen in `writing`, so the two are mechanically mutually exclusive. `PendingRewrites` that already exist during writing do not change the highest completed chapter, so the permit stays aligned with the same `NextChapter()`; the rewrite Worker may run, but it does not consume the forward permit.

## 7. Crash recovery

A chapter commit is a multi-step saga, so the permit cannot be represented by a boolean meaning "the next run may write one chapter". On recovery the Gate reconciles against three kinds of facts:

| Fact window | Gate behavior |
|---|---|
| Target chapter not completed, no PendingCommit | Keep the permit, allow starting/resuming that chapter |
| PendingCommit belongs to the target chapter | Keep the permit, let the commit recovery finish |
| Target chapter completed, PendingCommit cleared, commit checkpoint exists | Consume the permit |
| Target chapter completed but checkpoint missing | Report an error and pause |
| Permit points to an uncompleted chapter other than NextChapter | Report an error and pause |

Therefore, if the process crashes in any window (draft, state write, progress marking or signal write), the same permit is never wrongly applied to the next chapter.

## 8. Arbiter

The intervention schema uses `AdvanceHoldOp`:

```go
type AdvanceHoldOp struct {
	Cancel        bool                    `json:"cancel,omitempty"`
	After         domain.AdvanceHoldAfter `json:"after,omitempty"`
	TargetChapter int                     `json:"target_chapter,omitempty"`
	Reason        string                  `json:"reason,omitempty"`
}
```

Rules:

- An explicit "pause for a moment" uses `boundary`;
- Under `auto`, "edit the written chapters and let me approve when done" uses `rewrites_drained`;
- "Write up to chapter N" uses `chapter`, strictly distinguished from a length adjustment such as "the whole book has N chapters";
- `review` already stops chapter by chapter, so no synonymous hold is created redundantly;
- A target-chapter hold under `review` is a one-shot batch authorization explicitly signed by the user;
- "Continue" can cancel an existing hold but cannot issue a chapter permit;
- Switching modes can only use `/review on|off`, and releasing can only use `/next`.

The Engine calls RunMetaStore directly to apply the structured action, and does not disguise it as an LLM Tool.

## 9. User interface

### 9.1 `/review on|off`

- `/review on`: persist the per-chapter approval policy immediately; if a Worker is running, stop before the next forward new chapter once the current work finishes;
- `/review off`: switch back to automatic advancement and atomically clear the permit; it does not implicitly start an Engine that is already paused, and the event explicitly prompts the user to enter a continue instruction.

### 9.2 `/next`

Available only when all of the following hold:

- the Engine is not running;
- not in stage co-creation;
- the mode is `review`;
- there is no pending hold;
- the budget allows it;
- phase is `writing`.

The command issues an exact permit for `NextChapter()` and starts the Engine. The notification makes explicit that after that chapter is committed, the necessary review and arc/volume structure maintenance still complete, and then it waits for release again.

### 9.3 Status display

`UISnapshot` is the sole source of facts for the TUI, and contains:

- `AdvanceMode`;
- `AdvancePermitChapter`;
- `HasAdvanceHold`;
- `AdvanceHoldReason`.

The sidebar shows the auto/per-chapter approval status and the released chapter; while waiting, the input box prompts "enter revision feedback, or `/next` to release the next chapter". The notification kind is `advance_gate`.

## 10. Verification

Test coverage:

- atomic state transitions and compare-and-clear for the RunMeta mode, permit and hold;
- an unknown mode fails explicitly and does not rewrite RunMeta;
- pure-function recognition of forward new chapters versus rewrite/recovery;
- hold semantics for boundary, rewrites not drained, rewrites drained, and book completion;
- blocking without a permit, release by an exact permit, and an error for a wrong-chapter permit;
- the permit is kept during PendingCommit and consumed after a stable commit;
- pause when the completion marker conflicts with the checkpoint;
- no false alarm when permit and PendingRewrites interleave;
- an Engine end-to-end test proving that one permit stably yields exactly one new chapter;
- when the Gate has marked a pause but the old Engine goroutine is still exiting, `/next` explicitly rejects re-entry, and a later retry recovers idempotently under the same-chapter permit;
- hold-only, hold+dispatch and exit-race regressions.

## 11. Explicit non-goals

- Do not let the model decide the run mode or issue permits;
- Do not modify Route to fit the user confirmation policy;
- Do not turn rewrites, planning, review and structure maintenance into step-by-step confirmation;
- Do not add a generic PolicyEngine, StopCondition list or policy DSL;
- Do not provide pre-authorized multiple chapters or a permit queue;
- Do not keep the old pause model, compatibility fields, migration DTOs or dual-write paths;
- Do not silently degrade for unknown future modes.

If a new, repeatedly validated autonomy-boundary requirement appears in the future, the modes will be extended based on evidence; the current low regret cost is the future compatibility.
