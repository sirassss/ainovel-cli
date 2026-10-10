package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/models"
)

func TestUsageTrackerReplaySessionsReadsWorkerLogs(t *testing.T) {
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "meta", "sessions", "agents")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	rec := sessionRecord{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: 1200, Output: 300, CacheRead: 800,
		},
		Meta: &sessionRecordMeta{Provider: "openrouter", Model: "test-model"},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(sessionsDir, "writer-ch01.jsonl"), data, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	tk := NewUsageTracker(nil, nil)
	n, err := tk.ReplaySessions(dir)
	if err != nil {
		t.Fatalf("ReplaySessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed messages = %d, want 1", n)
	}
	_, input, output, cacheRead, _ := tk.Totals()
	if input != 1200 || output != 300 || cacheRead != 800 {
		t.Fatalf("replayed totals = input:%d output:%d cache:%d", input, output, cacheRead)
	}
}

// makeUsageMsg builds a message that the OnMessage callback can accept (with Usage).
// Role must be set to assistant explicitly: UsageTracker.Record now filters by role,
// and only assistant messages are accumulated (other roles naturally carry no usage).
func makeUsageMsg(input, cacheRead, cacheWrite, output int) agentcore.AgentMessage {
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
		},
	}
}

// Test_pushSample_RingBuffer verifies the rotation semantics of the sliding window:
// the first N calls append directly; after that the oldest entry is overwritten by sampleIdx. recentSums always reflects the "last N calls".
func Test_pushSample_RingBuffer(t *testing.T) {
	var tot agentTotals

	for i := 1; i <= recentSampleCap; i++ {
		pushSample(&tot, i, i*100)
	}
	if got := len(tot.samples); got != recentSampleCap {
		t.Fatalf("after %d pushes, samples len=%d want %d", recentSampleCap, got, recentSampleCap)
	}

	pushSample(&tot, 999, 99900)
	if got := len(tot.samples); got != recentSampleCap {
		t.Fatalf("after overflow, samples len=%d want %d (no growth)", got, recentSampleCap)
	}
	cacheRead, input := recentSums(&tot)
	expectedCacheRead := 999
	expectedInput := 99900
	for i := 2; i <= recentSampleCap; i++ {
		expectedCacheRead += i
		expectedInput += i * 100
	}
	if cacheRead != expectedCacheRead || input != expectedInput {
		t.Fatalf("recentSums after overflow = (%d, %d), want (%d, %d)",
			cacheRead, input, expectedCacheRead, expectedInput)
	}
}

// Test_UsageTracker_RecordAccumulates verifies that Record accumulates multiple roles correctly,
// overall total = sum of all roles; each role is independent.
func Test_UsageTracker_RecordAccumulates(t *testing.T) {
	tk := NewUsageTracker(nil, nil) // modelSet=nil → falls back to provider Cost, which does not affect the accumulation logic

	tk.Record("writer", "", makeUsageMsg(1000, 800, 0, 200))
	tk.Record("writer", "", makeUsageMsg(1500, 1200, 100, 300))
	tk.Record("editor", "", makeUsageMsg(500, 0, 0, 100))

	cost, in, out, cr, cw := tk.Totals()
	if in != 3000 || out != 600 || cr != 2000 || cw != 100 {
		t.Fatalf("totals = (in=%d out=%d cr=%d cw=%d), want (3000 600 2000 100)", in, out, cr, cw)
	}
	if cost != 0 {
		t.Errorf("cost should be 0 when modelSet=nil and no provider Cost, got %v", cost)
	}

	per := tk.PerAgent()
	if len(per) != 2 {
		t.Fatalf("per-agent len=%d want 2", len(per))
	}
	// PerAgent is sorted by CacheRead descending: writer (2000) should come before editor (0)
	if per[0].Role != "writer" || per[1].Role != "editor" {
		t.Fatalf("per-agent order = %s,%s want writer,editor", per[0].Role, per[1].Role)
	}
	if per[0].Input != 2500 || per[0].CacheRead != 2000 {
		t.Errorf("writer totals = (in=%d cr=%d), want (2500 2000)", per[0].Input, per[0].CacheRead)
	}
}

// Test_UsageTracker_ArchitectAliasNormalized verifies that architect_short/mid/long
// are all normalized to the same "architect" key (so a sub-role switched by /model is not split into multiple rows).
func Test_UsageTracker_ArchitectAliasNormalized(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("architect_short", "", makeUsageMsg(100, 50, 0, 20))
	tk.Record("architect_mid", "", makeUsageMsg(200, 100, 0, 40))
	tk.Record("architect_long", "", makeUsageMsg(300, 150, 0, 60))

	per := tk.PerAgent()
	if len(per) != 1 {
		t.Fatalf("aliases must merge to single role, got %d entries: %+v", len(per), per)
	}
	if per[0].Role != "architect" {
		t.Fatalf("merged role name = %q, want architect", per[0].Role)
	}
	if per[0].Input != 600 || per[0].CacheRead != 300 {
		t.Errorf("merged totals = (in=%d cr=%d), want (600 300)", per[0].Input, per[0].CacheRead)
	}
}

func Test_UsageTracker_PerModelAccumulates(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.accumulate("writer", "openrouter", "model-a", agentcore.Usage{Input: 1000, Output: 200, CacheRead: 700})
	tk.accumulate("editor", "openrouter", "model-b", agentcore.Usage{Input: 500, Output: 100})
	tk.accumulate("writer", "openrouter", "model-a", agentcore.Usage{Input: 300, Output: 80, CacheRead: 200})

	perModel := tk.PerModel()
	if len(perModel) != 2 {
		t.Fatalf("per-model len=%d want 2", len(perModel))
	}
	seen := map[string]AgentUsage{}
	for _, m := range perModel {
		seen[m.Model] = m
	}
	if seen["openrouter/model-a"].Input != 1300 || seen["openrouter/model-a"].CacheRead != 900 {
		t.Errorf("model-a totals = %+v", seen["openrouter/model-a"])
	}
	if seen["openrouter/model-b"].Output != 100 {
		t.Errorf("model-b totals = %+v", seen["openrouter/model-b"])
	}

	snap := tk.Snapshot()
	restored := NewUsageTracker(nil, nil)
	restored.applyState(snap)
	if got := restored.PerModel(); len(got) != 2 {
		t.Fatalf("restored per-model len=%d want 2: %+v", len(got), got)
	}
}

func Test_UsageTracker_RecordUsesActualUsageModel(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("writer", "", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openrouter",
			Model:    "google/gemini-2.5-pro",
			Input:    1000,
			Output:   200,
		},
	})

	perModel := tk.PerModel()
	if len(perModel) != 1 {
		t.Fatalf("per-model len=%d want 1: %+v", len(perModel), perModel)
	}
	if perModel[0].Model != "openrouter/google/gemini-2.5-pro" {
		t.Fatalf("model key = %q, want openrouter/google/gemini-2.5-pro", perModel[0].Model)
	}
	if perModel[0].Input != 1000 || perModel[0].Output != 200 {
		t.Fatalf("model totals = %+v", perModel[0])
	}
}

func Test_UsageTracker_ProviderOnlyDoesNotInventModelKey(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	tk.Record("writer", "", agentcore.Message{
		Role: agentcore.RoleAssistant,
		Usage: &agentcore.Usage{
			Provider: "openrouter",
			Input:    1000,
			Output:   200,
		},
	})

	if got := tk.PerModel(); len(got) != 0 {
		t.Fatalf("provider-only usage must not create model stats without a model, got %+v", got)
	}
}

// Test_UsageTracker_RecentWindowReflectsLatest verifies that the sliding window reflects the "last N calls"
// and is not dragged down by early low hits — exactly the "early drag vs steady-state low hit rate" problem P1 sets out to solve.
func Test_UsageTracker_RecentWindowReflectsLatest(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// First 5 calls with very low hits (first-chapter scenario)
	for i := 0; i < 5; i++ {
		tk.Record("writer", "", makeUsageMsg(1000, 0, 0, 200))
	}
	// Last 8 calls (>5) with high hits (steady-state scenario)
	for i := 0; i < 8; i++ {
		tk.Record("writer", "", makeUsageMsg(1000, 900, 0, 200))
	}

	per := tk.PerAgent()
	if len(per) != 1 {
		t.Fatalf("len=%d want 1", len(per))
	}
	w := per[0]

	// Cumulative: 8 of 13 calls have hits → 7200/13000 ≈ 55.4%
	cumulativeRate := float64(w.CacheRead) / float64(w.Input) * 100
	if cumulativeRate < 50 || cumulativeRate > 60 {
		t.Errorf("cumulative hit rate = %.1f%%, want ~55%%", cumulativeRate)
	}

	// Sliding window: of the last 10 calls, 8 high hits + 2 zero hits → 7200/10000 = 72%
	if w.RecentSamples != recentSampleCap {
		t.Errorf("recent samples = %d, want %d (window full)", w.RecentSamples, recentSampleCap)
	}
	recentRate := float64(w.RecentCacheRead) / float64(w.RecentInput) * 100
	if recentRate < 70 || recentRate > 75 {
		t.Errorf("recent hit rate = %.1f%%, want ~72%% (proves window dropped early misses)", recentRate)
	}
	// Key point: the last-N rate is clearly higher than the cumulative one, proving the early 0s have been pushed out of the window
	if recentRate <= cumulativeRate {
		t.Errorf("recent (%.1f%%) must exceed cumulative (%.1f%%) once window slides past early misses",
			recentRate, cumulativeRate)
	}
}

// Test_computeSaved verifies the saved algorithm: CacheRead × (Input price - CacheRead price);
// returns 0 when the price difference ≤ 0 or InputCost ≤ 0 (the CacheWrite premium is not deducted).
func Test_computeSaved(t *testing.T) {
	cases := []struct {
		name  string
		usage agentcore.Usage
		entry models.ModelEntry
		want  float64
	}{
		{
			name:  "anthropic 5m 命中节省 90%",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 80_000},
			entry: models.ModelEntry{InputCostPer1M: 3.0, CacheReadCostPer1M: 0.3},
			want:  80_000 * (3.0 - 0.3) / 1_000_000, // 0.216
		},
		{
			name:  "无命中 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 0},
			entry: models.ModelEntry{InputCostPer1M: 3.0, CacheReadCostPer1M: 0.3},
			want:  0,
		},
		{
			name:  "模型未标价 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 50_000},
			entry: models.ModelEntry{InputCostPer1M: 0, CacheReadCostPer1M: 0},
			want:  0,
		},
		{
			name:  "异常价差 saved=0",
			usage: agentcore.Usage{Input: 100_000, CacheRead: 50_000},
			entry: models.ModelEntry{InputCostPer1M: 1.0, CacheReadCostPer1M: 2.0}, // Cache is actually more expensive
			want:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSaved(tc.usage, tc.entry)
			if got != tc.want {
				t.Errorf("computeSaved=%v want %v", got, tc.want)
			}
		})
	}
}

// Test_UsageTracker_CacheCapableSticky verifies that once CacheCapable is set to true it never reverts.
// A cache-capable model was run in the past → the cumulative hit data is valid; switching mid-way to an unsupported model must not revert the flag.
//
// Simulate by assigning perAgent directly (the resolveCost path needs ModelSet+Registry, already covered at the integration layer).
func Test_UsageTracker_CacheCapableSticky(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// Simulate "ran a cache-capable model before + had hits"
	tk.perAgent["writer"] = &agentTotals{
		Input: 1000, CacheRead: 500, Output: 200, CacheCapable: true,
	}
	// Then append one "call to a model that does not support cache"
	tk.Record("writer", "", makeUsageMsg(500, 0, 0, 100))

	per := tk.PerAgent()
	if len(per) != 1 || per[0].Role != "writer" {
		t.Fatalf("expected single writer entry, got %+v", per)
	}
	if !per[0].CacheCapable {
		t.Errorf("CacheCapable must remain true after switching to non-capable model")
	}
	if per[0].CacheRead != 500 || per[0].Input != 1500 {
		t.Errorf("totals after merge = (in=%d cr=%d), want (1500 500)",
			per[0].Input, per[0].CacheRead)
	}
}

// Test_UsageTracker_PerAgentSkipsZero verifies that a role that has consumed no tokens does not appear in PerAgent.
func Test_UsageTracker_PerAgentSkipsZero(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	// Build a role without consuming tokens (extreme case)
	tk.perAgent["ghost"] = &agentTotals{}
	tk.Record("writer", "", makeUsageMsg(100, 50, 0, 20))

	per := tk.PerAgent()
	if len(per) != 1 || per[0].Role != "writer" {
		t.Fatalf("ghost role with zero tokens must be skipped, got %+v", per)
	}
}

// Test_UsageTracker_MissingAssistantUsageCounted verifies the decision boundary
// of the missingAssistantUsage count:
//   - The accumulation path only checks Usage != nil (not tied to Role)
//   - The diagnostic path requires Role=Assistant and non-empty Content — only that looks like "a real LLM response that
//     got no usage", matching the typical symptom of upstream streaming not sending the OpenAI include_usage final
//     chunk. Other cases (user/tool messages, an assistant with empty content)
//     do not count as missing.
func Test_UsageTracker_MissingAssistantUsageCounted(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	withContent := func(text string) agentcore.Message {
		return agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{agentcore.TextBlock(text)},
		}
	}

	// assistant + has Content + nil Usage → looks like a real response but lacks usage, counted by diagnostics
	tk.Record("writer", "", withContent("hi"))
	tk.Record("writer", "", withContent("again"))
	// assistant but Content is empty → abnormal-recovery path or placeholder message, not missing
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleAssistant})
	// user/tool messages naturally carry no usage, so they are not missing whether or not Content is empty
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleUser, Content: []agentcore.ContentBlock{agentcore.TextBlock("u")}})
	tk.Record("writer", "", agentcore.Message{Role: agentcore.RoleTool, Content: []agentcore.ContentBlock{agentcore.TextBlock("t")}})
	// Normal, with usage → goes through the accumulation path, not counted by diagnostics
	tk.Record("writer", "", makeUsageMsg(100, 50, 0, 20))

	if got := tk.MissingAssistantUsage(); got != 2 {
		t.Errorf("MissingAssistantUsage=%d, want 2", got)
	}
	_, in, _, _, _ := tk.Totals()
	if in != 100 {
		t.Errorf("正常路径累计被破坏，input=%d want 100", in)
	}
}

// Test_UsageTracker_CacheCapableFromFacts verifies that when the registry cannot find the model, CacheCapable
// can still be marked true from "facts": models on self-hosted / domestic-proxy backends are often not in BerriAI/litellm's
// pricing index, so resolveCost returns capable=false; but as long as the backend really returned
// CacheRead or CacheWrite > 0, that proves the model objectively supports prompt cache, and the per-role row
// must not show "not enabled".
func Test_UsageTracker_CacheCapableFromFacts(t *testing.T) {
	tk := NewUsageTracker(nil, nil) // modelSet=nil → resolveCost is always capable=false

	// One call with CacheWrite (simulating the first cache write; the registry did not mark capable, but facts prove support)
	tk.Record("writer", "", makeUsageMsg(1000, 0, 200, 100))
	per := tk.PerAgent()
	if len(per) != 1 || !per[0].CacheCapable {
		t.Fatalf("CacheWrite > 0 应立即标记 CacheCapable=true，got %+v", per)
	}
	if !tk.OverallCacheCapable() {
		t.Errorf("overall CacheCapable 也应同步置 true")
	}

	// Reverse: a role with no cache activity at all must keep CacheCapable false
	tk.Record("editor", "", makeUsageMsg(500, 0, 0, 100))
	per = tk.PerAgent()
	for _, a := range per {
		if a.Role == "editor" && a.CacheCapable {
			t.Errorf("editor 全程无 CacheRead/Write，CacheCapable 不应被错误标记为 true")
		}
	}
}

// Test_UsageTracker_AccumulatesAnyRoleWithUsage verifies that the accumulation path is decoupled from Role:
// even if some adapter in future assembles usage onto a message of a non-assistant role,
// it is still accumulated correctly. Holds the contract "assembly rules are decoupled from accumulation rules".
func Test_UsageTracker_AccumulatesAnyRoleWithUsage(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	// Build a theoretically uncommon non-assistant message that carries Usage
	hypothetical := agentcore.Message{
		Role:  agentcore.RoleSystem,
		Usage: &agentcore.Usage{Input: 200, Output: 50, CacheRead: 100},
	}
	tk.Record("writer", "", hypothetical)

	_, in, out, cr, _ := tk.Totals()
	if in != 200 || out != 50 || cr != 100 {
		t.Errorf("未按 Usage 字段累加，got (in=%d out=%d cr=%d) want (200 50 100)", in, out, cr)
	}
	if tk.MissingAssistantUsage() != 0 {
		t.Errorf("有 Usage 不应计入 missing")
	}
}

// Test_UsageTracker_OnCostCallback verifies the wiring point of the budget sentinel: after each bookkeeping,
// the callback outside the lock carries the latest cumulative cost (including the provider self-reported cost path).
func Test_UsageTracker_OnCostCallback(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	var got []float64
	tk.SetOnCost(func(total float64) { got = append(got, total) })

	msg := func(cost float64) agentcore.AgentMessage {
		return agentcore.Message{
			Role:  agentcore.RoleAssistant,
			Usage: &agentcore.Usage{Input: 100, Output: 10, Cost: &agentcore.Cost{Total: cost}},
		}
	}
	tk.Record("writer", "", msg(0.5))
	tk.Record("writer", "", msg(0.25))

	if len(got) != 2 || got[0] != 0.5 || got[1] != 0.75 {
		t.Fatalf("onCost should carry growing totals, got %v", got)
	}
}

// Test_UsageTracker_OnMissingUsageOnce verifies that the blind-spot callback fires only the first time.
func Test_UsageTracker_OnMissingUsageOnce(t *testing.T) {
	tk := NewUsageTracker(nil, nil)
	fired := 0
	tk.SetOnMissingUsage(func() { fired++ })

	noUsage := agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock("正文")}}
	tk.Record("writer", "", noUsage)
	tk.Record("writer", "", noUsage)
	tk.Record("editor", "", noUsage)

	if fired != 1 {
		t.Fatalf("onMissingUsage should fire exactly once, got %d", fired)
	}
}

// TestCacheBreakDetection verifies the four trends of cache chain break detection:
// prefix grows + hits drop sharply within the same session → break; change task (new spawn) → switch baseline without comparing;
// prefix shortens (in-session compaction) → only reset the baseline, no alert; the drop does not satisfy the dual thresholds (relative 5%
// and absolute 2000) → no alert.
func TestCacheBreakDetection(t *testing.T) {
	tk := NewUsageTracker(nil, nil)

	// Establish the baseline: prefix 30k, hits 28k.
	tk.Record("writer", "写第1章", makeUsageMsg(30000, 28000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 0 {
		t.Fatalf("首条消息不应判断裂，got %d", got)
	}

	// Prefix grows while hits drop sharply within the same session (28k→4k) → break.
	tk.Record("writer", "写第1章", makeUsageMsg(34000, 4096, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("前缀增长+命中骤降应判 1 次断裂，got %d", got)
	}

	// Prefix shortens within the same session (context compaction, 4.4k < 34k) → reset the baseline, no alert.
	tk.Record("writer", "写第1章", makeUsageMsg(4400, 0, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("前缀缩短应视为压缩重置，got %d", got)
	}

	// Small fall-back on the new baseline (drop < the 2000 absolute threshold) → no alert.
	tk.Record("writer", "写第1章", makeUsageMsg(36000, 30000, 0, 100))
	tk.Record("writer", "写第1章", makeUsageMsg(38000, 28500, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("降幅 1.5k 未过绝对阈值不应告警，got %d", got)
	}

	// Change task = new spawn = new cache lineage: even if the first request's prefix is not shorter than the last request of the previous session
	// (38k → 40k) and hits drop sharply (28.5k→0), there is no comparison and no alert. This is the "consecutive short sessions
	// false alarm" regression case: the detection dimension must align with the session granularity of prompt_cache_key.
	tk.Record("writer", "写第2章", makeUsageMsg(40000, 0, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 1 {
		t.Fatalf("换 task 换基线，跨会话不应比较，got %d", got)
	}

	// A break again within the new session → alert as normal (proves the new baseline is in effect).
	tk.Record("writer", "写第2章", makeUsageMsg(45000, 38000, 0, 100))
	tk.Record("writer", "写第2章", makeUsageMsg(48000, 5000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 2 {
		t.Fatalf("新会话内断裂应正常检测，got %d", got)
	}

	// Relative drop <5% (100k→96k, down 4%) → no alert (even though the absolute drop 4k > 2000).
	tk.Record("editor", "审阅第一弧", makeUsageMsg(120000, 100000, 0, 100))
	tk.Record("editor", "审阅第一弧", makeUsageMsg(125000, 96000, 0, 100))
	if got := tk.OverallCacheBreaks(); got != 2 {
		t.Fatalf("相对降幅 4%% 未过 5%% 阈值不应告警，got %d", got)
	}

	// Per-role attribution: the break is recorded under writer and goes into the Snapshot.
	snap := tk.Snapshot()
	if snap.Overall.CacheBreaks != 2 || snap.PerAgent["writer"].CacheBreaks != 2 {
		t.Fatalf("断裂计数应进快照：overall=%d writer=%d", snap.Overall.CacheBreaks, snap.PerAgent["writer"].CacheBreaks)
	}
}
