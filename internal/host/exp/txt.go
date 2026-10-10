package exp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// chapterTitleIndex looks up a title by chapter number; it returns an empty string if missing.
type chapterTitleIndex map[int]string

func buildTitleIndex(outline []domain.OutlineEntry) chapterTitleIndex {
	idx := make(chapterTitleIndex, len(outline))
	for _, e := range outline {
		if e.Title != "" {
			idx[e.Chapter] = e.Title
		}
	}
	return idx
}

// chapterLocation is a chapter's placement in the layered outline. It keeps only the volume info the export layout needs:
// arcs do not enter the export (from the reader's view an arc is an overly fine internal structure).
type chapterLocation struct {
	VolumeIdx       int
	VolumeTitle     string
	IsFirstOfVolume bool
}

// buildLocations builds {chapter -> location} following the global chapter order of the layered outline.
// Chapter numbers are rebuilt by the same rule as FlattenOutline (accumulated in order within arcs within volumes),
// to stay consistent with the chapter numbers in Progress.CompletedChapters. The arc level still has to be traversed (it is required to compute global chapter numbers),
// but it does not go into location: the export inserts dividers only at the start of a volume.
func buildLocations(volumes []domain.VolumeOutline) map[int]chapterLocation {
	if len(volumes) == 0 {
		return nil
	}
	locs := make(map[int]chapterLocation)
	ch := 0
	for _, v := range volumes {
		firstOfVol := true
		for _, a := range v.Arcs {
			for range a.Chapters {
				ch++
				locs[ch] = chapterLocation{
					VolumeIdx:       v.Index,
					VolumeTitle:     v.Title,
					IsFirstOfVolume: firstOfVol,
				}
				firstOfVol = false
			}
		}
	}
	return locs
}

// chapterHeaderRe matches the first line of a Markdown heading that carries a chapter number (# 第N章 / ## 第 12 章 ...). // han:keep
var chapterHeaderRe = regexp.MustCompile(`^#+\s+(?:第.+?章|(?i:chương)\s*\d+)`) // han:keep chapter headings in zh/vi chapter content

// atxTitleRe extracts the text part of an ATX heading (# title).
var atxTitleRe = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*$`)

// stripChapterTitleHeader strips the first line if it is a chapter heading that would duplicate the exporter's unified heading.
// Two cases: (1) "# 第N章 …" (with a chapter number); (2) a markdown heading whose text is exactly this chapter's title // han:keep
// (the writer often writes the bare chapter name as a heading on the first body line, such as "# 边村浮生", duplicating the exporter-generated // han:keep
// "第 N 章 边村浮生"). Other h1s (such as "# 序章") are treated as part of the body and kept. // han:keep examples are zh headings
// The caller is responsible for TrimSpace first, so leading blank lines are out of consideration.
func stripChapterTitleHeader(content, title string) string {
	first, rest, hasNewline := strings.Cut(content, "\n")
	if !isChapterTitleLine(first, title) {
		return content
	}
	if !hasNewline {
		return ""
	}
	return strings.TrimLeft(rest, "\n")
}

func isChapterTitleLine(line, title string) bool {
	if chapterHeaderRe.MatchString(line) {
		return true
	}
	if title = strings.TrimSpace(title); title == "" {
		return false
	}
	m := atxTitleRe.FindStringSubmatch(line)
	return len(m) == 2 && strings.TrimSpace(m[1]) == title
}

// renderTXT assembles the final text.
//
// Chapter order is determined by chapters (the caller has already deduplicated in ascending chapter number). bodies/titleIdx/locations
// all follow "degrade when missing": a missing title outputs only "第 N 章"; a missing layered location is treated as a flat outline. // han:keep
func renderTXT(
	lb labels,
	novelName string,
	chapters []int,
	titleIdx chapterTitleIndex,
	locations map[int]chapterLocation,
	bodies map[int]string,
) string {
	var b strings.Builder

	if name := strings.TrimSpace(novelName); name != "" {
		fmt.Fprintf(&b, lb.bookTitleFmt+"\n\n", name)
	}

	useLayered := len(locations) > 0

	for i, ch := range chapters {
		if useLayered {
			if loc, ok := locations[ch]; ok && loc.IsFirstOfVolume {
				b.WriteString("\n═══════════════════════════════════════════\n")
				fmt.Fprintf(&b, "           "+lb.txtVolumeFmt+"\n", loc.VolumeIdx, strings.TrimSpace(loc.VolumeTitle))
				b.WriteString("═══════════════════════════════════════════\n\n")
			}
		}

		title := strings.TrimSpace(titleIdx[ch])
		if title != "" {
			fmt.Fprintf(&b, lb.txtChapterFmt+"\n\n", ch, title)
		} else {
			fmt.Fprintf(&b, lb.chapterFmt+"\n\n", ch)
		}

		body := stripChapterTitleHeader(strings.TrimSpace(bodies[ch]), title)
		b.WriteString(body)
		b.WriteString("\n")
		if i < len(chapters)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}
