package diag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/store"
)

// sentinel is a piece of "novel text" that must never appear in the export.
const sentinel = "雪夜里主角揭穿了反派的惊天阴谋这是机密正文"

// writeSession writes messages in the sessions/*.jsonl format to a temporary output directory.
func writeSession(t *testing.T, rel string, msgs []agentcore.Message) string {
	t.Helper()
	dir := t.TempDir()
	writeSessionAt(t, dir, rel, msgs)
	return dir
}

func writeSessionAt(t *testing.T, dir, rel string, msgs []agentcore.Message) {
	t.Helper()
	path := filepath.Join(dir, "meta", "sessions", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func commitCall(chapterRaw string) agentcore.Message {
	args := json.RawMessage(`{"chapter":` + chapterRaw + `,"content":"` + sentinel + sentinel + `"}`)
	return agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.ToolCallBlock(agentcore.ToolCall{Name: "commit_chapter", Args: args})},
	}
}

func errResult(msg string) agentcore.Message {
	return agentcore.Message{
		Role:     agentcore.RoleTool,
		Content:  []agentcore.ContentBlock{agentcore.TextBlock(msg)},
		Metadata: map[string]any{"is_error": true},
	}
}

// TestExport_DeathLoopShape reproduces #34 end to end: the model stringifies the
// chapter argument of commit_chapter, causing a validation loop. It asserts the export
// pinpoints the loop and leaks no novel text.
func TestExport_DeathLoopShape(t *testing.T) {
	var msgs []agentcore.Message
	// Raw text emitted by a Worker (<4KB, bypassing session_compact) must be redacted.
	msgs = append(msgs, agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock(sentinel)},
	})
	// 14 rounds of commit_chapter(chapter:"7") + InputValidationError.
	for range 14 {
		msgs = append(msgs, commitCall(`"7"`))
		msgs = append(msgs, errResult("InputValidationError: chapter must be int"))
	}

	dir := writeSession(t, filepath.Join("agents", "writer-ch07.jsonl"), msgs)
	s := store.NewStore(dir)
	rep, rc := Diagnose(s)
	out := string(RenderExport(rep, rc))

	if strings.Contains(out, sentinel) {
		t.Fatalf("Novel text leaked! The export contains the sentinel:\n%s", out)
	}
	if !strings.Contains(out, `chapter: "7"`) {
		t.Errorf("Missing type-anomaly signal chapter: \"7\" (root cause of #34)\n%s", out)
	}
	if !strings.Contains(out, "InputValidationError") {
		t.Errorf("Error string not preserved\n%s", out)
	}
	if !strings.Contains(out, "×14") {
		t.Errorf("Repeat aggregation does not list ×14\n%s", out)
	}
	// Phase 2: runtime detection must classify this loop as a critical RepeatedToolError.
	if !strings.Contains(out, "Tool repeatedly reports the same error") {
		t.Errorf("Runtime detection did not produce RepeatedToolError\n%s", out)
	}
	if !strings.Contains(out, "[critical]") {
		t.Errorf("14 repeats should escalate to critical\n%s", out)
	}
}

// TestExport_NumberVsStringArg proves the scalar and string projections stay distinguishable:
// chapter:7 (number) stays 7, chapter:"7" (string) stays "7".
func TestExport_NumberVsStringArg(t *testing.T) {
	intDir := writeSession(t, filepath.Join("agents", "writer-ch07.jsonl"), []agentcore.Message{commitCall(`7`)})
	si := store.NewStore(intDir)
	repInt, rcInt := Diagnose(si)
	outInt := string(RenderExport(repInt, rcInt))
	if !strings.Contains(outInt, "chapter: 7") || strings.Contains(outInt, `chapter: "7"`) {
		t.Errorf("A numeric argument should render as chapter: 7 (unquoted)\n%s", outInt)
	}
}

// TestProjectValue_ProseArgRedacted guards the redaction boundary: short identifier-like
// values are kept, while values containing Chinese or spaces (e.g. a dispatch task or
// chapter title) are always redacted.
func TestProjectValue_ProseArgRedacted(t *testing.T) {
	keep := map[string]string{
		`"7"`:       `"7"`,       // stringified number (#34 signal)
		`"premise"`: `"premise"`, // enum
		`"writer"`:  `"writer"`,  // role name
		`7`:         `7`,         // numeric scalar
		`true`:      `true`,      // bool scalar
	}
	for in, want := range keep {
		if got := projectValue([]byte(in)); got != want {
			t.Errorf("%s should be kept: got %q want %q", in, got, want)
		}
	}
	// Chinese text / spaces must be redacted and must not contain the original text.
	prose := []string{`"第7章 雪夜的真相"`, `"雪夜杀机"`, `"主角揭穿阴谋"`}
	for _, in := range prose {
		got := projectValue([]byte(in))
		if !strings.HasPrefix(got, "<redacted") {
			t.Errorf("Short values with Chinese text or spaces should be redacted: %s → %q", in, got)
		}
		if strings.Contains(got, "雪夜") || strings.Contains(got, "主角") {
			t.Errorf("Redacted value still contains novel text: %s → %q", in, got)
		}
	}
}

// TestWriteExport_WritesFile proves the pure-function path: no TUI needed, it writes the fixed relative path.
func TestWriteExport_WritesFile(t *testing.T) {
	dir := writeSession(t, filepath.Join("agents", "writer-ch07.jsonl"), []agentcore.Message{commitCall(`"7"`), errResult("boom")})
	s := store.NewStore(dir)

	rep, rc := Diagnose(s)
	path, err := WriteExport(s, rep, rc)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}
	if want := filepath.Join(dir, filepath.FromSlash(ExportRelPath)); path != want {
		t.Errorf("Wrong path: got %s want %s", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "diag-export") {
		t.Errorf("Unexpected file content\n%s", data)
	}
	if strings.Contains(string(data), sentinel) {
		t.Errorf("The written file carries novel text")
	}
}

// TestRedactMessage_DupSha proves that repeated identical text yields the same sha (a loop signal).
func TestRedactMessage_DupSha(t *testing.T) {
	a := redactMessage("writer-ch07", agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock(sentinel)},
	})
	b := redactMessage("writer-ch07", agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock(sentinel)},
	})
	if a.TextSha == "" || a.TextSha != b.TextSha {
		t.Errorf("Identical text should give the same sha: %q vs %q", a.TextSha, b.TextSha)
	}
	if a.Redacted != 1 {
		t.Errorf("Should redact 1 text block, got %d", a.Redacted)
	}
}
