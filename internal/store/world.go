package store

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
)

// WorldStore manages the timeline, foreshadowing, character relationships, state changes, world rules, style rules, reviews and handoffs.
type WorldStore struct {
	io *IO

	timeline                *appendLog[domain.TimelineEvent]
	stateChanges            *appendLog[domain.StateChange]
	timelineProjectionReady bool
}

func NewWorldStore(io *IO) *WorldStore {
	return &WorldStore{
		io: io,
		timeline: newAppendLog(
			"timeline.jsonl",
			"timeline.json",
			timelineEventKey,
			cloneTimelineEvent,
		),
		stateChanges: newAppendLog(
			"meta/state_changes.jsonl",
			"meta/state_changes.json",
			stateChangeKey,
			func(change domain.StateChange) domain.StateChange { return change },
		),
	}
}

// ── Timeline ──

// SaveTimeline fully replaces the timeline facts and the human-readable projection.
func (s *WorldStore) SaveTimeline(events []domain.TimelineEvent) error {
	return s.io.WithWriteLock(func() error {
		if err := s.timeline.replaceUnlocked(s.io, events); err != nil {
			s.timelineProjectionReady = false
			return err
		}
		if err := s.io.WriteMarkdownUnlocked("timeline.md", renderTimeline(events, s.io.labels())); err != nil {
			s.timelineProjectionReady = false
			return err
		}
		s.timelineProjectionReady = true
		return nil
	})
}

// LoadTimeline reads the timeline.
func (s *WorldStore) LoadTimeline() ([]domain.TimelineEvent, error) {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.timeline.allUnlocked(s.io)
}

// AppendTimelineEvents appends timeline events. When the same event is submitted repeatedly it is deduplicated by a stable key, ensuring
// that a rerun after a commit_chapter crash does not pollute the timeline.
func (s *WorldStore) AppendTimelineEvents(newEvents []domain.TimelineEvent) error {
	return s.io.WithWriteLock(func() error {
		if !s.timelineProjectionReady {
			existing, err := s.timeline.allUnlocked(s.io)
			if err != nil {
				return err
			}
			if err := s.ensureTimelineProjectionUnlocked(existing); err != nil {
				return err
			}
		}

		added, err := s.timeline.appendUnlocked(s.io, newEvents)
		if err != nil {
			// When an append errors, the disk may already hold some complete records; before replay the projection must be
			// rebuilt from the fact log, not relying solely on the return value of added.
			s.timelineProjectionReady = false
			return err
		}
		if len(added) == 0 {
			return nil
		}
		if err := s.io.AppendLineUnlocked("timeline.md", []byte(renderTimelineEntries(added, s.io.labels()))); err != nil {
			s.timelineProjectionReady = false
			return err
		}
		return nil
	})
}

// ensureTimelineProjectionUnlocked reconciles timeline.md on the process's first append or after the last projection failed.
// The normal path does a full reconciliation only once, after which each chapter is appended in sync with the JSONL; the projection takes no part in fact reads.
func (s *WorldStore) ensureTimelineProjectionUnlocked(events []domain.TimelineEvent) error {
	if s.timelineProjectionReady {
		return nil
	}
	expected := renderTimeline(events, s.io.labels())
	actual, err := s.io.ReadFileUnlocked("timeline.md")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if string(actual) != expected {
		if err := s.io.WriteMarkdownUnlocked("timeline.md", expected); err != nil {
			return err
		}
	}
	s.timelineProjectionReady = true
	return nil
}

// LoadRecentTimeline returns the timeline events within the most recent window chapters.
func (s *WorldStore) LoadRecentTimeline(current, window int) ([]domain.TimelineEvent, error) {
	all, err := s.LoadTimeline()
	if err != nil {
		return nil, err
	}
	minCh := max(current-window, 1)
	var filtered []domain.TimelineEvent
	for _, e := range all {
		if e.Chapter >= minCh {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
}

// ── Foreshadowing ──

// SaveForeshadowLedger fully writes foreshadow_ledger.json + foreshadow_ledger.md (atomic write).
func (s *WorldStore) SaveForeshadowLedger(entries []domain.ForeshadowEntry) error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.WriteJSONUnlocked("foreshadow_ledger.json", entries); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("foreshadow_ledger.md", renderForeshadow(entries, s.io.labels()))
	})
}

// LoadForeshadowLedger reads the foreshadowing ledger.
func (s *WorldStore) LoadForeshadowLedger() ([]domain.ForeshadowEntry, error) {
	var entries []domain.ForeshadowEntry
	if err := s.io.ReadJSON("foreshadow_ledger.json", &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// UpdateForeshadow applies foreshadowing incremental operations in batch.
func (s *WorldStore) UpdateForeshadow(chapter int, updates []domain.ForeshadowUpdate) error {
	return s.io.WithWriteLock(func() error {
		var entries []domain.ForeshadowEntry
		if err := s.io.ReadJSONUnlocked("foreshadow_ledger.json", &entries); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
		idx := make(map[string]int, len(entries))
		for i, e := range entries {
			idx[e.ID] = i
		}
		for _, u := range updates {
			if strings.TrimSpace(u.ID) == "" {
				return fmt.Errorf("foreshadow id không được để trống")
			}
			switch u.Action {
			case "plant":
				if strings.TrimSpace(u.Description) == "" {
					return fmt.Errorf("plant foreshadow %q requires description", u.ID)
				}
				if i, ok := idx[u.ID]; ok {
					// A repeated plant in the same chapter is a full replay of the chapter record and must be idempotent; a different chapter hitting the same id
					// means a new foreshadowing item blocked by an old entry — previously this always did continue, and the new foreshadowing and its description were silently dropped.
					// In testing, a 22-chapter book planted 6 times but only 2 entries were recorded: the model used the same placeholder id every time
					// ("1" and "new"), 4 foreshadowing items evaporated without a trace, and nothing reported an error anywhere.
					if planted := entries[i].PlantedAt; planted != 0 && planted != chapter {
						return fmt.Errorf(
							"id chi tiết gài trước %q đã bị chương %d chiếm (%s); mỗi chi tiết gài trước cần một id riêng và ổn định, "+
								"đừng dùng lại placeholder như \"new\"/\"1\" — các chương sau dựa vào id để advance/resolve: %w",
							u.ID, planted, truncateForError(entries[i].Description), errs.ErrToolArgs)
					}
					if entries[i].Description == "" {
						entries[i].Description = u.Description
					}
					if entries[i].PlantedAt == 0 {
						entries[i].PlantedAt = chapter
					}
					if entries[i].Status == "" {
						entries[i].Status = "planted"
					}
					continue
				}
				idx[u.ID] = len(entries)
				entries = append(entries, domain.ForeshadowEntry{
					ID:          u.ID,
					Description: u.Description,
					PlantedAt:   chapter,
					Status:      "planted",
				})
			case "advance":
				if i, ok := idx[u.ID]; ok {
					entries[i].Status = "advanced"
				} else {
					return fmt.Errorf("advance unknown foreshadow %q", u.ID)
				}
			case "resolve":
				if i, ok := idx[u.ID]; ok {
					entries[i].Status = "resolved"
					entries[i].ResolvedAt = chapter
				} else {
					return fmt.Errorf("resolve unknown foreshadow %q", u.ID)
				}
			default:
				return fmt.Errorf("invalid foreshadow action %q", u.Action)
			}
		}
		if err := s.io.WriteJSONUnlocked("foreshadow_ledger.json", entries); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("foreshadow_ledger.md", renderForeshadow(entries, s.io.labels()))
	})
}

// LoadActiveForeshadow returns the foreshadowing entries not yet resolved.
func (s *WorldStore) LoadActiveForeshadow() ([]domain.ForeshadowEntry, error) {
	all, err := s.LoadForeshadowLedger()
	if err != nil {
		return nil, err
	}
	var active []domain.ForeshadowEntry
	for _, e := range all {
		if e.Status != "resolved" {
			active = append(active, e)
		}
	}
	return active, nil
}

// ── Character relationships ──

// SaveRelationships fully writes relationship_state.json + relationship_state.md (atomic write).
func (s *WorldStore) SaveRelationships(entries []domain.RelationshipEntry) error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.WriteJSONUnlocked("relationship_state.json", entries); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("relationship_state.md", renderRelationships(entries, s.io.labels()))
	})
}

// LoadRelationships reads the character relationship state.
func (s *WorldStore) LoadRelationships() ([]domain.RelationshipEntry, error) {
	var entries []domain.RelationshipEntry
	if err := s.io.ReadJSON("relationship_state.json", &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// UpdateRelationships merges relationship changes.
func (s *WorldStore) UpdateRelationships(changes []domain.RelationshipEntry) error {
	return s.io.WithWriteLock(func() error {
		var existing []domain.RelationshipEntry
		if err := s.io.ReadJSONUnlocked("relationship_state.json", &existing); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
		idx := make(map[string]int, len(existing))
		for i, e := range existing {
			idx[pairKey(e.CharacterA, e.CharacterB)] = i
		}
		for _, c := range changes {
			key := pairKey(c.CharacterA, c.CharacterB)
			if i, ok := idx[key]; ok {
				existing[i].Relation = c.Relation
				existing[i].Chapter = c.Chapter
			} else {
				idx[key] = len(existing)
				existing = append(existing, c)
			}
		}
		if err := s.io.WriteJSONUnlocked("relationship_state.json", existing); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("relationship_state.md", renderRelationships(existing, s.io.labels()))
	})
}

// ── State changes ──

// AppendStateChanges appends character state changes. When the same state change is submitted repeatedly it is deduplicated by a stable key.
func (s *WorldStore) AppendStateChanges(changes []domain.StateChange) error {
	return s.io.WithWriteLock(func() error {
		_, err := s.stateChanges.appendUnlocked(s.io, changes)
		return err
	})
}

// LoadStateChanges reads all state-change records.
func (s *WorldStore) LoadStateChanges() ([]domain.StateChange, error) {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	return s.stateChanges.allUnlocked(s.io)
}

// SaveStateChanges fully replaces the state-change facts, so the projection can be rebuilt after a chapter revision.
func (s *WorldStore) SaveStateChanges(changes []domain.StateChange) error {
	return s.io.WithWriteLock(func() error {
		return s.stateChanges.replaceUnlocked(s.io, changes)
	})
}

// ── World rules ──

// SaveWorldRules fully writes world_rules.json + world_rules.md (atomic write).
func (s *WorldStore) SaveWorldRules(rules []domain.WorldRule) error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.WriteJSONUnlocked("world_rules.json", rules); err != nil {
			return err
		}
		return s.io.WriteMarkdownUnlocked("world_rules.md", renderWorldRules(rules, s.io.labels()))
	})
}

// LoadWorldRules reads the world rules.
func (s *WorldStore) LoadWorldRules() ([]domain.WorldRule, error) {
	var rules []domain.WorldRule
	if err := s.io.ReadJSON("world_rules.json", &rules); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return rules, nil
}

// ── Style rules ──

// SaveStyleRules saves the writing style rules.
func (s *WorldStore) SaveStyleRules(rules domain.WritingStyleRules) error {
	return s.io.WriteJSON("meta/style_rules.json", rules)
}

// LoadStyleRules reads the writing style rules.
func (s *WorldStore) LoadStyleRules() (*domain.WritingStyleRules, error) {
	var rules domain.WritingStyleRules
	if err := s.io.ReadJSON("meta/style_rules.json", &rules); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &rules, nil
}

func (s *WorldStore) SaveAuthorRevisionStyle(style domain.AuthorRevisionStyle) error {
	return s.io.WriteJSON("meta/author_revision_style.json", style)
}

func (s *WorldStore) LoadAuthorRevisionStyle() (*domain.AuthorRevisionStyle, error) {
	var style domain.AuthorRevisionStyle
	if err := s.io.ReadJSON("meta/author_revision_style.json", &style); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &style, nil
}

// ── Reviews ──

// SaveReview saves a review result.
func (s *WorldStore) SaveReview(r domain.ReviewEntry) error {
	rel := fmt.Sprintf("reviews/%02d.json", r.Chapter)
	if r.Scope == "global" {
		rel = fmt.Sprintf("reviews/%02d-global.json", r.Chapter)
	}
	return s.io.WriteJSON(rel, r)
}

// HasArcReview checks whether the given chapter (the last chapter of an arc) has a saved scope=arc review.
func (s *WorldStore) HasArcReview(chapter int) (bool, error) {
	rv, err := s.LoadReview(chapter)
	if err != nil {
		return false, err
	}
	return rv != nil && rv.Scope == "arc", nil
}

// HasGlobalReview checks whether the given chapter has a saved scope=global review
// (save_review writes it to reviews/%02d-global.json; non-layered books trigger it per ReviewInterval).
func (s *WorldStore) HasGlobalReview(chapter int) (bool, error) {
	r, err := s.LoadGlobalReview(chapter)
	if err != nil {
		return false, err
	}
	return r != nil && r.Scope == "global", nil
}

// LoadGlobalReview reads the global review for the given cutoff chapter.
func (s *WorldStore) LoadGlobalReview(chapter int) (*domain.ReviewEntry, error) {
	var r domain.ReviewEntry
	if err := s.io.ReadJSON(fmt.Sprintf("reviews/%02d-global.json", chapter), &r); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// LoadReview reads the review result of a chapter.
func (s *WorldStore) LoadReview(chapter int) (*domain.ReviewEntry, error) {
	var r domain.ReviewEntry
	if err := s.io.ReadJSON(fmt.Sprintf("reviews/%02d.json", chapter), &r); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// LoadLastReview reads the most recent global review.
func (s *WorldStore) LoadLastReview(fromChapter int) (*domain.ReviewEntry, error) {
	for ch := fromChapter; ch >= 1; ch-- {
		var r domain.ReviewEntry
		if err := s.io.ReadJSON(fmt.Sprintf("reviews/%02d-global.json", ch), &r); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		return &r, nil
	}
	return nil, nil
}

// LoadReviewsAffectingChapter returns all reviews that explicitly put chapter into the rework queue,
// ordered newest to oldest. Arc/global reviews are stored at the review endpoint, so they can no longer be looked up by the target chapter's file name.
func (s *WorldStore) LoadReviewsAffectingChapter(chapter int) ([]domain.ReviewEntry, error) {
	entries, err := os.ReadDir(s.io.path("reviews"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var reviews []domain.ReviewEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(s.io.path("reviews/" + entry.Name()))
		if err != nil {
			return nil, err
		}
		var review domain.ReviewEntry
		if err := json.Unmarshal(data, &review); err != nil {
			return nil, fmt.Errorf("parse reviews/%s: %w", entry.Name(), err)
		}
		if slices.Contains(review.AffectedChapters, chapter) ||
			(review.Scope == "chapter" && review.Chapter == chapter && review.Verdict != "accept" && len(review.AffectedChapters) == 0) {
			reviews = append(reviews, review)
		}
	}
	slices.SortFunc(reviews, func(a, b domain.ReviewEntry) int {
		return b.Chapter - a.Chapter
	})
	return reviews, nil
}

// ── render helpers ──

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func timelineEventKey(e domain.TimelineEvent) string {
	chars := append([]string(nil), e.Characters...)
	slices.Sort(chars)
	parts := make([]string, 0, len(chars)+2)
	parts = append(parts, e.Time, e.Event)
	parts = append(parts, chars...)
	return stableRecordKey(e.Chapter, parts...)
}

func cloneTimelineEvent(event domain.TimelineEvent) domain.TimelineEvent {
	event.Characters = append([]string(nil), event.Characters...)
	return event
}

func stateChangeKey(c domain.StateChange) string {
	return stableRecordKey(c.Chapter, c.Entity, c.Field, c.OldValue, c.NewValue)
}

// stableRecordKey encodes variable text with a length prefix, so delimiters inside the content cannot cause dedup collisions.
func stableRecordKey(chapter int, parts ...string) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(chapter))
	for _, part := range parts {
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}

func renderTimeline(events []domain.TimelineEvent, l mdLabels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.timeline)
	b.WriteString(renderTimelineEntries(events, l))
	return b.String()
}

func renderTimelineEntries(events []domain.TimelineEvent, l mdLabels) string {
	var b strings.Builder
	for _, e := range events {
		chars := ""
		if len(e.Characters) > 0 {
			chars = l.openParen + strings.Join(e.Characters, l.listSep) + l.closeParen
		}
		fmt.Fprintf(&b, "- **"+l.chapterFmt+" [%s]**%s%s%s\n", e.Chapter, e.Time, l.colon, e.Event, chars)
	}
	return b.String()
}

func renderForeshadow(entries []domain.ForeshadowEntry, l mdLabels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.foreshadow)
	for _, e := range entries {
		status := e.Status
		if e.ResolvedAt > 0 {
			status = fmt.Sprintf(l.resolvedAtFmt, e.ResolvedAt)
		}
		fmt.Fprintf(&b, "- **[%s]** %s — "+l.plantedAtFmt+"\n",
			e.ID, e.Description, e.PlantedAt, status)
	}
	return b.String()
}

func renderRelationships(entries []domain.RelationshipEntry, l mdLabels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.relationships)
	for _, e := range entries {
		fmt.Fprintf(&b, "- **%s ↔ %s**%s%s"+l.atChapterFmt+"\n",
			e.CharacterA, e.CharacterB, l.colon, e.Relation, e.Chapter)
	}
	return b.String()
}

func renderWorldRules(rules []domain.WorldRule, l mdLabels) string {
	grouped := make(map[string][]domain.WorldRule)
	var order []string
	for _, r := range rules {
		cat := r.Category
		if cat == "" {
			cat = "other"
		}
		if _, exists := grouped[cat]; !exists {
			order = append(order, cat)
		}
		grouped[cat] = append(grouped[cat], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.worldRules)
	for _, cat := range order {
		fmt.Fprintf(&b, "## %s\n\n", cat)
		for _, r := range grouped[cat] {
			fmt.Fprintf(&b, "- **%s**%s%s\n", l.rule, l.colon, r.Rule)
			if r.Boundary != "" {
				fmt.Fprintf(&b, "  - %s%s%s\n", l.boundary, l.colon, r.Boundary)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ── Mechanical-violation facts per chapter ──
//
// The rule_violations from commit_chapter (warning-level results of the user_rules mechanical check) are persisted here;
// when the editor reviews that chapter it reads them via novel_context(chapter=N) and maps them into the seven-dimension review
// (editor.md §mechanical-check mapping). The writer sees them too when reworking that chapter. Append-only; the latest entry for a chapter wins.

// ChapterViolations is the mechanical-violation record of one chapter.
type ChapterViolations struct {
	Chapter    int               `json:"chapter"`
	Violations []rules.Violation `json:"violations"`
	At         string            `json:"at"`
}

const ruleViolationsFile = "meta/rule_violations.jsonl"

// SaveRuleViolations appends a chapter's mechanical violations (an empty list is also appended — it overwrites the old record, meaning cleared after the rewrite).
func (s *WorldStore) SaveRuleViolations(chapter int, violations []rules.Violation) error {
	rec := ChapterViolations{Chapter: chapter, Violations: violations, At: time.Now().Format(time.RFC3339)}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.io.AppendLine(ruleViolationsFile, append(data, '\n'))
}

// LoadRuleViolations reads a chapter's latest mechanical-violation record; returns nil if there is none.
func (s *WorldStore) LoadRuleViolations(chapter int) []rules.Violation {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	data, err := os.ReadFile(s.io.path(ruleViolationsFile))
	if err != nil {
		return nil
	}
	var latest []rules.Violation
	var found bool
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec ChapterViolations
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Chapter == chapter {
			latest, found = rec.Violations, true
		}
	}
	if !found {
		return nil
	}
	return latest
}

// truncateForError shortens the description, so an entire foreshadowing text is not stuffed into the error message.
func truncateForError(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 30 {
		return string(r)
	}
	return string(r[:30]) + "…"
}
