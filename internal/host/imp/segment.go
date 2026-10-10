package imp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// BoundaryDecision is the model's boundary judgment for a single owned range (RFC §8.2).
type BoundaryDecision struct {
	UnitID    string `json:"unit_id"`
	Anchor    string `json:"anchor,omitempty"`
	Kind      string `json:"kind"` // chapter / group / front_matter / back_matter
	Title     string `json:"title,omitempty"`
	Uncertain bool   `json:"uncertain,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

const (
	kindChapter     = "chapter"
	kindGroup       = "group"
	kindFrontMatter = "front_matter"
	kindBackMatter  = "back_matter"
)

// boundaryBatch is the structured return of one segmentation call.
type boundaryBatch struct {
	Boundaries []BoundaryDecision `json:"boundaries"`
}

// ChapterSpan is a committable chapter after segmentation confirmation: title + byte range of the normalized text (including the title line).
type ChapterSpan struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Start  int    `json:"start_byte"`
	End    int    `json:"end_byte"`
}

// MatterSpan is a volume/section title or an explicit auxiliary region.
type MatterSpan struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	Start int    `json:"start_byte"`
	End   int    `json:"end_byte"`
}

// Segmentation is the segmentation result that passed full-text coverage validation (the upstream of confirmation and per-chapter analysis).
type Segmentation struct {
	Chapters  []ChapterSpan `json:"chapters"`
	Matter    []MatterSpan  `json:"matter,omitempty"`    // group / front / back
	Uncertain []int         `json:"uncertain,omitempty"` // chapter numbers marked uncertain, for preview hints
	Notes     []string      `json:"notes,omitempty"`     // notes needing manual review from segmentation time (e.g. an empty-body placeholder title merged into the previous section)
}

// Content returns the normalized body text (including the title line) of chapter i.
func (s *Segmentation) Content(normalized []byte, i int) string {
	c := s.Chapters[i]
	return string(normalized[c.Start:c.End])
}

// resolveSegmentation maps ordered boundary decisions to a Segmentation that passed full-text coverage validation (RFC §8.3).
// Pure function: model output and code validation are separate; "is this line a chapter title" is not re-judged by Go, but the coverage invariant must hold.
func resolveSegmentation(normalized []byte, units []SourceUnit, decisions []BoundaryDecision) (*Segmentation, error) {
	if len(decisions) == 0 {
		return nil, fmt.Errorf("Không nhận diện được ranh giới nào")
	}
	// Precondition: units must be sorted in numeric (Line,Part) order (no ID lexicographic order).
	for i := 1; i < len(units); i++ {
		if !unitLess(units[i-1], units[i]) {
			return nil, fmt.Errorf("SourceUnit không được sắp theo thứ tự số (Line,Part): %s đứng trước %s", units[i-1].ID, units[i].ID)
		}
	}
	unitByID := make(map[string]SourceUnit, len(units))
	for _, u := range units {
		unitByID[u.ID] = u
	}

	type point struct {
		byte int
		d    BoundaryDecision
	}
	points := make([]point, 0, len(decisions))
	for i, d := range decisions {
		switch d.Kind {
		case kindChapter, kindGroup, kindFrontMatter, kindBackMatter:
		default:
			return nil, fmt.Errorf("Ranh giới [%d] kind không hợp lệ: %q", i, d.Kind)
		}
		b, err := resolveBoundaryByte(unitByID, d.UnitID, d.Anchor)
		if err != nil {
			return nil, err
		}
		points = append(points, point{byte: b, d: d})
	}
	// Occasional disorder and duplication from the model are coordinate-discipline problems; Go repairs them deterministically rather than vetoing terminally - discarding the whole segmentation stage because
	// two boundaries are in reversed order after all chunks succeeded is an unacceptable cost (measured: 319 boundaries lost to 1 in-chunk inversion,
	// and the chunk cache would make the failure reproduce deterministically). Order between chunks is guaranteed by owned ranges not overlapping, so disorder can only occur
	// within a chunk: a stable sort by byte restores the true order with zero information loss; for duplicates at the same byte, keep the first occurrence and record it in Notes
	// for manual review in the confirmation preview.
	sort.SliceStable(points, func(i, j int) bool { return points[i].byte < points[j].byte })
	var notes []string
	uniq := points[:0]
	for _, p := range points {
		if n := len(uniq); n > 0 && uniq[n-1].byte == p.byte {
			// Fully identical duplicates are mechanical redundancy and are silently deduplicated; a same-position semantic conflict (different kind/title) was already
			// re-asked at call time, so reaching here can only come from an old cache predating the repair - keep the first occurrence and record it in Notes for manual review.
			if prev := uniq[n-1].d; prev.Kind != p.d.Kind || boundaryLabel(prev) != boundaryLabel(p.d) {
				notes = append(notes, fmt.Sprintf("Ranh giới %q và %q trùng nhau (byte %d), đã giữ cái trước",
					boundaryLabel(prev), boundaryLabel(p.d), p.byte))
			}
			continue
		}
		uniq = append(uniq, p)
	}
	points = uniq
	// Non-empty text before the first boundary (book-opening blurb / ads etc., where the model missed the starting boundary) is not terminally vetoed: Go deterministically
	// adds a front_matter to cover [0, first) and records Notes for manual review in the confirmation preview - the miss has already entered the chunk cache,
	// and a terminal veto would make a rerun reproduce the same failure with zero calls (same philosophy as absorbing empty-body chapters, RFC §8.3.5).
	// The semantic judgment itself was already handed back to the model at call time (chunkValidator.coverStart re-ask); this fallback only heals old caches.
	if head := points[0].byte; head != 0 && strings.TrimSpace(string(normalized[:head])) != "" {
		notes = append(notes, fmt.Sprintf("%d byte văn bản đầu chưa được model gán vào đâu (%s…), đã thu vào front_matter, hãy kiểm tra có bỏ sót chương không",
			head, snippet(string(normalized[:min(head, 48)]), 24)))
		points = append([]point{{byte: 0, d: BoundaryDecision{UnitID: units[0].ID, Kind: kindFrontMatter}}}, points...)
	}

	seg := &Segmentation{Notes: notes}
	chapterNo := 0
	// absorb merges a segment into the most recently produced span (either a chapter or an auxiliary region); returns false when there is nothing to merge into.
	absorb := func(end int) bool {
		ci, mi := len(seg.Chapters)-1, len(seg.Matter)-1
		switch {
		case ci >= 0 && (mi < 0 || seg.Chapters[ci].Start > seg.Matter[mi].Start):
			seg.Chapters[ci].End = end
		case mi >= 0:
			seg.Matter[mi].End = end
		default:
			return false
		}
		return true
	}
	for i, p := range points {
		start := p.byte
		if i == 0 {
			start = 0 // whitespace at the start absorbed by the first segment
		}
		end := len(normalized)
		if i+1 < len(points) {
			end = points[i+1].byte
		}
		title := strings.TrimSpace(p.d.Title)
		if title == "" {
			title = firstLine(normalized, p.byte, end)
		}
		switch p.d.Kind {
		case kindChapter:
			if strings.TrimSpace(bodyAfterTitle(normalized, p.byte, end)) == "" {
				// Real web-novel sources often have "locked / paid chapter" placeholders: the title is there but the body is missing. Do not fail wholesale -
				// a terminal veto would waste all model calls of the segmentation stage; the title line is merged into the previous section (not a character of text lost),
				// recorded in Notes for the confirmation preview to present, and if a human does not accept it they can rule with --guide (the stop point in RFC §8.4 exists for exactly this).
				seg.Notes = append(seg.Notes,
					fmt.Sprintf("Tiêu đề chương %q không có nội dung chương (byte %d..%d), đã gộp vào đoạn trước (thường gặp ở chương bị khóa/chương giữ chỗ trả phí)", title, start, end))
				if !absorb(end) {
					seg.Matter = append(seg.Matter, MatterSpan{Kind: kindFrontMatter, Title: title, Start: start, End: end})
				}
				continue
			}
			chapterNo++
			seg.Chapters = append(seg.Chapters, ChapterSpan{Number: chapterNo, Title: title, Start: start, End: end})
			if p.d.Uncertain {
				seg.Uncertain = append(seg.Uncertain, chapterNo)
			}
		default:
			seg.Matter = append(seg.Matter, MatterSpan{Kind: p.d.Kind, Title: title, Start: start, End: end})
		}
	}
	if chapterNo == 0 {
		return nil, fmt.Errorf("Chia tách không tạo ra chương nào (group không tính là chương)")
	}
	// A chapter with the same name is a deterministic signal of "one chapter wrongly split" (in sources with a title convention, chapter names should not repeat); only record Notes
	// for manual review in the confirmation preview (non-empty Notes blocks --yes) - whether to merge is not ruled by Go.
	titleAt := make(map[string]int, len(seg.Chapters))
	for _, c := range seg.Chapters {
		key := squashSpace(c.Title)
		if first, ok := titleAt[key]; ok && key != "" {
			seg.Notes = append(seg.Notes, fmt.Sprintf("Chương %d và chương %d trùng tiêu đề (%q), nghi cùng một chương bị chia nhầm, hãy kiểm tra",
				c.Number, first, snippet(c.Title, 24)))
		} else {
			titleAt[key] = c.Number
		}
	}
	return seg, nil
}

// squashSpace removes all whitespace, for title echo and same-name comparison - whitespace/decoration differences are not semantic differences.
func squashSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// firstLine returns the first line within [start,end) with whitespace removed.
func firstLine(normalized []byte, start, end int) string {
	s := string(normalized[start:end])
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// bodyAfterTitle returns the body text within [start,end) after dropping the first line (the title).
// A multi-line chapter's title occupies the first line alone and the body follows; a single-line segment without newline (anchor-split scenario) is entirely body,
// in which case the whole segment is returned rather than an empty string - otherwise a legitimate single-line / single-line multi-chapter novel would be wrongly rejected as "body is empty" (RFC §8.3).
func bodyAfterTitle(normalized []byte, start, end int) string {
	s := string(normalized[start:end])
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// planChunks splits units into non-overlapping, fully covering owned index ranges [start,end) by byte budget.
// Chunk size is computed from the context budget, not by a fixed number of lines or chapters (RFC §8.1).
func planChunks(units []SourceUnit, budgetBytes int) [][2]int {
	if len(units) == 0 {
		return nil
	}
	if budgetBytes <= 0 {
		return [][2]int{{0, len(units)}}
	}
	var chunks [][2]int
	start := 0
	acc := 0
	for i, u := range units {
		size := u.EndByte - u.StartByte
		if acc > 0 && acc+size > budgetBytes {
			chunks = append(chunks, [2]int{start, i})
			start = i
			acc = 0
		}
		acc += size
	}
	chunks = append(chunks, [2]int{start, len(units)})
	return chunks
}

// buildProjection assembles the structural projection payload of an owned range (with a little context); the model returns boundaries only for owned.
// It also returns the set of all unit_ids inside the projection (owned + context area), so output validation can distinguish hallucination from out-of-range.
func buildProjection(units []SourceUnit, owned [2]int, contextMargin, ctxBudget int, guidance string) (string, map[string]bool) {
	// The context area shrinks under both a unit-count and a byte cap (only by unit count when ctxBudget<=0): margin units are usually
	// ordinary lines, but virtual slices of an over-long line can reach MaxUnitBytes, and a few of them could swallow the whole input budget - context
	// is only reference information, not worth that price.
	lo, budget := owned[0], ctxBudget
	for lo > 0 && owned[0]-lo < contextMargin {
		if n := len(units[lo-1].Text); ctxBudget > 0 {
			if n > budget {
				break
			}
			budget -= n
		}
		lo--
	}
	hi, budget := owned[1], ctxBudget
	for hi < len(units) && hi-owned[1] < contextMargin {
		if n := len(units[hi].Text); ctxBudget > 0 {
			if n > budget {
				break
			}
			budget -= n
		}
		hi++
	}
	type projUnit struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	proj := struct {
		OwnedStart   string     `json:"owned_start"`
		OwnedEnd     string     `json:"owned_end"`
		Units        []projUnit `json:"units"`
		UserGuidance string     `json:"user_guidance,omitempty"`
	}{
		OwnedStart:   units[owned[0]].ID,
		OwnedEnd:     units[owned[1]-1].ID,
		UserGuidance: guidance,
	}
	ids := make(map[string]bool, hi-lo)
	for i := lo; i < hi; i++ {
		proj.Units = append(proj.Units, projUnit{ID: units[i].ID, Text: units[i].Text})
		ids[units[i].ID] = true
	}
	data, _ := json.MarshalIndent(proj, "", "  ")
	return string(data), ids
}

// segmentInputDigest covers the semantic inputs the segmentation action actually consumes: normalized source, user guidance, prompt version (RFC §6.3).
func segmentInputDigest(normalizedDigest, guidance, promptVersion string) string {
	return Digest([]byte(strings.Join([]string{"segment", promptVersion, normalizedDigest, guidance}, "\x00")))
}

// segmentChunkPath / segmentChunkDigest: artifact path and identity of the chunk-level boundary cache.
// The identity binds the segmentation identity (source + guidance + prompt version) and the chunk's owned unit range - any upstream change makes the cache naturally mismatch.
func segmentChunkPath(owned [2]int) string {
	return fmt.Sprintf("%s/chunk-%06d-%06d.json", dirSegmentChunks, owned[0], owned[1])
}

func segmentChunkDigest(identity, loID, hiID string) string {
	return Digest([]byte(strings.Join([]string{"segment-chunk", identity, loID, hiID}, "\x00")))
}

// Segment performs semantic segmentation on the whole normalized text: it calls the model per owned range to identify boundaries, then validates full-text coverage.
// contextMargin is the number of context units, chunkBytes the byte budget of an owned range, maxTokens the per-call output budget.
// When w is non-nil the boundary cache is persisted chunk by chunk (identity = segmentInputDigest): a single chunk can take minutes, and the failure of any chunk
// should not repay for calls of completed chunks - the same philosophy as analyze per chapter and synthesize per range; previously segmentation was the
// only expensive stage without in-stage persistence, so one failure meant redoing everything.
func Segment(ctx context.Context, m callModel, systemPrompt string, normalized []byte, units []SourceUnit, guidance string, chunkBytes, contextMargin, maxTokens int, prof callProfile, w *Workspace, identity string) (*Segmentation, error) {
	chunks := planChunks(units, planningBudget(chunkBytes, systemPrompt, guidance))
	unitByID := make(map[string]SourceUnit, len(units))
	for _, u := range units {
		unitByID[u.ID] = u
	}
	var decisions []BoundaryDecision
	// chunk handles one owned range: a cache hit makes zero calls; when the output is truncated by length and the range can be split again, halve the chunk
	// and retry recursively (the boundary JSON of many short chapters exceeds the visible output; same philosophy as analyze's batch shrinking) - a half chunk has its
	// own cache path, so retry results are not repaid; only truncation at the single-unit level is real capacity shortage.
	var chunk func(owned [2]int, cur, total int) ([]BoundaryDecision, error)
	chunk = func(owned [2]int, cur, total int) ([]BoundaryDecision, error) {
		lo, hi := units[owned[0]], units[owned[1]-1]
		rel, want := segmentChunkPath(owned), segmentChunkDigest(identity, lo.ID, hi.ID)
		if w != nil {
			if art, err := readArtifact[boundaryBatch](w, rel); err == nil && art.InputDigest == want {
				return art.Payload.Boundaries, nil
			}
		}
		// A single-chunk model call can take minutes; echoing progress + the cumulative boundary count per chunk keeps the panel from staying silent for the whole stretch and looking hung.
		prof.step(cur, total, "Chia khối %d/%d (%s..%s), đã nhận diện %d ranh giới...",
			cur, total, lo.ID, hi.ID, len(decisions))
		// The context-area byte cap is chunkBytes/8 but no lower than 4096: what must be blocked is virtual slices of an over-long line
		// (a single slice can reach MaxUnitBytes) swallowing the input budget; the margin overhead of ordinary lines is harmless anyway.
		payload, projIDs := buildProjection(units, owned, contextMargin, max(chunkBytes/8, 4096), guidance)
		ownedIDs := make(map[string]bool, owned[1]-owned[0])
		for i := owned[0]; i < owned[1]; i++ {
			ownedIDs[units[i].ID] = true
		}
		v := chunkValidator{projIDs: projIDs, ownedIDs: ownedIDs, unitByID: unitByID,
			normalized: normalized, coverStart: owned[0] == 0}
		batch, err := callStructured[boundaryBatch](ctx, m, segmentContract, systemPrompt, payload, maxTokens, prof, func(b *boundaryBatch) error {
			return v.validate(b.Boundaries)
		})
		if err != nil {
			var tr *errTruncated
			if errors.As(err, &tr) && owned[1]-owned[0] > 1 {
				mid := (owned[0] + owned[1]) / 2
				prof.step(0, 0, "Đầu ra ranh giới của khối %s..%s bị cắt (chương quá dày), chia đôi khối rồi thử lại", lo.ID, hi.ID)
				prof.logger().Warn("imp segment output truncated, halving block", "chunk", lo.ID+".."+hi.ID)
				left, lerr := chunk([2]int{owned[0], mid}, cur, total)
				if lerr != nil {
					return nil, lerr
				}
				right, rerr := chunk([2]int{mid, owned[1]}, cur, total)
				if rerr != nil {
					return nil, rerr
				}
				return append(left, right...), nil
			}
			return nil, fmt.Errorf("Khoảng chia tách %s..%s: %w", lo.ID, hi.ID, err)
		}
		// Context-area boundaries fall under the adjacent chunk's jurisdiction (it will report them again in its own owned range), and Go trims them directly:
		// coordinate discipline is enforced by code, and semantic retry is reserved for genuine semantic failures - the old behavior re-asked on out-of-range feedback,
		// and weak models often exhausted all 3 attempts and dragged down the whole chunk (RFC §8.1 "the model owns semantics, Go owns coordinates").
		kept := make([]BoundaryDecision, 0, len(batch.Boundaries))
		for _, bd := range batch.Boundaries {
			if ownedIDs[bd.UnitID] {
				kept = append(kept, bd)
			}
		}
		if n := len(batch.Boundaries) - len(kept); n > 0 {
			// Routine coordinate discipline rather than an anomaly, so echo with ordinary progress - a warning color would make users think something went wrong.
			prof.step(0, 0, "Đã cắt bỏ %d ranh giới báo thừa ở vùng ngữ cảnh (khối kề bên tự báo, không phải lỗi)", n)
		}
		// Echo the model's semantic judgment (the identified titles), so the user sees what the model understood, not just a mechanical count.
		if len(kept) > 0 {
			prof.step(0, 0, "Model nhận diện: %s", previewBoundaries(kept))
		}
		if w != nil {
			if err := writeArtifact(w, rel, want, boundaryBatch{Boundaries: kept}); err != nil {
				return nil, fmt.Errorf("Lưu khối chia tách %s..%s: %w", lo.ID, hi.ID, err)
			}
		}
		return kept, nil
	}
	for ci, owned := range chunks {
		kept, err := chunk(owned, ci+1, len(chunks))
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, kept...)
	}
	seg, err := resolveSegmentation(normalized, units, decisions)
	if err != nil {
		// When terminal integration fails the chunk cache has no value left: an always-matching digest would make a rerun reread the same boundaries with zero calls
		// and reproduce the same failure deterministically. Clearing the cache gives the model a chance to re-segment next time; the decision snapshot is written via errSemantic
		// uniformly to failures/ for post-mortem. A clear failure must be reported truthfully - falsely claiming it was cleared would make the user's rerun
		// reread the bad cache again (Debug-First).
		hint := "Đã xóa cache khối, chạy lại sẽ chia lại"
		if w != nil {
			if cerr := w.clearDir(dirSegmentChunks); cerr != nil {
				hint = fmt.Sprintf("Không xóa được cache khối: %v; trước khi chạy lại hãy xóa thủ công meta/import/segment-chunks/", cerr)
			}
		}
		raw, _ := json.MarshalIndent(decisions, "", "  ")
		return nil, &errSemantic{Raw: string(raw), Err: fmt.Errorf("Không tích hợp được kết quả chia tách toàn sách (%s): %w", hint, err)}
	}
	return seg, nil
}

// planningBudget deducts the request's structural overhead from the input budget: the system prompt and guidance are deducted at their actual length, and the remainder is
// scaled by 3/4 to account for the blow-up from wrapping the projection JSON (id/quotes/escapes ≈ 1/3 of the body text) - the owned body text is only part of the request,
// and planning at full amount would exceed the real input budget with a long prompt or a large context area. The lower bound chunkBytes/4 keeps an over-long prompt from
// squeezing the budget negative; chunkBytes<=0 means no budget (single chunk) and is passed through unchanged.
func planningBudget(chunkBytes int, systemPrompt, guidance string) int {
	if chunkBytes <= 0 {
		return chunkBytes
	}
	b := (chunkBytes - len(systemPrompt) - len(guidance)) * 3 / 4
	return max(b, chunkBytes/4)
}

// boundaryLabel gives a boundary decision a readable label: title first, falling back to kind@unit_id when there is no title.
func boundaryLabel(d BoundaryDecision) string {
	if t := strings.TrimSpace(d.Title); t != "" {
		return t
	}
	return d.Kind + "@" + d.UnitID
}

// previewBoundaries compresses a batch of boundary decisions into a one-line title preview (at most 3 + a count), for panel echo.
func previewBoundaries(bs []BoundaryDecision) string {
	titles := make([]string, 0, 3)
	for _, b := range bs {
		titles = append(titles, snippet(boundaryLabel(b), 24))
		if len(titles) == 3 {
			break
		}
	}
	s := strings.Join(titles, " / ")
	if len(bs) > len(titles) {
		s += fmt.Sprintf(" (tổng %d chỗ)", len(bs))
	}
	return s
}

// chunkValidator carries the call-time validation context of one segmentation call: a unit_id outside the projection is a hallucination; a boundary in the owned
// area must also have a valid kind, a resolvable anchor and no semantic conflict at the same position; the first chunk must have a boundary covering the text start.
// If these bad values were not blocked at call time they would be written into the cache with the chunk - the digest always matches, a rerun rereads the same bad data with zero calls,
// and the failure reproduces deterministically (RFC §8.3). Semantic judgment (which to keep, what the opening is) goes back to the model via re-ask,
// and Go does not answer on its behalf; context-area boundaries are destined to be trimmed by coordinate discipline, so no re-ask is made for them.
type chunkValidator struct {
	projIDs, ownedIDs map[string]bool
	unitByID          map[string]SourceUnit
	normalized        []byte
	coverStart        bool // first chunk: non-empty text before the text start must have a boundary owner
}

func (v chunkValidator) validate(bs []BoundaryDecision) error {
	seen := make(map[int]BoundaryDecision)
	first := -1
	for _, b := range bs {
		if b.UnitID == "" {
			return fmt.Errorf("Ranh giới thiếu unit_id")
		}
		if !v.projIDs[b.UnitID] {
			return fmt.Errorf("unit_id %q của ranh giới không tồn tại trong phép chiếu lần này", b.UnitID)
		}
		if !v.ownedIDs[b.UnitID] {
			continue
		}
		switch b.Kind {
		case kindChapter, kindGroup, kindFrontMatter, kindBackMatter:
		default:
			return fmt.Errorf("Ranh giới %s kind không hợp lệ: %q (chỉ được là chapter/group/front_matter/back_matter)", b.UnitID, b.Kind)
		}
		at, err := resolveBoundaryByte(v.unitByID, b.UnitID, b.Anchor)
		if err != nil {
			return err
		}
		// Title echo: the title of a chapter/group must actually exist in the boundary unit's original text (ignoring whitespace differences) -
		// a fabricated title is blocked here by fact (measured: in one source of 157 chapters, 67 were boundaries the model made up on mid-chapter continuation text +
		// invented titles). Semantic discretion still belongs to the model: for sources truly without a title convention, uncertain can be set to keep a summarized title;
		// descriptive titles of front/back matter are low risk and are not checked.
		if (b.Kind == kindChapter || b.Kind == kindGroup) && !b.Uncertain {
			if t := squashSpace(b.Title); t != "" && !strings.Contains(squashSpace(v.unitByID[b.UnitID].Text), t) {
				return fmt.Errorf("Ranh giới %s có tiêu đề %q không tìm thấy trong nguyên văn đơn vị đó: nếu đây là phần tiếp nối nội dung của chương trước thì đừng đặt ranh giới cho nó (do ranh giới trước đó quản, boundaries có thể rỗng); nếu nguồn ở đây thực sự không có dòng tiêu đề mà tiêu đề do bạn tự tóm tắt thì đặt uncertain=true",
					b.UnitID, snippet(b.Title, 24))
			}
		}
		// A same-position conflict (different kind/title) is a semantic problem, and which to keep is not ruled by Go; fully identical duplicates
		// are mechanical redundancy and, once let through, are silently deduplicated by resolve.
		if prev, ok := seen[at]; ok {
			if prev.Kind != b.Kind || boundaryLabel(prev) != boundaryLabel(b) {
				return fmt.Errorf("Ranh giới %q và %q rơi vào cùng một vị trí (%s), xung đột ngữ nghĩa, hãy chỉ giữ cái đúng",
					boundaryLabel(prev), boundaryLabel(b), b.UnitID)
			}
		} else {
			seen[at] = b
		}
		if first < 0 || at < first {
			first = at
		}
	}
	if v.coverStart {
		head := first
		if head < 0 {
			head = len(v.normalized) // first chunk reported no owned boundary at all: all starting text is unowned
		}
		if head > 0 && strings.TrimSpace(string(v.normalized[:head])) != "" {
			return fmt.Errorf("%d byte văn bản đầu (%s…) chưa thuộc ranh giới nào, hãy bổ sung ranh giới ở đầu văn bản (front_matter/chapter/group)",
				head, snippet(string(v.normalized[:min(head, 48)]), 24))
		}
	}
	return nil
}
