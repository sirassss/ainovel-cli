package imp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
)

func TestUnitLessNumericNotLexical(t *testing.T) {
	// Lexicographic order would judge L900 > L1000 and L1257.2 > L1800; numeric order must be the opposite.
	if !unitLess(SourceUnit{Line: 900}, SourceUnit{Line: 1000}) {
		t.Fatal("L900 应 < L1000（数值序）")
	}
	if !unitLess(SourceUnit{Line: 1257, Part: 2}, SourceUnit{Line: 1800}) {
		t.Fatal("L1257.2 应 < L1800")
	}
	if !unitLess(SourceUnit{Line: 1257, Part: 1}, SourceUnit{Line: 1257, Part: 2}) {
		t.Fatal("同行 part 应按数值序")
	}
	if unitLess(SourceUnit{Line: 5}, SourceUnit{Line: 5}) {
		t.Fatal("相等不应 less")
	}
}

func TestBuildSourceUnitsRoundtrip(t *testing.T) {
	norm := []byte("第一章\n正文一\n\n第二章\n正文二")
	units := buildSourceUnits(norm, 0)
	// Rejoin: each unit's text plus '\n' between lines should restore the normalized text.
	var b strings.Builder
	for i, u := range units {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(u.Text)
		if u.Text != string(norm[u.StartByte:u.EndByte]) {
			t.Fatalf("unit %s 字节范围与文本不符", u.ID)
		}
	}
	if b.String() != string(norm) {
		t.Fatalf("拼回不符：%q", b.String())
	}
	if units[0].ID != "L1" || units[3].ID != "L4" {
		t.Fatalf("ID 不符：%s %s", units[0].ID, units[3].ID)
	}
}

func TestBuildSourceUnitsVirtualShard(t *testing.T) {
	// A single line far over budget -> split into several virtual units, with boundaries at UTF-8 character boundaries.
	long := strings.Repeat("字", 100) // 3 bytes per character = 300 bytes
	units := buildSourceUnits([]byte(long), 30)
	if len(units) < 2 {
		t.Fatalf("超预算行应分片，得到 %d", len(units))
	}
	var b strings.Builder
	for _, u := range units {
		if u.Line != 1 || u.Part == 0 {
			t.Fatalf("虚拟分片应同 Line、Part>=1：%+v", u)
		}
		b.WriteString(u.Text) // slices of the same line, no newline separator
	}
	if b.String() != long {
		t.Fatal("虚拟分片拼回丢字")
	}
}

func TestResolveBoundaryByteAnchor(t *testing.T) {
	units := []SourceUnit{{ID: "L1", Line: 1, StartByte: 0, EndByte: 10, Text: "楔子风起楔"}}
	m := map[string]SourceUnit{"L1": units[0]}
	if _, err := resolveBoundaryByte(m, "L1", "风起"); err != nil {
		t.Fatalf("唯一锚点应成功：%v", err)
	}
	if _, err := resolveBoundaryByte(m, "L1", "楔"); err == nil {
		t.Fatal("重复锚点应失败")
	}
	if _, err := resolveBoundaryByte(m, "L1", "缺失"); err == nil {
		t.Fatal("不存在锚点应失败")
	}
	if _, err := resolveBoundaryByte(m, "L9", ""); err == nil {
		t.Fatal("不存在 unit 应失败")
	}
}

func TestPlanChunksCoversWithoutGap(t *testing.T) {
	units := buildSourceUnits([]byte(strings.Repeat("行内容\n", 50)), 0)
	chunks := planChunks(units, 40)
	if len(chunks) < 2 {
		t.Fatalf("应分多块，得 %d", len(chunks))
	}
	// Seamless, non-overlapping and fully covering.
	if chunks[0][0] != 0 || chunks[len(chunks)-1][1] != len(units) {
		t.Fatal("未完整覆盖")
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i][0] != chunks[i-1][1] {
			t.Fatalf("块 %d 与前块不相接：%v", i, chunks)
		}
	}
}

func segFixture() ([]byte, []SourceUnit) {
	norm := []byte("前言\n感谢阅读\n第一章 风起\n正文一\n卷二\n第二章 云涌\n正文二")
	return norm, buildSourceUnits(norm, 0)
}

func TestResolveSegmentationHappy(t *testing.T) {
	norm, units := segFixture()
	// L1 前言(front) / L3 第一章 / L5 卷二(group) / L6 第二章 // han:keep fixture titles: preface / chapter 1 / volume 2 / chapter 2
	decisions := []BoundaryDecision{
		{UnitID: "L1", Kind: kindFrontMatter, Title: "前言"},
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L5", Kind: kindGroup, Title: "卷二"},
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 云涌"},
	}
	seg, err := resolveSegmentation(norm, units, decisions)
	if err != nil {
		t.Fatalf("覆盖校验应通过：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("章节数应为 2（group 不计），得 %d", len(seg.Chapters))
	}
	if seg.Chapters[0].Number != 1 || seg.Chapters[1].Number != 2 {
		t.Fatal("章节号应连续")
	}
	if !strings.Contains(seg.Content(norm, 0), "正文一") {
		t.Fatalf("章一正文不符：%q", seg.Content(norm, 0))
	}
	// Coverage: the first segment (front_matter) starts at 0, and the last chapter covers to the end of the text.
	if len(seg.Matter) == 0 || seg.Matter[0].Kind != kindFrontMatter || seg.Matter[0].Start != 0 {
		t.Fatalf("首段应为从 0 起的 front_matter：%+v", seg.Matter)
	}
	if seg.Chapters[len(seg.Chapters)-1].End != len(norm) {
		t.Fatal("末章应覆盖到文本尾")
	}
}

func TestResolveSegmentationRejections(t *testing.T) {
	norm, units := segFixture()
	cases := []struct {
		name string
		ds   []BoundaryDecision
	}{
		{"无章节", []BoundaryDecision{
			{UnitID: "L1", Kind: kindFrontMatter},
		}},
		{"非法kind", []BoundaryDecision{
			{UnitID: "L1", Kind: "verse"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := resolveSegmentation(norm, units, c.ds); err == nil {
				t.Fatalf("应被拒绝：%s", c.name)
			}
		})
	}
}

// TestResolveSegmentationReordersAndDedups guards the coordinate discipline of the terminal fallback: occasional in-chunk disorder by the model is restored
// deterministically by byte sorting (measured: 319 boundaries once lost to 1 inversion, and the chunk cache would make the failure reproduce deterministically);
// duplicates at the same byte keep the first occurrence and record Notes for the confirmation preview.
func TestResolveSegmentationReordersAndDedups(t *testing.T) {
	norm, units := segFixture()
	seg, err := resolveSegmentation(norm, units, []BoundaryDecision{
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L1", Kind: kindChapter, Title: "开篇"}, // out of order: positioned before L3
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 云涌"},
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 重复"}, // duplicate at the same byte
	})
	if err != nil {
		t.Fatalf("乱序/重复应被确定性修复而非拒绝：%v", err)
	}
	if len(seg.Chapters) != 3 {
		t.Fatalf("应得 3 章，得 %d：%+v", len(seg.Chapters), seg.Chapters)
	}
	if seg.Chapters[0].Title != "开篇" || seg.Chapters[0].Start != 0 {
		t.Fatalf("排序后首章应为位置最前的边界：%+v", seg.Chapters[0])
	}
	if seg.Chapters[2].Title != "第二章 云涌" {
		t.Fatalf("同字节重复应保留先出现者：%+v", seg.Chapters[2])
	}
	if len(seg.Notes) != 1 || !strings.Contains(seg.Notes[0], "trùng nhau") {
		t.Fatalf("重复边界应记入 Notes：%v", seg.Notes)
	}
}

// TestResolveSegmentationAbsorbsLeadingText guards the deterministic repair of a missed start: if the model misses the boundary for non-empty
// leading text such as a book-opening blurb / ads, it must not be vetoed terminally - the miss has already entered the chunk cache, and a veto would make a rerun with zero calls reproduce the
// failure deterministically. Go adds a front_matter to cover [0, first) and records Notes for the confirmation preview.
func TestResolveSegmentationAbsorbsLeadingText(t *testing.T) {
	norm, units := segFixture()
	// Only chapters from L3 on were reported: the non-empty text of L1/L2 has no owner.
	seg, err := resolveSegmentation(norm, units, []BoundaryDecision{
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 云涌"},
	})
	if err != nil {
		t.Fatalf("起始未归属文本应被收为 front_matter 而非拒绝：%v", err)
	}
	if len(seg.Matter) != 1 || seg.Matter[0].Kind != kindFrontMatter || seg.Matter[0].Start != 0 {
		t.Fatalf("应补出从 0 起的 front_matter：%+v", seg.Matter)
	}
	if len(seg.Chapters) != 2 || seg.Chapters[0].Start == 0 {
		t.Fatalf("章节不应吞掉头部文本：%+v", seg.Chapters)
	}
	if len(seg.Notes) != 1 || !strings.Contains(seg.Notes[0], "chưa được model gán") {
		t.Fatalf("应记录人工核对说明：%v", seg.Notes)
	}
}

// TestResolveSegmentationNotesDuplicateTitles guards the visibility of same-name chapters: in sources with a title convention chapter names
// should not repeat, and a repeat is a deterministic signal of "one chapter wrongly split" - only record Notes (blocks --yes, shown in the preview) for manual
// review; whether to merge is not ruled by Go.
func TestResolveSegmentationNotesDuplicateTitles(t *testing.T) {
	norm, units := segFixture()
	seg, err := resolveSegmentation(norm, units, []BoundaryDecision{
		{UnitID: "L1", Kind: kindFrontMatter, Title: "前言"},
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L6", Kind: kindChapter, Title: "第一章风起"}, // same name (whitespace differences ignored)
	})
	if err != nil {
		t.Fatalf("同名章应放行并记 Notes：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应得 2 章，得 %d", len(seg.Chapters))
	}
	if len(seg.Notes) != 1 || !strings.Contains(seg.Notes[0], "trùng tiêu đề") {
		t.Fatalf("应记一条同名核对说明：%v", seg.Notes)
	}
}

// TestChunkValidatorOwnedDiscipline guards the coverage of call-time validation: an invalid kind in the owned area,
// a bad anchor, a same-position semantic conflict, and an unowned start of the first chunk must be re-asked with feedback at call time - letting them through would enter the cache with the chunk,
// and if only the terminal resolve finds them, a rerun with zero calls rereads the same bad data; context-area boundaries are destined to be trimmed, so no re-ask for them;
// fully identical duplicates at the same position are mechanical redundancy and are silently deduplicated by resolve once let through.
func TestChunkValidatorOwnedDiscipline(t *testing.T) {
	norm, units := segFixture()
	unitByID := map[string]SourceUnit{}
	proj, owned := map[string]bool{}, map[string]bool{}
	for _, u := range units {
		unitByID[u.ID] = u
		proj[u.ID] = true
	}
	owned["L1"], owned["L2"], owned["L3"] = true, true, true
	v := chunkValidator{projIDs: proj, ownedIDs: owned, unitByID: unitByID, normalized: norm}

	cases := []struct {
		name    string
		bs      []BoundaryDecision
		wantErr bool
	}{
		{"owned 非法 kind", []BoundaryDecision{{UnitID: "L1", Kind: "volume"}}, true},
		{"owned 坏 anchor", []BoundaryDecision{{UnitID: "L3", Kind: kindChapter, Anchor: "不存在的锚"}}, true},
		{"owned 合法 anchor", []BoundaryDecision{{UnitID: "L3", Kind: kindChapter, Anchor: "第一章"}}, false},
		{"上下文区非法 kind 不重问", []BoundaryDecision{{UnitID: "L6", Kind: "volume"}}, false},
		{"投影外幻觉 ID", []BoundaryDecision{{UnitID: "L99", Kind: kindChapter}}, true},
		{"同位语义冲突重问", []BoundaryDecision{
			{UnitID: "L1", Kind: kindChapter, Title: "前言"},
			{UnitID: "L1", Kind: kindFrontMatter, Title: "前言"},
		}, true},
		{"同位完全重复放行", []BoundaryDecision{
			{UnitID: "L1", Kind: kindChapter, Title: "前言"},
			{UnitID: "L1", Kind: kindChapter, Title: "前言"},
		}, false},
		// Title echo: a chapter/volume name must actually exist in the boundary unit's original text - a fabricated title on a phantom boundary is blocked here.
		{"编造章节标题重问", []BoundaryDecision{{UnitID: "L2", Kind: kindChapter, Title: "第某章 我编的"}}, true},
		{"归纳标题须 uncertain 放行", []BoundaryDecision{{UnitID: "L2", Kind: kindChapter, Title: "第某章 我编的", Uncertain: true}}, false},
		{"回显容忍空白差异", []BoundaryDecision{{UnitID: "L3", Kind: kindChapter, Title: "第一章风起"}}, false},
		{"编造卷名重问", []BoundaryDecision{{UnitID: "L2", Kind: kindGroup, Title: "卷九"}}, true},
		{"附属描述性标题不核对", []BoundaryDecision{{UnitID: "L2", Kind: kindFrontMatter, Title: "引言"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := v.validate(c.bs); (err != nil) != c.wantErr {
				t.Fatalf("wantErr=%v，得 %v", c.wantErr, err)
			}
		})
	}

	// First-chunk start coverage: L1/L2 are non-empty but have no boundary owner -> re-ask; passes after the start boundary is added.
	vs := v
	vs.coverStart = true
	if err := vs.validate([]BoundaryDecision{{UnitID: "L3", Kind: kindChapter}}); err == nil {
		t.Fatal("首块起始未归属应重问")
	}
	if err := vs.validate([]BoundaryDecision{
		{UnitID: "L1", Kind: kindFrontMatter}, {UnitID: "L3", Kind: kindChapter},
	}); err != nil {
		t.Fatalf("起点已覆盖应通过：%v", err)
	}
	if err := vs.validate(nil); err == nil {
		t.Fatal("首块零边界应重问（全部起始文本未归属）")
	}
}

// TestSegmentClearsChunksOnResolveFailure guards the master switch of "cache reproduces deterministically": when terminal integration fails
// the chunk cache has no value left (the digest always matches, a rerun rereads the same boundaries with zero calls and dies again), so it must be cleared to give the model
// a chance to re-segment next time; the decision snapshot is written uniformly to failures/ via errSemantic.
func TestSegmentClearsChunksOnResolveFailure(t *testing.T) {
	norm, units := segFixture()
	// The model labels the whole book as front_matter: no chapters, Go cannot repair deterministically, terminal failure.
	m := &mockModel{responses: []string{boundariesJSON(boundaryFixture("L1", "", kindFrontMatter, "前言"))}}
	w := &Workspace{dir: t.TempDir()}
	_, err := Segment(context.Background(), m, "sys", norm, units, "", 0, 0, 4096, callProfile{}, w, "id-1")
	if err == nil {
		t.Fatal("无章节应终局失败")
	}
	var se *errSemantic
	if !errors.As(err, &se) {
		t.Fatalf("终局失败应为 errSemantic（统一落 failures/），得 %T", err)
	}
	if _, statErr := os.Stat(filepath.Join(w.dir, dirSegmentChunks)); !os.IsNotExist(statErr) {
		t.Fatalf("终局失败后块缓存应被清除：%v", statErr)
	}
}

// mockModel returns preset responses in order, for typed-call contract tests.
// stops may specify a stop reason per call; by default stop or StopReasonStop is used.
type mockModel struct {
	responses []string
	stops     []agentcore.StopReason
	i         int
	stop      agentcore.StopReason
}

func (m *mockModel) Generate(_ context.Context, _ []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	idx := m.i
	r := m.responses[idx%len(m.responses)]
	sr := m.stop
	if idx < len(m.stops) {
		sr = m.stops[idx]
	}
	if sr == "" {
		sr = agentcore.StopReasonStop
	}
	m.i++
	return &agentcore.LLMResponse{Message: agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(r)},
		StopReason: sr,
	}}, nil
}

// TestResolveSegmentationSingleLineChapters guards #9: a single-line segment without newline (anchor-split scenario) is entirely body,
// and a single-line / single-line multi-chapter novel must not be wrongly rejected as "body is empty".
func TestResolveSegmentationSingleLineChapters(t *testing.T) {
	normalized := []byte("第一章甲的故事第二章乙的故事") // the whole text is one line, no newline
	units := buildSourceUnits(normalized, 0)
	decisions := []BoundaryDecision{
		{UnitID: "L1", Kind: kindChapter, Title: "第一章"},                // no anchor -> byte 0
		{UnitID: "L1", Anchor: "第二章", Kind: kindChapter, Title: "第二章"}, // in-line anchor cuts out chapter 2
	}
	seg, err := resolveSegmentation(normalized, units, decisions)
	if err != nil {
		t.Fatalf("单行多章应被接受：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应切出 2 章，得 %d", len(seg.Chapters))
	}
	if got := seg.Content(normalized, 0); got != "第一章甲的故事" {
		t.Fatalf("首章正文范围不对：%q", got)
	}
}

func TestSegmentWithMockModel(t *testing.T) {
	norm, units := segFixture()
	resp := boundariesJSON(
		boundaryFixture("L1", "", kindFrontMatter, "前言"),
		boundaryFixture("L3", "", kindChapter, "第一章 风起"),
		boundaryFixture("L5", "", kindGroup, "卷二"),
		boundaryFixture("L6", "", kindChapter, "第二章 云涌"),
	)
	m := &mockModel{responses: []string{resp}}
	seg, err := Segment(context.Background(), m, "sys", norm, units, "", 0, 0, 4096, callProfile{}, nil, "")
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应得 2 章，得 %d", len(seg.Chapters))
	}
}

// TestResolveSegmentationAbsorbsEmptyChapter guards dirty-source tolerance: real web-novel sources often have "locked / paid chapter"
// placeholder titles (title present, body missing). Such boundaries must not fail wholesale - a terminal veto would waste all model calls of the segmentation stage;
// the placeholder segment is merged into the previous section (not a character of text lost) and recorded in Notes for the confirmation preview to present for manual review.
func TestResolveSegmentationAbsorbsEmptyChapter(t *testing.T) {
	norm, units := segFixture()
	// The L5 "卷二" line (volume 2) is labeled by the model as a chapter title: its span [L5,L6) has no body -> merged into chapter 1. // han:keep fixture title
	decisions := []BoundaryDecision{
		{UnitID: "L1", Kind: kindFrontMatter, Title: "前言"},
		{UnitID: "L3", Kind: kindChapter, Title: "第一章 风起"},
		{UnitID: "L5", Kind: kindChapter, Title: "第五章 [本章节已锁定]"},
		{UnitID: "L6", Kind: kindChapter, Title: "第二章 云涌"},
	}
	seg, err := resolveSegmentation(norm, units, decisions)
	if err != nil {
		t.Fatalf("空正文占位章应被吸收而非整体失败：%v", err)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("应得 2 章（占位并入前段），得 %d", len(seg.Chapters))
	}
	if got := seg.Content(norm, 0); !strings.Contains(got, "卷二") {
		t.Fatalf("占位段应并入第一章（文本不丢）：%q", got)
	}
	if len(seg.Notes) != 1 || !strings.Contains(seg.Notes[0], "已锁定") {
		t.Fatalf("应记录一条人工核对说明：%v", seg.Notes)
	}
	// A chapter with empty body at the very first point: no previous section to merge into -> becomes front_matter, also without failing.
	seg, err = resolveSegmentation(norm, units, []BoundaryDecision{
		{UnitID: "L1", Kind: kindChapter, Title: "占位"}, // [L1,L2) a single-line title with no body
		{UnitID: "L2", Kind: kindChapter, Title: "第一章"},
	})
	if err != nil {
		t.Fatalf("首点空正文应落为 front_matter：%v", err)
	}
	if len(seg.Matter) != 1 || seg.Matter[0].Kind != kindFrontMatter {
		t.Fatalf("首点空正文应为 front_matter：%+v", seg.Matter)
	}
}

// TestSegmentClipsContextBoundaries guards the Go-side enforcement of coordinate discipline: boundaries the model returns in the context area
// do not trigger a semantic re-ask (weak models often exhaust 3 attempts and drag down the whole chunk) and are trimmed directly by code - that boundary falls under the adjacent chunk's jurisdiction,
// the adjacent chunk will report it in its own owned range, and keeping it would cause cross-chunk duplicates / disorder.
func TestSegmentClipsContextBoundaries(t *testing.T) {
	norm, units := segFixture()
	chunks := planChunks(units, planningBudget(40, "sys", "")) // consistent with Segment's internal planning
	if len(chunks) < 2 {
		t.Fatalf("fixture 应分出至少 2 块，得 %d", len(chunks))
	}
	// Per-chunk response: one chapter boundary at the owned first unit (no title goes through the firstLine fallback, avoiding the title echo check -
	// what is tested here is coordinate discipline); the first chunk additionally smuggles in a boundary for the next chunk's first unit (context area).
	responses := make([]string, len(chunks))
	for ci, owned := range chunks {
		boundaries := []map[string]any{boundaryFixture(units[owned[0]].ID, "", kindChapter, "")}
		if ci == 0 {
			boundaries = append(boundaries, boundaryFixture(units[chunks[1][0]].ID, "", kindChapter, ""))
		}
		responses[ci] = boundariesJSON(boundaries...)
	}
	// The clipping note goes through ordinary progress echo (routine coordinate discipline, not a warning - a warn color would make users think something went wrong).
	var clipNotes int
	prof := callProfile{progress: func(_, _ int, s string) {
		if strings.Contains(s, "cắt bỏ") {
			clipNotes++
		}
	}}
	seg, err := Segment(context.Background(), &mockModel{responses: responses}, "sys", norm, units, "", 40, 2, 4096, prof, nil, "")
	if err != nil {
		t.Fatalf("上下文区边界应被裁掉而非失败：%v", err)
	}
	if len(seg.Chapters) != len(chunks) {
		t.Fatalf("应得 %d 章（越界边界不重复计入），得 %d", len(chunks), len(seg.Chapters))
	}
	if clipNotes != 1 {
		t.Fatalf("应回显 1 条裁剪说明，得 %d", clipNotes)
	}
}

// TestSegmentReusesChunkArtifacts guards chunk-level breakpoints: segmentation persists a boundary cache chunk by chunk, and on rerun chunks whose digest matches
// are reused directly with zero model calls - segmentation is the most expensive stage, and a failure of any chunk should not repay completed chunks (same philosophy as analyze/synthesize).
func TestSegmentReusesChunkArtifacts(t *testing.T) {
	norm, units := segFixture()
	chunks := planChunks(units, planningBudget(40, "sys", "")) // consistent with Segment's internal planning
	responses := make([]string, len(chunks))
	for ci, owned := range chunks {
		responses[ci] = boundariesJSON(boundaryFixture(units[owned[0]].ID, "", kindChapter, ""))
	}
	w := &Workspace{dir: t.TempDir()}
	m1 := &mockModel{responses: responses}
	seg1, err := Segment(context.Background(), m1, "sys", norm, units, "", 40, 2, 4096, callProfile{}, w, "id-1")
	if err != nil {
		t.Fatalf("首跑：%v", err)
	}
	if m1.i != len(chunks) {
		t.Fatalf("首跑应调用 %d 次，得 %d", len(chunks), m1.i)
	}
	m2 := &mockModel{responses: responses}
	seg2, err := Segment(context.Background(), m2, "sys", norm, units, "", 40, 2, 4096, callProfile{}, w, "id-1")
	if err != nil {
		t.Fatalf("重跑：%v", err)
	}
	if m2.i != 0 {
		t.Fatalf("digest 匹配的块应零调用复用，实际调用 %d 次", m2.i)
	}
	if len(seg2.Chapters) != len(seg1.Chapters) {
		t.Fatalf("复用结果应一致：%d != %d", len(seg2.Chapters), len(seg1.Chapters))
	}
	// Identity change (switching prompt version / guidance / source) -> the cache naturally mismatches and everything is redone.
	m3 := &mockModel{responses: responses}
	if _, err := Segment(context.Background(), m3, "sys", norm, units, "", 40, 2, 4096, callProfile{}, w, "id-2"); err != nil {
		t.Fatalf("身份变化重跑：%v", err)
	}
	if m3.i != len(chunks) {
		t.Fatalf("身份变化应全部重做（%d 次调用），得 %d", len(chunks), m3.i)
	}
}

// TestSegmentShrinksChunkOnTruncation guards the output budget loop: many short chapters make a single chunk's boundary JSON
// exceed the visible output (stop=length), so the chunk must be halved and retried rather than failing wholesale - same philosophy as analyze's batch shrinking.
func TestSegmentShrinksChunkOnTruncation(t *testing.T) {
	norm, units := segFixture() // 7 units, single chunk [0,7), mid=3
	left := boundariesJSON(boundaryFixture("L1", "", kindChapter, ""))
	right := boundariesJSON(boundaryFixture("L6", "", kindChapter, "第二章 云涌"))
	m := &mockModel{
		responses: []string{`{"boundaries":[]}`, left, right},
		stops:     []agentcore.StopReason{agentcore.StopReasonLength}, // first call truncated, the two half chunks normal
	}
	seg, err := Segment(context.Background(), m, "sys", norm, units, "", 0, 0, 4096, callProfile{}, nil, "")
	if err != nil {
		t.Fatalf("截断应缩块重试而非失败：%v", err)
	}
	if m.i != 3 {
		t.Fatalf("应为 1 次截断 + 2 次半块调用，得 %d", m.i)
	}
	if len(seg.Chapters) != 2 {
		t.Fatalf("缩块结果应完整覆盖（2 章），得 %d", len(seg.Chapters))
	}
}

// TestPlanningBudget guards the structural overhead deduction of segmentation planning budget: the owned body text is only part of the request.
func TestPlanningBudget(t *testing.T) {
	if got := planningBudget(0, "sys", "g"); got != 0 {
		t.Fatalf("无预算应透传，得 %d", got)
	}
	if got := planningBudget(1000, strings.Repeat("s", 100), strings.Repeat("g", 100)); got != 600 {
		t.Fatalf("(1000-200)*3/4 应为 600，得 %d", got)
	}
	if got := planningBudget(1000, strings.Repeat("s", 2000), ""); got != 250 {
		t.Fatalf("超长提示应触发下限 chunkBytes/4=250，得 %d", got)
	}
}

// TestBuildProjectionContextByteCap guards the context-area byte cap: virtual slices of an over-long line (a single slice can reach
// MaxUnitBytes) would swallow the input budget; context is only reference information, so it shrinks by the byte cap rather than being taken in full.
func TestBuildProjectionContextByteCap(t *testing.T) {
	_, units := segFixture()
	if _, ids := buildProjection(units, [2]int{2, 3}, 2, 1, ""); len(ids) != 1 || !ids["L3"] {
		t.Fatalf("字节上限应裁掉上下文单元，只剩 owned：%v", ids)
	}
	if _, ids := buildProjection(units, [2]int{2, 3}, 2, 0, ""); len(ids) != 5 {
		t.Fatalf("无字节上限时应含前后各 2 个上下文单元（共 5），得 %v", ids)
	}
}

func TestCallStructuredTruncation(t *testing.T) {
	m := &mockModel{responses: []string{`{"boundaries":[]}`}, stop: agentcore.StopReasonLength}
	_, err := callStructured[boundaryBatch](context.Background(), m, segmentContract, "s", "p", 16, callProfile{}, nil)
	var trunc *errTruncated
	if err == nil || !asTruncated(err, &trunc) {
		t.Fatalf("长度截断应返回 *errTruncated，得 %v", err)
	}
}

func asTruncated(err error, target **errTruncated) bool {
	t, ok := err.(*errTruncated)
	if ok {
		*target = t
	}
	return ok
}

func TestChunkValidatorMissingTitleNamesUnitThenTitle(t *testing.T) {
	units := []SourceUnit{{ID: "L1", Line: 1, StartByte: 0, EndByte: 12, Text: "Nội dung khác"}}
	v := chunkValidator{
		projIDs:  map[string]bool{"L1": true},
		ownedIDs: map[string]bool{"L1": true},
		unitByID: map[string]SourceUnit{"L1": units[0]},
	}
	err := v.validate([]BoundaryDecision{{UnitID: "L1", Kind: kindChapter, Title: "Chương bịa"}})
	if err == nil {
		t.Fatal("tiêu đề không có trong nguyên văn phải bị từ chối")
	}
	msg := err.Error()
	iUnit, iTitle := strings.Index(msg, `L1`), strings.Index(msg, `"Chương bịa"`)
	if iUnit < 0 || iTitle < 0 || !strings.Contains(msg, "Ranh giới L1") || strings.Contains(msg, `Ranh giới "Chương bịa"`) {
		t.Fatalf("unit id và tiêu đề bị đảo vai: %s", msg)
	}
}
