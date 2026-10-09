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

// ErrOutlineChapterNotFound 表示章节尚未进入当前大纲。
var ErrOutlineChapterNotFound = errors.New("outline chapter not found")

// OutlineStore 管理故事前提、大纲（扁平/分层）和指南针。
type OutlineStore struct{ io *IO }

func NewOutlineStore(io *IO) *OutlineStore { return &OutlineStore{io: io} }

// SavePremise 保存故事前提到 premise.md。
func (s *OutlineStore) SavePremise(content string) error {
	return s.io.WriteMarkdown("premise.md", content)
}

// LoadPremise 读取 premise.md。不存在时返回空字符串。
func (s *OutlineStore) LoadPremise() (string, error) {
	data, err := s.io.ReadFile("premise.md")
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(data), err
}

// SaveOutline 同时保存 outline.json 和 outline.md（原子写入）。
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

// LoadOutline 从 outline.json 读取结构化大纲。
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

// GetChapterOutline 获取指定章节的大纲条目。
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

// SaveLayeredOutline 以分层大纲为唯一来源，保存分层视图并同步重建扁平派生视图。
// 调用方不需要、也不应再单独维护 outline.json/outline.md。
func (s *OutlineStore) SaveLayeredOutline(volumes []domain.VolumeOutline) error {
	return s.io.WithWriteLock(func() error {
		return s.saveLayeredViewsUnlocked(volumes)
	})
}

// LoadLayeredOutline 读取分层大纲。
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

// ClearLayeredOutline 清理分层大纲文件。
func (s *OutlineStore) ClearLayeredOutline() error {
	return s.io.WithWriteLock(func() error {
		if err := s.io.RemoveFileUnlocked("layered_outline.json"); err != nil {
			return err
		}
		return s.io.RemoveFileUnlocked("layered_outline.md")
	})
}

// GetChapterFromLayered 从分层大纲中按全局章节号查找。
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

// LocateChapter 根据全局章节号定位所在的卷和弧。
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

// ArcBoundary 弧边界信息。
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
	NeedsNewVolume bool // 卷末且当前 layered_outline 没有下一卷
}

// HasNextArc 是否还有后续弧。
func (b *ArcBoundary) HasNextArc() bool {
	return b.NextVolume > 0 || b.NextArc > 0
}

// CheckArcBoundary 检查某章是否为弧/卷的最后一章。
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

	// Next*/NeedsExpansion/NeedsNewVolume 只在弧末才有意义，否则会让协调者误以为要提前展开下一弧。
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

// CompletedArcBoundaries 按故事顺序返回已完成的详细弧边界。
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

// expandArcUnlocked 内部方法，在 Store.ExpandArc 跨域协调中调用。
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
					// 幂等重试仍须重写下方所有派生视图；上次可能只完成了
					// layered_outline.json，尚未写 flat outline/Markdown。
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

// appendVolumeUnlocked 内部方法，在 Store.AppendVolume 跨域协调中调用。
func (s *OutlineStore) appendVolumeUnlocked(vol domain.VolumeOutline) ([]domain.VolumeOutline, error) {
	var volumes []domain.VolumeOutline
	if err := s.io.ReadJSONUnlocked("layered_outline.json", &volumes); err != nil {
		return nil, fmt.Errorf("load layered_outline: %w", err)
	}
	// AppendVolume 的下一步还要更新 Progress。若进程在“大纲已追加、Progress
	// 未更新”之间中断，恢复会用同一持久化载荷重试；完全相同的末卷应视为幂等，
	// 让同参数重试继续补齐 Progress，而不是因重复 Index 永久卡死。
	if len(volumes) == 0 || !reflect.DeepEqual(volumes[len(volumes)-1], vol) {
		if err := validateAppendVolume(volumes, vol); err != nil {
			return nil, err
		}
		volumes = append(volumes, vol)
	}
	// 即使末卷已存在也重写全部派生视图；上次可能恰好在 layered JSON 落盘后、
	// flat outline/Markdown 写入前中断。
	if err := s.saveLayeredViewsUnlocked(volumes); err != nil {
		return nil, err
	}
	return volumes, nil
}

// saveLayeredViewsUnlocked 以分层大纲为唯一来源，统一重建其 Markdown 与扁平派生视图。
// 调用方必须持有 OutlineStore 的写锁。
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
		return nil, fmt.Errorf("from_chapter=%d vượt quá cuối đại cương %d: %w",
			fromChapter, len(outline), errs.ErrToolPrecondition)
	}
	updated := append([]domain.OutlineEntry(nil), outline[:fromChapter-1]...)
	updated = append(updated, replacement...)
	if len(updated) == 0 {
		return nil, fmt.Errorf("Đại cương sau khi sửa không được để trống: %w", errs.ErrToolPrecondition)
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

// reviseLayeredTail 替换 fromChapter 所在弧从该章起的尾段。若 fromChapter 正好
// 位于当前扁平大纲末尾之后，则追加到最后一个已展开弧。
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
		return fmt.Errorf("from_chapter=%d nằm ngoài phạm vi đại cương đã mở rộng", fromChapter)
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

// SaveCompass 保存终局方向指南针。
func (s *OutlineStore) SaveCompass(compass domain.StoryCompass) error {
	if compass.EndingDirection == "" {
		return fmt.Errorf("ending_direction không được để trống")
	}
	return s.io.WriteJSON("meta/compass.json", compass)
}

// LoadCompass 读取终局方向指南针。
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

// SaveFoundationAudit 保存 Architect 对当前基础设定版本的语义审查。
func (s *OutlineStore) SaveFoundationAudit(a domain.FoundationAudit) error {
	return s.io.WriteJSON("meta/foundation_audit.json", a)
}

// LoadFoundationAudit 读取最近一次基础设定语义审查。
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

// ── Writer 大纲反馈池 ──
//
// commit_chapter 的 feedback(偏离/建议)持久化于此,architect 下次结构操作
// (expand_arc / append_volume / update_compass)经 novel_context 消费后清空。
// 事实闭环:工具落盘 → 上下文注入 → 结构操作即消费(docs/engine-arbiter.md 阻断1)。

// ChapterFeedback 一条带章节号的大纲反馈。
type ChapterFeedback struct {
	Chapter          int      `json:"chapter"`
	StoryChanged     bool     `json:"story_changed,omitempty"`
	ChangeSummary    string   `json:"change_summary,omitempty"`
	Deviation        string   `json:"deviation,omitempty"`
	Suggestion       string   `json:"suggestion,omitempty"`
	DownstreamIssues []string `json:"downstream_issues,omitempty"`
	At               string   `json:"at"`
}

// RequiresImmediateReview 区分外部修订影响与普通写作反馈。普通反馈留到下一次
// 自然结构操作统一吸收；外部修订可能使即将续写的大纲失效，必须先交 Architect。
func (f ChapterFeedback) RequiresImmediateReview() bool {
	return f.StoryChanged || strings.TrimSpace(f.ChangeSummary) != "" || len(f.DownstreamIssues) > 0
}

const outlineFeedbackFile = "meta/outline_feedback.jsonl"
const outlineFeedbackResolutionFile = "meta/outline_feedback_resolution.json"

// AppendOutlineFeedback 追加一条 writer 反馈。相同章节与内容视为同一事实，
// 使 commit 在 ProgressMarked 前崩溃重放时不会重复累加附属反馈。
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

// LoadPendingOutlineFeedback 读取未消费的反馈(旧→新)。损坏行显式返回错误，
// 防止 Architect 在缺失部分反馈的上下文上继续结构操作并随后清空原文件。
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

// ClearOutlineFeedback 清空反馈池(architect 结构操作成功 = 反馈已被参考)。
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
