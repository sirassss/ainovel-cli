package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// References holds the embedded reference material.
type References struct {
	// V0
	ChapterGuide      string
	HookTechniques    string
	QualityChecklist  string
	OutlineTemplate   string
	CharacterTemplate string
	ChapterTemplate   string
	// V1
	Consistency      string
	ContentExpansion string
	DialogueWriting  string
	// V2
	StyleReference   string // supplementary style reference (may be empty)
	LongformPlanning string // general long-form planning reference
	Differentiation  string // general differentiation design reference
	ArcTemplates     string // genre arc templates (loaded by style, may be empty)
	AntiAITone       string // anti-AI-tone criteria library (shared by writer/editor, injected throughout)
}

// ContextTool assembles the context needed for the current chapter.
type ContextTool struct {
	store      *store.Store
	refs       References
	style      string
	styleStats *StyleStatsIndex
}

type contextReads struct {
	warnings []string
	seen     map[string]struct{}
	err      error
}

func (r *contextReads) warn(scope string, err error) {
	if err == nil || os.IsNotExist(err) {
		return
	}
	msg := fmt.Sprintf("Đọc %s thất bại: %v", scope, err)
	if r.seen == nil {
		r.seen = make(map[string]struct{})
	}
	if _, ok := r.seen[msg]; ok {
		return
	}
	r.seen[msg] = struct{}{}
	r.warnings = append(r.warnings, msg)
}

func (r *contextReads) require(scope string, err error) {
	if r.err != nil || err == nil || os.IsNotExist(err) || errors.Is(err, store.ErrOutlineChapterNotFound) {
		return
	}
	r.err = fmt.Errorf("Đọc %s thất bại: %w", scope, err)
}

// NewContextTool creates the context tool. styleStats must be shared with commit_chapter,
// otherwise the context would keep reading stale statistics after a chapter is rewritten.
// user_rules are injected by buildUserRules reading this book's snapshot (meta/user_rules.json) directly, no longer depending on load options.
func NewContextTool(
	store *store.Store,
	refs References,
	style string,
	styleStats *StyleStatsIndex,
) *ContextTool {
	if styleStats == nil {
		panic("tools: NewContextTool requires StyleStatsIndex")
	}
	return &ContextTool{store: store, refs: refs, style: style, styleStats: styleStats}
}

func (t *ContextTool) Name() string { return "novel_context" }
func (t *ContextTool) Description() string {
	return "Lấy trạng thái hiện tại của tiểu thuyết và ngữ cảnh sáng tác." +
		"Không truyền chapter: trả về progress_status (các trường tiến độ như phase/flow/next_chapter/pending_rewrites) cùng thiết lập nền tảng, dùng để quyết định bước tiếp theo." +
		"Truyền chapter=N: trả thêm ngữ cảnh viết của chương đó như tóm tắt tiền tình, chi tiết gài trước, trạng thái nhân vật, quy tắc văn phong"
}
func (t *ContextTool) Label() string { return "Nạp ngữ cảnh" }

// Pure read tool; may be scheduled concurrently.
func (t *ContextTool) ReadOnly(_ json.RawMessage) bool        { return true }
func (t *ContextTool) ConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *ContextTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương. Không truyền thì trả về trạng thái tiến độ và thiết lập nền tảng (dành cho Architect); truyền thì trả thêm ngữ cảnh viết của chương đó (dành cho Writer/Editor)")),
	)
}

func (t *ContextTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Chapter int `json:"chapter"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}

	result := make(map[string]any)
	reads := &contextReads{}

	if a.Chapter > 0 {
		// Writer path: load the full base data + chapter context
		t.buildBaseContext(result, reads)
		seed := newChapterContextEnvelope()
		state := t.prepareChapterContext(a.Chapter, &seed, reads)
		seed.apply(result)
		t.buildChapterContext(result, state, reads)
		// Mechanical violation facts for this chapter (checked against user_rules at commit time and landed on disk):
		// the editor review maps them into the seven dimensions accordingly (editor.md mechanical-check mapping); the writer self-checks on rewrite.
		if violations := t.store.World.LoadRuleViolations(a.Chapter); len(violations) > 0 {
			result["rule_violations"] = violations
		}
		// episodic is a memo of text already written, not material still to be written.
		if epi, ok := result["episodic_memory"].(map[string]any); ok && len(epi) > 0 {
			epi["_usage"] = "Vùng này là ghi nhớ sự thật của phần nội dung chương đã viết (để đối chiếu nhất quán và nối mạch); nhắc lại nguyên văn các nội dung này trong nội dung chương mới là lỗi lặp"
		}
	} else {
		// Architect path: return only state + structured data, and do not load the full original text
		t.buildProgressStatus(result, reads)
		t.buildArchitectContext(result, reads)
	}

	// Inject working_memory.user_rules (canonical path). The architect path originally had no working_memory;
	// buildUserRules creates on demand a container holding only user_rules. When the snapshot is missing, fall back to the built-in defaults,
	// always emitting a stable structure so the LLM does not see user_rules=null and take an abnormal branch.
	if a.Chapter > 0 {
		t.buildSimulationProfile(result, "working_memory", reads)
	} else {
		t.buildSimulationProfile(result, "planning_memory", reads)
	}

	t.buildUserRules(result, reads)

	if reads.err != nil {
		return nil, reads.err
	}
	if len(reads.warnings) > 0 {
		result["_warnings"] = reads.warnings
	}

	// Priority budget: when the total size exceeds the threshold, trim low-priority data; the summary is rebuilt after trimming,
	// making sure the displayed field counts and _trimmed match the final payload.
	budget := 60 * 1024
	if a.Chapter > 0 {
		budget = 100 * 1024
	}
	return finalizeContextPayload(result, a.Chapter, budget)
}

func finalizeContextPayload(result map[string]any, chapter, budget int) (json.RawMessage, error) {
	if err := trimByBudget(result, budget); err != nil {
		return nil, err
	}
	result["_loading_summary"] = buildLoadingSummary(result, chapter)
	if err := trimByBudget(result, budget); err != nil {
		return nil, err
	}
	result["_loading_summary"] = buildLoadingSummary(result, chapter)

	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal context payload: %w", err)
	}
	if len(data) > budget {
		return nil, fmt.Errorf("context payload exceeds budget after summary rebuild: size=%d budget=%d", len(data), budget)
	}
	return data, nil
}

// buildLoadingSummary counts the amount of each kind of data in the assembled result and produces a one-line readable summary.
func buildLoadingSummary(result map[string]any, chapter int) string {
	var parts []string
	working, _ := result["working_memory"].(map[string]any)
	episodic, _ := result["episodic_memory"].(map[string]any)
	planning, _ := result["planning_memory"].(map[string]any)
	foundation, _ := result["foundation_memory"].(map[string]any)
	referencePack, _ := result["reference_pack"].(map[string]any)

	if chapter > 0 {
		parts = append(parts, fmt.Sprintf("ch=%d", chapter))
		if tier, ok := episodic["planning_tier"].(domain.PlanningTier); ok && tier != "" {
			parts = append(parts, fmt.Sprintf("tier=%s", tier))
		}
	} else {
		parts = append(parts, "architect")
		if tier, ok := planning["planning_tier"].(domain.PlanningTier); ok && tier != "" {
			parts = append(parts, fmt.Sprintf("tier=%s", tier))
		}
	}

	if pos, ok := episodic["position"].(map[string]any); ok {
		parts = append(parts, fmt.Sprintf("V%dA%d", pos["volume"], pos["arc"]))
	}

	var items []string

	if n := firstSliceLen(episodic["character_snapshots"], foundation["character_snapshots"]); n > 0 {
		items = append(items, fmt.Sprintf("nhân vật:%d (snapshot)", n))
	} else if n := firstSliceLen(episodic["characters"], foundation["characters"]); n > 0 {
		items = append(items, fmt.Sprintf("nhân vật:%d", n))
	}

	if len(working) > 0 {
		items = append(items, fmt.Sprintf("bộ nhớ làm việc:%d", len(working)))
	}
	if len(episodic) > 0 {
		items = append(items, fmt.Sprintf("bộ nhớ tình tiết:%d", len(episodic)))
	}
	if len(planning) > 0 {
		items = append(items, fmt.Sprintf("bộ nhớ kế hoạch:%d", len(planning)))
	}
	if len(foundation) > 0 {
		items = append(items, fmt.Sprintf("bộ nhớ nền tảng:%d", len(foundation)))
	}

	if n := firstSliceLen(working["volume_summaries"], planning["volume_summaries"]); n > 0 {
		items = append(items, fmt.Sprintf("tóm tắt tập:%d", n))
	}
	if n := firstSliceLen(working["arc_summaries"], planning["arc_summaries"]); n > 0 {
		items = append(items, fmt.Sprintf("tóm tắt cung:%d", n))
	}
	if n := sliceLen(working["recent_summaries"]); n > 0 {
		items = append(items, fmt.Sprintf("tóm tắt chương:%d", n))
	}

	if n := sliceLen(planning["layered_outline"]); n > 0 {
		items = append(items, fmt.Sprintf("dàn ý phân tầng:%d tập", n))
	}

	if n := sliceLen(working["timeline"]); n > 0 {
		items = append(items, fmt.Sprintf("dòng thời gian:%d", n))
	}
	if n := firstSliceLen(episodic["foreshadow_ledger"], foundation["foreshadow_ledger"]); n > 0 {
		items = append(items, fmt.Sprintf("chi tiết gài trước:%d", n))
	}
	if n := sliceLen(episodic["relationship_state"]); n > 0 {
		items = append(items, fmt.Sprintf("quan hệ:%d", n))
	}
	if n := sliceLen(episodic["recent_state_changes"]); n > 0 {
		items = append(items, fmt.Sprintf("thay đổi trạng thái:%d", n))
	}
	if _, ok := working["previous_tail"]; ok {
		items = append(items, "đuôi chương trước:ok")
	}
	if _, ok := referencePack["style_rules"]; ok {
		items = append(items, "quy tắc văn phong:ok")
	}
	if n := sliceLen(episodic["related_chapters"]); n > 0 {
		items = append(items, fmt.Sprintf("chương liên quan:%d", n))
	}
	if selected, ok := result["selected_memory"].(map[string]any); ok && len(selected) > 0 {
		if n := sliceLen(selected["story_threads"]); n > 0 {
			items = append(items, fmt.Sprintf("gợi lại manh mối:%d", n))
		}
		if n := sliceLen(selected["review_lessons"]); n > 0 {
			items = append(items, fmt.Sprintf("gợi lại đánh giá:%d", n))
		}
	}

	if refs, ok := referencePack["references"].(map[string]string); ok && len(refs) > 0 {
		items = append(items, fmt.Sprintf("tham khảo:%d mục", len(refs)))
	}
	if len(referencePack) > 0 {
		items = append(items, fmt.Sprintf("gói tham khảo:%d", len(referencePack)))
	}
	if _, ok := result["memory_policy"]; ok {
		items = append(items, "chiến lược bộ nhớ:ok")
	}
	if _, ok := working["simulation_profile"]; ok {
		items = append(items, "chân dung mô phỏng văn phong:ok")
	} else if _, ok := planning["simulation_profile"]; ok {
		items = append(items, "chân dung mô phỏng văn phong:ok")
	}
	if warnings, ok := result["_warnings"].([]string); ok && len(warnings) > 0 {
		items = append(items, fmt.Sprintf("cảnh báo:%d", len(warnings)))
	}
	if trimmed, ok := result["_trimmed"].([]string); ok && len(trimmed) > 0 {
		items = append(items, fmt.Sprintf("cắt bớt:%s", strings.Join(trimmed, ",")))
	}

	if len(items) > 0 {
		parts = append(parts, strings.Join(items, " "))
	}
	return strings.Join(parts, " | ")
}

// sliceLen tries to get the slice length of an any value.
func sliceLen(v any) int {
	switch s := v.(type) {
	case []domain.ChapterSummary:
		return len(s)
	case []domain.ArcSummary:
		return len(s)
	case []domain.VolumeSummary:
		return len(s)
	case []domain.CharacterSnapshot:
		return len(s)
	case []domain.TimelineEvent:
		return len(s)
	case []domain.ForeshadowEntry:
		return len(s)
	case []domain.RelationshipEntry:
		return len(s)
	case []domain.StateChange:
		return len(s)
	case []domain.VolumeOutline:
		return len(s)
	case []domain.Character:
		return len(s)
	case []domain.RelatedChapter:
		return len(s)
	case []domain.RecallItem:
		return len(s)
	case []planningVolumeOutline:
		return len(s)
	default:
		return 0
	}
}

func firstSliceLen(values ...any) int {
	for _, value := range values {
		if n := sliceLen(value); n > 0 {
			return n
		}
	}
	return 0
}

// loadFilteredCharacters filters characters by Tier and scene appearance.
// core/important are always returned; secondary/decorative are returned only when mentioned in the current chapter's outline.
func (t *ContextTool) loadFilteredCharacters(result map[string]any, chapter int, reads *contextReads) {
	chars, err := t.store.Characters.Load()
	if err != nil {
		reads.require("characters", err)
		return
	}
	if len(chars) == 0 {
		return
	}

	// Get the scene description from the current chapter's outline, used to match secondary characters
	entry, err := t.store.Outline.GetChapterOutline(chapter)
	if err != nil {
		reads.require("current_chapter_outline", err)
		result["characters"] = chars
		return
	}
	if entry == nil {
		result["characters"] = chars
		return
	}
	sceneText := strings.Join(entry.Scenes, " ") + " " + entry.CoreEvent + " " + entry.Title

	var filtered []domain.Character
	for _, c := range chars {
		switch c.Tier {
		case "secondary", "decorative":
			if matchCharacter(sceneText, c) {
				filtered = append(filtered, c)
			}
		default: // core, important, or unset
			filtered = append(filtered, c)
		}
	}
	result["characters"] = filtered
}

// matchCharacter checks whether the scene text contains a character's formal name or any alias.
func matchCharacter(text string, c domain.Character) bool {
	if strings.Contains(text, c.Name) {
		return true
	}
	for _, alias := range c.Aliases {
		if strings.Contains(text, alias) {
			return true
		}
	}
	return false
}

// loadLayeredSummaries loads layered summaries: volume summaries + the current volume's arc summaries + chapter summaries within the arc.
func (t *ContextTool) loadLayeredSummaries(result map[string]any, chapter, summaryWindow int, reads *contextReads) {
	vol, arc, err := t.store.Outline.LocateChapter(chapter)
	if err != nil {
		reads.require("layered_outline_position", err)
		return
	}

	// 1. Volume summaries of completed volumes
	if volSummaries, err := t.store.Summaries.LoadAllVolumeSummaries(); err == nil && len(volSummaries) > 0 {
		result["volume_summaries"] = volSummaries
	} else {
		reads.require("volume_summaries", err)
	}

	// 2. Arc summaries of completed arcs in the current volume (excluding the current arc)
	if arcSummaries, err := t.store.Summaries.LoadArcSummaries(vol); err == nil && len(arcSummaries) > 0 {
		var prior []domain.ArcSummary
		for _, s := range arcSummaries {
			if s.Arc < arc {
				prior = append(prior, s)
			}
		}
		if len(prior) > 0 {
			result["arc_summaries"] = prior
		}
	} else {
		reads.require("arc_summaries", err)
	}

	// 3. Chapter summaries of the most recent N chapters within the current arc
	if summaries, err := t.store.Summaries.LoadRecentSummaries(chapter, summaryWindow); err == nil && len(summaries) > 0 {
		result["recent_summaries"] = summaries
	} else {
		reads.require("recent_summaries", err)
	}
}

// loadLayeredCharacters loads characters in Layered mode: prefer the latest snapshot, fall back to the original settings + Tier filtering.
func (t *ContextTool) loadLayeredCharacters(result map[string]any, chapter int, reads *contextReads) {
	snapshots, err := t.store.Characters.LoadLatestSnapshots()
	if err == nil && len(snapshots) > 0 {
		result["character_snapshots"] = snapshots
		// Also keep the core/important characters from the original settings (the snapshot may not contain newly introduced characters)
		t.loadFilteredCharacters(result, chapter, reads)
		return
	}
	reads.require("character_snapshots", err)
	// With no snapshot, fall back to the original settings
	t.loadFilteredCharacters(result, chapter, reads)
}

// writerReferences returns the writing reference material. Chapter 1 gets everything; later chapters have templates that are no longer needed trimmed away.
func (t *ContextTool) writerReferences(chapter int) map[string]string {
	refs := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			refs[k] = v
		}
	}
	// Progressive loading: always keep the core references; the first 3 chapters additionally load the full writing guide
	add("consistency", t.refs.Consistency)
	add("hook_techniques", t.refs.HookTechniques)
	add("quality_checklist", t.refs.QualityChecklist)
	add("anti_ai_tone", t.refs.AntiAITone) // anti-AI-tone criteria are injected throughout and are not trimmed by chapter
	if chapter <= 3 {
		add("chapter_guide", t.refs.ChapterGuide)
		add("dialogue_writing", t.refs.DialogueWriting)
		add("style_reference", t.refs.StyleReference)
	}

	// Supplementary references loaded only for the first chapter
	if chapter <= 1 {
		add("chapter_template", t.refs.ChapterTemplate)
		add("content_expansion", t.refs.ContentExpansion)
	}
	return refs
}

func (t *ContextTool) architectReferences() map[string]string {
	refs := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			refs[k] = v
		}
	}
	add("outline_template", t.refs.OutlineTemplate)
	add("character_template", t.refs.CharacterTemplate)
	add("longform_planning", t.refs.LongformPlanning)
	add("differentiation", t.refs.Differentiation)
	add("style_reference", t.refs.StyleReference)
	add("arc_templates", t.refs.ArcTemplates)
	add("anti_ai_tone", t.refs.AntiAITone) // architect de-AI-tones the outline; also covers the editor on the Chapter=0 path
	return refs
}

// foundationStatus checks the completeness of the foundation settings and returns the list of missing items.
// It shares the store.FoundationMissing criteria with the save_foundation tool, guaranteeing that the ready/missing the LLM sees from
// novel_context always matches the foundation_ready returned by save_foundation
// (details such as the required long-form compass items will not drift).
func (t *ContextTool) foundationStatus() (map[string]any, error) {
	missing, err := t.store.FoundationMissing()
	if err != nil {
		return nil, err
	}
	status := map[string]any{"ready": len(missing) == 0}
	if len(missing) > 0 {
		status["missing"] = missing
	}
	if len(missing) == 1 && missing[0] == "foundation_audit" {
		fingerprint, err := t.store.FoundationFingerprint()
		if err != nil {
			return nil, err
		}
		status["fingerprint"] = fingerprint
	}
	if audit, err := t.store.Outline.LoadFoundationAudit(); err != nil {
		return nil, err
	} else if audit != nil && !audit.Ready {
		status["last_audit"] = audit
	}
	return status, nil
}

// trimByBudget trims result by priority so that the total JSON size does not exceed budget bytes.
// Priority (low to high): references < voice_samples < style_anchors < previous_tail < timeline
//
//	< recent_state_changes < foreshadow_ledger < relationship_state < the rest (not trimmed)
//
// style_stats is a size-bounded whole-book core signal and takes no part in trimming.
//
// Trimmed keys are recorded in result["_trimmed"] for log troubleshooting.
func trimByBudget(result map[string]any, budget int) error {
	// Measure the current size first
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("measure context payload: %w", err)
	}
	if len(data) <= budget {
		return nil
	}

	// List the trimmable keys by priority from low to high
	trimOrder := []string{
		"references",
		"voice_samples",
		"style_anchors",
		"style_rules",
		"previous_tail",
		"timeline",
		"recent_state_changes",
		"foreshadow_ledger",
		"relationship_state",
	}

	trimmed, _ := result["_trimmed"].([]string)
	trimmed = append([]string(nil), trimmed...)
	for _, key := range trimOrder {
		if !deleteContextKey(result, key) {
			continue
		}
		trimmed = append(trimmed, key)
		result["_trimmed"] = append([]string(nil), trimmed...)
		data, err = json.Marshal(result)
		if err != nil {
			return fmt.Errorf("measure trimmed context payload: %w", err)
		}
		if len(data) <= budget {
			return nil
		}
	}
	return fmt.Errorf("context payload exceeds budget after trimming: size=%d budget=%d", len(data), budget)
}

func deleteContextKey(result map[string]any, key string) bool {
	deleted := false
	for _, containerKey := range []string{
		"working_memory",
		"episodic_memory",
		"planning_memory",
		"foundation_memory",
		"reference_pack",
		"selected_memory",
	} {
		section, ok := result[containerKey].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := section[key]; ok {
			delete(section, key)
			deleted = true
		}
	}
	return deleted
}

// buildRelatedChapters looks up, from structured data, historical chapters related to the current chapter.
// It recommends along four dimensions (foreshadowing, character appearances, state changes, relationships) and returns at most 5 after deduplication.
// All data is passed in as parameters; no extra IO is done.
func (t *ContextTool) buildRelatedChapters(
	chapter int,
	entry *domain.OutlineEntry,
	foreshadow []domain.ForeshadowEntry,
	relationships []domain.RelationshipEntry,
	stateChanges []domain.StateChange,
	reads *contextReads,
) []domain.RelatedChapter {
	const recentWindow = 10
	const maxResults = 5

	seen := make(map[int]struct{})
	var results []domain.RelatedChapter
	add := func(ch int, reason string) {
		if ch <= 0 || ch >= chapter {
			return
		}
		// The most recent few chapters are too close, so they are not recommended
		if ch > chapter-recentWindow {
			return
		}
		if _, ok := seen[ch]; ok {
			return
		}
		seen[ch] = struct{}{}
		results = append(results, domain.RelatedChapter{Chapter: ch, Reason: reason})
	}

	// Concatenate the outline text for keyword matching
	outlineText := entry.Title + " " + entry.CoreEvent
	for _, s := range entry.Scenes {
		outlineText += " " + s
	}

	// 1. Foreshadowing lookup: is the description of active foreshadowing related to the current chapter's outline
	for _, f := range foreshadow {
		if strings.Contains(outlineText, f.ID) || containsAny(outlineText, strings.Fields(f.Description)) {
			add(f.PlantedAt, fmt.Sprintf("chương gieo chi tiết gài trước %s(%s)", f.ID, truncateRunes(f.Description, 15)))
		}
		if len(results) >= maxResults {
			break
		}
	}

	// 2. Character appearance lookup: one batched pass, cutting IO from O(characters x chapters) to O(chapters)
	chars, err := t.store.Characters.Load()
	if err != nil {
		reads.warn("related_chapters.characters", err)
	}
	outlineChars := matchOutlineCharacters(outlineText, chars)
	if len(outlineChars) > 0 {
		appearances, err := t.store.Summaries.FindCharacterAppearances(outlineChars, chapter, recentWindow)
		if err != nil {
			reads.warn("related_chapters.summaries", err)
		}
		for _, name := range outlineChars {
			if len(results) >= maxResults {
				break
			}
			if ch, ok := appearances[name]; ok {
				add(ch, fmt.Sprintf("chương nhân vật '%s' xuất hiện lần cuối", name))
			}
		}
	}

	// 3. State change lookup: operates on the already loaded slice, zero IO
	for _, name := range outlineChars {
		if len(results) >= maxResults {
			break
		}
		ch := findLastStateChange(stateChanges, name, chapter)
		if ch > 0 && ch <= chapter-recentWindow {
			add(ch, fmt.Sprintf("chương thay đổi trạng thái của '%s'", name))
		}
	}

	// 4. Relationship lookup: the last change in the relationship between the pairs of characters involved in the current chapter
	if len(relationships) > 0 && len(outlineChars) >= 2 {
		charSet := make(map[string]struct{}, len(outlineChars))
		for _, c := range outlineChars {
			charSet[c] = struct{}{}
		}
		for _, r := range relationships {
			if len(results) >= maxResults {
				break
			}
			_, aIn := charSet[r.CharacterA]
			_, bIn := charSet[r.CharacterB]
			if aIn && bIn {
				add(r.Chapter, fmt.Sprintf("chương thay đổi quan hệ %s-%s", r.CharacterA, r.CharacterB))
			}
		}
	}

	return results
}

// findLastStateChange finds, in the loaded state change list, the chapter number of an entity's most recent change.
func findLastStateChange(changes []domain.StateChange, entity string, currentChapter int) int {
	for i := len(changes) - 1; i >= 0; i-- {
		if changes[i].Entity == entity && changes[i].Chapter < currentChapter {
			return changes[i].Chapter
		}
	}
	return 0
}

// matchOutlineCharacters matches appearing character names from the outline text.
func matchOutlineCharacters(text string, chars []domain.Character) []string {
	var matched []string
	for _, c := range chars {
		if strings.Contains(text, c.Name) {
			matched = append(matched, c.Name)
			continue
		}
		for _, alias := range c.Aliases {
			if strings.Contains(text, alias) {
				matched = append(matched, c.Name)
				break
			}
		}
	}
	return matched
}

// containsAny checks whether text contains any word in words (only words of at least 2 characters match, to avoid noise).
func containsAny(text string, words []string) bool {
	for _, w := range words {
		if len([]rune(w)) >= 2 && strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func (t *ContextTool) selectStoryThreads(state contextBuildState) []domain.RecallItem {
	if state.currentEntry == nil {
		return nil
	}
	if len(state.foreshadow) < storyThreadRecallThreshold {
		return nil
	}

	const maxThreads = 5
	var items []domain.RecallItem
	seen := make(map[string]struct{})
	picked := make(map[string]struct{}) // IDs of foreshadowing already selected, used to dedupe the age backfill
	add := func(item domain.RecallItem) {
		key := item.Kind + "|" + item.Key + "|" + item.Summary
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		picked[item.Key] = struct{}{}
		items = append(items, item)
	}

	// 1. Relevance recall: foreshadowing that overlaps with the current chapter's focus words.
	focusTerms := recallFocusTerms(state.currentEntry, state.chapterPlan)
	focusText := strings.Join(focusTerms, " ")
	for _, entry := range state.foreshadow {
		if !matchesRecallTerms(entry.ID+" "+entry.Description, focusTerms) && !strings.Contains(focusText, entry.ID) {
			continue
		}
		add(domain.RecallItem{
			Kind:    "story_thread",
			Key:     entry.ID,
			Chapter: entry.PlantedAt,
			Reason:  "Chương hiện tại có thể cần nối tiếp chi tiết gài trước đã có",
			Summary: fmt.Sprintf("Chi tiết gài trước “%s” được gieo ở chương %d: %s", entry.ID, entry.PlantedAt, truncateRunes(entry.Description, 30)),
		})
		if len(items) >= maxThreads {
			return items
		}
	}

	// 2. Age backfill: foreshadowing unrelated to the current chapter but long unresolved (oldest first), filling the remaining slots.
	//    This covers the natural blind spot of relevance recall: a thread that dangles alone for too long without hitting a keyword in this chapter.
	for _, entry := range agingForeshadow(state.foreshadow, state.chapter, picked) {
		add(domain.RecallItem{
			Kind:    "story_thread",
			Key:     entry.ID,
			Chapter: entry.PlantedAt,
			Reason:  "Chi tiết gài trước treo lâu chưa thu, cần chú ý kịp thời đẩy tiếp hoặc thu lại",
			Summary: fmt.Sprintf("Chi tiết gài trước “%s” được gieo ở chương %d, đã %d chương chưa thu: %s", entry.ID, entry.PlantedAt, state.chapter-entry.PlantedAt, truncateRunes(entry.Description, 30)),
		})
		if len(items) >= maxThreads {
			break
		}
	}

	return items
}

// agingForeshadow returns unresolved foreshadowing with age >= foreshadowAgingChapters, sorted oldest first,
// skipping those in picked that relevance recall already selected. The input all is already the active (unresolved) list, so there is no need to filter by status again.
func agingForeshadow(all []domain.ForeshadowEntry, chapter int, picked map[string]struct{}) []domain.ForeshadowEntry {
	var aging []domain.ForeshadowEntry
	for _, e := range all {
		if _, ok := picked[e.ID]; ok {
			continue
		}
		if e.PlantedAt <= 0 || chapter-e.PlantedAt < foreshadowAgingChapters {
			continue
		}
		aging = append(aging, e)
	}
	sort.SliceStable(aging, func(i, j int) bool {
		return aging[i].PlantedAt < aging[j].PlantedAt
	})
	return aging
}

func (t *ContextTool) selectReviewLessons(chapter int, reads *contextReads) []domain.RecallItem {
	if chapter <= 1 {
		return nil
	}

	var items []domain.RecallItem
	seen := make(map[string]struct{})
	add := func(item domain.RecallItem) {
		key := item.Summary
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		items = append(items, item)
	}

	appendReview := func(review *domain.ReviewEntry) bool {
		if review == nil {
			return false
		}
		for i, miss := range review.ContractMisses {
			add(domain.RecallItem{
				Kind:    "review_lesson",
				Key:     fmt.Sprintf("review-%d-contract-%d", review.Chapter, i),
				Chapter: review.Chapter,
				Reason:  "Đánh giá gần nhất chỉ ra thiếu sót so với contract",
				Summary: fmt.Sprintf("Chương %d thiếu sót so với contract: %s", review.Chapter, miss),
			})
			if len(items) >= 3 {
				return true
			}
		}
		for i, issue := range review.Issues {
			switch issue.Severity {
			case "", "warning", "error", "critical":
				add(domain.RecallItem{
					Kind:    "review_lesson",
					Key:     fmt.Sprintf("review-%d-issue-%d", review.Chapter, i),
					Chapter: review.Chapter,
					Reason:  "Đánh giá gần nhất chỉ ra cần tránh lỗi lặp",
					Summary: fmt.Sprintf("Nhắc nhở đánh giá chương %d: %s", review.Chapter, truncateRunes(issue.Description, 36)),
				})
			}
			if len(items) >= 3 {
				return true
			}
		}
		return false
	}

	for ch := chapter - 1; ch >= max(chapter-3, 1); ch-- {
		review, err := t.store.World.LoadReview(ch)
		if err != nil {
			reads.warn("review", err)
			continue
		}
		if appendReview(review) {
			return items
		}
	}

	globalReview, err := t.store.World.LoadLastReview(chapter - 1)
	if err != nil {
		reads.warn("global_review", err)
	} else if appendReview(globalReview) {
		return items
	}
	return items
}

func recallFocusTerms(entry *domain.OutlineEntry, plan *domain.ChapterPlan) []string {
	if entry == nil {
		return nil
	}
	var terms []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			terms = append(terms, v)
		}
	}

	add(entry.Title)
	add(entry.CoreEvent)
	add(entry.Hook)
	for _, scene := range entry.Scenes {
		add(scene)
	}
	if plan != nil {
		add(plan.Goal)
		add(plan.Hook)
		for _, point := range plan.Contract.PayoffPoints {
			add(point)
		}
		add(plan.Contract.HookGoal)
	}
	return terms
}

func matchesRecallTerms(text string, terms []string) bool {
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if len([]rune(term)) < 2 {
			continue
		}
		if strings.Contains(text, term) || strings.Contains(term, text) {
			return true
		}
		if hasMeaningfulOverlap(term, text) {
			return true
		}
	}
	return false
}

func hasMeaningfulOverlap(a, b string) bool {
	ar := []rune(strings.TrimSpace(a))
	br := []rune(strings.TrimSpace(b))
	if len(ar) < 5 || len(br) < 5 {
		return false
	}
	shorter := len(ar)
	if len(br) < shorter {
		shorter = len(br)
	}
	threshold := 5
	switch {
	case shorter >= 12:
		threshold = 7
	case shorter >= 9:
		threshold = 6
	}
	return longestCommonSubstringRunes(ar, br) >= threshold
}

const storyThreadRecallThreshold = 6
const storyThreadRecallMinSelected = 2

// foreshadowAgingChapters: a foreshadowing still unresolved more than this many chapters after being planted is considered "long-dangling".
// Such foreshadowing is backfilled into story_threads even when unrelated to the current chapter's keywords, so it is not completely forgotten in a long-form work
// (relevance recall naturally sees only threads related to this chapter, and cannot see one that dangles alone for too long).
// Age is a fact derived purely by code (current chapter - planting chapter); it only states "unresolved for N chapters" and gives no instructions.
const foreshadowAgingChapters = 30

func longestCommonSubstringRunes(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	best := 0
	for i := 1; i <= len(a); i++ {
		curr := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			if a[i-1] != b[j-1] {
				continue
			}
			curr[j] = prev[j-1] + 1
			if curr[j] > best {
				best = curr[j]
			}
		}
		prev = curr
	}
	return best
}

// truncateRunes truncates a string to the given number of runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
