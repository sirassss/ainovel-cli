# Context Management Guide

This document describes the current context management system of `ainovel-cli`, including:

- Why context management is needed
- Where context comes from
- How it is compressed, recovered and handed off at runtime
- The value, trigger conditions and applicable scenarios of each strategy
- Where to look first when something goes wrong

The goal is not to introduce abstract concepts, but to let a later maintainer open this one document and quickly understand the current implementation and the entry points for troubleshooting.

## 1. Design goals

The context management of this project is not for a general chat scenario but for novel creation. It has to solve several kinds of problems at once:

1. A long conversation exceeds the model's context window.
2. What novel creation needs to retain is not "the chat history itself" but structured narrative memory.
3. After compression the Writer must not lose character states, foreshadowing, chapter plans, style constraints, or pending review fixes.
4. When resuming writing, we cannot assume the model still "remembers what was discussed before"; we must rely first on persisted artifacts.

So we adopt a "layered memory" scheme:

- Short-term memory: the tail of recent messages that are kept
- Mid-term memory: the `ContextSummary` produced by compression
- Long-term memory: the structured artifacts in the project store
- Recovery memory: handoff / restore pack / novel_context

## 2. Overall architecture

### 2.1 Main layers

Context management is currently divided into four layers:

1. `agentcore/context`
   Responsible for the generic context budget, strategy pipeline, and compression/recovery framework.

2. `internal/tools/novel_context`
   Responsible for assembling the structured data of the novel project into context usable in the current turn.

3. `internal/orchestrator/store_summary_*`
   Responsible for the Writer-specific store-based fast compression.

4. `internal/orchestrator/writer_restore.go`
   Responsible for appending a post-compression restore pack after `FullSummary`, making sure the Writer can keep writing.

### 2.2 Data flow

There are two main context paths at runtime:

1. Normal working path
   - The Agent calls `novel_context`
   - `novel_context` reads chapter summaries, plans, characters, timeline and other data from the store
   - This data enters the current turn's prompt

2. Context-too-long path
   - `ContextManager` detects token pressure
   - Compresses in strategy order
   - Tries lightweight compression and store-based compression first
   - Goes to the LLM `FullSummary` only when that is not enough
   - Injects the restore pack after `FullSummary`

## 3. Key files

### 3.1 Generic context engine

- `../agentcore/context/strategy.go`
- `../agentcore/context/engine.go`
- `../agentcore/context/strategy_tool.go`
- `../agentcore/context/strategy_trim.go`
- `../agentcore/context/strategy_summary.go`
- `../agentcore/context/message.go`
- `../agentcore/context/summary_run.go`

Purpose:

- Defines `Strategy` / `ForceCompactionStrategy`
- Runs the strategy chain based on the budget
- Handles the representation of `ContextSummary` and its LLM conversion
- Handles the LLM summary compression of `FullSummary`

### 3.2 Project-side wiring

- `internal/orchestrator/agents.go`

Purpose:

- Assembles the Writer's `ContextManager` (the Coordinator was retired on 2026-07-12, see docs/engine-arbiter.md)
- Injects the extra `StoreSummaryCompact` into the Writer
- Configures the novel-specific `FullSummary` prompt for the Writer
- Configures `writerRestorePack` for the Writer

### 3.3 Project-side compression and recovery

- `internal/orchestrator/store_summary_strategy.go`
- `internal/orchestrator/store_summary_builder.go`
- `internal/orchestrator/writer_restore.go`

Purpose:

- Before the LLM summary, prefer fast compression using store data
- Uniformly builds the structured context needed for Writer compression and recovery
- After `FullSummary`, appends a purely in-memory restore message

### 3.4 Structured context assembly

- `internal/tools/novel_context.go`
- `internal/tools/novel_context_builders.go`
- `internal/domain/runtime.go`

Purpose:

- Defines `ContextProfile` / `MemoryPolicy`
- Decides how many chapter summaries and how much timeline to load, and whether to enable layered summaries
- Assembles chapters, characters, foreshadowing, timeline, review lessons, etc. from the store

### 3.5 Handoff and recovery

- `internal/orchestrator/handoff_policy.go`
- `internal/orchestrator/recovery_engine.go`

Purpose:

- In long-form / rewrite / review phases, prefer relying on handoff
- On recovery, splice the structured handoff pack into the prompt

### 3.6 Observability

- `internal/orchestrator/run.go`
- `internal/orchestrator/runtime.go`
- `internal/entry/tui/panels.go`

Purpose:

- Records context rewrite events
- Outputs the strategy name, token change, and number of messages retained
- Lets the TUI show whether the current context is `projected` or `compacted`

## 4. How the ContextManager is assembled

The Writer goes through `newContextManager` (rebuilt by the factory on every spawn according to the current model window). Before its retirement the Coordinator went through the same factory, and its configuration is kept in the table below as a historical comparison.

Key parameters of the current `contextManagerConfig`:

- `ContextWindow`
  The model's total context window.

- `ReserveTokens`
  Tokens reserved for the model's output.

- `KeepRecentTokens`
  The budget for the tail of recent messages to keep as far as possible during compression.

- `ToolMicrocompact`
  Configuration for tool-result microcompaction.

- `ExtraStrategies`
  Extra project-side compression strategies. Currently the Writer uses it to attach `StoreSummaryCompact`.

- `Summary`
  Configuration of `FullSummary`, including the custom prompt and the post-summary hook.

Current actual configuration values:

| Parameter | Writer | Coordinator (retired, historical comparison) |
|------|--------|-------------|
| ReserveTokens | 16,384 | 32,000 |
| KeepRecentTokens | 20,000 | 30,000 |
| CommitOnProject | false | true |
| IdleThreshold | 5min | none |
| ExtraStrategies | StoreSummaryCompact | none |
| Custom Summary Prompt | novel narrative version | default (coding-assistant version) |

Compression trigger threshold = `ContextWindow - ReserveTokens`. For example, with a 128K window the Writer triggers at ~112K.

The current Writer strategy pipeline order is:

1. `ToolResultMicrocompact`
2. `LightTrim`
3. `StoreSummaryCompact`
4. `FullSummary`

This order has a clear meaning:

- First clean up tool noise by the cheapest means
- Then trim over-long text blocks
- If the store data is sufficient, do zero-LLM structured compression directly
- Only at the end fall back to the LLM summary

## 5. What each strategy does

### 5.1 ToolResultMicrocompact

Implementation location:

- `../agentcore/context/strategy_tool.go`

Purpose:

- Clears historical `tool_result`
- Replaces old tool results with short placeholder text

Value:

- Tool return content is usually large and low in information density
- Many old tool results are only "process noise", not novel memory

Characteristics of the current Writer configuration:

- `IdleThreshold = 5m` is set

This means:

- If the most recent assistant message has been idle beyond the threshold
- The number of retained old tool results is reduced more aggressively

Applicable scenarios:

- After multiple rounds of `novel_context`
- After multiple rounds of read / check / draft tools

### 5.2 LightTrim

Implementation location:

- `../agentcore/context/strategy_trim.go`

Purpose:

- Truncates very long text blocks
- Keeps the head and the tail, replacing the middle with a placeholder

Value:

- Keeps the message structure unchanged
- Low cost
- Well suited to handling over-long original chapter text or large outputs

Applicable scenarios:

- A single message is too long, but a summary of the whole history is not needed yet

### 5.3 StoreSummaryCompact

Implementation location:

- `internal/orchestrator/store_summary_strategy.go`
- `internal/orchestrator/store_summary_builder.go`

Purpose:

- When the Writer context gets too long
- Prefer replacing old messages with the structured memory in the persisted store
- Does not call the LLM

It is not a conversation summary but a "structured memory replacement".

The core data it currently retains includes:

- Current progress
- Recent chapter summaries
- Current chapter plan
- Current chapter outline
- Current arc summary
- Current volume summary
- Character snapshots
- Active foreshadowing
- Pending review issues
- Recent timeline
- Style rules

Preconditions for triggering:

- The current chapter is greater than 1
- The store already has enough historical summaries
- And the current chapter has at least some working-state data
  - `chapter_plan` or `current_outline`

Value:

- Reduces the number of LLM compressions
- Avoids drift of key novel information during summarization
- Makes long-term memory rely first on facts persisted to disk rather than on chat history

Why only the Writer uses it:

- This is a novel business strategy, not a generic framework strategy
- The Editor / Architect have different context patterns (single-shot tasks, low window pressure)
- It is most reasonable to validate it first on the Writer, which needs continuous creative memory the most

### 5.4 FullSummary

Implementation location:

- `../agentcore/context/strategy_summary.go`
- `../agentcore/context/summary_run.go`

Purpose:

- When the layers above are not enough, use the model to generate a `ContextSummary`
- Keep the tail of recent messages
- Turn the earlier context into a structured checkpoint

How the Writer differs from the default coding assistant:

- The Writer uses a custom summary prompt
- The summary content is explicitly required to retain:
  - Current progress
  - Immediate character states
  - Active foreshadowing and clues
  - Review feedback and pending fixes
  - Style and pacing
  - Key decisions
  - Next steps
  - Key context

Value:

- It is the final fallback strategy
- Even when the store data is insufficient, continuity can still be maintained through the LLM

### 5.5 Circuit Breaker

Implementation location:

- `../agentcore/context/engine.go`

Purpose:

- When compression fails consecutively up to a threshold (default 3 times), skip compression for the current turn
- When skipping, still emit a `RewriteEvent` (`Reason = "circuit_breaker"`)
- The TUI shows the scope as "breaker skipped"
- Uses half-open mode: after skipping one turn it retries next time; success resets it, and another failure skips again

Why it is needed:

- The LLM summary may fail consecutively because of the network, model refusals, etc.
- Without a breaker, every turn's Project would attempt and fail, wasting API calls
- In a long-form writing session this waste accumulates

Troubleshooting:

- If the TUI keeps showing "breaker skipped", the LLM summary path has a problem
- Check the context rewrite events with `reason=circuit_breaker` in slog
- The breaker does not affect `StoreSummaryCompact` (it does not call the LLM)

### 5.6 Token estimation (CJK-aware)

Implementation location:

- `../agentcore/context/usage.go`

Purpose:

- All budget control and compression trigger timing depend on token estimation
- `estimateTextTokens` automatically detects whether the text is dominated by CJK characters
- CJK-dominated text: `runes × 1.5`
- ASCII-dominated text: `bytes / 4`

Why the standard `bytes/4` cannot be used:

- One Chinese character in UTF-8 = 3 bytes
- `bytes/4` would estimate one Chinese character as 0.75 tokens, while the actual value is about 1.5 tokens
- A 2x underestimate makes compression trigger far too late

Scope of impact:

- `EstimateTokens` (a single message)
- `EstimateTotal` (a message list)
- `EstimateContextTokens` (hybrid estimate: LLM-reported Usage + estimate of tail messages)
- Budget trimming in `store_summary_builder.go`

Note: a ToolCall's args are JSON (ASCII-dominated) and still use `bytes/4`, unaffected by the CJK adjustment.

## 6. Why the Writer has two sets of "post-compression memory"

The Writer currently has two paths that look similar but have different responsibilities:

### 6.1 StoreSummaryCompact

Responsibility:

- Directly replaces old messages during the compression process

Characteristics:

- Happens before `FullSummary`
- Zero LLM
- Replaces earlier history with the store

### 6.2 writerRestorePack

Implementation location:

- `internal/orchestrator/writer_restore.go`

Responsibility:

- Appends a restore message after `FullSummary`

Characteristics:

- Happens after the LLM compression
- Injected through `PostSummaryHook`
- Used to supplement the structured information the Writer must see to resume creative work

Why both are needed:

- `StoreSummaryCompact` does not always hit
  - For example in the first chapter or when the store data is insufficient
- No matter how well `FullSummary` is done, it may still omit precise information from the store
- So the restore pack acts as the last line of insurance

The two now share `store_summary_builder.go` to avoid drift in how they are measured.

## 7. What novel_context does

Implementation location:

- `internal/tools/novel_context.go`
- `internal/tools/novel_context_builders.go`

`novel_context` is not a compression strategy; it is the runtime "structured context assembler".

It divides the data in the store into several categories:

- `working_memory`
  - Current chapter plan
  - Current chapter outline
  - Recent chapter summaries
  - Timeline
  - checkpoint
  - previous tail

- `episodic_memory`
  - Character states
  - Relationship states
  - Recent state changes
  - Foreshadowing

- `reference_pack`
  - More stable settings and reference data

- `selected_memory`
  - A small amount of important memory selected for the current task

Value:

- It determines the structured novel context that is actually "fed to the model" every turn
- `StoreSummaryCompact` does not call it itself, but reuses the same kind of data sources and assembly approach

## 8. ContextProfile and MemoryPolicy

Implementation location:

- `internal/domain/runtime.go`

### 8.1 ContextProfile

Purpose:

- Decides the size of the loading window by the total number of chapters

Current rules:

- `<= 15` chapters
  - Summaries of the most recent `10` chapters
  - Timeline of the most recent `10` chapters

- `<= 50` chapters
  - Summaries of the most recent `5` chapters
  - Timeline of the most recent `8` chapters

- `> 50` chapters
  - Summaries of the most recent `3` chapters
  - Timeline of the most recent `5` chapters
  - Layered summaries enabled

Value:

- Controls the context size
- Avoids stuffing all the history into the prompt in long-form works

### 8.2 MemoryPolicy

Purpose:

- Writes the current context usage strategy out explicitly
- For `novel_context` to output
- For handoff / reminder / diagnostic logic to use

Key fields:

- `SummaryWindow`
- `TimelineWindow`
- `LayeredSummaries`
- `SummaryStrategy`
- `HandoffPreferred`
- `ReadOnlyThreshold`

Value:

- Turns "how the system should currently use memory" from implicit logic into an explicit runtime policy

## 9. What handoff does

Implementation location:

- `internal/orchestrator/handoff_policy.go`

When a work enters a longer, more complex phase that depends more on structured artifacts, the system leans toward handoff.

The handoff pack records:

- Current phase and flow
- Next chapter position
- Recent commits
- Recent reviews
- Recent summaries
- Current memory policy
- Recovery guidance text

Value:

- Does not depend on chat history when recovering from an interruption
- In rewrite, review and long-form scenarios, relies first on structured artifacts

## 10. Observability and troubleshooting

### 10.1 Context rewrite events

Implementation location:

- `internal/orchestrator/run.go`

Every context rewrite is output through `contextRewriteCallback`:

- `reason`
- `strategy`
- `committed`
- `tokens_before`
- `tokens_after`
- `messages_before`
- `messages_after`
- `compacted_count`
- `kept_count`
- `split_turn`
- `incremental`
- `summary_runes`
- `duration_ms`

This goes into all of:

- `slog`
- the runtime boundary queue
- the TUI `COMPACT` event

### 10.2 What can be seen in the TUI

The TUI shows:

- The current context tokens (with a health gradient color)
- The context window
- The current context scope (including "breaker skipped")
- The name of the most recent strategy
- The number of summaries

Meaning of the context percentage colors (implemented in `internal/entry/tui/layout.go`):

| Color | Condition | Meaning |
|------|------|------|
| Green | < 70% | Plenty of headroom, far from the compression threshold |
| Yellow | 70-85% | Approaching the compression threshold |
| Red | > 85% | About to compress or compressing |

Display labels for Scope:

| Scope | Display | Meaning |
|-------|------|------|
| baseline | Baseline | Normal state |
| projected | Projected | Temporary compression preview |
| compacted | Committed | Compression has taken effect |
| recovered | Recovered | Recovery after an overflow |
| skipped | Breaker skipped | Compression was skipped by the circuit breaker |

Value:

- Lets you quickly judge the health of the current context
- At yellow/red you can expect compression to happen soon
- Seeing "breaker skipped" means the LLM summary path has a problem

### 10.3 Where to look first when something goes wrong

#### Scenario 1: The Writer loses the chapter plan after compression

Look first at:

- Whether `novel_context` stably injects `chapter_plan`
- Whether `store_summary_builder.go` gets `chapterPlan`
- Whether `writerRestorePack` is refreshed

Key files:

- `internal/tools/novel_context_builders.go`
- `internal/orchestrator/store_summary_builder.go`
- `internal/orchestrator/session.go`

#### Scenario 2: Character states / foreshadowing are lost after compression

Look first at:

- `LoadLatestSnapshots`
- `LoadActiveForeshadow`
- `store_summary_builder.go`
- Whether the Writer summary prompt has been overridden

#### Scenario 3: Compression is frequent but never hits store_summary

Look first at:

- Whether the current chapter is `<= 1`
- Whether recent summaries / arc / volume summary already exist
- Whether `chapter_plan` or `current_outline` exists
- Whether the final `writer.Context.Strategy` recorded is `full_summary`

#### Scenario 4: Context is insufficient after recovery

Look first at:

- Whether the handoff was generated
- Whether the restore pack is refreshed
- Whether the recovery prompt injects the handoff

#### Scenario 5: Too many tool results bloat the context

Look first at:

- Whether `ToolResultMicrocompact` hits
- Whether `IdleThreshold` takes effect

## 11. Trade-offs of the current implementation

### Directions explicitly upheld

1. Do not stuff novel business logic into `agentcore`
2. Rely first on the structured store, not on chat history
3. The Writer uses a dedicated novel summary prompt
4. Share the builder between compression and recovery as much as possible, to avoid drift in how they are measured

### Limitations deliberately kept for now

1. `StoreSummaryCompact` is used only by the Writer
2. The first chapter does not hit store-based compact
3. When the store data is insufficient it still falls back to `FullSummary`
4. `writerRestorePack` is additive compensation and does not replace `FullSummary`

These limitations are not defects but boundaries drawn at this stage to control complexity.

## 12. One-sentence summary

The context management of this project is not as simple as "compressing a long conversation shorter", but rather:

`Maintain continuity first with structured novel memory, and let the LLM summarize the conversation only when necessary; and in all three of compression, recovery and handoff, rely as much as possible on the same set of persisted artifacts.`

If you want to change this system later, first hold on to the following three points:

1. Do not let the Writer's key memory depend only on chat history again.
2. Do not let `store_summary` and `writer_restore` diverge in how they are measured.
3. When a continuity problem appears, first check whether the structured artifacts entered the context, and only then decide whether to change the prompt.
