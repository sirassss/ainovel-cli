// Package hancheck keeps Chinese characters out of the runtime strings of the Go code,
// and enforces comment language, legacy-term and Title Case gates.
//
// Directories listed in `pending` are not yet localized and are skipped; delete the
// entry once a directory is clean so the test enforces it. Strings kept on purpose
// (matching data of the zh mode, legacy-version data) carry the `han:keep` marker on
// the same line or on the line right above the literal.
package hancheck

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var pendingFlag = flag.String("pending", "", "directory (e.g. internal/domain) to scan even though it is in pending")

const keepMarker = "han:keep"

var pending = map[string]string{}

// pendingComments lists directories whose comments are not yet English; delete the
// entry once the directory is cleaned.
var pendingComments = map[string]string{
	"assets":                  "phase2",
	"cmd/ainovel-cli":         "phase2",
	"internal/agents":         "phase2",
	"internal/agents/guard":   "phase2",
	"internal/arbiter":        "phase2",
	"internal/bootstrap":      "phase2",
	"internal/chapterfacts":   "phase2",
	"internal/domain":         "phase2",
	"internal/entry/headless": "phase2",
	"internal/entry/startup":  "phase2",
	"internal/entry/tui":      "phase2",
	"internal/flow":           "phase2",
	"internal/host":           "phase2",
	"internal/host/exp":       "phase2",
	"internal/host/imp":       "phase2",
	"internal/host/sim":       "phase2",
	"internal/llmcontract":    "phase2",
	"internal/llmretry":       "phase2",
	"internal/logger":         "phase2",
	"internal/notify":         "phase2",
	"internal/revision":       "phase2",
	"internal/rules":          "phase2",
	"internal/store":          "phase2",
	"internal/tools":          "phase2",
	"internal/userrules":      "phase2",
	"internal/utils":          "phase2",
}

type violation struct {
	file string
	line int
	text string
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

func hasCJK(s string) bool {
	return strings.IndexFunc(s, isCJK) >= 0
}

func scanFile(path string) ([]violation, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(src), "\n")
	kept := func(line int) bool {
		if strings.Contains(lines[line-1], keepMarker) {
			return true
		}
		return line >= 2 && strings.Contains(lines[line-2], keepMarker)
	}
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			s = lit.Value
		}
		if !hasCJK(s) {
			return true
		}
		line := fset.Position(lit.Pos()).Line
		if kept(line) {
			return true
		}
		out = append(out, violation{file: path, line: line, text: s})
		return true
	})
	return out, nil
}

func isVietnameseRune(r rune) bool {
	return strings.ContainsRune("ăâđêôơưĂÂĐÊÔƠƯàáãèéìíĩòóõùúũýÀÁÃÈÉÌÍĨÒÓÕÙÚŨÝ", r) || (r >= 0x1EA0 && r <= 0x1EF9)
}

func hasVietnamese(s string) bool {
	return strings.IndexFunc(s, isVietnameseRune) >= 0
}

// scanComments reports every comment line (// and /* */, tests included) that holds
// Han or Vietnamese text and has no han:keep marker on the same or previous line.
func scanComments(path string) ([]violation, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(src), "\n")
	kept := func(line int) bool {
		return strings.Contains(lines[line-1], keepMarker) || (line >= 2 && strings.Contains(lines[line-2], keepMarker))
	}
	var out []violation
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			start := fset.Position(c.Pos()).Line
			for i, text := range strings.Split(c.Text, "\n") {
				if (hasCJK(text) || hasVietnamese(text)) && !kept(start+i) {
					out = append(out, violation{file: path, line: start + i, text: strings.TrimSpace(text)})
				}
			}
		}
	}
	return out, nil
}

// goDirs returns the directories (relative to the repo root) holding .go files.
// includeTests also counts _test.go files; includeSelf keeps internal/hancheck.
func goDirs(t *testing.T, root string, includeTests, includeSelf bool) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, top := range []string{"internal", "cmd", "assets"} {
		err := filepath.Walk(filepath.Join(root, top), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() && info.Name() == "testdata" {
				return filepath.SkipDir
			}
			if !info.IsDir() && strings.HasSuffix(p, ".go") && (includeTests || !strings.HasSuffix(p, "_test.go")) {
				rel, _ := filepath.Rel(root, filepath.Dir(p))
				seen[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		if d == "internal/hancheck" && !includeSelf {
			continue
		}
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

func TestNoHan(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range goDirs(t, root, false, false) {
		dir := dir
		// A "/" in a subtest name breaks the -run filter, so use "_".
		t.Run(strings.ReplaceAll(dir, "/", "_"), func(t *testing.T) {
			if reason, ok := pending[dir]; ok && *pendingFlag != dir {
				t.Skip(reason)
			}
			entries, err := os.ReadDir(filepath.Join(root, dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				vs, err := scanFile(filepath.Join(root, dir, name))
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range vs {
					text := []rune(strings.ReplaceAll(v.text, "\n", " "))
					if len(text) > 80 {
						text = text[:80]
					}
					t.Errorf("%s:%d: %s", filepath.ToSlash(strings.TrimPrefix(v.file, root+string(filepath.Separator))), v.line, string(text))
				}
			}
		})
	}
}

func TestScannerDetectsHan(t *testing.T) {
	src := "package x\n\nvar a = \"核心\"\nvar b = \"ok：x\"\nvar c = \"ok\"\nvar d = \"数据\" // han:keep\n// han:keep\nvar e = \"保留\"\n"
	p := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := scanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].text != "核心" || vs[1].text != "ok：x" {
		t.Fatalf("violations = %+v, want 核心 and ok：x", vs)
	}
}

func TestScannerDetectsComments(t *testing.T) {
	src := "// 中文\n// tiếng Việt\n// plain english\npackage x\n\nfunc f() {\n\tx := 1 // 保留 han:keep\n\t_ = x\n}\n"
	p := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := scanComments(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].line != 1 || vs[1].line != 2 {
		t.Fatalf("violations = %+v, want lines 1 and 2", vs)
	}
}

func TestHasVietnamese(t *testing.T) {
	for s, want := range map[string]bool{"tiếng Việt": true, "đ": true, "ơ": true, "à": true, "nghĩa": true, "ũ": true, "Ĩ": true, "plain english": false, "ë": false, "中文": false} {
		if got := hasVietnamese(s); got != want {
			t.Errorf("hasVietnamese(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestCommentsEnglish keeps Go comments (tests included) in English.
func TestCommentsEnglish(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range goDirs(t, root, true, true) {
		dir := dir
		t.Run(strings.ReplaceAll(dir, "/", "_"), func(t *testing.T) {
			if reason, ok := pendingComments[dir]; ok && *pendingFlag != dir {
				t.Skip(reason)
			}
			entries, err := os.ReadDir(filepath.Join(root, dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
					continue
				}
				vs, err := scanComments(filepath.Join(root, dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range vs {
					t.Errorf("%s:%d: %s", dir+"/"+e.Name(), v.line, v.text)
				}
			}
		})
	}
}

func TestScannerCommentEdgeCases(t *testing.T) {
	src := "// han:keep\n// 上一行保留\n// 中文\n/* ok\n   中文第三行\n   fine */\n/* 开头\n   nghĩa */\npackage x\n"
	p := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := scanComments(p)
	if err != nil {
		t.Fatal(err)
	}
	// Line 2 is kept by the marker above it; line 3 is not (marker is two lines up).
	var got []int
	for _, v := range vs {
		got = append(got, v.line)
	}
	want := []int{3, 5, 7, 8}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
}
