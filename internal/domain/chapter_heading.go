package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// Chapter numbers are written in one of three forms depending on language: Vietnamese "Chương 12", Chinese 「第 12 章」, English "Chapter 12" (han:keep).
	// Regex that recognizes zh/vi/en chapter headings. han:keep
	chapterNumberRe = regexp.MustCompile(`^\s*(?i:(chương|chapter)\s*(\d+)|第\s*(\d+)\s*章)`)

	// Heading decoration the model puts on the first line: a heading line from # to ######, or a whole line in **bold**.
	headingDecorRe = regexp.MustCompile(`^\s*(?:#{1,6}\s*|\*\*)\s*|\s*\*\*\s*$`)
)

// FixChapterNumber rewrites the chapter number in a title to the real chapter number.
//
// Observed incident: the outline generated during planning named chapters "Chương 19..23", but when persisted they were actually chapters 1..5 (han:keep),
// so a ChapterRecord had chapter=2 with title="Chương 20" - a self-contradicting record (han:keep),
// and readers saw gaps in the table of contents. The chapter number is a fact the engine knows; it must not be decided by the model's memory.
//
// A title without a recognizable chapter number is returned unchanged: no guessing, no forcing one in.
func FixChapterNumber(title string, chapter int) string {
	m := chapterNumberRe.FindStringSubmatchIndex(title)
	if m == nil || chapter <= 0 {
		return title
	}
	head := title[m[0]:m[1]]
	for _, g := range [][2]int{{m[4], m[5]}, {m[6], m[7]}} {
		if g[0] < 0 {
			continue
		}
		if n, err := strconv.Atoi(title[g[0]:g[1]]); err == nil && n != chapter {
			// Replace only the digits, preserving the original form and letter case (Chương / 第…章 / Chapter) (han:keep).
			head = head[:g[0]-m[0]] + strconv.Itoa(chapter) + head[g[1]-m[0]:]
		}
	}
	return head + title[m[1]:]
}

// ApplyChapterHeading guarantees that the first line of the chapter content is a proper level-1 heading.
//
// The engine already holds the correct title in ChapterFacts.Title, yet persisted the model's output as-is:
// in a measured run, 9 of 15 chapters had no heading line at all, 1 was written as a ## level-2 heading, and 1 as **bold**.
// A heading is structure, not creative work - the engine now renders it uniformly, so the result no longer depends on whether the model writes one.
//
// The first line is replaced only when it really is a heading (identical to title, or carrying a chapter-number prefix); otherwise the heading is always prepended,
// so that a sentence of content that merely starts in bold is not mistaken for a heading and swallowed.
func ApplyChapterHeading(content, title string, chapter int) string {
	title = strings.TrimSpace(FixChapterNumber(strings.TrimSpace(title), chapter))
	if title == "" {
		return content
	}

	lines := strings.Split(content, "\n")
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first < len(lines) && isChapterHeadingLine(lines[first], title) {
		lines = lines[first+1:]
	} else {
		lines = lines[first:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return fmt.Sprintf("# %s\n\n%s", title, strings.Join(lines, "\n"))
}

func isChapterHeadingLine(line, title string) bool {
	bare := strings.TrimSpace(headingDecorRe.ReplaceAllString(strings.TrimSpace(line), ""))
	if bare == "" {
		return false
	}
	if strings.EqualFold(bare, title) {
		return true
	}
	// Starts with a chapter number and is short enough: it is a heading line, not a content paragraph.
	return chapterNumberRe.MatchString(bare) && len([]rune(bare)) <= 80
}
