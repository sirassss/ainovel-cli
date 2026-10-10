package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/agentcore"
)

// TestSessionStore_MetaInjected_AssistantWithUsage verifies that only "assistant + has Usage" messages
// get _meta attached, which is the precondition for exact pricing on the replay path.
func TestSessionStore_MetaInjected_AssistantWithUsage(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	lookup := ModelLookup(func(agentName string) (string, string) {
		return "meme", "gpt-5.4"
	})
	logger := s.SubAgentLogger(lookup)

	logger("writer", "写第 1 章", agentcore.Message{
		Role:  agentcore.RoleUser,
		Usage: nil,
	})
	logger("writer", "写第 1 章", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200,
		},
	})
	logger("writer", "写第 1 章", agentcore.Message{
		Role:  agentcore.RoleAssistant,
		Usage: nil, // assistant but no usage (the stream did not carry a final usage chunk)
	})

	entries := readJSONL(t, filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if len(entries) != 3 {
		t.Fatalf("entries=%d want 3", len(entries))
	}
	if _, has := entries[0]["_meta"]; has {
		t.Errorf("user message should NOT have _meta")
	}
	if _, has := entries[2]["_meta"]; has {
		t.Errorf("assistant without Usage should NOT have _meta")
	}
	meta, ok := entries[1]["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("assistant+Usage should have _meta map, got %T %v", entries[1]["_meta"], entries[1]["_meta"])
	}
	if meta["provider"] != "meme" || meta["model"] != "gpt-5.4" {
		t.Errorf("_meta = %v want provider=meme model=gpt-5.4", meta)
	}
}

// TestSessionStore_MetaModelSwitch verifies that after switching models mid-run, the _meta of subsequent messages changes too.
// This is plan B's exact support for "/model switching within the same process".
func TestSessionStore_MetaModelSwitch(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))

	current := "model-a"
	lookup := ModelLookup(func(agentName string) (string, string) {
		return "meme", current
	})
	logger := s.SubAgentLogger(lookup)

	logger("writer", "写第 1 章", makeAssistantWithUsage())
	current = "model-b" // Simulate a /model switch
	logger("writer", "写第 1 章", makeAssistantWithUsage())

	entries := readJSONL(t, filepath.Join(dir, "meta/sessions/agents/writer-ch01.jsonl"))
	if len(entries) != 2 {
		t.Fatalf("entries=%d want 2", len(entries))
	}
	for i, want := range []string{"model-a", "model-b"} {
		meta, ok := entries[i]["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("entry[%d] missing _meta", i)
		}
		if got := meta["model"]; got != want {
			t.Errorf("entry[%d] model = %v want %s", i, got, want)
		}
	}
}

// TestSessionStore_NilLookup verifies that writing still works normally when lookup=nil,
// just without _meta.
func TestSessionStore_NilLookup(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	logger := s.SubAgentLogger(nil)
	logger("writer", "写第 1 章", makeAssistantWithUsage())

	rel, err := s.subAgentPath("writer", "写第 1 章")
	if err != nil {
		t.Fatal(err)
	}
	entries := readJSONL(t, filepath.Join(dir, rel))
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	if _, has := entries[0]["_meta"]; has {
		t.Errorf("nil lookup should not produce _meta")
	}
	// But the other fields (role/usage) must be normal
	if entries[0]["role"] != "assistant" {
		t.Errorf("role lost: %v", entries[0]["role"])
	}
}

func TestSessionStoreContinuesAgentSequenceAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first := NewSessionStore(newIO(dir)).SubAgentLogger(nil)
	first("architect_long", "处理反馈", makeAssistantWithUsage())

	second := NewSessionStore(newIO(dir)).SubAgentLogger(nil)
	second("architect_long", "扩展大纲", makeAssistantWithUsage())

	if got := len(readJSONL(t, filepath.Join(dir, "meta/sessions/agents/architect_long-001.jsonl"))); got != 1 {
		t.Fatalf("first session entries = %d, want 1", got)
	}
	if got := len(readJSONL(t, filepath.Join(dir, "meta/sessions/agents/architect_long-002.jsonl"))); got != 1 {
		t.Fatalf("second session entries = %d, want 1", got)
	}
}

func makeAssistantWithUsage() agentcore.Message {
	return agentcore.Message{
		Role:  agentcore.RoleAssistant,
		Usage: &agentcore.Usage{Input: 1000, Output: 200, TotalTokens: 1200},
	}
}

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal line: %v\n%s", err, string(line))
		}
		out = append(out, m)
	}
	return out
}

func TestExtractChapterFromTaskText(t *testing.T) {
	cases := map[string]string{
		"Viết chương 4":       "ch04",
		"Viết lại chương 12":  "ch12",
		"Trau chuốt Chương 3": "ch03",
		"写第 3 章":              "ch03", // zh-mode task. han:keep
		"Tạo tóm tắt tập 1":   "",
		"Viết chương 0":       "",
	}
	for task, want := range cases {
		if got := extractChapter(task); got != want {
			t.Errorf("extractChapter(%q) = %q, want %q", task, got, want)
		}
	}
}
