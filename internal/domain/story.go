package domain

import (
	"fmt"
	"strings"
)

// BookMetadata is the work information aimed at readers and publications.
// Creative settings belong to Foundation and run progress belongs to Progress; neither carries this data.
type BookMetadata struct {
	Title    string `json:"title"`
	Synopsis string `json:"synopsis"`
}

// Normalized returns the canonical value that is persistable and comparable.
func (b BookMetadata) Normalized() BookMetadata {
	b.Title = strings.TrimSpace(b.Title)
	b.Synopsis = strings.TrimSpace(b.Synopsis)
	return b
}

// Validate checks the required fields of the work information.
func (b BookMetadata) Validate() error {
	b = b.Normalized()
	if b.Title == "" {
		return fmt.Errorf("book title is required")
	}
	if b.Synopsis == "" {
		return fmt.Errorf("book synopsis is required")
	}
	return nil
}

// OutlineEntry is an outline entry, corresponding to one chapter.
type OutlineEntry struct {
	Chapter   int      `json:"chapter"`
	Title     string   `json:"title"`
	CoreEvent string   `json:"core_event"`
	Hook      string   `json:"hook"`
	Scenes    []string `json:"scenes"`
}

// Character is a character profile.
type Character struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"` // Aliases / titles / nicknames (e.g. "the Useless Boy", "Brother Yan")
	Role        string   `json:"role"`
	Description string   `json:"description"`
	Arc         string   `json:"arc"`
	Traits      []string `json:"traits"`
	Tier        string   `json:"tier,omitempty"` // core / important / secondary / decorative (default important)
}

// VolumeOutline is a volume-level outline (long-form layered mode).
type VolumeOutline struct {
	Index int          `json:"index"`
	Title string       `json:"title"`
	Theme string       `json:"theme"`           // Core conflict / theme of this volume
	Final bool         `json:"final,omitempty"` // Finale volume: the whole book wraps up in this volume (declared by the Architect at append_volume)
	Arcs  []ArcOutline `json:"arcs"`
}

// IsExpanded reports whether the volume has been expanded (has arc-level structure).
func (v *VolumeOutline) IsExpanded() bool { return len(v.Arcs) > 0 }

// FinaleVolume returns the number of the declared finale volume, or 0 if none has been declared.
// The finale fact = "the last volume carries the Final marker": once declared, the whole book enters the wrap-up state (planning closes threads, and the final volume's structure
// completes the book as soon as it is written); if an unmarked new volume is appended afterwards, the new volume becomes the last volume and the wrap-up state lifts naturally -
// so no revoke tool is needed, and the state can always be derived from the outline data.
func FinaleVolume(volumes []VolumeOutline) int {
	if n := len(volumes); n > 0 && volumes[n-1].Final {
		return volumes[n-1].Index
	}
	return 0
}

// StoryCompass is the ending-direction compass, replacing the fixed skeleton volume list.
// The Architect may update it at each volume boundary, allowing the story's direction to evolve as writing proceeds.
type StoryCompass struct {
	EndingDirection string   `json:"ending_direction"`          // Ending direction (a thematic description)
	OpenThreads     []string `json:"open_threads,omitempty"`    // Active long-running threads (must be wrapped up before the ending)
	EstimatedScale  string   `json:"estimated_scale,omitempty"` // Fuzzy scale (e.g. "expected 4-6 volumes")
	LastUpdated     int      `json:"last_updated,omitempty"`    // Number of completed chapters at the time of the update
}

// ArcOutline is an arc-level outline.
type ArcOutline struct {
	Index             int            `json:"index"` // Arc index within the volume
	Title             string         `json:"title"`
	Goal              string         `json:"goal"`                         // Arc goal (setup, development, turn, resolution)
	EstimatedChapters int            `json:"estimated_chapters,omitempty"` // Estimated chapter count of a skeleton arc (cleared once expanded)
	Chapters          []OutlineEntry `json:"chapters"`
}

// IsExpanded reports whether the arc has been expanded (has detailed chapters).
func (a *ArcOutline) IsExpanded() bool { return len(a.Chapters) > 0 }

// ArcExpansion is the complete plan the Architect makes at a structure boundary for an arc not yet written.
// Title/Goal are not mechanical copies of the skeleton: the model may revise plans that have not happened yet based on the content already written.
type ArcExpansion struct {
	Title    string         `json:"title"`
	Goal     string         `json:"goal"`
	Chapters []OutlineEntry `json:"chapters"`
}

// EstimatedChapterCapacity computes the internal capacity estimate of a layered outline: expanded arcs count by their real chapter count,
// and skeleton arcs by EstimatedChapters. It is used only for context strategy and is not the book's total chapter count; the chapters that are truly detailed
// and writable always come from FlattenOutline, and this value must not be exposed to users or models.
func EstimatedChapterCapacity(volumes []VolumeOutline) int {
	n := 0
	for _, v := range volumes {
		for _, a := range v.Arcs {
			if a.IsExpanded() {
				n += len(a.Chapters)
			} else {
				n += a.EstimatedChapters
			}
		}
	}
	return n
}

// FlattenOutline expands the layered outline into a flat chapter list, keeping global chapter numbers contiguous.
func FlattenOutline(volumes []VolumeOutline) []OutlineEntry {
	var result []OutlineEntry
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for _, e := range a.Chapters {
				e.Chapter = ch
				result = append(result, e)
				ch++
			}
		}
	}
	return result
}

// WorldRule is a world-building rule entry.
type WorldRule struct {
	Category string `json:"category"` // magic / technology / geography / society / other
	Rule     string `json:"rule"`     // Rule description
	Boundary string `json:"boundary"` // Boundary that must not be violated
}

// RenumberVolumes renumbers volumes and arcs by position, starting from 1.
//
// Planning models are unreliable when writing index: they often count from 0, or even write 0 for every arc within the same volume.
// ExpandArc / ArcScope look arcs up by index value, so duplicate or 0 values make an arc permanently unaddressable -
// the symptom only surfaces several steps later when expand_arc reports an "invalid parameters" error, and the error points in completely the wrong direction.
//
// The array order is the fact and index is merely its name, so indexes are rewritten uniformly by position before being persisted.
func RenumberVolumes(volumes []VolumeOutline) {
	for vi := range volumes {
		volumes[vi].Index = vi + 1
		for ai := range volumes[vi].Arcs {
			volumes[vi].Arcs[ai].Index = ai + 1
		}
	}
}

// Once the same hook / core event repeats this many times it is judged a stalled outline. Twice may be a deliberate two-part structure,
// but from three times on there is no legitimate way to write it: the reader is left hanging on the same suspense for three chapters and nobody pays it off.
const (
	maxHookRepeat      = 3
	maxCoreEventRepeat = 2
)

// StalledOutline detects an outline that is "spinning in place": the chapter titles differ, but the hook or core event is one sentence
// copied several times. The Writer then executes faithfully - each chapter restates the previous one and adds a little, reading like a rewrite rather than a continuation.
//
// Neither the Writer nor the Editor can notice this kind of defect: both look at a single chapter, and a single chapter is self-consistent on its own.
// It can only be caught by comparing the whole book at the point where the outline is persisted.
//
// An empty string means it passed; otherwise it is a diagnostic that can be returned directly to the planner.
func StalledOutline(entries []OutlineEntry) string {
	hooks := map[string]int{}
	events := map[string]int{}
	for _, e := range entries {
		if h := strings.TrimSpace(e.Hook); h != "" {
			hooks[h]++
		}
		if c := strings.TrimSpace(e.CoreEvent); c != "" {
			events[c]++
		}
	}
	if worst, n := mostRepeated(hooks); n >= maxHookRepeat {
		return fmt.Sprintf("Dàn ý quay vòng: cùng một hook lặp ở %d/%d chương — %q. "+
			"hook của mỗi chương phải là hệ quả mới phát sinh trong chương đó và được chương kế tiếp đáp lại; hãy viết lại từng chương", n, len(entries), truncateRunes(worst, 40))
	}
	if worst, n := mostRepeated(events); n >= maxCoreEventRepeat {
		return fmt.Sprintf("Dàn ý quay vòng: cùng một core_event lặp ở %d chương — %q. "+
			"Mỗi chương phải xảy ra việc khác nhau và làm thay đổi tình thế; hãy viết lại từng chương", n, truncateRunes(worst, 40))
	}
	return ""
}

func mostRepeated(m map[string]int) (string, int) {
	var key string
	best := 0
	for k, n := range m {
		if n > best {
			key, best = k, n
		}
	}
	return key, best
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// SkeletonArcs counts the skeleton arcs not yet expanded, for the pre-completion check.
//
// The completion check compares only the flat outline, and the flat outline is derived by FlattenOutline from the "expanded arcs" -
// skeleton arcs contribute 0 chapters and are completely invisible to that check. Observed incident: two skeleton arcs of Volume 1 totaling 38 chapters were never expanded,
// the Architect jumped straight to Volume 2 and declared the book complete after writing 15 chapters, and the check let it through because next(16) > len(flat)(15).
//
// A legitimate exit remains for wrapping up early: append_volume with "final": true declares the finale volume.
func SkeletonArcs(volumes []VolumeOutline) []string {
	var out []string
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			if a := &volumes[vi].Arcs[ai]; !a.IsExpanded() {
				out = append(out, fmt.Sprintf("Tập %d cung %d \"%s\"", volumes[vi].Index, a.Index, a.Title))
			}
		}
	}
	return out
}

// maxArcChapters is the upper limit on the number of detailed chapters in a single arc.
//
// The constraint comes from the end-of-arc review, not from narrative taste: at an arc boundary the Editor must read the whole arc to give review comments.
// Measured: one arc of 20 chapters = 113792 characters ≈ 37k tokens of content, which with the outline / snapshots / prompts on top fits no
// usable model - it does not fit a local 32k window, and the free cloud tier at 8 tok/s broke the stream 14 times in a row,
// finally freezing the whole pipeline at the arc boundary. 8 chapters ≈ 45k characters ≈ 15k tokens leaves headroom on both sides.
//
// This is also structurally a good thing: cramming 20 chapters into one arc already shows that the arc's goal has not converged.
const maxArcChapters = 8

// OversizedArc checks whether an arc's size exceeds the limit; if it does, it returns a diagnostic that can be returned directly to the planner.
// An empty string means it passed.
//
// chapters takes the larger of the "detailed chapter count" and the "skeleton estimated chapter count": an arc that wrote estimated=20 at the skeleton stage
// will inevitably hit the same wall at expand_arc, and by then twenty chapters have already gone by - structural problems should be reported when the structure is persisted.
func OversizedArc(label string, chapters int) string {
	if chapters <= maxArcChapters {
		return ""
	}
	return fmt.Sprintf("%s có %d chương chi tiết, vượt giới hạn %d chương mỗi cung. "+
		"Đánh giá cuối cung phải đọc trọn cả cung, cung quá dài thì model nào cũng không đánh giá nổi (thực tế 20 chương đã làm kẹt cả pipeline). "+
		"Hãy tách thành nhiều cung, mỗi cung có mục tiêu độc lập: lần gọi này chỉ mở rộng cung đầu tiên trong phạm vi %d chương, "+
		"các cung còn lại để dạng phác thảo, tới biên thì mở rộng tiếp",
		label, chapters, maxArcChapters, maxArcChapters)
}
