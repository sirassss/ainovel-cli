package host

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/store"
)

// buildStoryStateSummary assembles a concise summary of the story so far, so the stage co-creation assistant knows "what has already been written".
// It reuses the store accessors and takes only the high-level facts needed to plan direction (progress / compass / latest volume / main characters / active foreshadowing);
// it does not pull the body text or feed the full novel_context JSON: co-creation is a conversation and wants a readable overview, not writing context.
// Any missing item is skipped (best-effort); an empty string means no usable progress yet.
func buildStoryStateSummary(s *store.Store) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	var warnings []string
	warn := func(scope string, err error) {
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Đọc %s thất bại: %v", scope, err))
		}
	}

	if book, err := s.Book.Load(); book != nil {
		fmt.Fprintf(&b, "- Tên sách: %s\n", book.Title)
	} else {
		warn("book", err)
	}

	if progress, err := s.Progress.Load(); progress != nil {
		fmt.Fprintf(&b, "- Tiến độ: đã hoàn thành %d chương", len(progress.CompletedChapters))
		if progress.Layered {
			outline, outlineErr := s.Outline.LoadOutline()
			if outlineErr != nil {
				warn("outline", outlineErr)
			} else if len(outline) > 0 {
				fmt.Fprintf(&b, " / hiện đã chi tiết hóa %d chương (phần sau lập kế hoạch động theo cung)", len(outline))
			}
		} else if progress.TotalChapters > 0 {
			fmt.Fprintf(&b, " / kế hoạch %d chương", progress.TotalChapters)
		}
		fmt.Fprintf(&b, ", khoảng %d chữ, chương kế là chương %d\n", progress.TotalWordCount, progress.NextChapter())
		if progress.Layered && progress.CurrentVolume > 0 {
			fmt.Fprintf(&b, "- Vị trí hiện tại: tập %d cung %d\n", progress.CurrentVolume, progress.CurrentArc)
		}
	} else {
		warn("progress", err)
	}

	if compass, err := s.Outline.LoadCompass(); compass != nil {
		if dir := strings.TrimSpace(compass.EndingDirection); dir != "" {
			fmt.Fprintf(&b, "- Hướng kết cục: %s\n", dir)
		}
		if compass.EstimatedScale != "" {
			fmt.Fprintf(&b, "- Quy mô ước tính: %s\n", compass.EstimatedScale)
		}
		if len(compass.OpenThreads) > 0 {
			fmt.Fprintf(&b, "- Tuyến dài đang hoạt động: %s\n", strings.Join(compass.OpenThreads, "; "))
		}
	} else {
		warn("story_compass", err)
	}

	// Summary of the latest volume, so the assistant knows where the story has just got to
	if vols, err := s.Summaries.LoadAllVolumeSummaries(); len(vols) > 0 {
		last := vols[len(vols)-1]
		fmt.Fprintf(&b, "- Tập gần nhất \"%s\": %s\n", last.Title, truncate(last.Summary, 200))
	} else {
		warn("volume_summaries", err)
	}

	// Main characters (core/important), at most 8
	if chars, err := s.Characters.Load(); len(chars) > 0 {
		var names []string
		for _, c := range chars {
			if c.Tier == "secondary" || c.Tier == "decorative" {
				continue
			}
			line := c.Name
			if role := strings.TrimSpace(c.Role); role != "" {
				line += " (" + role + ")"
			}
			names = append(names, line)
			if len(names) >= 8 {
				break
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(&b, "- Nhân vật chính: %s\n", strings.Join(names, ", "))
		}
	} else {
		warn("characters", err)
	}

	// Unresolved foreshadowing, at most 6 entries
	if fs, err := s.World.LoadActiveForeshadow(); len(fs) > 0 {
		var items []string
		for _, f := range fs {
			items = append(items, truncate(f.Description, 40))
			if len(items) >= 6 {
				break
			}
		}
		fmt.Fprintf(&b, "- Chi tiết gài trước chưa thu: %s\n", strings.Join(items, "; "))
	} else {
		warn("foreshadow", err)
	}

	if len(warnings) > 0 {
		fmt.Fprintf(&b, "- Cảnh báo dữ liệu: %s\n", strings.Join(warnings, "; "))
	}

	return strings.TrimSpace(b.String())
}

// stageSystemPrompt assembles the full system prompt for stage co-creation: stage prompt + current story state summary.
// The summary hangs at the end as a data appendix (separated from the format rules by a divider line), echoing the "progress is below" guidance in the prompt.
func stageSystemPrompt(s *store.Store) string {
	if s.Language() == "zh" {
		prompt := stageCoCreateSystemPromptZH
		if summary := buildStoryStateSummary(s); summary != "" {
			prompt += "\n\n---\n## 当前故事状态\n（以下是已写内容的客观摘要，供你规划后续时参照，不要在 <draft> 里照抄原文）\n" + summary // han:keep
		}
		return prompt
	}
	prompt := stageCoCreateSystemPrompt
	if summary := buildStoryStateSummary(s); summary != "" {
		prompt += "\n\n---\n## Trạng thái truyện hiện tại\n(Dưới đây là tóm tắt khách quan về nội dung đã viết, để bạn tham chiếu khi lập kế hoạch phần sau, đừng chép nguyên văn vào <draft>)\n" + summary
	}
	return prompt
}
