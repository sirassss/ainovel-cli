package rules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Lint 内置产品底线检查：扫描正文中的机制残留，与用户规则无关，commit 时始终执行。
// 与 Check 同契约——仅返事实（铁律一），不阻断流程，由评审/用户裁定。
//
// 当前三类（全部来自真实长跑产物的实证缺陷）：
//   - markdown_residue：正文残留 ** 加粗、首行之外的 # 标题行（导出 txt 会裸露符号）
//   - non_cjk_fragments / cjk_leak：文字混杂片段，方向按正文主文字自动判定
//     （中文正文报拉丁片段；越南语等拉丁文字正文报汉字片段）
func Lint(text string) []Violation {
	var vs []Violation
	vs = appendMarkdownResidue(vs, text)
	vs = appendScriptMixing(vs, text)
	vs = appendBrokenWords(vs, text)
	vs = appendSelfDuplication(vs, text)
	vs = appendEnglishResidue(vs, text)
	return vs
}

func appendMarkdownResidue(vs []Violation, text string) []Violation {
	if n := strings.Count(text, "**"); n > 0 {
		vs = append(vs, Violation{
			Rule:     "markdown_residue",
			Target:   "**",
			Actual:   n,
			Severity: SeverityWarning,
		})
	}
	headings := 0
	seenContent := false
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// 第一个非空行的 # 标题是章文件的合法格式（不按行号写死，容忍前导空行）
		first := !seenContent
		seenContent = true
		if !first && strings.HasPrefix(t, "#") {
			headings++
		}
	}
	if headings > 0 {
		vs = append(vs, Violation{
			Rule:     "markdown_residue",
			Target:   "#",
			Actual:   headings,
			Severity: SeverityWarning,
		})
	}
	return vs
}

var (
	latinFragmentRe = regexp.MustCompile(`[A-Za-z]{2,}`)
	// 含 CJK 标点：漏进越南语正文的不只是汉字，还有「。，、？！」这类全角标点。
	// 实测第 4 章正文里一个孤立的「。」——纯汉字规则完全看不见。
	cjkFragmentRe = regexp.MustCompile(`[\p{Han}\x{3000}-\x{303f}\x{ff01}-\x{ff5e}]+`)
)

// appendScriptMixing 报告正文里「另一种文字」的混入片段。
//
// 哪种文字算混入由正文自身决定，不读配置：中文正文里裸混 "pattern" 是缺陷，
// 而越南语正文每个词都是拉丁字母，按同一条规则会在每章误报上千次，把评审
// 上下文淹掉——实测一章命中 1021 次而全章并无一个汉字。反过来，越南语正文
// 里漏出「根系之力」才是真缺陷，旧规则完全看不见。
//
// 因此先按字符占比判定正文主文字，再只报少数派。两种题材各自成立，且配置
// 写错也不会失效。合法外来词（品牌名/缩写）仍会命中——warning 级事实，由评审裁定。
func appendScriptMixing(vs []Violation, text string) []Violation {
	latin := latinFragmentRe.FindAllString(text, -1)
	han := cjkFragmentRe.FindAllString(text, -1)

	// 比的是「汉字个数」与「拉丁词个数」，不是两边的字符数：一个汉字约等于一个词，
	// 而一个拉丁词有好几个字母。按字符数比，中文正文里混几个 "pattern"/"DNA"
	// 就会把拉丁字符数顶过汉字数，从而误判正文语种。
	rule, matches := "non_cjk_fragments", latin
	if runeCount(han) <= len(latin) {
		// 正文是拉丁文字（越南语等）：汉字才是混入。
		rule, matches = "cjk_leak", han
	}
	if len(matches) == 0 {
		return vs
	}

	seen := make(map[string]struct{})
	var examples []string
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		if len(examples) < 3 {
			examples = append(examples, m)
		}
	}
	return append(vs, Violation{
		Rule:     rule,
		Target:   strings.Join(examples, ", "),
		Actual:   len(matches),
		Severity: SeverityWarning,
	})
}

func runeCount(ss []string) int {
	n := 0
	for _, s := range ss {
		n += utf8.RuneCountInString(s)
	}
	return n
}

// brokenWordRe 匹配被段落分隔切断的单词：一行以字母结尾，跨过空行后又以小写字母开头。
// 越南语正文实测：「n Tông, Ng」+ 空行 +「ọc Lâm dừng bước」——人名 Ngọc 被劈成两半。
// 拉丁文字里一个词不会跨段落，因此这个形状没有正当写法，可直接判为缺陷。
var brokenWordRe = regexp.MustCompile(`(?m)[\p{L}]\n\s*\n[[:space:]]*[\p{Ll}]`)

func appendBrokenWords(vs []Violation, text string) []Violation {
	matches := brokenWordRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return vs
	}
	return append(vs, Violation{
		Rule:     "broken_word",
		Target:   strings.Join(strings.Fields(matches[0]), "⏎"),
		Actual:   len(matches),
		Severity: SeverityWarning,
	})
}

// paraMinRunes 是参与重复比对的段落下限。短段落天然会重复（"Hắn gật đầu."、
// 一声"Bắt đầu!"），只有成段的文字逐字重现才是生成事故。
const paraMinRunes = 20

// paragraphs 切出够长、值得比对的正文段落，跳过标题行。
func paragraphs(text string) []string {
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "#") {
			continue
		}
		if utf8.RuneCountInString(p) >= paraMinRunes {
			out = append(out, p)
		}
	}
	return out
}

// selfDupThreshold / crossDupThreshold 是重复字符占本章的比例上限。
//
// 阈值来自实测：一本 22 章的书里 19 章两项都是 0.0%，出事的三章分别是
// 自重复 43.5%（第21章，同一段落出现 5 次）、跨章 32.2%（第14章照抄第13章）
// 与 12.9%（第22章照抄第21章）。信号是二元的，中间没有灰区，因此取 10%
// 给"刻意重复的副歌/咒诀"留足余地。
const (
	selfDupThreshold  = 0.10
	crossDupThreshold = 0.10
)

func appendSelfDuplication(vs []Violation, text string) []Violation {
	paras := paragraphs(text)
	if len(paras) == 0 {
		return vs
	}
	count := make(map[string]int, len(paras))
	total := 0
	for _, p := range paras {
		count[p]++
		total += utf8.RuneCountInString(p)
	}
	dup, worst, worstN := 0, "", 0
	for p, n := range count {
		if n > 1 {
			dup += utf8.RuneCountInString(p) * (n - 1)
			if n > worstN {
				worst, worstN = p, n
			}
		}
	}
	if total == 0 || float64(dup)/float64(total) < selfDupThreshold {
		return vs
	}
	return append(vs, Violation{
		Rule:     "self_duplication",
		Target:   fmt.Sprintf("×%d %s", worstN, truncateRunes(worst, 50)),
		Limit:    fmt.Sprintf("<%.0f%%", selfDupThreshold*100),
		Actual:   fmt.Sprintf("%.0f%%", 100*float64(dup)/float64(total)),
		Severity: SeverityError,
	})
}

// CheckAgainstPrevious 检出"新章其实是上一章的副本"。
//
// Writer 有 read_chapter，会先看上一章写了什么，然后把它抄下来当续写——实测
// 第14章 32.2% 的字符逐字来自第13章，第22章 12.9% 来自第21章。
//
// self_duplication 看不见这类事故：它只在单章范围内统计，而照抄的那一章内部
// 完全干净。因此必须单独按"上文"比对。previous 为空则跳过。
func CheckAgainstPrevious(text string, previous []string) []Violation {
	cur := paragraphs(text)
	if len(cur) == 0 || len(previous) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, prev := range previous {
		for _, p := range paragraphs(prev) {
			seen[p] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}

	dup, total, sample := 0, 0, ""
	for _, p := range unique(cur) {
		n := utf8.RuneCountInString(p)
		total += n
		if _, ok := seen[p]; ok {
			dup += n
			if sample == "" {
				sample = p
			}
		}
	}
	if total == 0 || float64(dup)/float64(total) < crossDupThreshold {
		return nil
	}
	return []Violation{{
		Rule:     "copied_previous_chapter",
		Target:   truncateRunes(sample, 60),
		Limit:    fmt.Sprintf("<%.0f%%", crossDupThreshold*100),
		Actual:   fmt.Sprintf("%.0f%%", 100*float64(dup)/float64(total)),
		Severity: SeverityError,
	}}
}

func unique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, x := range in {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// englishFunctionWordRe 只收英语虚词——它们绝不会作为借词进入越南语正文。
// 内容词（flow / lesson / footsteps 之类）故意不收：现代题材里可能是合理外来语，
// 而"the flow"这类短语已被其中的 the 命中，不必冒误报的风险。
var englishFunctionWordRe = regexp.MustCompile(
	`(?i)\b(not|the|and|but|for|from|with|they|this|that|just|one|was|were|have|when|which|while|into|over)\b`)

// vietnameseMarkRe 匹配越南语专有字母：7 个基字母（ăâđêôơư，含大写）加上
// Latin Extended Additional 区 U+1EA0-U+1EF9——该区几乎专供越南语声调字母。
// 只列少数预组合字符是不够的：一句普通越南语里大半声调字母都落在那个区间内。
var vietnameseMarkRe = regexp.MustCompile(`[ăâđêôơưĂÂĐÊÔƠƯ\x{1ea0}-\x{1ef9}]`)

// vietnameseMarkFloor 是判定"正文为越南语"所需的专有字母数。真实一章有成百上千个；
// 设下限只为在英文/中文作品里让本规则彻底静音。
const vietnameseMarkFloor = 12

// appendEnglishResidue 报告越南语正文里夹带的英语虚词。
//
// cjk_leak 对此完全无能：越南语与英语同属拉丁字母，混进来的 "not"/"the" 与正文
// 在字符层面无从区分。而这不是小事——实测第18章出现 27 次 not、9 次 the、3 次 from，
// 例如「Lá cây bắt đầu chuyển động—not nhanh chóng mà nhẹ nhàng」。
func appendEnglishResidue(vs []Violation, text string) []Violation {
	if len(vietnameseMarkRe.FindAllString(text, vietnameseMarkFloor)) < vietnameseMarkFloor {
		return vs
	}
	matches := englishFunctionWordRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return vs
	}
	seen := make(map[string]struct{})
	var examples []string
	for _, m := range matches {
		low := strings.ToLower(m)
		if _, ok := seen[low]; ok {
			continue
		}
		seen[low] = struct{}{}
		if len(examples) < 4 {
			examples = append(examples, low)
		}
	}
	return append(vs, Violation{
		Rule:     "english_residue",
		Target:   strings.Join(examples, ", "),
		Actual:   len(matches),
		Severity: SeverityError,
	})
}
