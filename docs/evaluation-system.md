# ainovel-cli Evaluation System

> The evaluation system is not a new set of check scripts. It takes the project's **existing fact diagnoser (`diag`), book-wide style statistics (`stylestat`) and seven-dimension native review (`ReviewEntry`) as the evaluators**, and wraps them in an offline batch harness. There is one definition of each fact, so the two sides can no longer drift apart.

---

## 0. Why a redesign is needed

Stability has been solved: a long-form novel of 235 chapters / 1.27 million characters was written in a single run, and the rolling-planning closed loop holds (see `architecture.md` §9.1). The bottleneck has shifted: **quality must be iterable**:

- After changing a prompt, is the flow still stable? Are the tool chain, state progression and persisted facts still correct?
- Have the prose, outline and review quality really improved, or did this one run just happen to draw a good random result?
- In a long-form book, do characters, the timeline, foreshadowing and context stay reliable?
- Has **book-wide style fossilization** (sentence-pattern tics dozens of times per chapter, chapter endings sharing one shape, verbatim repetition across chapters) got better or worse? This is the real culprit behind the 6.5/10 measured on 196 chapters, and single-chapter review is blind to it by nature.

Today these judgments rest on "gut feeling + manual spot reads". The evaluation system must turn a prompt change from gut feeling into an engineering process with **regression, evidence and manual sample reading**.

But this project neither needs nor should copy the generic industry eval platform (dataset / experiment / scorer / database / Web UI). The reason is simple: **the core of those capabilities, deterministic checks and quality signals, already exists in the project, written in Go and sharing one fact model with the runtime.**

---

## 1. Core thesis: the evaluators already exist

Of the four kinds of evaluators in an evaluation system, three are already implemented in the codebase; they were just never invoked as "evaluators":

| Evaluator | Existing project capability | Entry point | Output |
|---|---|---|---|
| **Deterministic fact diagnosis** | A set of artifact rules + runtime rules in `internal/diag` | `diag.Diagnose(store)` | `Report{Stats, Findings}`, each Finding carries Severity/Evidence |
| **Book-wide style regression** | `internal/stylestat` | `stylestat.Compute(input)` | Per-chapter average of sentence patterns, repeated sentences across chapters, share of short chapter-ending sentences, title format mixing |
| **Quality verdict (rubric)** | A versioned rubric (initially derived from the seven dimensions of `editor.md`) | LLM Judge (A/B against a fixed yardstick) | consistency/character/pacing/continuity/foreshadow/hook/aesthetic |
| **Behavior-redacted export** | `internal/diag` export | `diag.WriteExport(store, rep, rc)` | A behavior skeleton, for manual sample reading and archiving |

`diag.Analyze(s *store.Store)` takes a Store and produces a complete `Report`, so **it can already run offline on any output directory**. `stylestat.Compute` is a pure function. This means the evaluation system does not need to re-implement "was the chapter persisted, did progress advance, does the checkpoint exist, is there leftover pending, did the flow fall into a dead loop". diag does all of that, and each rule maps to a real pitfall already hit in practice (`PhaseFlowMismatch`, `OrphanedSteer`, `OutlineExhausted`, and `repeatedErrors`/`stuckStep` correspond to historical failures such as idleResume / outline-exhaustion livelock / tool calls printed as text).

> **The evaluation system's job is not to build checks, but to: drive in batch + run the existing evaluators on the output + map Findings/statistics to gates + aggregate the report.**

---

## 2. Design principles

### 2.1 The evaluator is the diagnoser; never rebuild deterministic checks

Deterministic checks only call `diag.Diagnose`; the evaluation layer does not re-parse `progress.json` / `checkpoints.jsonl` / `sessions/*.jsonl`. The reason is this project's DRY iron rule: **"what is a legal state" may have only one definition.** If the evaluation re-parsed checkpoints in Python to decide whether a commit is missing, there would be two definitions of "commit complete"; when the runtime changed a diag rule the evaluation would not follow, and the gate would immediately become distorted.

→ The evaluation harness is written in **Go** and calls `diag` and `stylestat` in-process, sharing `internal/domain` and `internal/store` with the runtime. This is the most fundamental difference between this design and the previous version.

### 2.2 Book-wide style regression is the first quality signal

A single-chapter LLM Judge sees every chapter as "normal", yet the bottleneck is precisely cross-chapter fossilization. So the **deterministic backbone of quality regression is `stylestat`, not the LLM Judge**.

**Precondition: `stylestat.Compute` returns nil directly for fewer than 5 chapters** (`minChapters=5` in `stylestat.go`; with too small a sample the frequencies are meaningless). Therefore style regression **only takes effect at the Quality / Longform tiers with ≥5 chapters**; a 1-chapter Smoke gets no style signal, which determines the cost and default strategy below. The metrics include:

- the per-chapter average count of the variant's sentence patterns vs baseline (`patterns[].per_chapter`)
- the share of chapters closing on a short sentence (`ending.short_ratio` approaching 1 is a disease)
- the number of verbatim repeated sentences across chapters (`repeated_sentences`)
- title format mixing (`title_formats`)
- the opening time-word rate (`opening_time_rate`)

These are zero-LLM-cost, deterministic, and aimed squarely at the quality bottleneck. **The LLM Judge is a supplement; the stylestat delta is the main line.**

### 2.3 The LLM Judge aligns to the seven-dimension native rubric and does not start its own

The Judge invents no new scoring dimensions: its dimensions are strictly the seven items of `domain.DimensionScore`, used for baseline/variant comparison.

**But the rubric must be versioned and pinnable**, stored as a snapshot in `evals/rubrics/*.json`, not read live from `editor.md` at runtime. The reason: when the thing under test is `editor.md` itself, if the judge changed together with `editor.md`, the evaluation baseline would drift; a judge sharing its source with the subject under test makes it impossible to tell whether a change to the editor is good or bad. So the rubric is initially **derived** from the editor's seven dimensions (guaranteeing the same definition), and afterwards **evolves independently with an explicit version bump**; the report records which rubric version was used.

### 2.4 Deterministic Findings decide the gate; the LLM and humans only give quality verdicts

This aligns with the architecture iron rule "statistics belong to code, rulings belong to the LLM":

- **Only deterministic evidence can block a merge**: `SevCritical` Findings from `diag`, and failures of the contract assertions declared by the case.
- **The LLM Judge and manual sample reading produce warnings and ranking leads**; they do not on their own decide a merge.
- In one sentence: `Finding.Severity` maps directly to the gate level, with no new severity taxonomy introduced.

### 2.5 Evaluation only observes and never intervenes in control flow

The evaluation reuses `diag` but **discards diag's `Action` and `Planner`**, which belong to runtime control flow. In the evaluation context `diag.Report` contributes only `Stats` and `Findings`; Actions are always ignored. The evaluation does not auto-fix prompts, auto-roll-back or resume. This extends the observer discipline (`architecture.md` §2.3) into the evaluation context.

### 2.6 Failures are exposed explicitly

No mocking success, no swallowed errors, no templates pretending to pass. If any of the model, tools, config, filesystem, parsing or judge fails, the report records the reason explicitly. **The failure itself is the evaluation result**: if a case crashes, the gate is FAIL, not "skipped".

### 2.7 Validate only one variable at a time

The hard constraints of A/B: same request, same config, same model/provider, same style, isolated output directories. Baseline = the current official prompt; Variant = replace only the prompt file being validated this time. Do not change Writer/Architect/Editor/Arbiter in the same experiment.

---

## 3. Architecture overview

```text
[Cases]  evals/cases/*.json —— a set of fact-layer assertions, not generic dataset rows
   │
[Runner]  internal/eval —— assembles and drives host in-process (stops at the chapter cap); bundle.Prompts in-memory override makes the variant
   │       baseline run ┐
   │       variant  run ┘  each with its own isolated output directory
   ▼
[Collectors]  collect from each output directory:
   ├── diag.Diagnose(store)      → Report{Stats, Findings}      (facts + runtime)
   ├── stylestat.Compute(input)  → book-wide style statistics   (quality regression backbone)
   ├── case contract assertions  → expected checkpoint/phase/tool contracts (what diag does not cover)
   ├── usage / cost / token      → read from meta/usage.json
   └── tool_calls                → read real tool calls from meta/sessions/*.jsonl
   ▼
[Graders]
   ├── deterministic gate: Finding.Severity + contract assertions → hard_fail / regression
   ├── stylestat delta: style metric difference, variant vs baseline
   ├── LLM Judge (optional): seven-dimension rubric A/B comparison
   └── Human: manual reading of the baseline/variant artifacts
   ▼
[Report]  report.json (machine-readable) + report.md (human-readable) + behavior-redacted export
   └── Gate: PASS / WARN / FAIL
```

Dependency direction: `eval → host → agents → tools → store → domain`, reusing `diag` / `stylestat` laterally. The evaluation layer **does not depend back** on runtime control flow; it only reads the Store and the read-only evaluators.

> **The current implementation covers the deterministic main line**: without `--variant` the run is `mode=single`; with `--variant` it is `mode=ab`, which runs baseline and variant in isolation for the same case and generates a delta. Collectors are wired to `diag.Diagnose`, the case contract, `stylestat.Compute`, `meta/usage.json` and the session tool call counts; Graders are wired to the deterministic gate, the baseline/variant diag delta, the cost/token/tool call delta and the stylestat delta. The Runner assembles via `host.New` directly and carries its own chapter-cap stop, **and does not reuse `headless.Run`, which has no chapter cap**. The LLM Judge and Human remain optional later layers and do not take part in the current deterministic gate.

---

## 4. Why Go in-process, not shell + Python

| Dimension | shell copies source + Python parses (old path) | Go in-process (this design) |
|---|---|---|
| Deterministic checks | Python re-parses JSON, two definitions alongside the diag rules | Call `diag.Diagnose(store)` directly, one definition |
| Variant switching | Copy the whole source tree + re-run `go build` for two binaries | After the `bundle.OverridePrompt(...)` in-memory override, assemble the host: zero copy, zero recompile |
| Style regression | Must rewrite stylestat's Chinese sentence splitting in Python | Call `stylestat.Compute` directly |
| Judge rubric | Dimensions scattered in Python | Reuse `domain.DimensionScore`, same source as production |
| Drift risk | High: the runtime changes the fact model, the evaluation does not follow | Low: a field change surfaces at compile time |

The old `prompt_ab.sh` had to copy the source and recompile because the prompts are embedded in the binary (`go:embed`). But `assets.Bundle.Prompts` is a plain struct, so **the runner can make a variant by changing one field in memory**, with no need to copy the source at all. This is the biggest simplification that comes for free from writing the harness in Go.

> **Implementation constraint**: `assets.Load` uniformly appends the `WithSimulationGuidance` suffix to the Worker prompts (architect/writer/editor) via `loadPrompts`. If a variant only stuffed bare text into `bundle.Prompts.Writer`, it would lose the voice-simulation profile suffix that the baseline has, and the A/B would not be equivalent.
>
> The correct approach is to override through `assets.OverridePrompt`, which internally goes through exactly the same wrapping as `Load`; eval does not duplicate the wrapping logic.

> The previous version of this document kept `prompt_ab.sh` / `prompt_ab_report.py` and "gradually extracted capabilities". This design abandons that path: the problems they solve (isolated runs + metric aggregation) are a subset inside the in-process Go harness, and forcing reuse would only saddle us with interface glue across three languages (shell/Python/Go). **The Go harness is the only main path**; the current Go harness already covers baseline/variant isolated runs, repeat aggregation and deterministic deltas. The old scripts (`scripts/prompt_ab.sh`, `scripts/prompt_ab_report.py`) and their operating manual `docs/prompt-ab.md` were deleted together when this design landed and are no longer kept.

---

## 5. Case Manifest

A Case is the smallest unit of evaluation input, and also a set of **fact-layer assertions**. It is described in JSON so that rules are not scattered across command-line arguments.

```jsonc
{
  "id": "writer_first_chapter_xianxia",
  "category": "smoke",
  "role": "writer",
  "description": "Verify the quality of the Writer's first chapter prose and the stability of the tool chain",
  "prompt": "写一本修仙长篇，主角从边城杂役起步，靠异常记忆破解宗门旧案卷入长生局。", // <!-- han:keep --> English gloss: "Write a long-form xianxia novel; the protagonist starts as a menial servant in a border town and, through an anomalous memory, cracks an old sect case and is drawn into the immortality game."
  "style": "fantasy",
  "max_chapters": 1,
  "target_prompts": ["writer.md"],
  "rubric": "writer_chapter",

  "expect": {
    "phase": "writing",
    "min_completed_chapters": 1,
    "required_checkpoints": ["chapter:1:plan", "chapter:1:draft", "chapter:1:commit"],
    "no_pending": ["pending_commit", "pending_steer"]
  },

  "gate": {
    "max_severity": "warning",
    "max_cost_delta_ratio": 0.3,
    "max_tool_call_delta_ratio": 0.3,
    "stylestat_regression": "warn"
  }
}
```

**Field semantics**:

- `expect`: case-level contract assertions, **declaring only the expectations that diag's generic rules cannot cover and that are strongly tied to this case** (for example "this smoke case must produce exactly chapter:1:commit"). Generic checks such as "no leftover pending / phase-flow consistent / no chapter gaps" are left to diag and are not re-declared in the case.
- `category`: the evaluation tier, one of `smoke` / `workflow` / `quality` / `longform` / `recovery` / `steering`. It decides which gate set runs and whether stylestat/Judge is on by default.
- `role`: the role under test, one of `writer` / `architect` / `editor`. It is orthogonal to `category`: the tier decides "how deep to validate", the role decides "which Worker to validate". The Workflow tier selects the assertion set by `role`.
- `max_severity`: the highest severity a diag Finding may have. Exceeding it is a hard fail.
- `gate.max_cost_delta_ratio` / `gate.max_tool_call_delta_ratio`: the threshold for how much the variant's cost and tool calls may grow relative to baseline; the default is `0.3` when omitted, an explicit `0` means no growth is allowed, and a negative number turns that delta gate off.
- `rubric`: which versioned LLM Judge scoring sheet to enable. If absent, the Judge does not run.
- `gate.stylestat_regression`: `block` / `warn` / `off`, controlling whether style regression blocks (only effective for cases with ≥5 chapters).

---

## 6. Evaluation tiers

Each tier states clearly **which existing evaluator it uses**, to avoid "the evaluation layer writing the judgment all over again".

### 6.1 Smoke (must run on every prompt change; the minimal set)

It only judges whether the system can still run stably, not the prose. 1 chapter / the planning phase is enough to expose problems.

| case | Goal | Main evaluator |
|---|---|---|
| `writer_first_chapter` | Writer completes the first chapter and commits | `expect.required_checkpoints` + diag |
| `architect_short` | Short-form planning saves the full premise/outline/characters/world_rules | the foundation check from the same source as diag `MissingSummaries` + `expect` |
| `architect_long` | Long-form planning saves layered_outline/compass and expands the first arc | diag `OutlineExhausted`/`CompassDrift` + `expect` |
| `editor_review` | At the review point the Editor saves a review (all seven dimensions present) | `ReviewEntry` field assertions |

Cost: 1 chapter × baseline+variant, seconds to minutes, no Judge, no stylestat (fewer than 5 chapters, so `Compute` returns nil). CI runs only this tier by default.

### 6.2 Workflow (verify that Agent behavior conforms to the architecture contract)

**Key discipline: assert the contract, not the exact tool sequence.** The architecture bets on the LLM deciding the flow autonomously (`architecture.md` §2.1); hard-coding the tool order would reintroduce, at the evaluation layer, the "hard-coding for LLM behavior" that §10.13 rejects. So only **inevitable facts** are asserted here:

- Writer: the `chapter:N:commit` checkpoint exists; after the commit the subagent ends its turn (no overlong trailing prose); the draft checkpoint precedes the commit. It does **not** assert "this exact order novel_context→read_chapter→plan→draft→check→commit is required".
- Architect: during writing the outline only grows and is never fully overwritten (the checkpoints of `expand_arc`/`append_volume`, with no second full `layered_outline` write); after expansion the flat outline and the layered outline have the same chapter count.
- Editor: `ReviewEntry.Verdict` is legal (accept/polish/rewrite); rewrite/polish must produce affected chapters; at an arc end there is an `arc_summary` checkpoint and at a volume end a `volume_summary` checkpoint.
- Engine dispatch: the Route instruction matches the Worker actually run (read from the session trace, with diag `repeatedErrors` as the loop backstop); semantic rulings are checked against `meta/decisions.jsonl`.

Most of these can be covered directly by diag rules + checkpoint assertions; a few (trailing prose after the commit) need one lightweight trace check added in the collector.

### 6.3 Quality (runs only after the flow passes, to evaluate content quality)

Two legs:

1. **stylestat delta (deterministic, main line)**: the style metric difference, variant vs baseline. This is the hard evidence for quality regression. **The case is required to run a full ≥5 chapters** (otherwise `Compute` returns nil and this item is marked `insufficient_sample`), so a pure 1-chapter Quality case gets no style regression and `max_chapters` must be set to 5 or more.
2. **LLM Judge (auxiliary)**: seven-dimension rubric A/B (see §8).

Only cases that pass §6.1/§6.2 enter Quality: if the flow is wrong, there is no point talking about quality.

### 6.4 Longform & Recovery (major changes / nightly)

Not necessarily run every time. It covers long-form stability and recovery capability, which is exactly the home ground of diag's runtime rules and context rules:

- Continuous writing of the first 3 / 5 chapters → diag `GhostCharacter`/`TimelineGaps`/`RelationshipStagnation`/`ChapterGaps` + stylestat cross-chapter repetition.
- Arc-end review + next-arc expansion → `OutlineExhausted`/`StaleForeshadow`/`CompassDrift`.
- User intervention mid-run (steering case) → whether user_rules land in `meta/user_rules.json` and whether later chapters obey them.
- Crash recovery: run to the draft of chapter N then kill → Resume → diag confirms that `checkpoints.jsonl` has no duplicate step, an already-persisted draft is not rewritten, and `pending_commit` is finally cleared.
- Tool-call bloat / cost anomalies → diag `repeatedErrors`/`stuckStep`/`streamIdleStorm` + usage delta.

---

## 7. Deterministic gate

The gate level is derived directly from the **Severity of diag Findings** + the **case contract assertions**, with no separate taxonomy.

### 7.1 Hard Fail (blocks the merge)

- Process panic / headless returns an error.
- diag produces a `SevCritical` Finding (`InvalidPendingRewrites` / `PhaseFlowMismatch`, etc.).
- A case `expect` contract assertion fails: missing commit checkpoint, phase not reached, declared pending not cleared.
- The variant has more errors / critical Findings than baseline (regressed to worse).

### 7.2 Regression (warning by default; whether it blocks is decided by the case gate)

- diag adds `SevWarning` Findings (variant has more than baseline).
- The growth in tool calls / cost / input tokens / output tokens exceeds the case threshold (default 30%).
- **stylestat regression**: the per-chapter average of sentence patterns rises, the share of short chapter-ending sentences rises, repeated sentences across chapters increase, or title mixing appears; `gate.stylestat_regression` decides warn/block.
- Chapter word count is below 60% or above 180% of baseline (the same-source threshold as diag `WordCountAnomaly`).

### 7.3 Quality Gate (manual backstop)

- The LLM Judge is only auxiliary and for ranking.
- If the Judge deems the variant clearly worse → manual sample reading must confirm it.
- If manual sample reading concludes there is degradation → block.
- If the Judge deems the variant better but there is a deterministic hard fail → still block.

### 7.4 Recommended merge conditions

Routine prompt changes: all Smoke passes + all Workflow passes for the target role (the 1-chapter Smoke contains no style regression; if a ≥5-chapter Quality case was run, stylestat shows no obvious regression).
Major changes: add 2-3 Quality cases + 1-2 Longform cases + manual sample reading.

---

## 8. LLM Judge

The Judge is a quality aid, essentially **an offline baseline/variant comparison using a versioned rubric (initially derived from the seven dimensions of editor.md)**. The rubric is a fixed yardstick that evolves independently of the production `editor.md` (reason in §2.3), and the report records the rubric version used.

### 8.1 Input (keep the size under control; never stuff in the whole book)

- The user's original request + the current chapter outline/contract.
- The prose of the **same chapter** from baseline and variant.
- Summaries of the last 1-2 chapters + a character state summary (read from the store).
- The relevant stylestat slice for that chapter (so the Judge sees facts such as "this sentence is repeated 7 times across the book").

### 8.2 Output (structured, aligned to the seven dimensions)

```json
{
  "scores": {
    "consistency": 8, "character": 7, "pacing": 8, "continuity": 8,
    "foreshadow": 7, "hook": 7, "aesthetic": 6
  },
  "winner": "variant",
  "confidence": "medium",
  "reasons": ["variant advances the action in a more focused way", "baseline recaps earlier events more heavily"],
  "risks": ["variant sets up supporting characters' motivation slightly less"]
}
```

- The dimensions are strictly the seven items of `domain.DimensionScore`, each scored 0-10.
- `winner` ∈ baseline/variant/tie; `confidence` ∈ low/medium/high.
- Each `reasons`/`risks` entry is ≤ 80 characters, and quotations from the text must be short.

### 8.3 Boundaries

The Judge **must not**: decide whether the flow passes, modify artifacts, auto-edit prompts, serve as the sole basis for a merge, or generate long excerpts of the original text.
The Judge **may**: rank items for manual review, flag obvious degradation, summarize A/B differences, and expose side effects of a prompt change.

---

## 9. Report

Each experiment generates `report.json` (machine-readable, can regenerate the markdown) + `report.md` (human-readable) + `artifacts/{case_id}/{baseline,variant}/` (raw artifacts). With `--repeat N` the path is `artifacts/{case_id}/rN/{baseline,variant}/`.

### 9.1 Metric deltas

The report shows the variant's difference relative to baseline, with the absolute value and the ratio side by side:

```text
completed: baseline=5 variant=5   ← with ≥5 chapters the style metrics are meaningful
tool_calls: baseline=12 variant=16  +4 (+33.3%)
cost_usd: baseline=0.42 variant=0.55  +0.13 (+31.0%)
output_tokens: baseline=8200 variant=9100  +900 (+11.0%)
critical_findings: baseline=0 variant=0
warning_findings: baseline=1 variant=2  +1
stylestat.pattern_top_per_chapter: baseline=3.1 variant=5.4  +2.3   ← style regression
stylestat.ending_short_ratio: baseline=0.42 variant=0.71  +0.29     ← chapter-ending uniformity worsens
```

### 9.2 Repeat summary

With `--repeat N` it does not look only at the last run; the current implementation shows the pass rate, the number of hard fails, the number of warnings, and the min/avg/max of cost/tool_calls. The winner distribution will be added once the Judge is wired in, to avoid mixing model-judge noise into the default deterministic report.

```text
writer_first_chapter_xianxia repeat=3
- pass_rate: 3/3
- cost_usd: avg=0.41 min=0.38 max=0.44
- tool_calls: avg=13 min=12 max=15
- stylestat.pattern_top_per_chapter: avg delta=+0.4 (no significant regression)
```

### 9.3 Minimum viable report

```text
Gate: FAIL

Hard Fail:
- writer_first_chapter_xianxia: missing checkpoint chapter:1:commit

Warnings:
- writer_dialogue_density: tool_calls +35%
- writer_anti_ai_tone: ending_short_ratio +0.28 (style regression)

Quality:
- writer_anti_ai_tone: judge prefers variant, confidence=medium

Artifacts:
- workspace/evals/20260629-120000/report.json
```

---

## 10. Directory layout and commands

```text
internal/eval/
  case.go        Case manifest struct + loading
  eval.go        CLI orchestration: single / A/B / repeat
  runner.go      assembles and drives host (stops at the chapter cap + drain to Done), bundle.OverridePrompt in-memory override
  collect.go     runs diag.Diagnose + stylestat.Compute + usage/tool_calls + contract assertions on the output directory
  grade.go       Finding→gate mapping + baseline/variant delta + stylestat gate decision
  report.go      report.json + report.md

cmd/ainovel-cli  entry of the eval subcommand

evals/
  cases/         smoke/ workflow/ quality/ longform/ recovery/ steering/
  rubrics/       writer_chapter.json / architect_outline.json / editor_review.json
  variants/      writer-anti-ai-tone/writer.md etc. (each directory holds only the prompts to be replaced)
  reports/       archive of historical reports
```

Commands:

```bash
# batch over multiple cases (CI runs only smoke by default, judge off)
ainovel-cli eval --cases evals/cases/smoke \
  --variant evals/variants/writer-anti-ai-tone \
  --out workspace/evals/writer-anti-ai-tone --ci
```

**Flags implemented in this phase**: `--cases` (a directory or a single manifest), `--variant` (variant prompt directory; once given, the baseline+variant A/B runs automatically), `--repeat N` (run each case N times), `--config`, `--out`, `--max-chapters N` (overrides the case default), `--timeout` (per-case wall-clock cap), `--ci` (suppresses per-event output; a non-zero exit code means hard fail, with or without this flag).

**Planned (not yet implemented; do not use on the command line, or you get an undefined-flag error)**: `--judge`/`--no-judge` (Phase 3 LLM Judge). For major prompt changes, for now use the deterministic A/B + repeat:

```bash
# major prompt change: A/B + repeat to reduce randomness
ainovel-cli eval --cases evals/cases/quality \
  --variant evals/variants/writer-anti-ai-tone \
  --repeat 3 --ci
```

---

## 11. What is explicitly not done

Violating any of these means the evaluation has strayed from its positioning.

1. **Do not duplicate diag's generic diagnostic logic in the evaluation layer** — generic judgments (leftover pending, phase/flow consistency, chapter gaps, dead loops) always go through `diag`, so fact judgments have only one definition. Case-level contract assertions (`expect.required_checkpoints`, etc.) may read the `store`/checkpoint API directly, but only as **thin assertions**: verifying the specific expectations strongly tied to this case, and never rewriting the generic rules diag already has.
2. **Do not re-implement deterministic rules** — diag already has a set of artifact rules + runtime rules. If a rule is missing, add it in diag; the evaluation layer only consumes.
3. **Do not rewrite stylestat's Chinese style logic in Python** — call the Go package directly.
4. **Do not let the LLM Judge decide whether the flow passes** — the gate recognizes only deterministic evidence.
5. **Do not let the evaluation intervene in control flow** — discard diag's Action/Planner; no auto-fixing prompts, no rollback, no resume, no publishing.
6. **Do not assert exact tool-call sequences** — only assert the contract (a commit happened, the checkpoint exists), protecting the "LLM-driven flow" bet.
7. **Do not introduce a database / Web UI / online evaluation platform** — what this stage needs is repeatable, practical, low-cost local regression.
8. **Do not copy the source and recompile to make a variant** — override `bundle.Prompts` in memory.
9. **Do not mock success or swallow errors** — a failure in any step is recorded explicitly, and a crashed case is FAIL.
10. **Cases do not change frequently with prompts** — a case is a stable test set; changing the case to make a variant pass is cheating.

---

## 12. Phased rollout

### Phase 1 · Runner + deterministic gate (MVP, prove the hypothesis first)

- `internal/eval`: Case struct + runner (in-process headless + bundle override) + collect (calls `diag.Diagnose`) + grade (Finding→gate + `expect` contract).
- Put 3-4 cases in `evals/cases/smoke/`.
- The report first emits `report.json` + a minimal markdown.

**Acceptance**: one command runs the smoke set to completion; the gate **can catch** a Writer skipping the commit, leftover pending, a missing checkpoint, and a phase mismatch (diag could already check all of these; what is being verified is that the harness wires them correctly).

### Phase 2 · A/B + repeat + stylestat regression (implemented)

- `--variant` runs baseline and variant automatically and outputs isolated artifacts.
- `--repeat N` aggregates pass rate, hard fail runs, warning runs and cost/tool_calls min/avg/max.
- collect adds `stylestat.Compute`, and grade adds the style delta.
- The report shows the baseline-variant comparison of per-chapter sentence patterns / share of short chapter-ending sentences / repeated sentences across chapters / title mixing.

**Acceptance**: with a case of ≥5 chapters and a variant that "worsens the sentence-pattern tic", style regression can flag a warning; a case with too few chapters explicitly shows `insufficient_sample` instead of being wrongly judged as passing.

### Phase 3 · LLM Judge

- `evals/rubrics/` + `judge.go`, seven-dimension rubric A/B.
- Judge failure (illegal JSON) → the report records the failure, without affecting the deterministic result.

**Acceptance**: the Judge output goes into json+md and does not pollute the deterministic gate.

### Phase 4 · Longform & Recovery

- 3-5 consecutive chapters / arc-end review / user intervention / pending_commit replay / context compaction stress cases.
- Reuse the diag context + runtime rules.

**Acceptance**: able to find a duplicated timeline, leftover pending, a missing arc-end summary, and tool loops.

---

## 13. Case maintenance guidelines

- **Keep the count restrained**: Smoke 3-5, Workflow 3-5 per role, Quality 2-4, Longform/Recovery 2-3 each. Too many and nobody wants to run them.
- **Good cases**: short and clear input, covering a real risk, exposing problems within few chapters, not depending on the model generating fixed sentences, not specifying style preferences too finely.
- **Bad cases**: overlong input, multiple goals at once, needing dozens of chapters before a judgment is possible, resting only on subjective feeling.
- **Variant naming**: `writer-anti-ai-tone` / `architect-rolling-outline` / `editor-strict-review`; each directory holds only the prompts to be replaced.

---

## 14. Risks and boundaries

- **Model randomness**: the same prompt varies across runs. For important changes, look at the trend with `--repeat 3`.
- **Cost**: the Judge and longform burn money. Locally the default runs only **smoke** (1 chapter × baseline+variant, deterministic diag gate, no Judge, no stylestat); **stylestat is enabled only for Quality/Longform with ≥5 chapters** (smoke has too few chapters, `Compute` returns nil, and the report marks `insufficient_sample`); the full suite is reserved for major changes.
- **Judge bias**: the Judge is also a model; it prefers neat, explanatory text, which is not necessarily a good-reading novel. So it serves only as an aid, and stylestat is the deterministic main line.
- **Over-metricization**: word count / tool-call count / cost / style statistics are all signals, not goals. Whether a stylestat number is a disease is for a human to decide by genre, and **the thresholds are not hard-coded** (consistent with editor.md).
- **No online auto-rollback**: this is an offline regression tool; it is not responsible for automatically editing prompts or releasing online.

---

## 15. Summary

The value of this evaluation system is not to judge literary quality automatically, but to turn a prompt change from "gut feeling" into one with "regression, evidence and manual sample reading".

Its fundamental difference from the previous design is a single sentence: **the evaluators are already in the codebase.** `diag` is the deterministic fact diagnoser, `stylestat` is the book-wide style regression checker, and the seven dimensions of `ReviewEntry` are the native rubric. What the evaluation system has to do is a thin Go harness: drive in batch, collect, map Findings and statistics to gates, and aggregate the report. It is not to rewrite these fact judgments in another language.

One definition of each fact, never drifting. This is exactly the discipline this project upholds from architecture to evaluation: **minimal harness, maximum reuse, determinism belongs to code, rulings belong to the LLM and humans.**

---

## 16. References

The generic structure of industry LLM evals (dataset / experiment / scorer / trace / regression gate) is the intellectual source of this design, but it is **deliberately not copied**: in this project the "scorer" is the existing `diag`/`stylestat`, the "trace" is the existing checkpoint/session fact layer, and the "dataset" is cases that stick to fact-layer assertions.

- OpenAI Evals · https://developers.openai.com/api/docs/guides/evals (Note: its hosted Evals platform has announced a retirement timeline; cite only its **ideas** of structured testing / automatic grading / human calibration, and do not treat it as a future dependency)
- Braintrust · https://www.braintrust.dev/foundations/what-is-an-eval
- LangSmith · https://docs.langchain.com/langsmith/evaluation-concepts
