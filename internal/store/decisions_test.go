package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionStore_AppendAndRecent(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	first, err := s.Decisions.Append(DecisionRecord{
		Kind: "intervention", Decider: "arbiter",
		Input: "重写第3章", Facts: json.RawMessage(`{"phase":"writing"}`),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if first.ID == "" || first.At == "" || first.SchemaVersion != decisionSchemaVersion {
		t.Fatalf("Append 应补齐 ID/At/SchemaVersion: %+v", first)
	}

	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "继续写"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	// Failed ruling: error is an audit fact and must be persisted verbatim and read back.
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "plan_start", Decider: "arbiter", Input: "凡人修仙", Error: "USER_INACTIVE"}); err != nil {
		t.Fatalf("append 3: %v", err)
	}

	recent, err := s.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("应有 3 条记录, got %d", len(recent))
	}
	if recent[2].Error != "USER_INACTIVE" || len(recent[2].Decision) != 0 {
		t.Fatalf("失败裁定应带 error 且无 decision: %+v", recent[2])
	}
	if recent[0].Input != "重写第3章" || recent[1].Input != "继续写" {
		t.Fatalf("记录顺序应为旧→新: %+v", recent)
	}

	// n truncation: take only the most recent 1
	last, err := s.Decisions.Recent(1)
	if err != nil || len(last) != 1 || last[0].Input != "凡人修仙" {
		t.Fatalf("Recent(1) 应取最新一条, got %+v err=%v", last, err)
	}
}

func TestDecisionStore_InputTruncation(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	huge := strings.Repeat("长", maxDecisionInputBytes) // 3 bytes per character, far above the limit
	rec, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: huge})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !rec.InputTruncated || len(rec.Input) > maxDecisionInputBytes {
		t.Fatalf("超限 input 必须截断并标记: truncated=%v len=%d", rec.InputTruncated, len(rec.Input))
	}
	// The truncated record is still readable
	recent, err := s.Decisions.Recent(1)
	if err != nil || len(recent) != 1 {
		t.Fatalf("读回失败: %v", err)
	}
}

// A committed corrupt line in the middle of the file (with fully committed lines after it) must hard-fail — no ruling on a damaged history.
func TestDecisionStore_RecentRejectsCommittedCorruptLine(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "好的"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A corrupt line terminated with '\n' (fully committed but corrupt), followed by another complete record.
	if err := s.Decisions.io.AppendLine(decisionsFile, []byte("{\"schema_version\":1,\"kind\":\"interv\n")); err != nil {
		t.Fatalf("append corrupt: %v", err)
	}
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "之后"}); err != nil {
		t.Fatalf("append trailing: %v", err)
	}
	if _, err := s.Decisions.Recent(10); err == nil {
		t.Fatal("文件中部的已提交损坏行必须显式报错")
	}
}

// A partial tail line left by a crash (an uncommitted append whose last byte is not '\n') is tolerated as not-exist: drop the partial line and return the complete records
// before it instead of hard-failing — otherwise a single crash would permanently poison the append-only audit.
func TestDecisionStore_RecentToleratesUncommittedTail(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "好的"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a partial tail line interrupted by a crash: no trailing newline.
	if err := s.Decisions.io.AppendLine(decisionsFile, []byte(`{"schema_version":1,"kind":"interv`)); err != nil {
		t.Fatalf("append partial: %v", err)
	}
	recent, err := s.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("尾部残行应被容忍,不应报错: %v", err)
	}
	if len(recent) != 1 || recent[0].Input != "好的" {
		t.Fatalf("应丢弃残行并保留已提交记录,得到: %+v", recent)
	}
	// Recovery must really truncate the disk tail, not merely ignore it for this read; otherwise the next append would splice the two
	// JSON fragments into a permanently corrupt one. Reading again after the append should stay a complete closed loop.
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "恢复后"}); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	recent, err = s.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("recent after append: %v", err)
	}
	if len(recent) != 2 || recent[0].Input != "好的" || recent[1].Input != "恢复后" {
		t.Fatalf("尾部恢复后应可继续追加，得到: %+v", recent)
	}
	raw, err := os.ReadFile(filepath.Join(dir, decisionsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("恢复后的审计文件必须以提交换行结尾: %q", raw)
	}
}

// Even if the tail happens to be complete JSON, without the newline the protocol requires it is an uncommitted record; recovery must drop
// it and ensure later appends do not produce a `}{` splice.
func TestDecisionStore_RecoveryDropsValidJSONWithoutCommitNewline(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := s.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "已提交"}); err != nil {
		t.Fatal(err)
	}
	partial, err := json.Marshal(DecisionRecord{SchemaVersion: decisionSchemaVersion, Kind: "intervention", Input: "未提交"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decisions.io.AppendLine(decisionsFile, partial); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: the next read or append is the audit recovery boundary.
	reopened := NewStore(dir)
	if err := reopened.Init(); err != nil {
		t.Fatalf("restart init: %v", err)
	}
	if _, err := reopened.Decisions.Append(DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: "重启后"}); err != nil {
		t.Fatal(err)
	}
	recent, err := reopened.Decisions.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].Input != "已提交" || recent[1].Input != "重启后" {
		t.Fatalf("未提交的无换行 JSON 不应被接纳，得到: %+v", recent)
	}
}
