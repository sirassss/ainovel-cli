# Unified Design of User Rules

## In one sentence

All long-term writing rules are normalized into the same book rule snapshot; at runtime, that snapshot is injected only through `novel_context`, and the raw rule text is no longer stuffed into the prompt over and over.

```text
Startup prompt / user rules files / long-term requirements given during a run
        ↓
LLM semantic normalization (per source)
        ↓
Go deterministic merge (by priority)  ←  system default rules (built into the code, go straight into the merge, no LLM)
        ↓
output/novel/meta/user_rules.json
        ↓
novel_context injection
        ↓
Shared by Architect / Writer / Editor / commit checks
```

## Implementation status (2026-07-19, landed + gaps fixed after review)

This design has been implemented, and `go build` / `go vet` / `go test` are all green across 24 packages. One round of code review found 4 gaps, all now fixed: ① the startup-prompt rules were wired only into the dead method `Host.Start`, while the real entry goes through `StartPrepared` and so missed building the snapshot — the raw prompt is now passed straight through from both the quick and cocreate entries, which uniformly call `Host.PrepareUserRules`; ② a snapshot persistence failure was swallowed — `PrepareUserRules` now returns an error and aborts book creation when persistence fails (the resume path stays best-effort, to avoid introducing a new failure mode for existing books); ③ rules-file read errors were silently skipped — `raw.go` now logs errors other than "does not exist" (permissions and the like); ④ the README still taught the old YAML/front matter and linked to a deleted file — it has been rewritten.

The landing is largely consistent with this document; the implementation choices after the structured-output upgrade are as follows:

1. **Normalization has only one `Contract.Schema`; two sets of prompts are not maintained.**
   When the model declares support, the native JSON Schema is sent down; when it is unsupported or its capability is unknown, the unified contract layer injects the same Schema into the prompt.
   In both modes the Schema is re-checked on the Go side, followed by value-domain and cross-field business validation.
2. **A single invalid field value degrades to "that field is missing", rather than degrading the whole source.**
   If a field is an empty placeholder or has an invalid type, sanitize drops that field (treated as undeclared) and keeps the remaining valid fields of that source;
   only a "whole normalization failure" (network / model / invalid JSON / parse failure) downgrades the whole source to raw preferences
   and sets `status=degraded`. This way one bad field does not drag down the other valid rules from the same source. Output errors that the model can repair carry
   the exact reason and continue to self-heal, with the lifecycle controlled by `context`; explicit terminating errors go to the log and degrade by source.

Code locations: `internal/rules` (pure data + deterministic merge: snapshot.go / raw.go / types.go), `internal/userrules`
(LLM normalization + orchestration + persistence: normalize.go / service.go), `internal/store/user_rules.go` (snapshot storage),
`internal/userrules/service.go` (persistence of rules added during a run), `assets/prompts/arbiter-intervention.md` (three-way triage).
The system-default mechanical baseline has moved from `assets/rules/default.md` into the code-built-in `rules.SystemDefaults()`, and the YAML parsing path and the
yaml.v3 dependency have been deleted. **Not verified**: the full chain of a real LLM book opening / the Arbiter rules action during a run (the normalizer offline prototype has been verified 10/10).

## Why

The Writer does not reliably receive the user's original full prompt for every chapter. It mainly relies on this chapter's task and `novel_context(chapter=N)`.

So long-term rules cannot depend on conversation-history memory, and should not be secretly guessed from natural language with regex either. The correct approach is to explicitly normalize long-term rules into state, and then have `novel_context` distribute them uniformly.

The "normalization" here must use the large model's natural-language understanding rather than enumerate phrasings in Go. The program defines only a small number of mechanically checkable fields, and is responsible for the schema, deterministic merging, validation, persistence, and commit checks; expressions such as "around 1,500 characters per chapter", "no chapter over 2,000 characters", and "don't write lines like 'wheel of fate' again" are understood semantically by the LLM.

## Unified state

At runtime this book maintains only one source of truth for user rules:

```text
output/novel/meta/user_rules.json
```

The shape is kept simple:

```json
{
  "version": 1,
  "status": "ready",
  "structured": {
    "genre": "xianxia",
    "forbidden_chars": [],
    "forbidden_phrases": ["to some extent"],
    "fatigue_words": {}
  },
  "preferences": "The protagonist is calm and restrained; explain less, and use action and dialogue more.",
  "sources": [
    "startup_prompt",
    ".ainovel/rules/style.md"
  ],
  "uncertain": [
    "Use fewer metaphors: no explicit threshold, handled as a style preference"
  ]
}
```

Field boundaries:

- `version`: the snapshot schema version, to ease future migration.
- `status`: `ready` / `degraded`, marking whether normalization fully succeeded; used only for echo and diagnostics, and does not enter creative judgment.
- `structured`: rules that code can check mechanically or consume stably.
- `preferences`: natural-language preferences that cannot be checked mechanically but are long-term valid for creation.
- `sources`: source audit, does not enter creative judgment.
- `uncertain`: normalization diagnostics, used only for echo and troubleshooting, and does not enter creative judgment.

Only `structured` and `preferences` are injected into the model; `version` / `status` / `sources` / `uncertain` are operations and diagnostics metadata and do not go into `working_memory.user_rules`. Technical errors do not go into the snapshot, only into the log (see §Failure and degradation).

## Input sources

Long-term rules have four input sources:

1. **Startup prompt**: the long-term requirements the user wrote when opening the book.
2. **User rules files**: global or project-level long-term preferences, read as ordinary natural language.
3. **System default rules**: the mechanical baseline built into the code.
4. **Long-term requirements given during a run**: the user says mid-run "from now on, always do it this way"; the Arbiter extracts a `rules` action, and the Host calls `AddRuntimeRule`.

These input sources do not enter the Writer prompt directly, and are not read repeatedly at runtime. They take part in normalization only when the snapshot is generated or updated, and the result is merged into `meta/user_rules.json`.

## Rules files

A rules file is an ordinary long-term prompt; it is not a runtime prompt and not a configuration file. It serves only as normalization input and does not support YAML:

```md
# Writing preferences

1200-1600 characters per chapter.
The protagonist is calm and restrained; no bleeding-heart naivety.
Explain less, and push the story with action and dialogue.
Do not use "to some extent".
```

After the system reads it, it is normalized into:

```json
{
  "structured": {
    "forbidden_phrases": ["to some extent"]
  },
  "preferences": "1200-1600 characters per chapter; the protagonist is calm and restrained, no bleeding-heart naivety; explain less, and push the story with action and dialogue."
}
```

If the file contains YAML front matter, it is likewise treated as ordinary text and not as a structured declaration. Structured results come only from the unified normalization flow.

If the user modifies a rules file after startup, the current book does not change automatically; the snapshot must be regenerated. This way an existing book does not drift in behavior because a global rules file changed.

## Semantic normalization

Normalization is an independent, schema-constrained LLM call — each source is normalized once on its own, not mixed into creative generation, and not hard-parsed with regular expressions or keyword tables.

Input:

- The original text of a single source (startup prompt / one rules file / one requirement given during a run)
- A description of the `structured` fields the system currently supports

System default rules are not in this list — they are compiled structured rules built into the code and go straight into §Merge rules without passing through the normalizer.

Output:

- That source's candidate `structured`
- That source's candidate `preferences`
- `sources`
- `uncertain`

Go-side responsibilities:

- Provide the schema.
- Validate field types and value domains.
- Merge the sources deterministically by the priority in §Merge rules (the LLM does not rule on source priority).
- Save the snapshot.
- Inject the snapshot in `novel_context`.
- Use the same snapshot for mechanical checks in `commit_chapter`.

LLM-side responsibilities:

- Understand the natural-language rules of a single source.
- Promote clear, mechanically checkable rules into `structured`.
- Keep aesthetic, style, and character preferences as `preferences`.
- Stay conservative about uncertain content and do not invent thresholds on its own.

### Conservative promotion

`structured` holds hard rules or stable parameters; it is not a "model guessing zone". Promotion must be conservative:

- Write into `structured` only when the user's expression is explicit and unambiguous.
- `forbidden_chars` / `forbidden_phrases` are error-level fields and must be especially conservative; promote only for explicit prohibitions such as "do not use X", "X is banned", or "don't write X".
- `fatigue_words` are promoted only when the user gives explicit words and a threshold; threshold-less requirements such as "use fewer metaphors", "don't be so bookish", or "reduce verbal tics" go into `preferences`.
- Length/word-count wishes ("3000 characters per chapter", "shorter") always go into `preferences`: chapter length is a semantic judgment call about narrative pacing and gets no mechanical check — turning a number into a hard line would tempt the model to pad text to cross it.
- Requirements that cannot be mechanized, have no explicit threshold, or depend on contextual judgment all go into `preferences`.

Principle:

```text
Better to miss promoting into structured, degrading to a soft preference;
never wrongly promote into structured, creating hard false positives every chapter.
```

The cost of under-extraction is a somewhat weaker style preference; the cost of wrong extraction is an erroneous rule fact generated every chapter.

## Failure and degradation

Normalization is an enhancement path, not a precondition for the main creation. A failure of model understanding must never block book writing.

- **Degrade per source**: when a source's normalization fails (network / model / invalid JSON / schema validation failure), that source degrades to raw preferences and produces no `structured`; the other successful sources contribute their `structured` as usual.
- **Context-controlled self-healing**: retryable request errors, format/Schema errors in prompt mode, and business-validation errors keep self-healing until success or until `context` ends; there is no fixed attempt count. Native-contract violations, refusals, truncation, erroneous termination, and non-retryable request errors are surfaced immediately and degrade by source.
- **Technical errors go to the log**: technical errors such as JSON / schema / network are written to the log, do not enter `working_memory.user_rules`, and are not used as creative input.
- **Snapshot marker**: when any source degrades, the snapshot gets `status=degraded`.
- **Continue as long as it can be persisted**: as long as `meta/user_rules.json` can be written, the main creation must continue.
- **Abort only on persistence failure**: abort only when the snapshot cannot be written to disk, because later runs would have no stable source of truth.

The `AddRuntimeRule` contract (during a run): when the normalizer fails, it saves a degraded snapshot
and does not inject normalization errors such as JSON/schema/network into the creative flow; it returns an error only when persistence fails.

## System default rules

`System defaults` is a mechanical baseline built into the code; it is not a user rules file and does not use YAML.

It does not pass through LLM normalization — it is already in structured form and goes directly, as the lowest-priority source, into the Go merge of §Merge rules. This way the default rules have no LLM failure, drift, or cost problem.

The system-default mechanical rules were originally kept temporarily in `assets/rules/default.md` (an old implementation detail, not a user YAML that must be kept compatible); when this design landed they were moved into the code-built-in `rules.SystemDefaults()`, and the YAML parsing path was deleted (see §Implementation status).

During the migration, the necessary comments explaining where the thresholds came from were kept, for example that some fatigue-word thresholds come from empirical evidence in long-run artifacts. This is not to stay compatible with the old YAML, but to let future maintainers know why a default threshold exists and when it should be adjusted.

## Merge rules

The merge order follows "the more specific, the higher the priority":

```text
System defaults
→ Global rules compiled result
→ Project rules compiled result
→ Startup prompt compiled result
→ Runtime user update
```

A higher-priority source overrides a lower one.

The merge is executed deterministically by Go: the LLM only normalizes the natural language of a single source into candidate `structured`/`preferences`, and Go does field overriding and text concatenation in the order above; priority is not left to the LLM to rule on.

- `structured`: overridden per field; a field of the same name from a later source overrides the earlier source.
- `preferences`: do not override each other; they are concatenated into readable text in priority order (higher-priority sources last), so that the LLM can see the source order.

Known limitation: `preferences` are ordered by priority, but Go does not resolve conflicts. In a long run, if the user successively gives mutually contradictory soft preferences (for example first "calm and restrained", then "chatty"), both stay in the text and the LLM weighs them by order and context; anything that needs a deterministic hard override should be expressed as a mechanizable `structured` field.

## Persistence entry points

Normalization, merging, and persistence are one and the same piece of logic, but there are two callers, which must be kept distinct, otherwise startup preparation would get mixed into the main creative context:

- **Book opening / refresh (startup side, deterministic)**: the Host / startup flow calls this logic directly to generate the initial snapshot, without entering the main creative loop. This is a deterministic startup-preparation task.
- **Update during a run (intervention ruling action)**: the `rules` action triaged by the Arbiter is passed by the Host directly to `userrules.Service.AddRuntimeRule`, reusing the same validation / merge / persistence logic, and merging the new rule, which has no progress starting point, into the snapshot as a `Runtime user update`.

(For the implementation, it is suggested to consolidate this logic into one internal service shared by both callers; the concrete naming is left to the implementation.)

Whichever caller it is, the result is finally written into the same `meta/user_rules.json`. The persistence logic does only three things:

1. Validate the structured fields.
2. Merge into this book's current snapshot by the priority in §Merge rules.
3. Return the complete saved rule facts.

It does not:

- Dispatch subagents.
- Modify the outline.
- Silently swallow invalid fields (record and degrade, see §Failure and degradation).
- Inject the raw text directly as the final prompt.

Example of an update during a run: the user says "from now on, always do it this way" (no progress starting point) → the Arbiter rules it as a `rules` action → the Host normalizes that item through `AddRuntimeRule` → it is merged into the snapshot at the highest priority as a `Runtime user update` → echoed via the event stream.

## Echo

Every time the `user_rules` snapshot is generated or updated, the normalization result must be echoed to the user:

```text
Generated this book's rule snapshot:
- Mechanical rules: 1200-1600 characters per chapter; banned phrase "to some extent"
- Style preferences: protagonist calm and restrained; explain less, push the story with action and dialogue
- Not promoted to mechanical rules: use fewer metaphors (no explicit threshold, handled as a style preference)
```

- Startup / refresh: reuse the existing startup-rules logging capability to print the snapshot, with no new mechanism; in the cocreate scenario the echo can be folded into the cocreate confirmation step.
- During a run: after `AddRuntimeRule` succeeds, it is echoed via the event stream ("writing rules updated and persisted"; the current literal is `Quy tắc viết đã được cập nhật và lưu`).
- Degradation: when `status=degraded`, the echo states clearly which sources could not be parsed, that the book is currently running on raw preferences, and that the snapshot can be regenerated.

The echo is not a second approval gate; its purpose is to let the user know what the system understood, so that after finding an error they can regenerate the snapshot.

## How agents consume it

All agents see only:

```json
working_memory.user_rules
```

Division of duties:

- Architect: adjusts per-chapter plot density and the number of chapter splits according to the length wishes in `preferences`.
- Writer: writes according to the hard rules in `structured`, and adjusts style according to `preferences`.
- Editor: reviews against the same set of rules.
- `commit_chapter`: uses `structured` for mechanical checks and returns violations.

The Writer neither re-interprets the original startup prompt nor reads the raw rules files.

## Intervention classification: three destinations

Interventions during a run are classified into three kinds by "what is to be changed":

- **How to write** (writing craft / style / quality: word count, word choice, banned expressions, sentence patterns, dialogue share, title format, etc.) → Arbiter `rules` action, normalized and merged into `meta/user_rules.json`. Examples: "1500 characters per chapter", "titles use only Chinese", "the protagonist should be calm and restrained overall", "a bit more dialogue".
- **What to write** (plot / structure / character direction / length) → architect, landing in the compass / outline / character profiles. Examples: "write more combat storyline in this volume", "from chapter 30 the protagonist's tone turns cold", "extend to 40 chapters".
- **Change what is already written** (rewrite / revise specified chapters) → editor, enqueued into PendingRewrites.

Criterion: **"how to write" → rules; "what to write" → architect; "change what is already written" → editor**.

## Implementation steps

1. Add the `meta/user_rules.json` store.
2. Add an independent LLM normalization pass (per source), using schema-constrained output of candidate `structured/preferences/sources/uncertain`.
3. Add the Go-side deterministic merge: apply field overriding and text concatenation across the sources by priority to produce the snapshot.
4. Consolidate normalization / merging / persistence into one piece of logic shared by two callers: the startup side calls it directly to generate the initial snapshot; during a run, the intervention-ruling `rules` action reuses it via `AddRuntimeRule`. On failure, handle per §Failure and degradation: the source degrades to raw preferences, the snapshot gets `status=degraded`, and the main creation continues.
5. Move the system-default mechanical rules from the current `assets/rules/default.md` into a code-built-in structure or a JSON asset, keeping the comments on threshold origins; delete the YAML parsing path for user rules, with no compatibility layer.
6. After a rules file is read, its body is no longer injected directly as a prompt, but normalized and then merged into the `user_rules` snapshot.
7. `novel_context` injects only `working_memory.user_rules` from `meta/user_rules.json`.
8. `commit_chapter` checks with the same `user_rules.structured`.
10. Intervention triage (now carried by the Arbiter, `assets/prompts/arbiter-intervention.md`) explicitly splits into three by "what is to be changed": long-term writing-style / quality requirements go through the `rules` action and land in the snapshot; plot / structure / characters / length go to architect; rework of already-written chapters goes to editor (see §Intervention classification: three destinations).

## Acceptance criteria

- If the user's startup prompt says "1200-1600 characters per chapter", the Writer's first-chapter `novel_context` shows the original text of this wish in `preferences`.
- A rules file written only in natural language is also normalized into the same `user_rules` when the snapshot is generated.
- Rules files neither need nor support YAML; everything is normalized as natural-language rules.
- At runtime the rules files are no longer read; only `meta/user_rules.json` is read.
- The default mechanical rules no longer come from a YAML rules file, and user rules have no YAML compatibility layer either.
- Normalization does not use regex/keyword hard-coding; natural-language understanding is done by the LLM.
- Vague rules are not promoted into error-level `structured` fields.
- System default rules do not pass through the LLM and go straight into the Go merge.
- Source priority and field overriding are executed deterministically by Go, and the same input produces the same snapshot.
- When the user says "from now on, always do it this way" during a run, it is merged into the snapshot via the Arbiter rules action, and later chapters' `novel_context` can see the update.
- A normalization failure does not block book writing: the failed source degrades to raw preferences, the snapshot gets `status=degraded`, and the main creation continues; only a snapshot that cannot be persisted aborts.
- A normalization failure returns `status=degraded` and does not throw technical errors up to pollute the main flow.
- After the snapshot is generated or updated, `structured` / `preferences` / the not-promoted items are echoed; on degradation the echo states the degraded sources.
- Opening a new book does not inherit the previous book's `user_rules`.
- Invalid structured fields are not silently ignored: record them and degrade that source, without blocking the main flow.

## Explicitly not doing (judged unnecessary, not phase cutting)

The following capabilities bring no benefit under current requirements and stay out of the design, to avoid over-design:

- Field-level deletion / revocation semantics such as `clear_fields`.
- Automatic refresh that watches rules-file changes (after editing a file, just explicitly regenerate the snapshot).
- Time anchors / override resolution for `preferences` (use `structured` if a hard override is needed).
- Persisting a `diagnostics` array in the snapshot (technical errors go to the log; the snapshot keeps only `status`).
- Auto-generating the schema field descriptions from Go types (maintaining a short description by hand is enough).

The design principle is unchanged: the LLM is responsible for understanding natural language, and Go is responsible for deterministic merging, validation, persistence, and checking.
