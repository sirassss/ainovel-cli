package imp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"maps"
	"os"
	"slices"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// analysisSchemaVersion is the per-chapter facts schema version, included in InputDigest.
const analysisSchemaVersion = 2

// ImportedCharacterFact / ImportedWorldFact are compact observations used for whole-book synthesis; they do not directly write formal characters or world rules.
// They carry at least the chapter number so synthesis results have a stable source (RFC §9.1).
type ImportedCharacterFact struct {
	Chapter int    `json:"chapter"`
	Name    string `json:"name"`
	Note    string `json:"note,omitempty"`
}

type ImportedWorldFact struct {
	Chapter  int    `json:"chapter"`
	Category string `json:"category,omitempty"`
	Fact     string `json:"fact"`
}

// ImportedChapterFacts is the structured product of reverse-engineering a single chapter (RFC §9.1).
type ImportedChapterFacts struct {
	Chapter             int                        `json:"chapter"`
	Title               string                     `json:"title"`
	Summary             string                     `json:"summary"`
	KeyEvents           []string                   `json:"key_events"`
	CoreEvent           string                     `json:"core_event"`
	Hook                string                     `json:"hook,omitempty"`
	Scenes              []string                   `json:"scenes,omitempty"`
	Characters          []string                   `json:"characters,omitempty"`
	CharacterEvidence   []ImportedCharacterFact    `json:"character_evidence,omitempty"`
	WorldEvidence       []ImportedWorldFact        `json:"world_evidence,omitempty"`
	TimelineEvents      []domain.TimelineEvent     `json:"timeline_events,omitempty"`
	ForeshadowUpdates   []domain.ForeshadowUpdate  `json:"foreshadow_updates,omitempty"`
	RelationshipChanges []domain.RelationshipEntry `json:"relationship_changes,omitempty"`
	StateChanges        []domain.StateChange       `json:"state_changes,omitempty"`
	HookType            string                     `json:"hook_type"`
	DominantStrand      string                     `json:"dominant_strand"`
}

// AnalysisBatchResult is the structured return of one batch call; each element is one chapter's facts.
type AnalysisBatchResult struct {
	Chapters []ImportedChapterFacts `json:"chapters"`
}

// ChapterAnalysisPayload is the payload of a single-chapter analysis artifact; chapters in the same batch record the same BatchStart/BatchEnd.
type ChapterAnalysisPayload struct {
	BatchStart int                  `json:"batch_start"`
	BatchEnd   int                  `json:"batch_end"`
	Facts      ImportedChapterFacts `json:"facts"`
}

// AnalyzeBudget is the dual input/output budget for per-chapter analysis (RFC §9.2).
// Input approximates the context window in bytes; output approximates the completion limit by reserving a conservative amount of facts per chapter.
type AnalyzeBudget struct {
	ContextBytes     int // input budget (body text + ledger + overhead)
	MaxOutputTokens  int // visible output budget (completion limit)
	PerChapterOutput int // conservative output reserve per chapter
	PromptOverhead   int // fixed input overhead of system/ledger (bytes)
}

func analysisPath(chapter int) string {
	return fmt.Sprintf("%s/%06d.json", dirAnalyses, chapter)
}

// analyzedChapters returns the number of analysis artifacts contiguous from chapter 1 whose InputDigest matches the current segmentation identity / version / body text (RFC §9.6).
// Missing, parse failure or digest mismatch all truncate here, so upstream changes (re-segmentation, prompt/schema version change) naturally invalidate downstream analysis.
func analyzedChapters(w *Workspace, seg *Segmentation, normalized []byte, segIdentity, promptVersion string) int {
	n := 0
	for c := 1; c <= len(seg.Chapters); c++ {
		a, err := readArtifact[ChapterAnalysisPayload](w, analysisPath(c))
		if err != nil {
			break
		}
		if a.InputDigest != chapterInputDigest(segIdentity, promptVersion, seg, normalized, c-1) {
			break
		}
		n++
	}
	return n
}

// analyzedChaptersStrict has the same freshness semantics as analyzedChapters, but surfaces corrupt or unreadable
// existing artifacts. State recovery uses the strict version, to avoid treating a real read error as "not yet analyzed" and overwriting with a redo.
func analyzedChaptersStrict(w *Workspace, seg *Segmentation, normalized []byte, segIdentity, promptVersion string) (int, error) {
	n := 0
	for c := 1; c <= len(seg.Chapters); c++ {
		a, err := readArtifact[ChapterAnalysisPayload](w, analysisPath(c))
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("Đọc kết quả phân tích của chương %d: %w", c, err)
		}
		if a.InputDigest != chapterInputDigest(segIdentity, promptVersion, seg, normalized, c-1) {
			break
		}
		n++
	}
	return n, nil
}

// discardAnalysesAfter deletes per-chapter analysis artifacts with chapter number > keep, making "re-analyzing a chapter invalidates all analyses after it" hold (#4a).
// In normal forward analysis there are no artifacts after keep anyway, so it is an idempotent no-op; it cleans the stale tail only when re-analyzing midway (past the fresh prefix).
// A deletion failure must propagate: this is the sole enforcement point of that invariant, and swallowing the error would let the stale tail (whose per-chapter digest always matches)
// be reused as a fresh prefix, so synthesis would consume a mix of old and new facts with no error at all.
func discardAnalysesAfter(w *Workspace, keep, total int) error {
	for c := keep + 1; c <= total; c++ {
		if err := os.Remove(w.path(analysisPath(c))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("Dọn kết quả phân tích cũ %s: %w", analysisPath(c), err)
		}
	}
	return nil
}

// loadPriorFacts reads the persisted facts of chapters 1..count, for ledger construction.
func loadPriorFacts(w *Workspace, count int) []ImportedChapterFacts {
	var out []ImportedChapterFacts
	for c := 1; c <= count; c++ {
		a, err := readArtifact[ChapterAnalysisPayload](w, analysisPath(c))
		if err != nil {
			break
		}
		out = append(out, a.Payload.Facts)
	}
	return out
}

func loadPriorFactsStrict(w *Workspace, count int) ([]ImportedChapterFacts, error) {
	out := make([]ImportedChapterFacts, 0, count)
	for c := 1; c <= count; c++ {
		a, err := readArtifact[ChapterAnalysisPayload](w, analysisPath(c))
		if err != nil {
			return out, fmt.Errorf("Đọc dữ kiện phân tích của chương %d: %w", c, err)
		}
		out = append(out, a.Payload.Facts)
	}
	return out, nil
}

// buildLedger derives compact continuity context from analyzed chapters: character aliases + active foreshadowing IDs + latest state.
func buildLedger(prior []ImportedChapterFacts) string {
	if len(prior) == 0 {
		return ""
	}
	names := map[string]bool{}
	active := map[string]string{} // foreshadow id -> desc
	var recent []string
	for _, f := range prior {
		for _, c := range f.Characters {
			names[c] = true
		}
		for _, fu := range f.ForeshadowUpdates {
			switch fu.Action {
			case "plant", "advance":
				if fu.Description != "" {
					active[fu.ID] = fu.Description
				} else if _, ok := active[fu.ID]; !ok {
					active[fu.ID] = ""
				}
			case "resolve":
				delete(active, fu.ID)
			}
		}
	}
	if len(prior) > 0 {
		last := prior[len(prior)-1]
		for _, sc := range last.StateChanges {
			recent = append(recent, fmt.Sprintf("%s.%s=%s", sc.Entity, sc.Field, sc.NewValue))
		}
	}
	var b strings.Builder
	if len(names) > 0 {
		b.WriteString("Nhân vật đã biết: ")
		b.WriteString(strings.Join(slices.Sorted(maps.Keys(names)), ", "))
		b.WriteString("\n")
	}
	if len(active) > 0 {
		b.WriteString("Chi tiết gài trước đang hoạt động (dùng lại ID, đừng tạo mới):\n")
		for _, id := range slices.Sorted(maps.Keys(active)) {
			fmt.Fprintf(&b, "- %s: %s\n", id, active[id])
		}
	}
	if len(recent) > 0 {
		b.WriteString("Trạng thái gần nhất: ")
		b.WriteString(strings.Join(recent, "; "))
		b.WriteString("\n")
	}
	return b.String()
}

// planBatch returns the contiguous batch end point end from chapter start, under the dual input/output budget ([start,end), chapter index 0-based).
// At least 1 chapter; even a single chapter over budget forms its own batch, and the executor reports insufficient capacity upon truncation (RFC §9.2).
func planBatch(chapters []ChapterSpan, start, ledgerBytes int, b AnalyzeBudget) int {
	end := start + 1
	if b.ContextBytes <= 0 || b.MaxOutputTokens <= 0 || b.PerChapterOutput <= 0 {
		return end // budget not configured: per chapter
	}
	inAcc := ledgerBytes + b.PromptOverhead + chapterBytes(chapters, start)
	outAcc := b.PerChapterOutput
	for end < len(chapters) {
		cb := chapterBytes(chapters, end)
		if inAcc+cb > b.ContextBytes {
			break
		}
		if outAcc+b.PerChapterOutput > b.MaxOutputTokens {
			break
		}
		inAcc += cb
		outAcc += b.PerChapterOutput
		end++
	}
	return end
}

func chapterBytes(chapters []ChapterSpan, i int) int {
	return chapters[i].End - chapters[i].Start
}

// chapterInputDigest binds the analysis artifact identity per chapter: segmentation identity + prompt/schema version + chapter number + single-chapter body text.
// Per chapter rather than batch-level binding - batch partitioning is an execution detail that varies with model capability, and switching models should not invalidate already analyzed chapters wholesale;
// binding segIdentity (the InputDigest of the segmentation artifact) ensures all analyses naturally mismatch after re-segmentation (RFC §9.1/§6.3).
func chapterInputDigest(segIdentity, promptVersion string, seg *Segmentation, normalized []byte, i int) string {
	var b strings.Builder
	b.WriteString("analyze\x00")
	b.WriteString(promptVersion)
	fmt.Fprintf(&b, "\x00v%d\x00", analysisSchemaVersion)
	b.WriteString(segIdentity)
	fmt.Fprintf(&b, "\x00ch%d\x00", seg.Chapters[i].Number)
	b.WriteString(seg.Content(normalized, i))
	return Digest([]byte(b.String()))
}

// validateBatch validates in two layers: batch level (contiguous, no gaps, no duplicates), and per-chapter value domains and references (RFC §9.4).
func validateBatch(r *AnalysisBatchResult, seg *Segmentation, start, end int) error {
	want := end - start
	if len(r.Chapters) != want {
		return fmt.Errorf("Số chương của lô %d != dự kiến %d", len(r.Chapters), want)
	}
	for i, f := range r.Chapters {
		want := seg.Chapters[start+i]
		if f.Chapter != want.Number {
			return fmt.Errorf("Mục %d của lô có số chương %d != %d", i, f.Chapter, want.Number)
		}
		if strings.TrimSpace(f.Summary) == "" || strings.TrimSpace(f.CoreEvent) == "" {
			return fmt.Errorf("Chương %d: summary/core_event không được để trống", f.Chapter)
		}
		if !domain.ValidHookType(strings.ToLower(f.HookType)) {
			return fmt.Errorf("Chương %d: hook_type không hợp lệ: %q", f.Chapter, f.HookType)
		}
		if !domain.ValidDominantStrand(strings.ToLower(f.DominantStrand)) {
			return fmt.Errorf("Chương %d: dominant_strand không hợp lệ: %q", f.Chapter, f.DominantStrand)
		}
		for j, fu := range f.ForeshadowUpdates {
			if fu.Action == "plant" && strings.TrimSpace(fu.Description) == "" {
				return fmt.Errorf("Chương %d: foreshadow[%d] plant cần description", f.Chapter, j)
			}
		}
		// Enums are validated in lowercase and so persisted in lowercase: commit_chapter does not re-verify enums, and case variants would pass straight into the formal state
		// (HookHistory etc. consume exact strings, and a variant is treated as an unknown type); once validated, normalize.
		r.Chapters[i].HookType = strings.ToLower(f.HookType)
		r.Chapters[i].DominantStrand = strings.ToLower(f.DominantStrand)
	}
	return nil
}

// AnalyzeNext assembles a batch starting from the first missing analysis and persists it atomically, returning the number of chapters committed this time.
// Truncation means "fail + shrink and regroup the batch" (default, §9.5); if the batch has shrunk to a single chapter and is still truncated, insufficient capacity is reported explicitly.
func AnalyzeNext(ctx context.Context, m callModel, systemPrompt string, w *Workspace, normalized []byte, seg *Segmentation, segIdentity, promptVersion string, budget AnalyzeBudget, prof callProfile) (int, error) {
	total := len(seg.Chapters)
	start := analyzedChapters(w, seg, normalized, segIdentity, promptVersion)
	if start >= total {
		return 0, nil
	}
	ledger := buildLedger(loadPriorFacts(w, start))
	end := planBatch(seg.Chapters, start, len(ledger), budget)

	for {
		payload := buildAnalyzePayload(normalized, seg, ledger, start, end)
		res, err := callStructured[AnalysisBatchResult](ctx, m, analysisContract, systemPrompt, payload, budget.MaxOutputTokens, prof, func(r *AnalysisBatchResult) error {
			return validateBatch(r, seg, start, end)
		})
		if err != nil {
			var tr *errTruncated
			if errors.As(err, &tr) {
				// On truncation, first salvage the largest contiguous valid prefix starting from the batch's first chapter; already committed parts are not redone (§9.5).
				if salvaged := salvagePrefix(tr.Raw, seg, start); len(salvaged) > 0 {
					for i, f := range salvaged {
						ch := start + i + 1
						digest := chapterInputDigest(segIdentity, promptVersion, seg, normalized, start+i)
						art := ChapterAnalysisPayload{BatchStart: start + 1, BatchEnd: end, Facts: f}
						if werr := writeArtifact(w, analysisPath(ch), digest, art); werr != nil {
							return i, fmt.Errorf("Lưu chương vớt được %d: %w", ch, werr)
						}
					}
					w.writeFailure(FailureMeta{Stage: "analyze", Detail: fmt.Sprintf("Lô %d-%d bị cắt do giới hạn độ dài", start+1, end),
						StopReason: "length", PrefixSalvage: fmt.Sprintf("available:%d", len(salvaged))}, tr.Raw)
					prof.logger().Info("imp analysis truncated, salvaging contiguous prefix", "batch_start", start+1, "salvaged", len(salvaged))
					echoChapterFacts(prof, salvaged)
					return len(salvaged), nil
				}
				// No salvageable prefix: record unusable and "fail + shrink and regroup the batch"; a single chapter still truncated reports insufficient capacity.
				w.writeFailure(FailureMeta{Stage: "analyze", Detail: fmt.Sprintf("Lô %d-%d bị cắt do giới hạn độ dài, không có tiền tố nào vớt được", start+1, end),
					StopReason: "length", PrefixSalvage: "unavailable"}, tr.Raw)
				if end-start > 1 {
					prof.logger().Warn("imp analysis truncated, shrinking batch to regroup", "batch", fmt.Sprintf("%d-%d", start+1, end), "prefix_salvage", "unavailable")
					end = start + (end-start)/2
					// A progress line without Key: lets the user see the batch-shrinking action, and also keeps the
					// backoff lines of the two independent calls before and after from being wrongly merged under the same Key (the Key contract only covers transient backoff within a single call).
					prof.step(0, 0, "Đầu ra bị cắt do giới hạn độ dài và không có tiền tố nào vớt được, thu nhỏ lô thành chương %d-%d rồi thử lại", start+1, end)
					continue
				}
				return 0, fmt.Errorf("Chương %d: lô một chương vẫn bị cắt do giới hạn độ dài, khả năng xuất của model không đủ", start+1)
			}
			return 0, err
		}
		for i, f := range res.Chapters {
			ch := start + i + 1
			digest := chapterInputDigest(segIdentity, promptVersion, seg, normalized, start+i)
			payloadArt := ChapterAnalysisPayload{BatchStart: start + 1, BatchEnd: end, Facts: f}
			if err := writeArtifact(w, analysisPath(ch), digest, payloadArt); err != nil {
				return i, fmt.Errorf("Lưu phân tích chương %d: %w", ch, err)
			}
		}
		echoChapterFacts(prof, res.Chapters)
		return end - start, nil
	}
}

// echoChapterFacts echoes the model's core understanding of each chapter to the panel - the user should see what the model understood,
// not just a mechanical batch count (§14.1).
func echoChapterFacts(prof callProfile, facts []ImportedChapterFacts) {
	for _, f := range facts {
		prof.step(0, 0, "Chương %d \"%s\": %s", f.Chapter, snippet(f.Title, 24), snippet(f.CoreEvent, 60))
	}
}

// buildAnalyzePayload assembles the batch input: contiguous chapter raw text + the ledger before the batch.
func buildAnalyzePayload(normalized []byte, seg *Segmentation, ledger string, start, end int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hãy phân tích chương %d-%d, trả về {\"chapters\":[mỗi chương một đối tượng sự thật]}, thứ tự mảng khớp với số chương.\n\n", start+1, end)
	if ledger != "" {
		b.WriteString("## Sổ liên tục (ledger, để tham khảo)\n\n")
		b.WriteString(ledger)
		b.WriteString("\n")
	}
	for i := start; i < end; i++ {
		c := seg.Chapters[i]
		fmt.Fprintf(&b, "## Chương %d: %s\n\n", c.Number, c.Title)
		b.WriteString(seg.Content(normalized, i))
		b.WriteString("\n\n---\n\n")
	}
	return b.String()
}

// salvagePrefix parses the largest contiguous valid prefix from a length-truncated batch response (RFC §9.5).
// It saves only objects contiguous from the batch's first chapter that pass per-chapter validation; it stops at the first incomplete/invalid/skipped number, and bytes after that are not interpreted.
// A pure function, called first by AnalyzeNext on capacity truncation, to avoid discarding prefix chapters that were already fully generated.
func salvagePrefix(raw string, seg *Segmentation, start int) []ImportedChapterFacts {
	arr := extractChaptersArray(raw)
	if arr == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(arr))
	if _, err := dec.Token(); err != nil { // consume '['
		return nil
	}
	var out []ImportedChapterFacts
	for dec.More() {
		var f ImportedChapterFacts
		if err := dec.Decode(&f); err != nil {
			break // first incomplete object, stop
		}
		idx := start + len(out)
		if idx >= len(seg.Chapters) || f.Chapter != seg.Chapters[idx].Number {
			break // skipped number / out of range
		}
		one := AnalysisBatchResult{Chapters: []ImportedChapterFacts{f}}
		if err := validateBatch(&one, seg, idx, idx+1); err != nil {
			break
		}
		out = append(out, one.Chapters[0]) // validateBatch already normalized enums in place; take the validated value
	}
	return out
}

// extractChaptersArray extracts the JSON array text after "chapters" (which may be truncated at the tail).
func extractChaptersArray(raw string) string {
	i := strings.Index(raw, "\"chapters\"")
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(raw[i:], '[')
	if j < 0 {
		return ""
	}
	return raw[i+j:]
}
