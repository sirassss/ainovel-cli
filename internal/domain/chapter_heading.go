package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// 章号写法按语种三选一：越南语「Chương 12」、中文「第 12 章」、英文「Chapter 12」。
	// han:keep regex nhận dạng đầu chương zh/vi
	chapterNumberRe = regexp.MustCompile(`^\s*(?i:(chương|chapter)\s*(\d+)|第\s*(\d+)\s*章)`)

	// 模型给出的首行标题装饰：# 到 ###### 的标题行，或整行 **加粗**。
	headingDecorRe = regexp.MustCompile(`^\s*(?:#{1,6}\s*|\*\*)\s*|\s*\*\*\s*$`)
)

// FixChapterNumber 把标题里的章号改写成真实章号。
//
// 实测事故：规划期生成的大纲把章节命名为「Chương 19..23」，落盘时实际是第 1..5 章，
// 于是 ChapterRecord 里 chapter=2 而 title="Chương 20"——同一条记录自相矛盾，
// 读者看到的目录跳号。章号是引擎已知的事实，不该由模型的记忆决定。
//
// 标题不含可识别章号时原样返回：不猜、不硬塞。
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
			// 只换数字，保留原本的写法与大小写（Chương / 第…章 / Chapter）。
			head = head[:g[0]-m[0]] + strconv.Itoa(chapter) + head[g[1]-m[0]:]
		}
	}
	return head + title[m[1]:]
}

// ApplyChapterHeading 保证正文首行是规范的一级标题。
//
// 引擎在 ChapterFacts.Title 里已持有正确标题，却直接把模型输出的正文原样落盘：
// 实测 15 章里 9 章根本没有标题行，1 章写成 ## 二级，1 章写成 **加粗**。
// 标题是结构，不是创作——这里统一由引擎渲染，模型写不写都不影响成品。
//
// 只在首行确实是标题时才替换它（与 title 相同，或带章号前缀），否则一律前置，
// 避免把一句以加粗开头的正文误当标题吃掉。
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
	// 章号开头且够短：是标题行，不是正文段落。
	return chapterNumberRe.MatchString(bare) && len([]rune(bare)) <= 80
}
