package hancheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Flip to false once the matching cleanup task has landed, so the gate is enforced.
var (
	termsPending     = false
	titleCasePending = true
)

// legacyTerms are the old Vietnamese terms replaced by the approved glossary (lowercase,
// matched case-insensitively).
var legacyTerms = []string{
	"đại cương", "phục bút", "thẩm duyệt", "chính văn", "hoàn kết", "tài phán", "quy hoạch",
	"tập khép", "nghiệm thu", "khung xương", "hợp đồng chương", "điểm móc", "móc câu",
	"sảng điểm", "gọt giũa", "đề cương",
}

// awkwardPhrases are known bad machine-translation artifacts.
var awkwardPhrases = []string{
	"bản thảo nội dung chương", "bản lập kế hoạch", "việc lập kế hoạch lập kế hoạch", "nội dung chương chương",
}

const (
	caseOKMarker = "case:ok"
	// termKeepMarker on the literal's line (or the line above) exempts a deliberate legacy term,
	// e.g. an alias kept so already-saved data still parses.
	termKeepMarker = "term:keep"
)

// keyNames are keyboard key names that stay capitalized in TUI hints.
var keyNames = map[string]bool{"Enter": true, "Esc": true, "Tab": true, "Ctrl": true, "Shift": true, "PgUp": true, "PgDn": true}

// titleCaseViolation reports whether a Vietnamese string has 2+ consecutive
// capitalized words (key names excepted).
func titleCaseViolation(s string) bool {
	if !hasVietnamese(s) && strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII && unicode.IsLetter(r) }) < 0 {
		return false
	}
	run := 0
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, ".,:;!?()[]{}\"'`")
		if keyNames[strings.SplitN(w, "+", 2)[0]] || !isTitleWord(w) {
			run = 0
			continue
		}
		if run++; run >= 2 {
			return true
		}
	}
	return false
}

// isTitleWord is true for a word starting uppercase and holding a lowercase letter
// (acronyms like TUI do not count).
func isTitleWord(w string) bool {
	rs := []rune(w)
	if len(rs) < 2 || !unicode.IsUpper(rs[0]) {
		return false
	}
	for _, r := range rs[1:] {
		if unicode.IsLower(r) {
			return true
		}
	}
	return false
}

// stringLits parses a Go file and calls fn for each string literal with its decoded
// value and starting line.
func stringLits(path string, fn func(s string, line int)) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				s = lit.Value
			}
			fn(s, fset.Position(lit.Pos()).Line)
		}
		return true
	})
	return nil
}

func termHits(path, text string, firstLine int) []string {
	var out []string
	for i, ln := range strings.Split(text, "\n") {
		low := strings.ToLower(ln)
		for _, list := range [][]string{legacyTerms, awkwardPhrases} {
			for _, term := range list {
				if strings.Contains(low, term) {
					out = append(out, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(path), firstLine+i, term))
				}
			}
		}
	}
	return out
}

// scanTerms returns "file:line: term" for each legacy term or awkward phrase in a
// .go file (string literals only) or any other text file (every line).
func scanTerms(path string) ([]string, error) {
	if !strings.HasSuffix(path, ".go") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return termHits(path, string(b), 1), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	err = stringLits(path, func(s string, line int) {
		if strings.Contains(lines[line-1], termKeepMarker) || (line > 1 && strings.Contains(lines[line-2], termKeepMarker)) {
			return
		}
		out = append(out, termHits(path, s, line)...)
	})
	return out, err
}

// scanTitleCase returns "file:line: text" for each Title Case Vietnamese literal
// without a case:ok marker on its line.
func scanTitleCase(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	err = stringLits(path, func(s string, line int) {
		if titleCaseViolation(s) && !strings.Contains(lines[line-1], caseOKMarker) {
			out = append(out, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(path), line, s))
		}
	})
	return out, err
}

// walkFiles calls fn for every regular file under dir for which keep returns true.
func walkFiles(t *testing.T, dir string, keep func(path string) bool, fn func(path string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "zh") {
			return filepath.SkipDir
		}
		if !d.IsDir() && keep(p) {
			fn(p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestNoLegacyTerms(t *testing.T) {
	if termsPending {
		t.Skip("legacy terms not yet cleaned")
	}
	root := filepath.Join("..", "..")
	check := func(p string) {
		hits, err := scanTerms(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			t.Error(strings.TrimPrefix(h, filepath.ToSlash(root)+"/"))
		}
	}
	for _, f := range []string{"README.md", "HUONG_DAN_SU_DUNG.md", "config.example.jsonc", "config/config.example.jsonc"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			check(filepath.Join(root, f))
		}
	}
	walkFiles(t, filepath.Join(root, "assets"), func(p string) bool { return strings.HasSuffix(p, ".md") }, check)
	for _, top := range []string{"internal", "cmd", "assets"} {
		walkFiles(t, filepath.Join(root, top), func(p string) bool {
			return strings.HasSuffix(p, ".go") && !strings.Contains(filepath.ToSlash(p), "internal/hancheck/")
		}, check)
	}
}

func TestTUINotTitleCase(t *testing.T) {
	if titleCasePending {
		t.Skip("TUI Title Case not yet cleaned")
	}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal/entry", "internal/host", "internal/bootstrap", "internal/notify"} {
		walkFiles(t, filepath.Join(root, dir), func(p string) bool {
			return strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go")
		}, func(p string) {
			hits, err := scanTitleCase(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hits {
				t.Error(strings.TrimPrefix(h, filepath.ToSlash(root)+"/"))
			}
		})
	}
}

func TestTitleCaseFilter(t *testing.T) {
	for s, want := range map[string]bool{
		"Tài Nguyên Đã Dùng": true,
		"Enter Gửi":          false,
		"Esc Thoát":          false,
		"Tổng quan":          false,
		"Ctrl+C Thoát":       false,
		"Resource Used":      false, // no diacritics: English, not checked
	} {
		if got := titleCaseViolation(s); got != want {
			t.Errorf("titleCaseViolation(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestScanTermsFindsLegacy(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "a.md")
	if err := os.WriteFile(md, []byte("ok\nViết Đại Cương trước\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gof := filepath.Join(dir, "a.go")
	if err := os.WriteFile(gof, []byte("package x\n\nvar a = \"ok\"\nvar b = `l1\nbản lập kế hoạch\n`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{md: ":2: đại cương", gof: ":5: bản lập kế hoạch"} {
		hits, err := scanTerms(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || !strings.HasSuffix(hits[0], want) {
			t.Errorf("scanTerms(%s) = %v, want one hit ending %q", filepath.Base(path), hits, want)
		}
	}
}

func TestScanTitleCaseFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	src := "package x\n\nvar a = \"Tài Nguyên Đã Dùng\"\nvar b = \"Tên Lửa\" // case:ok\nvar c = \"Tổng quan\"\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := scanTitleCase(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], ":3:") {
		t.Fatalf("hits = %v, want only line 3", hits)
	}
}

func TestScanTermsHonorsTermKeep(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	src := "package x\n\nvar a = map[string]string{\n\t\"Móc câu\": \"x\", // term:keep\n\t// term:keep\n\t\"Phục bút\": \"y\",\n\t\"Đại cương\": \"z\",\n}\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := scanTerms(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.HasSuffix(hits[0], ":7: đại cương") {
		t.Fatalf("hits = %v, want only line 7", hits)
	}
}
