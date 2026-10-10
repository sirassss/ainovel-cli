package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// ErrOutlineChapterNotFound indicates the chapter has not yet entered the current outline.
var ErrOutlineChapterNotFound = errors.New("outline chapter not found")

// OutlineStore manages the story premise, the outline (flat/layered) and the compass.
type OutlineStore struct{ io *IO }

func NewOutlineStore(io *IO) *OutlineStore { return &OutlineStore{io: io} }

// SavePremise saves the story premise to premise.md.
func (s *OutlineStore) SavePremise(content string) error {
	return s.io.WriteMarkdown("premise.md", content)
}

// LoadPremise reads premise.md. Returns an empty string if it does not exist.
func (s *OutlineStore) LoadPremise() (string, error) {
	data, err := s.io.ReadFile("premise.md")
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

// SaveOutline saves both outline.json and outline.md (atomic write).
func (s *OutlineStore) SaveOutline(entries []domain.OutlineEntry) error {
	return s.io.WithWriteLock(func() error {
		return s.saveOutlineUnlocked(entries)
	})
}

func (s *OutlineStore) saveOutlineUnlocked(entries []domain.OutlineEntry) error {
	if err := s.io.WriteJSONUnlocked("outline.json", entries); err != nil {
		return err
	}
	return s.io.WriteMarkdownUnlocked("outline.md", renderOutline(entries, s.io.labels()))
}

// LoadOutline reads the structured outline from outline.json.
func (s *OutlineStore) LoadOutline() ([]domain.OutlineEntry, error) {
	var entries []domain.OutlineEntry
	if err := s.io.ReadJSON("outline.json", &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// GetChapterOutline gets the outline entry for the given chapter.
func (s *OutlineStore) GetChapterOutline(chapter int) (*domain.OutlineEntry, error) {
	entries, err := s.LoadOutline()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Chapter == chapter {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("%w: chapter %d", ErrOutlineChapterNotFound, chapter)
}

// SaveLayeredOutline treats the layered outline as the single source: it saves the layered view and rebuilds the flat derived view in sync.
// Callers do not need to, and should not, maintain outline.json/outline.md separately.
func (s *OutlineStore) SaveLayeredOutline(volumes []domain.VolumeOutline) error {
	return s.io.WithWriteLock(func() error {
		return s.saveLayeredViewsUnlocked(volumes)
	})
}

// LoadLayeredOutline reads the layered outline.
func (s *OutlineStore) LoadLayeredOutline() ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSON("layered_outline.json", &volumes); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return volumes, nil
}

// ClearLayeredOutline removes the layered outline files.
func (s *OutlineStore) ClearLayeredOutline() error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.RemoveFileUnlocked("layered_outline.json"); err != nil {
			return err
		}
		return s.io.RemoveFileUnlocked("layered_outline.md")
	})
}

// GetChapterFromLayered looks up a chapter in the layered outline by global chapter number.
func (s *OutlineStore) GetChapterFromLayered(chapter int) (*domain.OutlineEntry, error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil {
		return nil, err
	}
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for i := range a.Chapters {
				if ch == chapter {
					e := a.Chapters[i]
					e.Chapter = ch
					return &e, nil
				}
				ch++
			}
		}
	}
	return nil, fmt.Errorf("%w: chapter %d in layered outline", ErrOutlineChapterNotFound, chapter)
}

// LocateChapter locates the volume and arc containing a chapter by its global chapter number.
func (s *OutlineStore) LocateChapter(chapter int) (volume, arc int, err error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil {
		return 0, 0, err
	}
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for range a.Chapters {
				if ch == chapter {
					return v.Index, a.Index, nil
				}
				ch++
			}
		}
	}
	return 0, 0, fmt.Errorf("%w: chapter %d in layered outline", ErrOutlineChapterNotFound, chapter)
}

// ArcBoundary is the arc boundary info.
type ArcBoundary struct {
	IsArcEnd       bool
	IsVolumeEnd    bool
	Volume         int
	Arc            int
	StartChapter   int
	EndChapter     int
	NextVolume     int
	NextArc        int
	NeedsExpansion bool
	NeedsNewVolume bool // End of volume and the current layered_outline has no next volume
}

// HasNextArc reports whether there are further arcs.
func (b *ArcBoundary) HasNextArc() bool {
	return b.NextVolume > 0 || b.NextArc > 0
}

// CheckArcBoundary checks whether a chapter is the last chapter of an arc/volume.
func (s *OutlineStore) CheckArcBoundary(chapter int) (*ArcBoundary, error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil || len(volumes) == 0 {
		return nil, err
	}

	type arcPos struct {
		volIdx, arcIdx int
		volume, arc    int
		chInArc        int
		arcLen         int
		arcStart       int
	}

	ch := 1
	var cur *arcPos
	for vi, v := range volumes {
		for ai, a := range v.Arcs {
			arcStart := ch
			for ci := range a.Chapters {
				if ch == chapter {
					cur = &arcPos{
						volIdx:   vi,
						arcIdx:   ai,
						volume:   v.Index,
						arc:      a.Index,
						chInArc:  ci,
						arcLen:   len(a.Chapters),
						arcStart: arcStart,
					}
				}
				ch++
			}
		}
	}
	if cur == nil {
		return nil, nil
	}

	b := &ArcBoundary{
		Volume:       cur.volume,
		Arc:          cur.arc,
		StartChapter: cur.arcStart,
		EndChapter:   cur.arcStart + cur.arcLen - 1,
	}

	isLastChInArc := cur.chInArc == cur.arcLen-1
	isLastArcInVol := cur.arcIdx == len(volumes[cur.volIdx].Arcs)-1

	// Next*/NeedsExpansion/NeedsNewVolume are meaningful only at the end of an arc; otherwise the coordinator would wrongly think the next arc must be expanded early.
	if !isLastChInArc {
		return b, nil
	}

	b.IsArcEnd = true
	if isLastArcInVol {
		b.IsVolumeEnd = true
	}

	found := false
	for vi := cur.volIdx; vi < len(volumes); vi++ {
		startArc := 0
		if vi == cur.volIdx {
			startArc = cur.arcIdx + 1
		}
		for ai := startArc; ai < len(volumes[vi].Arcs); ai++ {
			b.NextVolume = volumes[vi].Index
			b.NextArc = volumes[vi].Arcs[ai].Index
			b.NeedsExpansion = !volumes[vi].Arcs[ai].IsExpanded()
			found = true
			break
		}
		if found {
			break
		}
	}

	if b.IsVolumeEnd && !found {
		b.NeedsNewVolume = true
	}

	return b, nil
}

// CompletedArcBoundaries returns the completed detailed-arc boundaries in story order.
func (s *OutlineStore) CompletedArcBoundaries(lastCompleted int) ([]ArcBoundary, error) {
	volumes, err := s.LoadLayeredOutline()
	if err != nil {
		return nil, err
	}
	chapter := 1
	var result []ArcBoundary
	for _, volume := range volumes {
		for arcIndex, arc := range volume.Arcs {
			if len(arc.Chapters) == 0 {
				continue
			}
			start := chapter
			end := start + len(arc.Chapters) - 1
			chapter = end + 1
			if end > lastCompleted {
				return result, nil
			}
			result = append(result, ArcBoundary{
				IsArcEnd: true, IsVolumeEnd: arcIndex == len(volume.Arcs)-1,
				Volume: volume.Index, Arc: arc.Index, StartChapter: start, EndChapter: end,
			})
		}
	}
	return result, nil
}

// expandArcUnlocked is an internal method, called within the cross-domain coordination of Store.ExpandArc.
func (s *OutlineStore) expandArcUnlocked(volumeIdx, arcIdx int, expansion domain.ArcExpansion) ([]domain.VolumeOutline, error) {
	if strings.TrimSpace(expansion.Title) == "" {
		return nil, fmt.Errorf("Tiêu đề cung không được để trống")
	}
	if strings.TrimSpace(expansion.Goal) == "" {
		return nil, fmt.Errorf("Mục tiêu cung không được để trống")
	}
	if len(expansion.Chapters) == 0 {
		return nil, fmt.Errorf("Cung mở rộng phải có ít nhất một chương")
	}

	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w", err)
	}
	found := false
	for vi := range volumes {
		if volumes[vi].Index != volumeIdx {
			continue
		}
		for ai := range volumes[vi].Arcs {
			if volumes[vi].Arcs[ai].Index != arcIdx {
				continue
			}
			if volumes[vi].Arcs[ai].IsExpanded() {
				current := domain.ArcExpansion{
					Title:    volumes[vi].Arcs[ai].Title,
					Goal:     volumes[vi].Arcs[ai].Goal,
					Chapters: volumes[vi].Arcs[ai].Chapters,
				}
				if reflect.DeepEqual(current, expansion) {
					// An idempotent retry must still rewrite all the derived views below; the previous attempt may have completed only
					// layered_outline.json without yet writing the flat outline/Markdown.
					found = true
					break
				}
				return nil, fmt.Errorf("arc already expanded: volume=%d, arc=%d", volumeIdx, arcIdx)
			}
			volumes[vi].Arcs[ai].Title = expansion.Title
			volumes[vi].Arcs[ai].Goal = expansion.Goal
			volumes[vi].Arcs[ai].Chapters = expansion.Chapters
			volumes[vi].Arcs[ai].EstimatedChapters = 0
			found = true
			break
		}
		if found {
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("arc not found: volume=%d, arc=%d", volumeIdx, arcIdx)
	}
	if err := s.saveLayeredViewsUnlocked(volumes); err != nil {
		return nil, err
	}
	return volumes, nil
}

// appendVolumeUnlocked is an internal method, called within the cross-domain coordination of Store.AppendVolume.
func (s *OutlineStore) appendVolumeUnlocked(vol domain.VolumeOutline) ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w", err)
	}
	// The next step of AppendVolume is to update Progress. If the process is interrupted between "outline appended, Progress
	// not yet updated", recovery retries with the same persisted payload; an identical last volume must be treated as idempotent,
	// so the same-argument retry can go on to complete Progress instead of getting permanently stuck on a duplicate Index.
	if len(volumes) == 0 || !reflect.DeepEqual(volumes[len(volumes)-1], vol) {
		if err := validateAppendVolume(volumes, vol); err != nil {
			return nil, err
		}
		volumes = append(volumes, vol)
	}
	// Rewrite all derived views even if the last volume already exists; the previous attempt may have been interrupted right after the layered JSON was persisted
	// but before the flat outline/Markdown was written.
	if err := s.saveLayeredViewsUnlocked(volumes); err != nil {
		return nil, err
	}
	return volumes, nil
}

// saveLayeredViewsUnlocked treats the layered outline as the single source and uniformly rebuilds its Markdown and flat derived views.
// The caller must hold the OutlineStore write lock.
func (s *OutlineStore) saveLayeredViewsUnlocked(volumes []domain.VolumeOutline) error {
	domain.RenumberVolumes(volumes)
	if err := s.io.WriteJSONUnlocked("layered_outline.json", volumes); err != nil {
		return err
	}
	if err := s.io.WriteMarkdownUnlocked("layered_outline.md", renderLayeredOutline(volumes, s.io.labels())); err != nil {
		return err
	}
	if err := s.saveOutlineUnlocked(domain.FlattenOutline(volumes)); err != nil {
		return err
	}
	return nil
}

func (s *OutlineStore) reviseFlatTailUnlocked(fromChapter int, replacement []domain.OutlineEntry) ([]domain.OutlineEntry, error) {
	var outline []domain.OutlineEntry
	if err := s.io.ReadJSONUnlocked("outline.json", &outline); err != nil {
		return nil, fmt.Errorf("load outline: %w: %w", errs.ErrStoreRead, err)
	}
	if fromChapter > len(outline)+1 {
		return nil, fmt.Errorf("from_chapter=%d vượt quá cuối dàn ý %d: %w",
			fromChapter, len(outline), errs.ErrToolPrecondition)
	}
	updated := append([]domain.OutlineEntry(nil), outline[:fromChapter-1]...)
	updated = append(updated, replacement...)
	if len(updated) == 0 {
		return nil, fmt.Errorf("Dàn ý sau khi sửa không được để trống: %w", errs.ErrToolPrecondition)
	}
	for i := range updated {
		updated[i].Chapter = i + 1
	}
	if err := s.saveOutlineUnlocked(updated); err != nil {
		return nil, fmt.Errorf("save outline: %w: %w", errs.ErrStoreWrite, err)
	}
	return updated, nil
}

func (s *OutlineStore) reviseLayeredTailUnlocked(fromChapter int, replacement []domain.OutlineEntry) ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w: %w", errs.ErrStoreRead, err)
	}
	if err := reviseLayeredTail(volumes, fromChapter, replacement); err != nil {
		return nil, fmt.Errorf("%w: %w", errs.ErrToolPrecondition, err)
	}
	if err := s.saveLayeredViewsUnlocked(volumes); err != nil {
		return nil, fmt.Errorf("save layered outline: %w: %w", errs.ErrStoreWrite, err)
	}
	return volumes, nil
}

// reviseLayeredTail replaces the tail of the arc containing fromChapter, starting at that chapter. If fromChapter is exactly
// past the end of the current flat outline, it is appended to the last expanded arc.
func reviseLayeredTail(volumes []domain.VolumeOutline, fromChapter int, replacement []domain.OutlineEntry) error {
	chapter := 1
	targetVolume, targetArc, local := -1, -1, -1
	lastVolume, lastArc := -1, -1
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			chapters := volumes[vi].Arcs[ai].Chapters
			if len(chapters) == 0 {
				continue
			}
			lastVolume, lastArc = vi, ai
			if fromChapter >= chapter && fromChapter < chapter+len(chapters) {
				targetVolume, targetArc = vi, ai
				local = fromChapter - chapter
				break
			}
			chapter += len(chapters)
		}
		if targetVolume >= 0 {
			break
		}
	}
	if targetVolume < 0 && fromChapter == chapter && lastVolume >= 0 {
		targetVolume, targetArc = lastVolume, lastArc
		local = len(volumes[lastVolume].Arcs[lastArc].Chapters)
	}
	if targetVolume < 0 {
		return fmt.Errorf("from_chapter=%d nằm ngoài phạm vi dàn ý đã mở rộng", fromChapter)
	}

	arc := &volumes[targetVolume].Arcs[targetArc]
	updated := append([]domain.OutlineEntry(nil), arc.Chapters[:local]...)
	updated = append(updated, replacement...)
	if len(updated) == 0 {
		return fmt.Errorf("Cung mục tiêu sau khi sửa không được để trống")
	}
	arc.Chapters = updated
	arc.EstimatedChapters = 0
	return nil
}

func validateAppendVolume(existing []domain.VolumeOutline, vol domain.VolumeOutline) error {
	if len(existing) > 0 {
		maxIdx := existing[len(existing)-1].Index
		if vol.Index <= maxIdx {
			return fmt.Errorf("Index của tập %d phải lớn hơn giá trị lớn nhất hiện có %d", vol.Index, maxIdx)
		}
	}
	if len(vol.Arcs) == 0 {
		return fmt.Errorf("Tập mới phải có ít nhất một cung")
	}
	if !vol.Arcs[0].IsExpanded() {
		return fmt.Errorf("Cung đầu của tập mới phải có chương chi tiết")
	}
	return nil
}

// SaveCompass saves the end-game direction compass.
func (s *OutlineStore) SaveCompass(compass domain.StoryCompass) error {
	if compass.EndingDirection == "" {
		return fmt.Errorf("ending_direction không được để trống")
	}
	return s.io.WriteJSON("meta/compass.json", compass)
}

// LoadCompass reads the end-game direction compass.
func (s *OutlineStore) LoadCompass() (*domain.StoryCompass, error) {
	var c domain.StoryCompass
	if err := s.io.ReadJSON("meta/compass.json", &c); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// SaveFoundationAudit saves the Architect's semantic review of the current foundation version.
func (s *OutlineStore) SaveFoundationAudit(a domain.FoundationAudit) error {
	return s.io.WriteJSON("meta/foundation_audit.json", a)
}

// LoadFoundationAudit reads the most recent semantic review of the foundation.
func (s *OutlineStore) LoadFoundationAudit() (*domain.FoundationAudit, error) {
	var a domain.FoundationAudit
	if err := s.io.ReadJSON("meta/foundation_audit.json", &a); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func renderLayeredOutline(volumes []domain.VolumeOutline, l mdLabels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.layeredOutline)
	ch := 1
	for _, v := range volumes {
		fmt.Fprintf(&b, "## "+l.volumeFmt+"%s%s\n\n", v.Index, l.colon, v.Title)
		fmt.Fprintf(&b, "**%s**%s%s\n\n", l.theme, l.colon, v.Theme)
		for _, a := range v.Arcs {
			fmt.Fprintf(&b, "### "+l.arcFmt+"%s%s\n\n", a.Index, l.colon, a.Title)
			fmt.Fprintf(&b, "**%s**%s%s\n\n", l.goal, l.colon, a.Goal)
			if !a.IsExpanded() {
				fmt.Fprintf(&b, l.pendingArcFmt+"\n\n", a.EstimatedChapters)
				continue
			}
			for _, e := range a.Chapters {
				fmt.Fprintf(&b, "#### "+l.chapterFmt+"%s%s\n\n", ch, l.colon, e.Title)
				fmt.Fprintf(&b, "**%s**%s%s\n\n", l.coreEvent, l.colon, e.CoreEvent)
				if e.Hook != "" {
					fmt.Fprintf(&b, "**%s**%s%s\n\n", l.hook, l.colon, e.Hook)
				}
				ch++
			}
		}
	}
	return b.String()
}

func renderOutline(entries []domain.OutlineEntry, l mdLabels) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", l.outline)
	for _, e := range entries {
		fmt.Fprintf(&b, "## "+l.chapterFmt+"%s%s\n\n", e.Chapter, l.colon, e.Title)
		fmt.Fprintf(&b, "**%s**%s%s\n\n", l.coreEvent, l.colon, e.CoreEvent)
		if e.Hook != "" {
			fmt.Fprintf(&b, "**%s**%s%s\n\n", l.hook, l.colon, e.Hook)
		}
		if len(e.Scenes) > 0 {
			fmt.Fprintf(&b, "**%s**%s\n", l.scenes, l.colon)
			for i, sc := range e.Scenes {
				fmt.Fprintf(&b, "%d. %s\n", i+1, sc)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ── Writer outline feedback pool ──
//
// The feedback (deviations/suggestions) from commit_chapter is persisted here, and the architect's next structural operation
// (expand_arc / append_volume / update_compass) consumes it via novel_context and then clears it.
// Fact closed loop: tool writes to disk -> context injection -> consumed by the structural operation (docs/engine-arbiter.md blocker 1).

// ChapterFeedback is one outline feedback item tagged with a chapter number.
type ChapterFeedback struct {
	Chapter          int      `json:"chapter"`
	StoryChanged     bool     `json:"story_changed,omitempty"`
	ChangeSummary    string   `json:"change_summary,omitempty"`
	Deviation        string   `json:"deviation,omitempty"`
	Suggestion       string   `json:"suggestion,omitempty"`
	DownstreamIssues []string `json:"downstream_issues,omitempty"`
	At               string   `json:"at"`
}

// RequiresImmediateReview distinguishes the impact of an external revision from ordinary writing feedback. Ordinary feedback is left to be absorbed together at the next
// natural structural operation; an external revision may invalidate the outline about to be continued and must go to the Architect first.
func (f ChapterFeedback) RequiresImmediateReview() bool {
	return f.StoryChanged || strings.TrimSpace(f.ChangeSummary) != "" || len(f.DownstreamIssues) > 0
}

const outlineFeedbackFile = "meta/outline_feedback.jsonl"
const outlineFeedbackResolutionFile = "meta/outline_feedback_resolution.json"

// AppendOutlineFeedback appends one writer feedback item. The same chapter and content count as the same fact,
// so a crash-replay of commit before ProgressMarked does not double-count the attached feedback.
func (s *OutlineStore) AppendOutlineFeedback(fb ChapterFeedback) error {
	return s.io.WithWriteLock(func() error {
		existing, err := s.io.ReadFileUnlocked(outlineFeedbackFile)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		currentFeedback, err := parseOutlineFeedback(existing)
		if err != nil {
			return err
		}
		for _, current := range currentFeedback {
			if current.Chapter == fb.Chapter && current.StoryChanged == fb.StoryChanged &&
				current.ChangeSummary == fb.ChangeSummary && current.Deviation == fb.Deviation &&
				current.Suggestion == fb.Suggestion && reflect.DeepEqual(current.DownstreamIssues, fb.DownstreamIssues) {
				return nil
			}
		}
		if fb.At == "" {
			fb.At = time.Now().Format(time.RFC3339)
		}
		data, err := json.Marshal(fb)
		if err != nil {
			return err
		}
		return s.io.AppendLineUnlocked(outlineFeedbackFile, append(data, '\n'))
	})
}

// LoadPendingOutlineFeedback reads the unconsumed feedback (old to new). A corrupt line is returned as an explicit error,
// to keep the Architect from continuing a structural operation on a context missing part of the feedback and then clearing the original file.
func (s *OutlineStore) LoadPendingOutlineFeedback() ([]ChapterFeedback, error) {
	s.io.mu.RLock()
	defer s.io.mu.RUnlock()
	data, err := os.ReadFile(s.io.path(outlineFeedbackFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseOutlineFeedback(data)
}

func (s *OutlineStore) SaveOutlineFeedbackResolution(reason string, count int) error {
	return s.io.WriteJSON(outlineFeedbackResolutionFile, struct {
		Reason   string `json:"reason"`
		Resolved int    `json:"resolved"`
		At       string `json:"at"`
	}{Reason: reason, Resolved: count, At: time.Now().Format(time.RFC3339)})
}

func parseOutlineFeedback(data []byte) ([]ChapterFeedback, error) {
	var out []ChapterFeedback
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fb ChapterFeedback
		if err := json.Unmarshal([]byte(line), &fb); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", outlineFeedbackFile, lineNo+1, err)
		}
		out = append(out, fb)
	}
	return out, nil
}

// ClearOutlineFeedback clears the feedback pool (architect structural operation succeeded = feedback has been taken into account).
func (s *OutlineStore) ClearOutlineFeedback() error {
	s.io.mu.Lock()
	defer s.io.mu.Unlock()
	data, err := os.ReadFile(s.io.path(outlineFeedbackFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := parseOutlineFeedback(data); err != nil {
		return err
	}
	err = os.Remove(s.io.path(outlineFeedbackFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
