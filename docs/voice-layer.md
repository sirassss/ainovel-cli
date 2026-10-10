# Voice Layer Design

> Status: design finalized v2 (2026-07-12, incorporating external review: added override semantics, path semantics, assembly order, the eval entry point, and the evaluation statistics protocol), **ready to implement**.
> Priority: ahead of the control-plane evolution (docs/engine-arbiter.md), because the "AI-tone" problem is an active user pain point.

## 1. Background and problem definition

Users report that generated content reads with a "heavy AI tone". After investigation, the conclusion is: **the problem is not that voice knowledge is too deeply coupled with the workflow, but that the iteration loop is broken in two places**:

1. **Every change requires a recompile**: the voice semantic assets (anti-ai-tone.md, the writing standards in writer.md, styles/*.md) are all `go:embed`ed, so tweaking one phrase means rebuilding and releasing;
2. **There is no voice-specific measurement loop**: after a change, the only check is a human reading and judging by feel, with no objective before/after comparison, so optimization turns into guesswork.

## 2. Current inventory (voice-related assets, five layers in total)

| Layer | Location | Current state | User-tunable |
|----|------|------|---------|
| Semantic criteria | `assets/references/anti-ai-tone.md` | Shared by the writer (avoidance) and the editor (evidence citing); five categories: structure / word choice / description / dialogue / rhythm | ❌ embedded |
| Writing standards | `assets/prompts/writer.md` Writing Standards section | Mixed into the same embedded file as the execution protocol | ❌ |
| Style presets | `assets/styles/*.md` (4 files) | Selected at a single point via cfg.Style, appended to the writer prompt | ❌ and none can be added |
| Mechanical rules | `internal/rules` | Fatigue words / forbidden phrases / word count, enforced at commit | ✅ already has three override layers (note: its "project level" is bound to **cwd**, see 3.4) |
| Runtime preferences | Arbiter `rules` action | Natural language → structured, effective across restarts | ✅ |

There are also two key pieces of infrastructure: **stylestat** (book-wide sentence-pattern tic statistics, fed back to the writer as a "verbal-tic mirror", pure code with zero hallucination) and **eval's `OverridePrompt`** (the prompt A/B infrastructure already exists).

Conclusion: the tunability and measurement raw material of the mechanical layer are already in place; the gaps are concentrated in the **semantic layer being non-overridable** and the **measurement loop not being aimed at voice**.

## 3. Design

### 3.1 Core principle

**Split "how to write" (voice) out of "how to collaborate" (protocol): the former is turned into overridable data; the latter stays compiled in.**

### 3.2 Splitting writer.md: placeholder backfilled in place

The writing standards section of writer.md sits in the **middle** of the file (after the execution protocol, before supporting-cast continuity), so it cannot simply be appended at the tail. A placeholder approach is used:

- `writer.md` (protocol, embedded): keeps the execution protocol / resume from breakpoint / rewrite and polish / chapter contract / the description of the user-preference mechanism / **the entire word-count section (including writing advice)** / supporting-cast continuity / commit parameters; the original position of the writing standards section is replaced by a **single** `{{VOICE}}` placeholder
- `voice.md` (voice, overridable): the whole writing-standards section (removing AI-tone / sentence-pattern diversity / no recap of earlier events)

The word-count writing advice stays in the protocol file (adopted in the 2026-07-12 review): it is tightly coupled with enforcing the word-count contract, splitting it out would need a second placeholder and turn Voice into a multi-fragment format, which is not worth it for a piece of technique text that very few people want to override; users' preferences on word count go through user_rules. The file name stays `writer.md` (eval `OverridePrompt` is keyed by file name, so renaming only adds wiring).

**The assembly order must be byte-for-byte compatible with the current state**. The current order is `writer.md → simulationGuidance → style` (assets/load.go:84 + agents/build.go:247), so the single assembly function is:

```go
// The only entry point for production, eval and tests; in-place backfill of {{VOICE}} guarantees the split is lossless
func BuildWriterPrompt(protocolTemplate, voice, simulationGuidance, style string) string
// = replace(protocolTemplate, "{{VOICE}}", voice) + simulationGuidance + style
```

Lesson from precedent: the `WithSimulationGuidance` comment records the pitfall of "baseline wrapped, variant not wrapped → A/B not equivalent"; forked assembly paths are a breeding ground for the same kind of accident, so they are converged into a single function.

### 3.3 Override model: per-asset semantics (no ambiguity)

| Asset | Override semantics | Rationale |
|------|---------|------|
| `voice.md` | **Append**: the built-in is kept, and the global/per-book content is appended as marked segments | Whole-file replacement would leave users permanently stuck on an old built-in version; the common need is fine-tuning rather than rewriting |
| `anti-ai-tone.md` | **Append** (same as above) | The common need is to add criteria; users who want to overturn the built-in criteria are a tiny minority and are not designed for |
| `styles/<name>.md` | **Whole-file replacement by the same name**; a new file name means a new style | A style is an overall voice, and merging two styles is meaningless |
| `genres/<name>/style-references.md` | Whole-file replacement by the same name; a custom style with no reference is **allowed to have none, with no fallback to default** (a wrong reference is worse than none) | Same as above |
| user_rules | Highest runtime priority (current behavior untouched) | — |

Assembly with append semantics carries explicit boundary markers:

```
## Project default voice
...
## User global voice override (the following requirements take precedence over the project default)
...
## This book's voice override (the following requirements take precedence over all of the above)
...
```

**Honest boundary**: under append semantics, "the later one wins" is a priority hint to the LLM, not a mechanical guarantee. Voice is advisory content, so this is acceptable; constraints that need mechanical guarantees go through the rules layer (where the override is real). This boundary is written into the user documentation.

`arc-templates.md` belongs to the planning plane (it shapes story structure rather than voice) and is **not in the v1 whitelist**; recorded as an open item.

### 3.4 Path semantics: per-book binding to outputDir, not to cwd

```
per-book   <outputDir>/style/     >   global   ~/.ainovel/style/   >   built-in default (embed fallback)
```

- Binding to outputDir makes Voice **travel with the book**: resuming the same book from another directory loads the same voice; Docker/headless/TUI path resolution is consistent; multiple books sharing a cwd do not interfere with each other
- The `assets.Load` signature explicitly receives the resolution root (outputDir) and **does not read cwd internally**
- Note the difference from the rules layer: rules' `./.ainovel/rules` is bound to cwd (an existing convention in internal/rules/loader.go that this design does not touch); the user documentation states clearly that the two have different semantics: rules are "project level", voice is "per-book level"

Full structure of the user directory:

```
<outputDir>/style/            (~/.ainovel/style/ has the same structure)
  voice.md                    appended segment
  anti-ai-tone.md             appended segment
  styles/
    xianxia.md                new, or replaces the same-named built-in
  genres/
    xianxia/
      style-references.md     optional
```

A style name is its file name, validated against `[a-z0-9-]+`, and path characters are rejected.

### 3.5 Why opening this to users is safe

All protocol invariants live in the **fact layer**: draft comes before check, commit enforces the mechanical rule checks, out-of-range word counts are blocked, checkpoints are idempotent. None of them live in the prompt. However badly a user mangles voice.md, the guards and tool preconditions still take effect; the worst outcome is ugly prose, and the state machine cannot be broken.

### 3.6 When it takes effect, and the eval entry point

- v1 resolves at startup and **takes effect after a restart** (resume from breakpoint is precise to the step, so the restart cost is near zero; hot reload is not built)
- eval gains an **independent voice variant entry point** (e.g. `Bundle.OverrideVoice(raw)`) that internally goes through the same `BuildWriterPrompt` path. Doing voice A/B by overriding the whole writer.md is forbidden (it drags the protocol along, and the baseline/variant protocols might not be equal)

## 4. Measurement loop: the voice evaluation set

```
change voice/anti-ai-tone
  → voice evaluation set (fixed cases, eval voice-variant A/B)      ← the only new piece
  → stylestat metric comparison (deterministic hard metrics)
  + LLM judge scores item by item against the anti-ai-tone criteria with cited evidence (report only at first, not a hard gate)
```

Statistics protocol (fixed inputs guarantee only **comparability**, not reproducibility):

- baseline/variant are locked to the same model and reasoning parameters
- each case is repeated N≥3 times, reporting the mean, variance and raw samples
- the judge is blind (baseline/variant identity is not exposed)
- cases cover genre × chapter type (opening / everyday progression / climax / wrap-up)

## 5. Explicit non-goals (to prevent over-design)

- Do not open the protocol prompt to end users (`OverridePrompt` stays as an internal eval capability)
- No hot reload while running
- Do not open stylestat regex patterns as user configuration (the mechanical layer already has an extension entry: rules' fatigue_words/forbidden_phrases)
- No style marketplace / sharing mechanism (copying a style directory is naturally shareable)
- arc-templates is not in the v1 whitelist

## 6. Implementation steps and acceptance

1. Split writer.md (`{{VOICE}}` placeholder) + `BuildWriterPrompt` as the single assembly function
2. Three-layer resolver: `assets.Load(outputDir, style)` + per-asset semantics (the 3.3 table) + styles enumeration and merging; unit tests cover priority / default fallback / append boundary markers
3. eval `OverrideVoice` entry point
4. User documentation: directory structure, per-asset semantics, the path-semantics difference between rules and voice, examples
5. Voice evaluation set (can be deferred as an independent task)

**Acceptance criteria**: ① with no override files at all, the `BuildWriterPrompt` output is **byte-for-byte identical** to before the split; ② the three-layer priority and the append/replace semantics have table-driven unit tests; ③ after adding `styles/xianxia.md`, `style: xianxia` works as soon as the file is dropped in; ④ eval voice A/B uses the same assembly path as production (with a test proving it); ⑤ the full test suite and sim regression are green.

## 7. Relationship to the control-plane evolution

Fully orthogonal (content plane vs control plane), with no implementation dependency. Agreed order: **voice layer → voice evaluation set → Engine/Arbiter (advanced per the resolution in its document §8)**.
