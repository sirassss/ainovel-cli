# Observability Handbook

When running a long novel, how do you know whether each mechanism is actually working?

This document is not a copy of the diag rules. It is oriented toward **actual runs**: you have reached chapter N, so which file should you open, which field should you look at, and how do you tell healthy from abnormal.

---

## 1. General troubleshooting flow

```
1. /diag                       # automatic diagnosis; check the Findings section
2. cd output/{novel}/meta/     # cat the key artifacts directly
3. tail decisions.jsonl                # look at the most recent Arbiter rulings
4. ls -lt sessions/agents/             # find the most recent Worker session, then tail it
```

Facts that `/diag` does not cover (including the "pending diagnostics" items listed in this document) have to be checked by hand in steps 2-4.

### Filing an issue: the redacted diagnostic export

Every `/diag` run additionally writes `output/{novel}/meta/diag-export.md`, a **redacted** diagnostic (novel text / prompts / thinking removed, keeping only the behavioral skeleton: tool names, error strings, repeat counts, phase/flow, the stuck step, log error categories). For deadlock / interruption problems, paste this file into the GitHub issue; maintainers can locate the problem from it without needing the user's `output/` data.

---

## 2. Quick reference of key artifacts

Ordered by "the most common troubleshooting path when something goes wrong":

| Artifact | Path | What to look at | Healthy | Unhealthy |
|---|---|---|---|---|
| Progress | `meta/progress.json` | `phase` / `flow` / `completed_chapters` | phase advances monotonically, flow stays within the legal set | phase goes backward / flow is stuck in one state |
| Compass | `meta/compass.json` | gap between `last_updated` and the latest chapter | gap < 15 chapters | gap > 15 chapters (CompassDrift fires) |
| Supporting cast ledger | `meta/cast_ledger.json` | entry count / brief_role fill rate / name consistency | see §4 | see §4 |
| Foreshadowing ledger | `meta/foreshadow.json` | the longest stall (in chapters) of any `status="planted"` entry | < chapter count/3 | > chapter count/3 (StaleForeshadow fires) |
| Outline | `meta/layered_outline.json` | number of unwritten chapters remaining in the current volume | expanded 1-2 chapters ahead | writing the current chapter but the next chapter has no outline (OutlineExhausted) |
| Character profiles | `meta/characters.json` | whether core/important characters can be found in the last N chapter summaries | all found | absent (GhostCharacter fires) |
| Checkpoints | `meta/checkpoints.jsonl` | whether the `step` on the last line matches progress | consistent | inconsistent (crash recovery did not self-heal) |
| Ruling audit | `meta/decisions.jsonl` | facts/decision of the most recent rulings | accurate triage, sensible actions | rulings for the same kind of intervention keep failing |

---

## 3. Compass observability

**Fix date**: 2026-05-08 (commit `fix: update_compass tool auto-fills last_updated`)

### What to look at

```bash
cat output/{novel}/meta/compass.json
```

Field semantics:
- `ending_direction`: the ending direction (should match the "ending direction" section of `premise.md`)
- `open_threads`: active long-running threads (added or removed by the architect at each volume boundary)
- `estimated_scale`: estimated scale (e.g. "4-6 volumes", updated at each volume boundary)
- `last_updated`: **filled automatically by the tool** with the highest completed chapter number at update time (no longer relies on the LLM filling it in)

### Health assessment

| Signal | Assessment |
|---|---|
| `last_updated` is within `[latest-15, latest]` | Healthy |
| `last_updated` lags latest by more than 15 chapters | the architect did not update at arc/volume boundaries; check the architect-long.md prompt |
| `last_updated == 0` | **dirty data from before this fix**; the next update_compass will self-heal it |
| `ending_direction` does not match the "ending direction" section of premise.md | the architect quietly changed the user's intent; record it and decide whether to freeze the field (a design topic, see todo.md) |

### How to verify the fix works

Compare before and after running a long novel:
- **Before the fix**: after 30+ chapters, `compass.last_updated` is very likely `0` or some early chapter number
- **After the fix**: every time the architect calls `update_compass`, `last_updated` is overwritten by the tool layer with the current latest

---

## 4. Supporting cast ledger (cast_ledger) observability

**Feature landed**: 2026-05-08 (commit `feat: add supporting cast ledger to automatically track minor characters`)

### What to look at

```bash
cat output/{novel}/meta/cast_ledger.json | jq 'length'                     # total entry count
cat output/{novel}/meta/cast_ledger.json | jq '[.[] | select(.brief_role == "" or .brief_role == null)] | length'  # count missing brief_role
cat output/{novel}/meta/cast_ledger.json | jq '[.[] | select(.appearance_count >= 3)] | length'   # count of frequent appearances (>= 3)
cat output/{novel}/meta/cast_ledger.json | jq 'sort_by(-.appearance_count) | .[:10]'  # the 10 most frequent appearances
```

### Health assessment

| Dimension | Healthy | Abnormal | Response |
|---|---|---|---|
| **Entry count vs completed chapters** | ledger entry count ≈ completed chapters × 0.3-0.6 | > chapters × 0.8 (walk-on characters wrongly entered into the ledger) | check whether the `cast_intros` section of writer.md is explicit enough |
| **brief_role fill rate** | missing < 30% | missing > 50% | the Writer is badly under-filling it; the prompt guidance is insufficient |
| **Name similarity** | no suspected same-person multiple names | "李X" / "老李" / "X掌柜" appear at the same time (Li X / Old Li / Shopkeeper X) | LLM name drift: add a "use a consistent name" constraint to the prompt, or add a user steer merge tool | <!-- han:keep -->
| **Frequent appearances** | entries with `appearance_count >= 5` are rare | many entries appear frequently across arcs | consider promoting them to the core profiles (the stage-3 promotion path) |
| **Whether recall is consumed** | when the Writer writes an old character, the characters field of commit_chapter includes a name already in the ledger | the Writer reinvents the same name ("老周A" and "老周B" appear, i.e. Old Zhou A and Old Zhou B) | recent_cast recall is not being consumed; check the "supporting cast continuity" section of writer.md | <!-- han:keep -->

### Data flow verification (end to end)

After running 5 chapters:
1. `cat meta/cast_ledger.json` should be non-empty (unless every chapter uses only core characters)
2. If the Writer introduced "老周" (Old Zhou) in chapter 1: <!-- han:keep -->
   - `cast_ledger` should have a `老周` entry with `appearance_count=1` <!-- han:keep -->
3. If chapter 5 writes Old Zhou again:
   - `老周.appearance_count=2`, `last_seen_chapter=5` <!-- han:keep -->
4. In `meta/sessions/agents/writer-*.jsonl`, the novel_context return value for chapter 5 should show Old Zhou in `episodic_memory.recent_cast`
5. If the previous step shows it but the Writer did not consume it (the Old Zhou it wrote does not match chapter 1), this is a prompt problem

### No automatic diagnostics yet (but the snapshot is already loaded)

`diag.Snapshot.CastLedger` is already read in `Load()` and can be consumed directly by rules, but no rules have been written yet. Verification still relies on the manual `jq` commands above.

Candidate diagnostic rules to add later:
- `CastBriefRoleMissing`: alert when the missing rate is > 50%
- `CastBloat`: alert when the entry count is > chapters × 0.8
- `CastPromotionCandidate`: appearance_count ≥ 5 and spanning arcs → suggest promotion

Do not fix the thresholds now; wait for long-novel data and decide from the real distribution. The rule code itself needs only 30-50 lines.

---

## 5. Is the Writer working as expected

When running a long novel, the main concern is **whether the Writer really follows the prompt**. The most direct observation is the session log:

```bash
ls output/{novel}/meta/sessions/agents/    # one jsonl per subagent
tail -50 output/{novel}/meta/sessions/agents/writer-*.jsonl
```

Look at a few specific behaviors:

| Expected behavior | How it shows up in the jsonl |
|---|---|
| The Writer looked at recent_cast | the `episodic_memory.recent_cast` field in the novel_context tool return value is non-empty |
| The Writer filled cast_intros in commit_chapter | the tool_call argument `cast_intros` array is non-empty (only in chapters that introduce new characters) |
| The Writer used the related-chapter recommendations | `read_chapter` call count > 1 (default is 1; more means it looked back) |
| The Writer did not violate the tool order | the tool_call sequence is strictly `novel_context → read_chapter → plan_chapter → draft_chapter → check_consistency → commit_chapter` |

If the jsonl shows the Writer calling novel_context empty multiple times, or calling other tools after commit_chapter, the prompt is not holding it in check.

---

## 6. Red lines for long-run scenarios

When running a 100+ chapter novel, if any one of the following hits, stop and investigate:

- [ ] CompassDrift fires and is not cleared for 2 consecutive arcs
- [ ] cast_ledger entry count > completed chapters × 0.8
- [ ] brief_role fill rate in cast_ledger < 30%
- [ ] the same character appears under suspected multiple names ("老李" / "李掌柜" coexist, i.e. Old Li / Shopkeeper Li) <!-- han:keep -->
- [ ] when writing a new chapter, the Writer does not read the existing old characters in recent_cast (reinventing them)
- [ ] ≥ 5 consecutive empty novel_context calls appear in a Worker session
- [ ] after any chapter commit, `meta/checkpoints.jsonl` has no corresponding `commit_chapter` step

The first 4 are health checks for the new mechanisms introduced this time; the last 3 are stability checks for existing mechanisms.

---

## 7. Document maintenance conventions

**When adding a new fact-layer artifact (a new `meta/*.json` / `meta/*.jsonl`), also:**

1. Add a quick-reference row to §2 of this document
2. If the artifact needs dedicated observation (not just a simple "exists / does not exist" check), add a §X topic section
3. If you want automatic diagnostics, load it in `internal/diag/snapshot.go::Load` and add rules in `internal/diag/rules_*.go`

**Do not:**
- Do not copy all the rules in `internal/diag/` into this document (that is a rule reference, not an observability handbook)
- Do not write a diagnostic rule for every mechanism; thresholds set by guesswork will be wrong, so observe first and add rules later
