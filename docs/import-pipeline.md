# Semantic Import Pipeline for External Novels

> Status: implemented (v1, `internal/host/imp`; truncated-prefix salvage included as Phase 3 supplement)
> Date: 2026-07-15
> Goal: let importing an external novel keep benefiting from model capability upgrades, while providing engineering guarantees that nothing in the full text is lost, failures are diagnosable, crashes are recoverable, and publishing is verifiable.
> Revision: SourceUnit order is the numeric `(Line, Part)` order (§7.3/§8.3); truncated-prefix salvage is downgraded to an efficiency optimization that may be deferred and must be observable (§9.5/§13.3/§19); the model tier of semantic functions is opened up as a knob (§13.1/§17).
> Revision 2026-07-16: the model-tier knob landed as the roles config `import_segment/import_analyze/import_synthesize` (§13.1); natural-language re-segmentation landed as `--guide` and the workspace `guidance.txt` semantic input (§18.3); semantic failures uniformly save the raw response to failures/ (§14.2); segmentation confirmation supports a one-shot `y` in the panel (§8.4); unfinished imports are announced proactively at startup (§18.2). JSON Schema mode (§13.2, level 1) is not implemented yet; marked TODO, to be reworked together with the other model call sites across the repo.

## 1. In one sentence

Import is not "split the text with regexes, then have the model emit the whole book as one JSON", nor is it a freely running Import Agent; it is a **staged semantic compilation pipeline**:

> The model handles open semantics; the code handles coordinates, coverage, types, hashes, ordering and idempotency; all semantic artifacts are validated in an isolated workspace before being published to the formal book state.

```text
External text
  → deterministic reading and normalization
  → LLM identifies chapter / volume / ancillary-text boundaries
  → code verifies full-text coverage
  → user confirms the segmentation (auto-accept can be explicitly authorized)
  → LLM extracts per-chapter facts over consecutive chapter batches
  → LLM aggregates book-wide semantics hierarchically
  → code assembles and validates the Foundation
  → idempotent publish of Foundation and chapters
  → pause once by default; only an explicit --continue hands off through the normal gate
```

## 2. Why a refactor is required

The current implementation is:

```text
Regex chapter splitting
  → all chapter bodies fed into ReverseFoundation in one call
  → the model outputs premise / characters / world_rules / the full chapter outline / compass in one go
  → written immediately into the formal Foundation
  → then the same bodies are re-read chapter by chapter, analyzed and committed
```

It has four structural problems.

### 2.1 Chapter splitting enumerates open semantics

Chapter titles have no closed grammar. Adding more regexes for "第 N 章" (Chapter N), "卷 N" (Volume N), "Chapter N" and so on only covers formats already seen; it cannot cover author-defined titles, mixed layouts, volume/chapter hierarchies and future formats. <!-- han:keep -->

Worse, the current splitting makes unmatched boundaries vanish from the result, and may silently discard the text before the first title, empty chapters, and content judged to be trailing noise. The code cannot prove that these contents should be discarded.

### 2.2 Foundation call input and output both grow linearly with the chapter count

`ReverseFoundation` handles both whole-book understanding and generating the full chapter outline: the input contains all bodies and the output contains the detailed structure of every chapter. At 54 chapters the JSON may already be truncated; raising `max_tokens` only pushes the failure point to longer books.

### 2.3 Formal state is already modified before failure

The Foundation and chapters are published while being analyzed. When a later step fails, the user is left with a formal book state that is half imported and half not yet analyzed. The current `from=N` merely assumes the user knows where to resume from, and cannot prove that the source file, the segmentation result and the existing chapters are still consistent.

### 2.4 Many semantic conclusions are hard-coded

The current design also fixes:

- the imported text can only be one volume;
- it can only be divided into 1 to 3 arcs;
- short/mid/long is chosen by 25/80 thresholds on the number of imported chapters;
- `open_threads` are forcibly manufactured so that continuation is allowed;
- every chapter must have characters, a fixed number of events, and a fixed type of hook.

None of these are facts that can be mechanically proved from the file format; they should be judged by the model from the text, or expressed explicitly by the user.

## 3. Goals and non-goals

### 3.1 Goals

1. **Open formats are understood**: the user is not required to rewrite the novel into a built-in title format, nor to write regexes.
2. **The full text is accounted for**: every non-empty piece of source text must belong to an explicit chapter or ancillary region; silent loss is forbidden.
3. **Scale is controlled**: no more single call that reads the whole book and outputs all chapter objects; segmentation, dual-budget chapter batches and range synthesis all have local input/output bounds, and the global output grows only with real semantic complexity such as characters and volumes/arcs.
4. **Failure does not pollute**: no formal creative state is written until semantic analysis and Foundation validation are complete.
5. **Precise resume**: resume is based on the source snapshot and the artifacts' `InputDigest`, not on `from=N` or the user's memory.
6. **Model upgrades pass straight through**: a stronger model directly improves boundary recognition, fact extraction, volume/arc division and continuation judgment, with no new Go rules.
7. **Reuse the formal commit semantics**: publishing chapters keeps using the PendingCommit, checkpoint and digest idempotency of `commit_chapter`.
8. **Fully observable**: progress, model identity, usage, raw failure responses and final errors all have a defined place to land.
9. **Interactive and automated modes coexist**: by default the user confirms high-risk semantic boundaries, while an explicit unattended authorization is also provided; the automatic path does not rely on silent guessing.

### 3.2 Non-goals

- Do not build a Coordinator or a general long-running Agent loop.
- Do not build a general Workflow/PolicyEngine/task-graph framework.
- Do not automatically repair or rewrite the user's original text.
- Do not implement a database, vector retrieval or distributed parallelism for import.
- Do not support fuzzily merging another novel into an existing book.
- Do not implement migration of the old `from=N` state or dual-track compatibility.
- Do not extend to EPUB/PDF in this RFC; the first version still accepts only txt/md, the reading layer stays local, and it can be replaced in future without changing the downstream contracts.

## 4. Responsibility boundaries

| Question | Owner | Reason |
|---|---|---|
| Byte decoding, newline normalization | Go | File format and deterministic conversion |
| Which source position is a chapter title, volume title or ancillary text | LLM | Open semantics, cannot be enumerated |
| Which stable source position a title corresponds to | Go | SourceUnit, source anchors and byte ranges can be verified mechanically |
| What happens in a chapter | LLM | Literary semantic understanding |
| How characters, world rules, foreshadowing and relationships are summarized | LLM | Cross-chapter semantic induction |
| Volume/arc boundaries, whether the story is concluded, planning level | LLM | Depends on the shape of the narrative, not on fixed thresholds |
| Whether chapter ranges are increasing, non-overlapping and fully covering | Go | Provable invariants |
| Whether JSON types, closed-set enums and referenced chapter numbers are legal | Go | Typed contract |
| Whether an existing analysis can be reused | Host/Workspace | Reuse is allowed only if the real semantic input can rebuild the same `InputDigest` |
| When to write the formal book state | Host/Store | Publish protocol and crash recovery |
| Whether continuing with the current segmentation is authorized | User/Intent | Interactive confirmation or explicit `--yes`; the code does not answer on the user's behalf |

The LLM calls here are neither the Arbiter control plane nor the Worker creative loop. They are **semantic functions** with clear boundaries: typed facts in, typed semantic results out, executed by the Host after validation.

## 5. Overall architecture

```text
[TUI / Headless]
       │ /import <path> / auto-authorization / confirm / cancel
[Host]
       │ exclusive import lifecycle, events, model runtime
[imp.Runner]
       ├── LoadState → NextAction (derived only from workspace facts)
       ├── Source     read, decode, normalize, snapshot
       ├── Segment    structural projection → LLM boundary recognition → coverage validation
       ├── Analyze    dual-budget consecutive batches → per-chapter facts staged
       ├── Synthesize hierarchical induction → BookSynthesis
       ├── Validate   assemble and validate the complete Foundation
       └── Publish    formal Foundation → commit_chapter
               │
[meta/import workspace]          [formal Store]
source snapshot/segmentation/analysis/synthesis results      Progress/Checkpoint/Artifact/PendingCommit
```

The Runner is ordinary deterministic stage orchestration and has no free decision-making ability. Each time it executes only the one action derived by `NextAction`, and re-reads the facts after the action completes.

## 6. Workspace and state derivation

Facts during import are stored under the book directory:

```text
meta/import/
├── manifest.json
├── intent.json
├── source.txt
├── guidance.txt          # when present: the user's natural-language segmentation guidance (--guide), a semantic input of segmentation
├── segmentation.json
├── confirmation.json
├── analyses/
│   ├── 000001.json
│   ├── 000002.json
│   └── ...
├── range-digests/
│   ├── 000001-000050.json
│   └── ...
├── synthesis.json
├── story-resolution.json
└── failures/
    ├── last.json
    └── last-response.txt
```

The first version keeps the workspace. It is both the basis for resuming and the audit record of the import; no automatic cleanup or history archiving mechanism is added.

`intent.json` stores the explicit authorizations the user gave when starting the import (auto-confirm, preselection for the uncertain story status, whether to skip the completion Hold). These are user intents that must still be honored after resuming, not stage state that can be guessed from artifacts; after creation they are not silently rewritten by the Runner.

### 6.1 Manifest

```go
type ImportManifest struct {
	Version          int    `json:"version"`
	SourceName       string `json:"source_name"`
	RawSHA256        string `json:"raw_sha256"`
	NormalizedSHA256 string `json:"normalized_sha256"`
	Encoding         string `json:"encoding"`
	SizeBytes        int64  `json:"size_bytes"`
	CreatedAt        string `json:"created_at"`
}

type ImportIntent struct {
	Version             int    `json:"version"`
	AutoConfirm         bool   `json:"auto_confirm,omitempty"`
	StoryResolution     string `json:"story_resolution,omitempty"` // open / closed
	ContinueAfterImport bool   `json:"continue_after_import,omitempty"`
}
```

- `source.txt` is the normalized local snapshot, so resuming no longer depends on the original path still existing;
- the Manifest does not store the absolute source path, which avoids leaking machine directories and removes resume problems caused by moved files;
- Intent accepts only closed-set values and precisely stores the authorizations the user gave in the start command; on resume the old intent is not inferred back from the current advance mode;
- on a schema version mismatch, it explicitly requires continuing with the matching version or re-importing, and does not guess a migration.

On first creation, the manifest, intent and source are first written out in full and validated in a sibling temporary directory, then published by a directory rename to `meta/import/`; if `meta/import/` does not exist, there is no active workspace. This way the initial trio never enters `NextAction` in a half-initialized form, and no `stage=initializing` is needed for the creation process. When a leftover initialization directory is found at startup, it must be reported explicitly and its diagnostic information preserved; it is neither treated as a successful workspace automatically nor silently deleted.

### 6.2 Do not store drifting stage enums

Persistent state does not write control fields such as `stage=analyzing` or `current=37`. The next action is derived from the artifacts:

```text
no manifest/intent/source                                       → ingest
no segmentation                                                 → segment
no confirmation matching the segmentation input digest          → await_confirmation / auto_confirm
missing chapter analysis or analysis with a mismatched digest   → analyze_first_missing
no RangeDigest or synthesis with a matching input             → synthesize_first_missing
story_status=uncertain and no matching user choice              → await_story_resolution
formal artifacts inconsistent with synthesis                    → publish
all formal artifacts consistent                                 → done
```

The `Stage` in events is used only for UI display and is not the source of truth for resuming.

### 6.3 Unified artifact identity

No dependency graph is implemented. Every semantic artifact in the workspace uses the same identity rule:

```go
type Artifact[T any] struct {
	SchemaVersion int    `json:"schema_version"`
	InputDigest   string `json:"input_digest"`
	Payload       T      `json:"payload"`
}
```

`InputDigest` covers all the **semantic inputs** the action actually consumed, encoded in a fixed order and then hashed:

- segmentation: the normalized source content, the SourceUnit projection, the user guidance, and the segmentation prompt/schema version;
- confirmation: the segmentation content and the confirmation method;
- chapter analysis: the batch's chapter range and bodies, the continuity ledger before entering the batch, the prompt/schema version, and the user guidance;
- RangeDigest/BookSynthesis: the content of the ordered analyses or lower-level digests each consumes, and the synthesis prompt/schema version;
- story resolution: the synthesis content and the user's choice;
- publish: the canonicalized content of the domain objects to be published.

Execution facts such as provider/model, usage and thinking are written to provenance/session; a change in model configuration does not automatically invalidate an analysis that already succeeded; when the user wants re-analysis they explicitly delete the corresponding artifact. The cache-reuse decision looks only at whether the current action can rebuild the same `InputDigest`.

`NextAction` walks the fixed linear pipeline looking for the first artifact that is missing, fails to parse, or has a mismatched `InputDigest`. When the text is re-segmented, the user guidance is changed, or upstream facts change, downstream artifacts naturally mismatch; no invalidation rules of the form "which files to delete manually when the segmentation changes" are written.

At publish time the formal artifacts are compared item by item against the synthesis result; if identical, it is skipped idempotently, and if different, a conflict is reported rather than overwriting on a guess. Therefore `ResumeFrom` is deleted. To resume, running `/import` again is enough; the Runner continues from the first missing fact.

## 7. Reading the source file

### 7.1 Decoding

The first version supports:

- UTF-8 / UTF-8 BOM;
- GB18030 (covers common GBK novel text).

The decode result must return the chosen encoding, and it is written to the Manifest and the progress events. "Trying GB18030" must not be hidden as a silent fallback. If the text cannot be decoded reliably or unacceptable replacement characters appear, it fails directly, and the error includes the detection result.

### 7.2 Normalization

Only conversions that do not change the literary content are performed:

- remove the BOM;
- unify CRLF/CR to LF;
- keep blank lines, indentation, title lines and body characters;
- do not delete leading text, empty chapters, ads, copyright notices or so-called trailing noise.

All exclusion decisions are left to the segmentation's semantic result and shown in the preview.

### 7.3 Stable coordinates

A unified `SourceUnit` table is built over the normalized text:

```go
type SourceUnit struct {
	ID        string // L1257; an over-budget line is split into L1257.1, L1257.2
	Line      int
	Part      int
	StartByte int
	EndByte   int
	Text      string
}
```

- `ID` is used only for display and model references; all ordering, containment and increase checks are made by comparing the numeric `(Line, Part)` tuple, and lexicographic comparison of ID strings is forbidden (`"L900"` sorts lexicographically after `"L1000"`); the projection JSON keeps the string id, and the Go side parses it into `(Line, Part)` before comparing;
- a normal line maps to one unit, so the common path is still the intuitive line-number coordinate;
- when a single line exceeds the structural projection budget, Go generates multiple **virtual units**, only at UTF-8 character boundaries;
- virtual slices are not written back to `source.txt`, no soft line breaks are inserted, and no source character is changed;
- when a boundary lies inside a unit, the model returns the unit ID plus a verbatim-copied source anchor; Go requires the anchor to be unique within that unit and then maps it to an exact byte position;
- when the anchor does not exist or is not unique, the specific error is fed back to the model; guessing offsets, truncating text or asking the user to edit the manuscript first is forbidden.

Therefore ordinary chaptered text keeps the line-number model, and a whole text with no line breaks, a single line containing several chapters, or an abnormally long line is handled with the same coordinate types.

## 8. Semantic segmentation

### 8.1 Structural projection

The model sees a structural projection chunked by context budget:

```json
{
  "owned_units": {"start": "L1200", "end": "L1800"},
  "context_units": {"start": "L1180", "end": "L1820"},
  "units": [
    {"id": "L1200", "line": 1200, "text": "The wind blew in from outside the city gate.", "blank_before": true},
    {"id": "L1257", "line": 1257, "text": "Volume Two · The Northern Frontier", "blank_before": true, "blank_after": true}
  ],
  "user_guidance": ""
}
```

The context regions may overlap, but each call may return results only for `owned_units`, so there is no overlapping-chunk voting or conflict merging. Coordinate discipline is enforced by Go (revised 2026-07-16): a boundary the model returns in the context region does not trigger a semantic re-ask. That boundary belongs to the adjacent chunk (which will report it again in its own owned range), so the code simply trims it and echoes a note; semantic retries are reserved for genuine semantic failures (hallucinated IDs outside the projection, illegal kind, etc.). The old behavior re-asked with feedback on out-of-range boundaries, and weak models often burned all 3 attempts and dragged down the whole chunk.

The chunk size is computed from the current architect model's context window and a reserved budget, not from a fixed line count or a fixed chapter count. When the model context grows, the number of calls drops naturally. The planning budget is not used to the full (revised 2026-07-16): the owned body is only part of the request; planning deducts the actual length of the system prompt and the guidance, then applies a 3/4 factor for the blow-up from the projection JSON wrapping; the context region also has a byte cap (chunkBytes/8, with a floor of 4096) to stop virtual slices of an over-long line from swallowing the input budget. The output side has a symmetric safeguard: when a single chunk's boundary JSON is cut off by length (many short chapters), the chunk is halved and retried recursively. Each half has its own cache path, so work already done is not paid for again on retry; only a unit-level truncation that still persists means a true capacity shortfall.

Each chunk's boundary decisions are persisted as an artifact (`segment-chunks/chunk-*.json`; identity = segmentation identity + MaxUnitBytes + the chunk's owned range. The unit table is uniquely determined by "normalized source + MaxUnitBytes", so when a change of tier reshapes the slices of over-long lines, the cache naturally mismatches and misaligned old boundaries are never reused). If any chunk fails or is interrupted, completed chunks are reused on rerun with zero calls, the same philosophy as analyze per chapter and synthesize per range; once the final segmentation is persisted, the chunk-level cache is deleted. When the final integration (resolve) fails, the chunk cache is likewise cleared and a snapshot of the decisions is written to `failures/`: at that point the cache digest always matches, and keeping it would make a rerun re-read the same boundaries with zero calls and deterministically reproduce the same failure. A chapter boundary with an empty body (placeholder titles such as "locked / paid chapter", common in real web-novel sources) does not fail the whole run: it is merged into the preceding segment (not one character of text is lost) and recorded in `Segmentation.Notes` for the confirmation preview to show; if a human disagrees, `--guide` can rule on it.

### 8.2 Model output

```go
type BoundaryDecision struct {
	UnitID    string   `json:"unit_id"`
	Anchor    string   `json:"anchor,omitempty"`
	Kind      string   `json:"kind"` // chapter / group / front_matter / back_matter
	Title     string   `json:"title,omitempty"`
	Uncertain bool     `json:"uncertain,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
```

- `chapter` is a committable body unit, including the semantic judgment of whether a prologue, a preface chapter, a side story and the like count as chapters;
- `group` is evidence of upper-level structure such as a volume, part or section, and is not treated directly as a chapter;
- `front_matter` / `back_matter` mark ancillary regions that explicitly do not enter the chapter body;
- `anchor` must come verbatim from the corresponding unit; it may be omitted when the boundary lies at the start of a unit;
- `uncertain` is used only as a preview hint; the code sets no confidence threshold for it.

The model is not asked to generate regexes. Regexes would again squeeze open semantics back into a finite grammar and bring in escaping, local matching and a uniform-format assumption.

### 8.3 Code validation

Go validates only:

1. All unit IDs exist and fall inside the projection of the call (owned + context region; outside the projection is a hallucination, re-asked with feedback);
2. For boundaries in the owned region: kind belongs to the closed set; a non-empty anchor is unique within the corresponding unit and maps to a UTF-8 byte boundary; no semantic conflict at the same position (when kind/title differ, which one to keep is not ruled on by Go and the re-ask goes to the model; fully identical duplicates are mechanical redundancy, are let through and silently deduplicated); the first chunk must have a boundary covering the start of the text (whether the opening is a preface is judged by the model, not answered on its behalf by Go). All of these are validated at call time (revised 2026-07-16): a bad value that goes into the chunk cache and is only discovered at the final stage would, with the digest always matching, make the failure deterministically reproducible; boundaries in the context region are bound to be trimmed, so no re-ask is made for them;
2a. **Title echo** (revised 2026-07-16, seg-v2): after normalization (whitespace removed), the title of a chapter/group boundary must really exist in the original text of the boundary unit, otherwise it is re-asked at call time. In a measured paged-scrape source, 67 of 157 chapters were boundaries and titles fabricated by the model on mid-chapter continuation text (an ambiguity in the coverage discipline forced a boundary at the head of every chunk), and this fact check catches all of them. Semantic discretion still belongs to the model: for a source that genuinely has no title convention, set `uncertain=true` to keep an induced title (the preview shows the doubt marker); descriptive titles of front/back matter are low-risk and are not checked. The prompt is tightened in step: boundaries fall only at real structural separators, and when a chunk head is a continuation of the previous chapter, returning empty boundaries is the correct output (except for the head coverage of the first chunk);
3. Boundary order and duplicates are repaired deterministically by Go rather than rejected (revised 2026-07-16): the real order is restored by a stable sort on the resolved bytes. Order between chunks is guaranteed by the non-overlap of owned ranges, so disorder can only occur within a chunk, and sorting loses no information; for duplicates at the same byte, the first occurrence is kept and recorded in `Notes`. The old behavior required strictly increasing order or failed as a whole; in measurement 319 boundaries failed on a single in-chunk inversion, and the chunk cache made that failure deterministically reproducible. Order is always judged by the `(Line, Part)` numeric order, never by lexicographic comparison of IDs;
4. The body range of every produced chapter is non-empty (a placeholder title with an empty body is merged into the preceding segment, see §8.1);
5. Every non-empty piece of source text belongs to exactly one chapter, one group title, or an explicit front/back matter (non-empty text at the start that is not attributed to anything, such as a book-opening synopsis/ad whose boundary was missed, is deterministically collected by Go into front_matter and recorded in `Notes` for the confirmation preview, not vetoed at the final stage);
5a. Chapters with the same name (identical after whitespace removal) are recorded in `Notes` for human review (revised 2026-07-16): in a source with a title convention chapter names should not repeat, and a repeat is a deterministic signal that "the same chapter was cut wrongly"; whether to merge is not ruled on by Go, and a non-empty Notes blocks `--yes`;
6. There is no overlapping, out-of-range or unattributed range;
7. A group is not wrongly counted in the total number of chapters.

"Is L1257 semantically a chapter title" is not re-judged by Go.

### 8.4 User confirmation

In interactive mode, chapter analysis is not called before confirmation, and the formal Store is not written. The preview shows at least:

- the number of volumes/groups and chapters;
- all chapter titles, scrollable;
- the range and summary of leading and trailing ancillary text;
- empty chapters, abnormally long chapters, and boundaries the model marked uncertain;
- the start and end lines of each chapter, so the user can compare against the manuscript.

The user can:

- confirm (press `y` in the TUI preview panel, which internally reruns with AcceptSegmentation; this lets the current segmentation through once, is not written to intent, and the confirmation records `method=user_confirmed`);
- enter a natural-language note and re-recognize, for example `/import --guide=幕间·X 也是独立章节` ("Interlude · X is also a standalone chapter"); <!-- han:keep -->
- cancel and keep the workspace (Esc).

`/import <path> --yes` is an explicit unattended authorization: after coverage validation passes, the Runner writes the same confirmation artifact, recording `method=auto_authorized`, and then continues with analysis. Even when uncertain boundaries exist, `--yes` means the user chooses to trust this segmentation, but uncertain remains in the artifacts and logs. **Exception (revised 2026-07-16)**: when the segmentation carries tolerance notes (`Notes` is non-empty, meaning empty-chapter absorption, start-of-text fallback or coincident-boundary deduplication occurred), `--yes` does not auto-accept and stops at the confirmation preview; the structure was deterministically rewritten, and a blind authorization that has not seen the preview should not swallow that. A `y` (AcceptSegmentation) given after viewing the preview is not subject to this limit.

`--yes` only skips segmentation confirmation; it does not decide `story_status=uncertain` for the user, and does not skip the import-completion Hold. The user does not need to write regexes or fill in `from=N` by hand.

## 9. Per-chapter fact extraction over consecutive batches

After confirmation, starting from the first missing analysis, consecutive chapters are grouped into batches according to the current model's **dual input and output budgets**. In the first version batches run serially, with no concurrency between windows: foreshadowing IDs, character aliases and state changes have a temporal order, and the compact ledger produced by the previous batch is the input of the next.

Serial execution constrains only the execution strategy of the first version and is not a permanent architectural limit; analysis artifacts are still persisted independently per chapter, and if future evidence shows that parallel merging can preserve semantic quality, only the batch scheduling needs to be replaced.

### 9.1 Batch output, per-chapter artifacts

The `=== TAG ===` mixed envelope is abolished. Each call returns one structured batch object, and each array element is still the facts of one chapter:

```go
type ImportedChapterFacts struct {
	Chapter             int                        `json:"chapter"`
	Title               string                     `json:"title"`
	Summary             string                     `json:"summary"`
	KeyEvents           []string                   `json:"key_events"`
	CoreEvent           string                     `json:"core_event"`
	Hook                string                     `json:"hook"`
	Scenes              []string                   `json:"scenes"`
	Characters          []string                   `json:"characters"`
	CharacterEvidence   []ImportedCharacterFact    `json:"character_evidence,omitempty"`
	WorldEvidence       []ImportedWorldFact        `json:"world_evidence,omitempty"`
	TimelineEvents      []domain.TimelineEvent      `json:"timeline_events,omitempty"`
	ForeshadowUpdates   []domain.ForeshadowUpdate  `json:"foreshadow_updates,omitempty"`
	RelationshipChanges []domain.RelationshipEntry `json:"relationship_changes,omitempty"`
	StateChanges        []domain.StateChange       `json:"state_changes,omitempty"`
	HookType            string                     `json:"hook_type"`
	DominantStrand      string                     `json:"dominant_strand"`
}

type AnalysisBatchResult struct {
	Chapters []ImportedChapterFacts `json:"chapters"`
}

type ChapterAnalysisPayload struct {
	BatchStart int                  `json:"batch_start"`
	BatchEnd   int                  `json:"batch_end"`
	Facts      ImportedChapterFacts `json:"facts"`
}
```

Each `analyses/NNNNNN.json` is an `Artifact[ChapterAnalysisPayload]`. Chapters persisted from the same batch record the same `BatchStart/BatchEnd`; their `InputDigest` uses **per-chapter binding**: segmentation identity (the `InputDigest` of the segmentation artifact) + prompt/schema version + chapter number + the single chapter's body. The binding is per chapter rather than per batch partition because batch boundaries change with the model's input/output capacity (a stronger model naturally makes batches larger); if the batch partition were written into the identity, switching models would make already successful analyses mismatch as a whole and force a recompute that charges again for the same work. Binding to the segmentation identity guarantees that "re-segmenting, changing the prompt/schema version, or changing the source" makes downstream analyses mismatch naturally, while merely switching models does no collateral damage; this is the invalidation semantics that resuming really needs.

`ImportedCharacterFact` and `ImportedWorldFact` are compact observations used for book-wide synthesis and do not directly write formal characters or world rules. They carry at least the chapter number, so that synthesis results have a stable provenance.

### 9.2 Dual-budget batching

Batch planning satisfies both of the following:

```text
estimated input + system/prompt/ledger + reasoning reserve + estimated visible output ≤ context window
estimated visible output ≤ completion limit available to the provider/model
```

- the input estimate covers each chapter's title, body and the ledger before the batch;
- the output estimate is composed of the analyzer schema's fixed structural overhead and a conservative per-chapter fact reserve; it only decides how many chapters to pack this time and truncates no field;
- for models whose reasoning tokens share the completion budget with the visible JSON, the reasoning reserve must be deducted first;
- the stronger the provider/model output capacity, the larger the batch naturally becomes; a fixed rule such as "10/20 chapters per batch" must not be written;
- when a single chapter's input cannot fit into the context by itself, or even a single chapter's minimal structured output cannot fit into the completion budget, that chapter and the model's capacity are reported explicitly, and the body is neither truncated nor a trimmed-down success faked.

Therefore growth in the total chapter count only increases the number of batches, and no single response grows without bound with the size of the book; at the same time #83 is not moved from the whole-book granularity to a batch granularity unconstrained by output.

### 9.3 Batch context

A single batch call contains only:

- the original text and titles of the current consecutive chapter range;
- the compact character alias table derived from earlier chapters;
- active foreshadowing IDs with a one-line status each;
- a recent state summary where necessary.

The model processes the chapters in the batch in array order and can carry aliases, foreshadowing and state forward within the batch; after the batch ends, Go updates the compact ledger in the order of the validated facts. It does not depend on the book-wide Premise that has not yet been generated, nor re-read all earlier text. Chapter facts are the input of the Foundation, not the other way around, so no circular dependency forms.

### 9.4 Full-response validation

The code validates structure, value domains and references in two layers, and hard-codes no literary quality:

- batch level: the chapters array is consecutive in the expected chapter numbers, with no duplicates and no gaps, and the batch range, `InputDigest` and schema version match;
- per-chapter level: chapter/title are consistent with the source segmentation, summary/core_event are non-empty, the formal domain closed-set fields such as hook type and strand are legal, and the timeline, foreshadowing and state-change fields have legal types.

The code does not require "must have 3 to 6 events", "must have appearing characters" or "must have three scenes". A quiet chapter, a letter, an environment chapter or a chapter with only unnamed characters are all legal literary shapes.

When a JSON or semantic validation error occurs in a full response, none of its new chapters is committed; the specific error is fed back to the same model and the output-layer retry of §13.3 is used. After correcting, the model may rewrite earlier objects, so an ordinary validation failure must not save a partial array on its own.

### 9.5 Consecutive prefix on length truncation

> Implementation positioning: this section is a **token optimization on the error path**, not something resume correctness depends on. In v1 (Phase 3), truncation means "failure + shrink and regroup the batch", which is correct and recoverable on its own; consecutive-prefix salvage is implemented in a separate sub-phase (Phase 3 supplement) that can be switched on and accepted separately.

Only when the response is explicitly marked `StopReasonLength` and returned parseable partial text is it allowed to save the **maximal consecutive valid prefix** from the failed response:

1. use a streaming JSON decoder to enter the top-level `chapters` array;
2. starting from the first chapter of the batch, read the already fully closed JSON objects one by one;
3. each object independently passes the per-chapter validation of §9.4, and once it forms, together with the earlier objects, a consecutive sequence starting from the first chapter of the batch, it is immediately and atomically written to the corresponding chapter analysis artifact;
4. stop immediately at the first incomplete, illegal, skipped-number or duplicate object, and never interpret any bytes after it;
5. it is forbidden to add closing brackets, continue half a JSON, guess missing fields, or fish non-consecutive objects from later positions;
6. the raw response, the StopReason, the saved prefix range and the first failing chapter are all written to the failure artifact, the events and the logs;
7. `NextAction` regroups batches from the first missing analysis and does not redo the valid prefix that was already committed.

The typed call must record whether usable partial text was obtained this time: non-streaming structured modes such as JSON Schema may fail to give a parseable prefix when stopping on length. If the provider returns no partial text, cannot clearly prove that it was a length truncation, or not even one valid object was completed, then nothing is saved, a `prefix_salvage=unavailable` event/log is emitted, and it falls back to "failure + shrink and regroup the batch" rather than spinning silently. When a single-chapter batch is still truncated, it directly reports insufficient model output capacity, and does not loop on shrinking or manufacture empty facts.

Length truncation is a capacity error: it does not enter the semantic self-repair loop of "feed the validation error back to the same model", and the same batch is not retried unchanged.

### 9.6 Resume

Each chapter analysis is atomically written to `analyses/NNNNNN.json` as soon as it succeeds. After a crash:

- analyses whose `InputDigest` matches are reused directly, with no repeated charge;
- the first missing or mismatched analysis becomes the start of the next batch;
- after an upstream semantic input changes, analyses that cannot rebuild the same `InputDigest` naturally become invalid;
- the consecutive valid prefix committed after a length truncation follows exactly the same resume rules as artifacts from a normal completion;
- the user is not allowed to skip a failed chapter and keep generating non-consecutive later semantic facts.

## 10. Hierarchical synthesis

### 10.1 Why whole-book single-shot output can no longer be used

Book-wide synthesis needs cross-chapter understanding, but it does not need to read all bodies again, and should not output a detailed object for every chapter. The per-chapter facts already contain the chapter-level semantics; synthesis processes only these compact facts.

### 10.2 Map/Reduce shape

```text
ImportedChapterFacts × N
        ↓ split into consecutive ranges by the current context window
RangeDigest × M
        ↓ keep merging if necessary
BookSynthesis
```

For a short book, if all chapter facts fit at once, `BookSynthesis` is generated directly; only a long book produces `RangeDigest`. Whether to use layers is decided mechanically by the token budget, not by a chapter-count threshold.

A `RangeDigest` contains the plot progression, character changes, world facts, opened/closed foreshadowing and candidate structural boundaries of that consecutive range. Its output size is bounded by a single range; the final synthesis no longer re-outputs N detailed chapter objects and outputs only global facts and the volume/arc ranges.

### 10.3 Final synthesis result

```go
type BookSynthesis struct {
	Title         *string                `json:"title"`    // null when the body cannot confirm it; inferred from the file name
	Synopsis      string                 `json:"synopsis"` // reader-facing spoiler-free synopsis
	Premise       string                 `json:"premise"`
	Characters    []domain.Character     `json:"characters"`
	WorldRules    []domain.WorldRule     `json:"world_rules"`
	Structure     []ImportedVolumeRange  `json:"structure"`
	Compass       domain.StoryCompass    `json:"compass"`
	PlanningTier  domain.PlanningTier    `json:"planning_tier"`
	StoryStatus   string                 `json:"story_status"` // open / closed / uncertain
	StatusReason  string                 `json:"status_reason"`
}
```

The structure returns only ranges and does not re-output all chapters:

```go
type ImportedVolumeRange struct {
	Title string             `json:"title"`
	Theme string             `json:"theme"`
	Arcs  []ImportedArcRange `json:"arcs"`
}

type ImportedArcRange struct {
	Title        string `json:"title"`
	Goal         string `json:"goal"`
	StartChapter int    `json:"start_chapter"`
	EndChapter   int    `json:"end_chapter"`
}
```

The model decides the number of volumes and arcs itself; it may consult the group titles in the source file, but is not bound by "one volume" or "1 to 3 arcs". Go assembles the formal `OutlineEntry` from the title/core_event/hook/scenes of `ImportedChapterFacts`.

### 10.4 Story status

Import only rebuilds the facts of the body; it does not fabricate unresolved long-running threads just to keep the Engine going:

- `open`: the body has a real unresolved goal or tension, and the Compass is generated normally;
- `closed`: publish as a completed work, with the last volume marked Final; when a sequel is to be written, the user explicitly reopens and gives a new direction;
- `uncertain`: before publishing, the user is required to choose whether to treat it as unfinished or completed; if Intent already stores a choice via `--story=open|closed`, it is used directly, otherwise it enters an interactive wait. The choice is saved as a `story-resolution.json` artifact whose input is the current synthesis.

The code does not secretly guess the user's intent from whether `open_threads` is empty.

## 11. Foundation assembly and validation

The model outputs the synthesized semantics, and Go assembles the formal domain objects. Before publishing, the following must hold:

1. The Premise has a legal book title; when the body cannot confirm the title, the source file basename is used and the origin is labeled filename, so the model is not allowed to claim it is the "real title";
2. All volume and arc ranges are consecutive in order;
3. The first range starts at chapter 1, and the last range ends at chapter N;
4. Every chapter belongs to exactly one arc;
5. After `FlattenOutline` the chapter count is N, and the titles are consistent with the per-chapter facts;
6. Character names, world rules and the Compass satisfy the existing domain type constraints;
7. PlanningTier is a legal closed-set value, but the reason for choosing it comes from the model rather than a chapter-count threshold;
8. The closed/open status is consistent with the publish shape of Final and Compass;
9. The `InputDigest` of the Synthesis artifact can be rebuilt from the current ordered set of analyses.

When a structural constraint is violated, the specific error is fed back to the model to regenerate, continuing until success or context cancellation; no half-finished product is persisted.

## 12. Formal publishing

### 12.1 Publish preconditions

A new import may only enter when:

- there are no completed chapters;
- there is no in-flight chapter or PendingCommit;
- there is no other import workspace from a different source;
- the formal Foundation is empty, or exactly consistent with the digest already published by the current workspace.

The merge semantics of an existing novel and new external text are unclear, so the first version rejects it explicitly and does not guess at overwriting or appending.

### 12.2 Foundation publishing

Publish in the formal dependency order:

```text
planning tier
→ premise
→ characters
→ world rules
→ layered outline + flat outline
→ compass
→ progress reconciliation
```

For each step:

1. compute the digest of the content to be published;
2. if the formal artifact does not exist, write it atomically and append a checkpoint;
3. if it exists and the digest is identical, skip idempotently;
4. if it exists but differs, return a conflict error and do not overwrite.

After a mid-way crash it is enough to reconcile again from the first item, with no cross-file transaction and no Foundation Pending state machine.

### 12.3 Chapter publishing

Publish in chapter order, reusing the existing flow:

```text
save draft
→ Progress.StartChapter
→ commit_chapter (per-chapter facts)
```

`commit_chapter` already has the PendingCommit saga, checkpoint and the idempotency check for completed chapters. Import does not duplicate a second set of commit logic.

Crash windows:

| Window | Recovery behavior |
|---|---|
| Before draft | Save the same body again |
| After draft, before StartChapter | Reconcile the digest and continue |
| After StartChapter, before PendingCommit | Re-execute the commit of the same chapter |
| During PendingCommit | Recovered by the existing commit saga |
| After chapter complete | Skip if digest/checkpoint are consistent |
| Formal content conflicts with the source digest | Stop explicitly and report the conflicting chapter |

### 12.4 Import completion boundary

After all chapters are stably committed, an `AdvanceHoldAtBoundary` is set once, with the explicit reason "external novel import complete, awaiting acceptance before continuing". It protects only this cross-system import and does not change the user's long-term `auto/review` mode.

`--yes` only authorizes auto-accepting the segmentation and cannot implicitly skip this Hold. Only when the user also passes the independent `--continue` does the Runner not create the import-specific Hold; after that the normal advance mode still applies: `auto` may continue writing, while `review` still waits for `/next`.

The default TUI no longer hands off automatically without a prompt. After the user checks the Foundation and the chapter state, the existing continue entry is used to resume creation.

**Where the panel closes to**: after an import started from the welcome page ends successfully, Esc closing the panel runs `Resume()` once more (bootstrap's resume gate runs only once at startup), so the user lands directly on the workbench, held by the import-completion Hold at the next chapter boundary awaiting acceptance, rather than staying on the welcome page where an accidental Enter would "start a new book". In the error terminal state and in the workbench scenario, the panel is simply closed.

**Guard against new creation**: `PrepareUserRules` / `StartPrepared` refuse to create when the book directory already has completed chapters (`CompletedChapters` non-empty): StartPrepared resets checkpoints and progress right at the start, and a mis-trigger would silently wipe the whole book (including all just-imported chapters). Planning leftovers with no completed chapters are let through, preserving the self-healing paths of same-session retry with co-creation Ctrl+S and the make-up ruling at recovery.

### 12.5 Cross-restart publish gate

When the workspace exists and `NextAction != done`, `Host.New/Resume` must recognize the book as an unfinished import:

- viewing, diagnosing and executing `/import` to resume are allowed;
- starting the normal Engine, Continue, or dispatching the Writer is forbidden;
- the current resume action is shown explicitly, and a partially published Foundation/chapters is not treated as a semantically complete, continuable book.

The gate derives directly from the workspace and the formal Store, with no new `published bool`. This way, after a crash in any window of publishing, the normal creative flow will not consume the half-published state before the Runner has resumed.

## 13. Model call kernel

`imp` keeps a small, dedicated typed-call helper internally, and does not build a general LLM workflow framework.

### 13.1 Model selection

- the architect role model is used by default;
- the model tier of a semantic function is an open knob: segment/analyze/synthesize may each declare a tier, defaulting to architect, and the config layer may point the more mechanical segment at a cheaper tier. This is call configuration; it changes no semantic contract and does not write "single role" in as an architectural premise, so that the cost benefit of "cheap-tier models getting stronger" can also flow in;
  - where it landed: the roles config supports three role keys `import_segment` / `import_analyze` / `import_synthesize`; when not configured they fall back to architect. The dual budget and the thinking/structured options of each function are derived independently from the real capability of its own tier (the small window of a small tier constrains only its own function), and usage is accounted to the role of the tier actually used;
- hook into the failover configured for the selected role;
- use the reasoning effort of the selected role, and decide whether to send the thinking parameter via capability probing;
- write session metadata and usage under the real provider/model;
- join the existing budget sentinel (landed 2026-07-16): before start it goes through `Refuse()` with the same discipline as Start/Resume/Continue; at runtime a hard budget stop cancels the import's own context via `abortWithEvent` (the Host registers the cancel of the exclusive job, instead of merely pausing an Engine that is not running).

### 13.2 Structured output capability

The four kinds of import artifacts share `llmcontract.Execute`: when the model or the user config explicitly supports it, a native JSON Schema is sent; when the capability is unknown or explicitly unsupported, a Prompt Contract is generated automatically from the same Schema. Native mode validates the complete response, and compatibility mode extracts a balanced JSON object; both paths first run the same Schema validation, then decode the DTO and run business validation. After a request error, the schema must not be silently dropped for a retry; a capability misjudgment or a Provider rejection must be surfaced.

### 13.3 Separating request, semantic and capacity failures

- Request-layer errors: only the timeouts, rate limits and network errors that the adapter explicitly marks retryable are retried, using the existing backoff semantics, continuing until success or context cancellation;
- Output-layer errors: the specific JSON parse or Validate error is fed back to the same model, which keeps self-repairing until success or until the context is canceled by the user/budget system; native Schema contract violations, refusals and truncation are not blindly re-asked.
- Retries must not be silent: every request-layer backoff (currently echoed as `Gọi model lỗi (<error>), thử lại lần N`, i.e. "model call failed (<error>), retry attempt N", plus a "Thử lại sau Ns" ("retry in Ns") countdown) and every output-layer re-ask (`Kiểm tra đầu ra không đạt (<error>), hỏi lại lần N kèm phản hồi lỗi`, i.e. "output validation failed (<error>), re-asking attempt N with error feedback") are echoed to the import panel as progress events; without an echo the user would misjudge it as hung. A backoff event carries only the deadline (`RetryAt`), and the remaining seconds are computed by the rendering layer on every tick to form a live countdown (sharing the same mechanism with the event panel of the creation workbench); while running, the panel keeps a spinner + elapsed time at the top, and the log tail has the same streaming star cursor.
- Error echo must not be vague: a gateway message is often just one sentence "Provider returned error"; the echo and the failure text uniformly append the adapter's structured facts (error category / HTTP status / provider / model, extracted by `modelErrDetail` from the litellm error chain via errors.As), with the facts placed first so they are preserved first when truncating.
- Long-running stages must not be silent: segmentation per chunk and synthesis per range call the model inside the function (a single chunk can take several minutes), and progress is echoed per chunk / per range through `callProfile.step` (currently `Chia khối N/M (a..b), đã nhận diện K ranh giới...`, i.e. "segmenting chunk N/M, K boundaries identified so far"). The event Key is given only to request backoff (a transient inside one call, updated in place); validation re-asks are cross-call semantic events, each kept on its own line to preserve history; sharing a Key would let later chunks overwrite earlier ones, losing all troubleshooting clues.
- Full-log transcript: every progress event (including retry lines overwritten in place by the panel) is written to the **import-specific log** `<book root>/logs/import.log` (not mixed with the tui.log stream, so one import gets one file with the complete transcript); request backoffs and semantic re-asks are also written to the same log with the full error chain.
- Echo model semantics rather than only mechanical counts: segmentation echoes, per chunk, the titles the model recognized (`Model nhận diện: <title> / <title> / … (tổng N chỗ)`, i.e. "model recognized: … (N in total)", for example with titles like 第十二章 风雪夜, "Chapter 12 Snowy Night"), analysis echoes the core event per chapter (`Chương 12 "<title>": <core event>`), and when synthesis completes it echoes the book-wide summary (`Model tóm lược toàn sách: <premise summary>`, i.e. "model's whole-book summary") — the user should see what the model understood. <!-- han:keep -->
- Capacity errors: `StopReasonLength` is neither retried unchanged nor sent into the semantic self-repair loop; for an analysis batch, when the partial text is parseable, the consecutive valid prefix is saved per §9.5, otherwise `prefix_salvage=unavailable` is recorded and the batch is shrunk and regrouped; the other semantic functions fail explicitly and directly and keep the raw response.

Authentication, permission, model-unsupported and state-conflict errors fail immediately. There is no simulated success, no empty-object fallback, and no skipping of failed chapters.

### 13.4 Input and output budgets

Each semantic function has its own schema, input budget, reasoning reserve and visible output budget:

- segmentation output contains only the boundaries of the current owned range;
- an analysis batch is constrained by both the context window and the completion limit, and its output is the per-chapter facts of a bounded consecutive range;
- a RangeDigest contains only one consecutive range;
- BookSynthesis contains only global facts and volume/arc ranges, and does not repeat chapter objects.

Before every request is sent, the estimated input, the reasoning reserve, the requested max tokens and the estimated visible output are recorded. The estimate decides only chunking/batching and deletes no body or fact field. Therefore there is no structure in which "the more total chapters, the longer some single response must be", and the risk of output truncation cannot be ignored merely because the input fits.

## 14. Events, logs and diagnostics

### 14.1 Event stages

```go
const (
	StageIngesting            Stage = "ingesting"
	StageSegmenting           Stage = "segmenting"
	StageAwaitingConfirmation Stage = "awaiting_confirmation"
	StageAnalyzing            Stage = "analyzing"
	StageSynthesizing         Stage = "synthesizing"
	StageAwaitingStoryStatus  Stage = "awaiting_story_status"
	StageValidating           Stage = "validating"
	StagePublishing           Stage = "publishing"
	StageDone                 Stage = "done"
	StageError                Stage = "error"
)
```

Each event contains the action, the current chapter/range, the total, the elapsed time and an optional error. Analysis batch events additionally contain the batch range, the budget estimates, the StopReason and the committed prefix range. Events are a projection and take no part in resuming.

### 14.2 Errors must reach three places at once

1. The TUI import panel: auto-wrapped, keeping the full error chain;
2. `tui.log`: structured record of stage, chapter/range, model, attempt and error;
3. `meta/import/failures/`: saves the last failure metadata and the untrimmed model response.

The original novel body is not written to ordinary logs and does not enter the default redacted diagnostic export. The failure response lives in the user's own book directory, and the error message gives the path explicitly.

### 14.3 Session and Usage

Every semantic call records:

- a stable task name, such as `import/segment/0003`, `import/analyze/0054-0061`;
- the raw assistant response;
- provider/model and usage;
- structured mode, thinking level and the output validation result.

Usage is accounted uniformly to the architect role, so the budget sees the import cost.

## 15. Lifecycle and concurrency

- Import is mutually exclusive with write operations of the Engine, stage co-creation and simulation;
- only one Runner is allowed for the same book during import;
- user cancellation cancels in-flight model calls, and workspace facts already atomically persisted are kept;
- canceling before confirmation does not modify the formal Store;
- canceling after publishing has started does no speculative rollback; the next run can only resume publishing precisely;
- in the first version analysis batches are serial; within a batch, one model call returns the facts in chapter order, and formal publishing remains serial per chapter;
- `Host.New/Resume` applies the §12.5 gate while an import is unfinished, so the mutual-exclusion semantics hold across process restarts;
- whether export may run concurrently keeps the existing read-only semantics, but it can only see chapters that have already been formally published.

## 16. Core invariants

1. Every workspace artifact is identified by `SchemaVersion + InputDigest + Payload`; it can be reused only if the same `InputDigest` can be rebuilt from the current real semantic inputs.
2. The Manifest corresponds to exactly one normalized source snapshot; every non-empty piece of source text must have one and only one attribution.
3. The model may only reference the SourceUnit, source anchors and chapter numbers provided by the Host; Go accepts only coordinates that map uniquely back to source bytes.
4. An analysis batch may only commit a complete response, or, under `StopReasonLength`, the maximal consecutive valid prefix starting from the first chapter; any missing chapter blocks subsequent analysis and synthesis.
5. Volume/arc ranges must be consecutive, non-overlapping and fully cover `1..N`; the formal Foundation may only be published from a Synthesis that passed full validation.
6. Formal chapters may only be published in order through `commit_chapter`; an existing formal artifact may be reused idempotently only when the content digest is identical, and a difference fails as a conflict.
7. No model failure may be interpreted as "no content" or "go on to the next chapter", and half a JSON must not be repaired nor a failed chapter skipped.
8. `done` must be jointly proved by the workspace artifacts, the formal artifacts, Progress, PendingCommit and checkpoints; the normal Engine must not start before `done`.

## 17. Package structure and narrow interfaces

Keep `internal/host/imp`, split by responsibility:

```text
imp/
├── types.go       public Options/Event and semantic DTOs
├── source.go      reading, decoding, normalization, SourceUnit/anchor
├── workspace.go   meta/import atomic artifacts and InputDigest
├── call.go        import-specific typed LLM call
├── segment.go     structural projection, boundary semantic function, coverage validation
├── analyze.go     dual-budget consecutive batches, per-chapter facts and truncated prefix
├── synthesize.go  RangeDigest and BookSynthesis
├── publish.go     Foundation reconciliation and commit_chapter publishing
└── runner.go      LoadState → NextAction → execute
```

No `ImportEngine`, `Task`, `WorkflowInstance`, generic Repository or plugin registry is added.

The dependencies injected by the Host stay narrow:

```go
type Deps struct {
	Store         *store.Store
	CommitChapter ChapterCommitter
	Model         agentcore.ChatModel
	Runtime       ModelRuntime
	Prompts       Prompts
	Emit          func(Event)
}
```

`ModelRuntime` carries only call facts such as the context window, the completion limit, thinking, and session/usage callbacks, and reserves a model-tier selection slot for each semantic function (default architect); it neither makes `imp` depend back on the whole Host nor welds a single role in as an architectural premise.

## 18. User interface

### 18.1 New import

```text
/import <path> [--yes] [--story=open|closed] [--continue] [--guide=<segmentation guidance>]
```

Default behavior: create the source snapshot, segment semantically and open the confirmation preview; after publishing completes, set the import-specific Hold once. `from=N` is deleted.

The first three options are mutually independent explicit authorizations and are written to `intent.json`:

- `--yes`: auto-accept the segmentation once coverage validation passes; it does not decide the uncertain story status and does not skip the completion Hold;
- `--story=open|closed`: provides the user's choice in advance only when synthesis returns uncertain; when the model has clearly judged open/closed, the model's fact is not overridden;
- `--continue`: do not create the import-specific Hold; it does not bypass the normal advance mode, and under `review` `/next` is still needed.

`--guide` differs from the first three: it is not a start authorization but a semantic input of segmentation, persisted as the workspace `guidance.txt` (it may contain spaces and must be placed at the end of the command). See §18.3.

Therefore `/import book.txt --yes` still stops after the import completes; only by additionally passing `--continue` is the creation flow authorized to hand off when the normal gates allow.

### 18.2 Resume

When an active workspace exists for the same book, running `/import` with no arguments derives the next step directly from the facts and the saved intent; neither the source file path nor the start arguments are required for resuming. `/import <path>` with a new path must not overwrite an active workspace.

An unfinished import must be proactively visible and must not wait until creation is rejected by the gate to surface. It is implemented as three progressive hints:

1. At startup the TUI checks once (`imp.ResumeSummary`, which generates a staged description from `NextAction`), and the welcome screen prominently shows an unfinished-import notice (for example `Phát hiện lượt nhập chưa hoàn tất (Đã phân tích N/M chương), nhập /import để tiếp tục từ điểm dừng`, i.e. "unfinished import detected (N/M chapters analyzed so far), enter /import to continue from where it stopped");
2. When the user ignores the hint and tries to create, the cross-restart gate (§12.5) refuses the engine start and emits a warning event;
3. During a resumed run, the import panel shows the current stage and progress in real time.

### 18.3 Re-segmentation

After checking the preview, the user re-recognizes with `/import --guide=<natural-language note>`, for example `--guide=幕间·X 也是独立章节` ("Interlude · X is also a standalone chapter"). The guidance is written to the workspace `guidance.txt` and included in the `InputDigest` of segmentation: a change of guidance makes the old segmentation and the old confirmation, analyses and synthesis unable to rebuild the same `InputDigest`, so everything is naturally redone; no regex editor is provided. <!-- han:keep -->

### 18.4 Cancel

Canceling before confirmation only keeps the workspace; before publishing, the whole workspace can be discarded explicitly. After publishing has started, no discard operation that "pretends nothing happened" is provided; the only options are to finish resuming or for the user to handle the formal book separately.

## 19. Implementation order

### Phase 1: Workspace and pure state derivation

- Manifest, Intent, source snapshot, `Artifact/InputDigest`, atomic reads and writes;
- `LoadState/NextAction`;
- precondition validation for an empty book and same-source resume;
- delete the design dependence on `ResumeFrom`.

This phase calls no model; it first proves that the resume facts are unambiguous.

### Phase 2: Semantic segmentation and confirmation

- SourceUnit, virtual slices of over-long lines, source anchors and context-budget chunking;
- the BoundaryDecision typed call;
- full-text coverage validation;
- TUI preview, natural-language re-recognition, `--yes` and the confirmation artifact.

First verify "not one character missed" with non-standard titles, volume titles, prefaces and trailing notes.

### Phase 3: Per-chapter facts over consecutive batches

- `ImportedChapterFacts`;
- context/completion dual-budget batch planning;
- sequential analysis between batches and the compact continuity ledger;
- per-chapter `InputDigest` artifact resume;
- truncation means "failure + shrink and regroup the batch", and records whether partial text was usable;
- wiring of session, usage, failover, thinking, capacity errors and structural-feedback retries.

### Phase 3 supplement: Truncated-prefix salvage (efficiency optimization, can be deferred)

- parsing the `StopReasonLength` consecutive valid prefix (§9.5);
- enabled only when the partial text is parseable, and does not change resume correctness; separate switch, separate acceptance.

### Phase 4: Hierarchical synthesis and Foundation

- context-aware RangeDigest;
- BookSynthesis;
- range-style volume/arc structure;
- StoryStatus;
- complete Foundation assembly and validation.

### Phase 5: Publishing and handoff

- Foundation per-artifact digest reconciliation;
- publish by reusing `commit_chapter`;
- cancel/crash recovery;
- cross-restart Engine gate;
- default import-completion AdvanceHold and explicit `--continue`;
- complete TUI/log/failure artifact.

### Phase 6: Delete the old implementation

- delete the chapter-format adjudication in `splitter.go`;
- delete the tagged envelope;
- delete the whole-book `ReverseFoundation` call;
- delete the `pickScale` chapter-count thresholds;
- delete `ResumeFrom/from=N`;
- delete the prompt constraints of "fixed one volume, 1 to 3 arcs, forced open threads";
- after the implementation is complete, update the old-flow descriptions in README and architecture.

## 20. Testing and acceptance

### 20.1 Pure-function and property tests

- any legal segmentation satisfies full-text ranges with no overlap and no gap;
- an illegal SourceUnit, a non-unique source anchor, reversed order and duplicate boundaries are always rejected;
- boundary order is judged by the `(Line, Part)` numeric order; construct a unit set where "lexicographic order and numeric order give opposite conclusions" and assert it passes under the numeric order;
- both normal lines and virtual slices can be mapped losslessly back to the same normalized source bytes;
- any legal volume/arc ranges cover exactly `1..N`;
- the same semantic input stably produces the same `InputDigest`, and a change of any real input makes the corresponding artifact mismatch;
- dual-budget batching never exceeds the given context/completion constraints;
- NextAction is constant for the same fact snapshot.

Fuzz/property tests are done on coordinate mapping, range assembly, batch budgeting and `InputDigest`, without asserting that the model outputs any fixed title.

### 20.2 Model contract tests

- non-standard chapter names and mixed volume/chapter structures;
- a prologue / preface chapter / side story judged by the model semantically to be a chapter;
- front/back matter is shown explicitly rather than discarded;
- a whole single-line book, several chapters on one line, and over-budget lines are cut precisely through SourceUnit + anchor;
- a quiet chapter is allowed to have empty characters;
- illegal JSON, missing fields and out-of-range ranges enter the feedback retry;
- an analysis batch returns consecutive per-chapter objects, with no skipped or duplicate numbers;
- `StopReasonLength` saves only the maximal consecutive valid prefix; half an object and later non-consecutive objects are not saved;
- when a structured mode produces no parseable partial text, assert that it takes "failure + shrink and regroup the batch" and that the log is annotated `prefix_salvage=unavailable`;
- corrupted JSON under an ordinary `StopReasonStop` does not enter the truncated-prefix path;
- when a single chapter is still truncated it fails explicitly, without generating empty facts;
- parse/business errors of the Prompt Contract keep feeding back for self-repair until success or context cancellation; native Schema contract violations, refusals and truncation immediately keep the raw response and terminate;
- models that do not support thinking/JSON Schema do not receive illegal parameters.

Model tests assert contracts and invariants, not exact literary judgments.

### 20.3 Crash matrix

Cover at least:

- after the source snapshot;
- after segmentation, before confirmation;
- before and after the N-th object of an analysis batch is persisted;
- before and after the last chapter of a length-truncation prefix is persisted;
- in the middle of a RangeDigest;
- after Synthesis, before the Foundation;
- before and after each Foundation artifact;
- each window of draft/StartChapter/PendingCommit/progress/checkpoint;
- after the last chapter commits, before and after AdvanceHold;
- restarting after the Foundation/chapters are partially published and attempting a normal Host.Resume.

After a restart in each window, only the current action may continue; a successful model call must not be consumed again, and a failed artifact must not be skipped past. When `NextAction != done`, the normal Engine must be blocked by the gate until import resume is complete.

### 20.4 #83 regression shape

Construct inputs of 54 chapters and longer, and verify:

1. No single call is required to output a detailed 54-chapter outline;
2. An analysis batch is grouped according to both the input context and the visible-output completion budget, and does not pack in too many chapters merely because the input fits;
3. Phase 3 supplement: simulate a `StopReasonLength` of "the first 13 chapters complete, chapter 14 truncated", commit only the first 13 chapters, and the next action starts from chapter 14; before prefix salvage is implemented, the whole batch fails and the batch is regrouped from its first chapter;
4. Simulate a truncated response with no complete object: the error is shown in full, written to the log, the raw response is saved, and no analysis artifact is persisted;
5. Simulate ordinary corrupted JSON: it takes the structural-feedback retry rather than prefix salvage;
6. After the fix, only the first missing action is rerun, and completed chapters are not redone;
7. Non-standard titles enter the preview through semantic segmentation, and are not fixed by adding new regexes.

### 20.5 Final acceptance criteria

1. The default interactive mode lets the user see and confirm all chapter boundaries before the formal write to disk; `--yes` can explicitly auto-accept and leaves equivalent audit artifacts.
2. Any non-empty source text can find its unique attribution in the segmentation.
3. 200 to 500 chapters do not form a single model call that reads the whole book body and outputs all chapter objects; each analysis batch is constrained by both input and output budgets, and the global output expresses only global facts and volume/arc ranges.
4. After a crash in any stage, precise resume works without `from=N`.
5. The formal state stays unchanged until complete semantic validation.
6. After an interrupted publish the existing commit saga can resume, and chapters are not committed twice.
7. After a restart, an unfinished import cannot start the normal Engine; it can only be viewed, diagnosed or resumed.
8. `--yes` does not skip the completion Hold; only the independent `--continue` skips it, and it does not bypass the review gate.
9. The capabilities, usage, StopReason, budget estimates and errors of models and providers are all observable.
10. Switching to a stronger model improves segmentation, analysis and synthesis quality, and naturally enlarges the safe batch and reduces the number of calls, without modifying Go literary rules.

## 21. Future extensibility

The extensibility of this design comes from stable boundaries, not from pre-built abstractions:

- stronger model understanding: the three kinds of semantic function (Boundary/Chapter/Synthesis) become more accurate directly;
- a larger context or output window: the dual-budget planner automatically enlarges the safe analysis batch and reduces the number of chunks and Reduce layers;
- stronger structured output: the typed call automatically selects stronger provider constraints;
- cheap-tier models getting stronger: the more mechanical segment can be switched to a cheaper tier, and the cost benefit flows in with it, without changing the semantic contract;
- new input formats: only requires converting EPUB and the like to the same normalized text and SourceUnit coordinates;
- new book-wide semantics: add a field with a clear consumer in `ImportedChapterFacts` or `BookSynthesis`, without changing the resume and publish protocols;
- stronger user co-creation: add natural-language correction at the confirmation boundary, without writing format knowledge into the code.

What does not change is full-text coverage, `InputDigest` identity, range validation and idempotent publishing. These are bookkeeping that is not worth handing to the model no matter how strong it becomes; everything variable and semantic stays in the model functions, so the benefit of model upgrades can pass through to product results.

## 22. Final decision

Adopt a **staged semantic import pipeline**, rejecting two directions:

1. continuing to enlarge the chapter regexes and the chapter-count/arc-count thresholds;
2. handing the whole import over to a freely running long-loop Agent.

The final boundary is:

> **The model decides what the text means; the code guarantees where every character went, which input every result corresponds to, that the input and output of every call fit, where to continue after a failure, and when something qualifies to become a formal fact.**

This both preserves the model's autonomy and its future benefits, and keeps ainovel-cli's current lean architecture of Engine + typed semantic functions + a file-based fact layer.
