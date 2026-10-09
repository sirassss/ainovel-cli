// Package hancheck chặn chữ Hán lọt vào chuỗi chạy thật của mã Go.
//
// Mỗi thư mục trong `pending` chưa được Việt hóa nên bị bỏ qua; dịch xong thì xóa
// dòng của nó để test bắt buộc thư mục ấy sạch. Chuỗi cố tình giữ lại (dữ liệu so
// khớp của chế độ zh, dữ liệu phiên bản cũ) gắn marker `han:keep` trên cùng dòng
// hoặc dòng ngay trên literal.
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

var pendingFlag = flag.String("pending", "", "thư mục (vd internal/domain) cần quét dù đang nằm trong pending")

const keepMarker = "han:keep"

var pending = map[string]string{
	"internal/diag":      "phase2",
	"internal/eval":      "phase2",
	"internal/version":   "phase2",
	"internal/stylestat": "phase2",
	"internal/models":    "phase2",
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

// goDirs trả về các thư mục (tương đối so với gốc repo) có file .go không phải test.
func goDirs(t *testing.T, root string) []string {
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
			if !info.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
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
		if d == "internal/hancheck" {
			continue
		}
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

func TestNoHan(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range goDirs(t, root) {
		dir := dir
		// Dấu "/" trong tên subtest làm hỏng bộ lọc -run, nên đổi thành "_".
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
