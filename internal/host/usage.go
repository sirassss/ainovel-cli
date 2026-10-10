package host

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/models"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// recentSampleCap is the sliding-window size: keep only the (cacheRead, input)
// samples of each role's most recent N calls, used in the left column to compare "cumulative vs last N calls" hit rates and tell "early drag" from "steady-state low hit rate".
const recentSampleCap = 10

// Dual thresholds for judging a cache chain break (aligned with Claude Code's empirical experience): the hit volume drops more than
// 5% (relative) from the last call and the drop is ≥2000 tokens (absolute) to count as a break — a single relative threshold is
// drowned by small-prefix noise, and a single absolute threshold misses significant degradation on large prefixes.
const (
	cacheBreakKeepRatio     = 0.95
	cacheBreakMinDropTokens = 2000
)

// UsageTracker accumulates the LLM input/output tokens and USD cost of all agents in the whole session.
//
// How it works:
//   - Each time an agent's OnMessage callback fires, Record(agentName, msg) is called
//   - agentName maps to a role (architect_* normalized to architect), and the model currently bound to that role in ModelSet is looked up
//   - Model prices are looked up in models.DefaultRegistry, and the four items (non-cached input / output / cache read / cache write) are multiplied and summed
//   - When the registry lacks the model, fall back to msg.Usage.Cost.Total (supplied by the provider, may be 0)
//   - After a model hot swap (/model), later messages are priced with the new model automatically, while old messages keep their old cost
//
// Also maintains per-role dimensions (writer/editor/architect):
//   - cumulative hit data → overall optimization effect
//   - sliding window of the last N calls → tells early drag from steady-state low hit rate
//   - CacheCapable flag → tells "not enabled" from "really 0% hit"
//
// Thread-safe.
type UsageTracker struct {
	mu       sync.Mutex
	overall  agentTotals
	perAgent map[string]*agentTotals // key is the role name normalized by agentRoleName
	perModel map[string]*agentTotals // key is provider/model; falls back to model when the provider is unknown
	modelSet *bootstrap.ModelSet
	store    *storepkg.Store // May be nil (test scenarios); when nil all persistence methods are silent no-ops

	// cacheTrack is the per-role cache chain baseline (prefix length / hit volume / time of the last call),
	// used for break detection. Updated only on the live Record path — replaying history does not detect,
	// otherwise every startup would turn old breaks into false alarms. Not persisted.
	cacheTrack map[string]*cacheTrackState

	// missingAssistantUsage accumulates how many times "an assistant message was received but Usage is nil".
	// In practice this mostly happens when a self-hosted OpenAI-compatible backend does not send that final usage chunk at the end of streaming per the OpenAI
	// stream_options.include_usage protocol — partial.Usage
	// stays nil and all accumulated fields stay at 0. The counter lets the UI tell the user directly "upstream did not return
	// usage, nothing is broken on this side", instead of the user grinding through the cache panel code.
	missingAssistantUsage int
	loggedMissingUsage    bool // Warn only once per session, to avoid flooding tui.log

	// saveCh is triggered non-blockingly by Record after accumulating; autoSaveLoop listens and writes to disk with a debounce.
	// buffered=1: multiple consecutive Records collapse into one persist signal; when full, just drop it, and the next tick writes everything together.
	saveCh       chan struct{}
	autoSaveMu   sync.Mutex
	autoSaveDone chan struct{}

	// onCost is called outside the lock after each bookkeeping with the latest cumulative cost (BudgetSentinel over-the-line detection).
	// It must be set via SetOnCost before concurrent Record starts, and is read-only afterwards.
	onCost func(total float64)

	// onMissingUsage is called once when "assistant message has no Usage" is first found (at the same moment as the slog
	// warn). With a budget enabled this means a billing blind spot — cost stays 0 and the budget never triggers, so someone must be told.
	onMissingUsage func()
}

// usageSample is the hit sample of a single OnMessage, recording only the numerator and denominator of the hit rate.
type usageSample struct {
	CacheRead int
	Input     int
}

// cacheTrackState is the cache chain baseline of one role's current session. task (the spawn task text) is
// the session identity: a new task = a new spawn = a new cache lineage (prompt_cache_key carries #seq), where a low hit on the first request
// is normal, so just switch the baseline without comparing — otherwise a false break would be reported when "the previous session was short and the new session's first request has an even longer prefix".
// The Input semantics (including CacheRead, see the computeCost comment) happen to equal "the prefix length
// processed by the server", from which three trends can be told apart: prefix shortened = in-session compaction (legitimate, reset the baseline);
// prefix grew and hits rose with it = healthy chain; prefix grew but hits dropped sharply = break.
type cacheTrackState struct {
	task          string
	lastPrefix    int
	lastCacheRead int
	lastAt        time.Time
}

// agentTotals is the cumulative count of one agent.
//   - Saved is the difference "if billed at the non-cached price", back-computed from the current hit data
//   - CacheCapable is set to true only after the role has made at least one call to a "model known to support cache"
//   - samples is a fixed-length ring buffer: the first recentSampleCap calls are appended directly, after which it rotates by sampleIdx
type agentTotals struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite   int
	Cost         float64
	Saved        float64
	CacheCapable bool
	CacheBreaks  int // Number of cache chain breaks detected live (replay is not counted)
	samples      []usageSample
	sampleIdx    int
}

func NewUsageTracker(set *bootstrap.ModelSet, store *storepkg.Store) *UsageTracker {
	return &UsageTracker{
		modelSet:   set,
		store:      store,
		perAgent:   make(map[string]*agentTotals, 4),
		perModel:   make(map[string]*agentTotals, 4),
		cacheTrack: make(map[string]*cacheTrackState, 4),
		saveCh:     make(chan struct{}, 1),
	}
}

// Record dispatches an agent message onto two paths: accumulation / diagnostics.
//
// Accumulation only checks whether Usage exists — "which message carries Usage" is an agentcore/litellm adapter
// assembly detail (the upstream protocol puts usage at the top level of the response); if the assembly rules change in future, nothing here needs to change.
// Diagnostics require Role=Assistant and non-empty Content, to keep AbortMsg / abnormal-recovery / tool /
// user messages from polluting the missingAssistantUsage count.
func (t *UsageTracker) Record(agentName, task string, msg agentcore.AgentMessage) {
	if t == nil {
		return
	}
	m, ok := msg.(agentcore.Message)
	if !ok {
		return
	}
	if m.Usage == nil {
		if m.Role == agentcore.RoleAssistant && len(m.Content) > 0 {
			t.flagMissingUsage(agentName)
		}
		return
	}
	role := agentRoleName(agentName)
	t.noteCacheBreak(role, task, *m.Usage)
	provider, modelName := usageActualModel(m.Usage)
	t.accumulate(role, provider, modelName, *m.Usage)
}

// noteCacheBreak is cache chain break detection (pure observation, no repair, called only on the live Record path).
//
// Judgment: within the same session (role+task), the prefix (Input, including CacheRead) has not shortened, while the hit volume dropped
// >5% from the last call and the drop is ≥2000 tokens. A task change = new spawn = new cache lineage, so switch the baseline without
// comparing; a shortened prefix means context compaction, a legitimate drop that only resets the baseline without an alert. Attribution gives
// hints by priority: interval longer than TTL → suspected expiry; very short interval with client bytes that should be stable → suspected server-side eviction /
// routing drift (a relay polling upstreams is a common cause).
func (t *UsageTracker) noteCacheBreak(role, task string, u agentcore.Usage) {
	now := time.Now()
	prefix := u.Input // litellm providers all guarantee that Input includes CacheRead

	t.mu.Lock()
	st := t.cacheTrack[role]
	if st == nil || st.task != task {
		t.cacheTrack[role] = &cacheTrackState{task: task, lastPrefix: prefix, lastCacheRead: u.CacheRead, lastAt: now}
		t.mu.Unlock()
		return
	}
	prevPrefix, prevRead, prevAt := st.lastPrefix, st.lastCacheRead, st.lastAt
	st.lastPrefix, st.lastCacheRead, st.lastAt = prefix, u.CacheRead, now

	broke := prevPrefix > 0 && prefix >= prevPrefix &&
		float64(u.CacheRead) < float64(prevRead)*cacheBreakKeepRatio &&
		prevRead-u.CacheRead >= cacheBreakMinDropTokens
	if broke {
		t.overall.CacheBreaks++
		per := t.perAgent[role]
		if per == nil {
			per = &agentTotals{}
			t.perAgent[role] = per
		}
		per.CacheBreaks++
	}
	t.mu.Unlock()

	if !broke {
		return
	}
	gap := now.Sub(prevAt).Round(time.Second)
	hint := "nghi máy chủ loại bỏ cache hoặc định tuyến lệch (relay xoay vòng upstream là nguyên nhân thường gặp)"
	if gap > time.Hour {
		hint = "nghi TTL 1h đã hết hạn"
	} else if gap > 5*time.Minute {
		hint = "nghi TTL 5m đã hết hạn"
	}
	slog.Warn("cache chain broken: prefix did not shrink but cache hits dropped sharply",
		"module", "usage", "role", role,
		"cache_read", fmt.Sprintf("%d→%d", prevRead, u.CacheRead),
		"prefix", fmt.Sprintf("%d→%d", prevPrefix, prefix),
		"gap", gap.String(), "hint", hint)
	t.notifyDirty()
}

func usageActualModel(u *agentcore.Usage) (provider, modelName string) {
	if u == nil {
		return "", ""
	}
	return strings.TrimSpace(u.Provider), strings.TrimSpace(u.Model)
}

// flagMissingUsage accumulates one "looks like a real LLM response but got no usage" event, and prints the
// warn log only once per session to avoid flooding tui.log.
func (t *UsageTracker) flagMissingUsage(agentName string) {
	t.mu.Lock()
	t.missingAssistantUsage++
	shouldLog := !t.loggedMissingUsage
	t.loggedMissingUsage = true
	t.mu.Unlock()
	if shouldLog {
		slog.Warn("LLM response carried no usage data, so the cache/cost panel will have no running totals; usually the streaming upstream does not send the final usage chunk per OpenAI's include_usage protocol",
			"module", "usage", "agent", agentName)
		if t.onMissingUsage != nil {
			t.onMissingUsage()
		}
	}
	t.notifyDirty()
}

// SetOnMissingUsage registers the one-time callback for "usage missing found for the first time".
// It must be called once during Host construction, before concurrent Record starts.
func (t *UsageTracker) SetOnMissingUsage(cb func()) {
	if t == nil {
		return
	}
	t.onMissingUsage = cb
}

// notifyDirty triggers a persist signal non-blockingly, which autoSaveLoop actually writes with a debounce.
// The signal channel has buffered=1: multiple consecutive Records collapsing into one save request is enough.
func (t *UsageTracker) notifyDirty() {
	if t == nil || t.saveCh == nil {
		return
	}
	select {
	case t.saveCh <- struct{}{}:
	default:
	}
}

// accumulate adds a message that carries Usage to the three counters overall / per-role / per-model.
// Empty provider/model means "get the role's model from the current ModelSet" (live path); non-empty means
// "force pricing by the given model" (the replay path uses _meta from the session jsonl).
// resolveCost runs outside the lock (it only reads modelSet/Registry); inside the lock only additions are done.
func (t *UsageTracker) accumulate(role, provider, modelName string, u agentcore.Usage) {
	provider, modelName = t.effectiveModel(role, provider, modelName)
	cost, saved, capable := t.resolveCost(modelName, u)

	t.mu.Lock()
	addUsage(&t.overall, u, cost, saved, capable)

	per := t.perAgent[role]
	if per == nil {
		per = &agentTotals{}
		t.perAgent[role] = per
	}
	addUsage(per, u, cost, saved, capable)

	if key := modelUsageKey(provider, modelName); key != "" {
		perModel := t.perModel[key]
		if perModel == nil {
			perModel = &agentTotals{}
			t.perModel[key] = perModel
		}
		addUsage(perModel, u, cost, saved, capable)
	}
	total := t.overall.Cost
	t.mu.Unlock()

	t.notifyDirty()
	if t.onCost != nil {
		t.onCost(total)
	}
}

// SetOnCost registers the bookkeeping callback (carries the latest cumulative cost, called outside the lock).
// It must be called once during Host construction, before concurrent Record starts.
func (t *UsageTracker) SetOnCost(cb func(total float64)) {
	if t == nil {
		return
	}
	t.onCost = cb
}

func (t *UsageTracker) effectiveModel(role, provider, modelName string) (string, string) {
	provider = strings.TrimSpace(provider)
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		if t != nil && t.modelSet != nil {
			p, m, _ := t.modelSet.CurrentSelection(role)
			return p, m
		}
		return "", ""
	}
	if provider == "" && t != nil && t.modelSet != nil {
		p, m, _ := t.modelSet.CurrentSelection(role)
		if m == modelName {
			provider = p
		}
	}
	return provider, modelName
}

func modelUsageKey(provider, modelName string) string {
	provider = strings.TrimSpace(provider)
	modelName = strings.TrimSpace(modelName)
	switch {
	case modelName == "":
		return ""
	case provider == "":
		return modelName
	default:
		return provider + "/" + modelName
	}
}

// addUsage adds the tokens and cost of a single call onto a totals.
// It must be called while holding UsageTracker.mu.
//
// CacheCapable is judged by "facts" first: as soon as CacheRead or CacheWrite > 0 has been seen, that proves
// upstream really does prompt caching. The registry's CacheReadCostPer1M serves only as a fallback,
// because self-hosted backend models (mimo-v2.5-pro / domestic proxies etc.) are usually not in the BerriAI/litellm
// pricing index, yet the actual Usage does carry cache data, and the UI should not misjudge it as "not enabled".
func addUsage(t *agentTotals, u agentcore.Usage, cost, saved float64, capable bool) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheRead += u.CacheRead
	t.CacheWrite += u.CacheWrite
	t.Cost += cost
	t.Saved += saved
	if capable || u.CacheRead > 0 || u.CacheWrite > 0 {
		t.CacheCapable = true
	}
	pushSample(t, u.CacheRead, u.Input)
}

// pushSample pushes one sample into the ring buffer. The first recentSampleCap calls are a plain append, after which it rotates and overwrites.
func pushSample(t *agentTotals, cacheRead, input int) {
	s := usageSample{CacheRead: cacheRead, Input: input}
	if len(t.samples) < recentSampleCap {
		t.samples = append(t.samples, s)
		return
	}
	t.samples[t.sampleIdx] = s
	t.sampleIdx = (t.sampleIdx + 1) % recentSampleCap
}

// recentSums returns the sums of cacheRead and input within the sliding window, as the numerator and denominator of the "last N calls hit rate".
// It uses sum/sum rather than the "average of per-call ratios" to keep small samples (input=a few hundred tokens) from amplifying noise.
func recentSums(t *agentTotals) (cacheRead, input int) {
	for _, s := range t.samples {
		cacheRead += s.CacheRead
		input += s.Input
	}
	return cacheRead, input
}

// Totals returns a snapshot of the cumulative totals.
func (t *UsageTracker) Totals() (cost float64, input, output, cacheRead, cacheWrite int) {
	if t == nil {
		return 0, 0, 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.Cost, t.overall.Input, t.overall.Output, t.overall.CacheRead, t.overall.CacheWrite
}

// SavedUSD returns the cumulative USD saved through cache hits.
func (t *UsageTracker) SavedUSD() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.Saved
}

// OverallRecent returns the cacheRead sum, input sum and sample count within the sliding window (≤ recentSampleCap calls).
func (t *UsageTracker) OverallRecent() (cacheRead, input, samples int) {
	if t == nil {
		return 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, in := recentSums(&t.overall)
	return r, in, len(t.overall.samples)
}

// OverallCacheBreaks returns the total number of cache chain breaks detected live.
func (t *UsageTracker) OverallCacheBreaks() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.CacheBreaks
}

// OverallCacheCapable reports whether, overall, at least one call was made to a model known to support cache.
func (t *UsageTracker) OverallCacheCapable() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overall.CacheCapable
}

// MissingAssistantUsage returns the cumulative count of "assistant message received but Usage is nil".
// A value above 0 usually means upstream streaming did not send OpenAI's final usage chunk,
// and the UI uses it to show a hint rather than mistaking the cache module itself for broken.
func (t *UsageTracker) MissingAssistantUsage() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.missingAssistantUsage
}

// ── Persistence ──

// Snapshot copies the current cumulative state into a serializable domain.UsageState.
// The sliding-window samples do not go into the snapshot — it is a short-term diagnostic window and means little across processes.
func (t *UsageTracker) Snapshot() domain.UsageState {
	if t == nil {
		return domain.UsageState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := domain.UsageState{
		Schema:       domain.UsageSchemaVersion,
		UpdatedAt:    time.Now(),
		Overall:      totalsSnapshot(&t.overall),
		PerAgent:     make(map[string]domain.AgentUsageTotals, len(t.perAgent)),
		PerModel:     make(map[string]domain.AgentUsageTotals, len(t.perModel)),
		MissingUsage: t.missingAssistantUsage,
	}
	for role, v := range t.perAgent {
		state.PerAgent[role] = totalsSnapshot(v)
	}
	for model, v := range t.perModel {
		state.PerModel[model] = totalsSnapshot(v)
	}
	return state
}

// LoadFromStore reads the persisted snapshot from store.Usage and backfills it into memory. Returns true if
// a non-empty state (schema matches) was loaded; false means no file or unusable, and the caller
// should go on with the one-time session replay backfill.
func (t *UsageTracker) LoadFromStore() (bool, error) {
	if t == nil || t.store == nil {
		return false, nil
	}
	state, err := t.store.Usage.Load()
	if err != nil {
		return false, err
	}
	if state == nil {
		return false, nil
	}
	t.applyState(*state)
	return true, nil
}

// SaveNow writes the current snapshot to disk immediately. Both the autoSaveLoop / Close paths write through it.
func (t *UsageTracker) SaveNow() error {
	if t == nil || t.store == nil {
		return nil
	}
	return t.store.Usage.Save(t.Snapshot())
}

// StartAutoSave starts a goroutine that listens on saveCh and persists with a debounce. Before ctx is done it
// flushes the last unsaved state. Close triggers the flush + exit by cancelling ctx.
func (t *UsageTracker) StartAutoSave(ctx context.Context) {
	if t == nil || t.store == nil {
		return
	}
	done := make(chan struct{})
	t.autoSaveMu.Lock()
	t.autoSaveDone = done
	t.autoSaveMu.Unlock()
	go func() {
		defer close(done)
		t.autoSaveLoop(ctx)
	}()
}

// WaitAutoSave waits for the final flush after cancellation to complete. Host.Close calls cancel first,
// then waits here, so that autoSaveLoop and the pre-exit SaveNow do not write the same snapshot concurrently.
func (t *UsageTracker) WaitAutoSave() {
	if t == nil {
		return
	}
	t.autoSaveMu.Lock()
	done := t.autoSaveDone
	t.autoSaveMu.Unlock()
	if done != nil {
		<-done
	}
}

// autoSaveLoop throttles the high-frequency dirty signal into one disk write per 500ms.
//
// Design note: 500ms is an empirical value — with 1-2 LLM turns per chapter, 1-2 disk writes are entirely acceptable;
// even if the user quits with ctrl+C too soon for the timer to fire, the ctx cancellation path flushes one last time.
// A real crash (OS kill -9) loses the accumulation of the last 0.5s — the upstream session jsonl is still
// the complete fact, and the next startup patches the difference by replaying from sessions/.
func (t *UsageTracker) autoSaveLoop(ctx context.Context) {
	const debounce = 500 * time.Millisecond
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	var pending bool
	flush := func() {
		if err := t.SaveNow(); err != nil {
			slog.Warn("saving usage failed", "module", "usage", "err", err)
		}
		pending = false
	}
	for {
		select {
		case <-ctx.Done():
			if pending {
				flush()
			}
			return
		case <-t.saveCh:
			if pending {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(debounce)
			pending = true
		case <-timer.C:
			flush()
		}
	}
}

// applyState writes the persisted snapshot back into memory. Called only at startup (after LoadFromStore / replay),
// when autoSaveLoop has not yet started and Record cannot fire concurrently, so the lock is not needed; mu is kept in case
// tests or a future change in call order introduce concurrency.
func (t *UsageTracker) applyState(state domain.UsageState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.overall = totalsFromState(state.Overall)
	if state.PerAgent == nil {
		t.perAgent = make(map[string]*agentTotals, 4)
	} else {
		t.perAgent = make(map[string]*agentTotals, len(state.PerAgent))
		for role, v := range state.PerAgent {
			tot := totalsFromState(v)
			t.perAgent[role] = &tot
		}
	}
	if state.PerModel == nil {
		t.perModel = make(map[string]*agentTotals, 4)
	} else {
		t.perModel = make(map[string]*agentTotals, len(state.PerModel))
		for model, v := range state.PerModel {
			tot := totalsFromState(v)
			t.perModel[model] = &tot
		}
	}
	t.missingAssistantUsage = state.MissingUsage
}

// totalsSnapshot copies the in-memory agentTotals into a persistable domain.AgentUsageTotals.
// The samples ring buffer is deliberately not carried out — see the UsageState comment.
func totalsSnapshot(t *agentTotals) domain.AgentUsageTotals {
	if t == nil {
		return domain.AgentUsageTotals{}
	}
	return domain.AgentUsageTotals{
		Input:        t.Input,
		Output:       t.Output,
		CacheRead:    t.CacheRead,
		CacheWrite:   t.CacheWrite,
		Cost:         t.Cost,
		Saved:        t.Saved,
		CacheCapable: t.CacheCapable,
		CacheBreaks:  t.CacheBreaks,
	}
}

// totalsFromState restores the persisted form into in-memory agentTotals. samples is left empty and restarts
// accumulating from 0 after a restart; after a few rounds of Record the "last N calls hit rate" semantics recover.
func totalsFromState(s domain.AgentUsageTotals) agentTotals {
	return agentTotals{
		Input:        s.Input,
		Output:       s.Output,
		CacheRead:    s.CacheRead,
		CacheWrite:   s.CacheWrite,
		Cost:         s.Cost,
		Saved:        s.Saved,
		CacheCapable: s.CacheCapable,
		CacheBreaks:  s.CacheBreaks,
	}
}

// AgentUsage is the cumulative usage snapshot of one agent (exposed to the UI).
type AgentUsage struct {
	Role            string
	Model           string
	Input           int
	Output          int
	CacheRead       int
	CacheWrite      int
	Cost            float64
	Saved           float64
	CacheCapable    bool
	RecentCacheRead int
	RecentInput     int
	RecentSamples   int
}

// PerAgent returns each role's cumulative usage. Results are sorted by CacheRead descending; roles that have consumed no tokens are skipped.
func (t *UsageTracker) PerAgent() []AgentUsage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]AgentUsage, 0, len(t.perAgent))
	for role, v := range t.perAgent {
		if v.Input == 0 && v.Output == 0 {
			continue
		}
		recentRead, recentInput := recentSums(v)
		out = append(out, AgentUsage{
			Role:            role,
			Input:           v.Input,
			Output:          v.Output,
			CacheRead:       v.CacheRead,
			CacheWrite:      v.CacheWrite,
			Cost:            v.Cost,
			Saved:           v.Saved,
			CacheCapable:    v.CacheCapable,
			RecentCacheRead: recentRead,
			RecentInput:     recentInput,
			RecentSamples:   len(v.samples),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CacheRead != out[j].CacheRead {
			return out[i].CacheRead > out[j].CacheRead
		}
		return out[i].Input > out[j].Input
	})
	return out
}

// PerModel returns each model's cumulative usage. Results are sorted by cost descending, then by input volume descending.
func (t *UsageTracker) PerModel() []AgentUsage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]AgentUsage, 0, len(t.perModel))
	for model, v := range t.perModel {
		if v.Input == 0 && v.Output == 0 {
			continue
		}
		out = append(out, AgentUsage{
			Model:        model,
			Input:        v.Input,
			Output:       v.Output,
			CacheRead:    v.CacheRead,
			CacheWrite:   v.CacheWrite,
			Cost:         v.Cost,
			Saved:        v.Saved,
			CacheCapable: v.CacheCapable,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Input > out[j].Input
	})
	return out
}

// resolveCost returns the cost / saved / capable of this message together.
//   - cost: on a registry hit, multiply and sum the 4 items; on a miss, fall back to the provider's own cost
//   - saved: > 0 only when the registry hits, CacheRead > 0, and InputCost > CacheReadCost
//   - capable: registry hit and the model's CacheReadCostPer1M > 0 → known to support prompt caching
//
// modelName prefers the one passed in by the caller (in replay it comes from _meta.model in the session jsonl).
func (t *UsageTracker) resolveCost(modelName string, u agentcore.Usage) (cost, saved float64, capable bool) {
	if entry, ok := models.DefaultRegistry().Resolve(modelName); ok {
		c := computeCost(u, *entry)
		s := computeSaved(u, *entry)
		canCache := entry.CacheReadCostPer1M > 0
		if c > 0 {
			return c, s, canCache
		}
	}
	if u.Cost != nil {
		return u.Cost.Total, 0, false
	}
	return 0, 0, false
}

// agentRoleName normalizes a subagent name to a role name.
// architect_short/mid/long all map to architect; anything else is returned as is.
func agentRoleName(agentName string) string {
	if strings.HasPrefix(agentName, "architect_") {
		return "architect"
	}
	return agentName
}

// computeCost computes the USD spend of this call from the $/1M tokens unit prices.
//
// Semantic premise (guaranteed uniformly by each litellm provider, see the Usage assembly points in anthropic.go / bedrock.go /
// openai.go / gemini.go / compat.go):
//
//	u.Input  = all input tokens, **including** CacheRead; excluding CacheWrite
//	u.Output = output tokens
//
// Therefore nonCachedInput = u.Input - u.CacheRead holds for every provider.
// The fallback branch is kept so that if some provider returns dirty data in future, it does not crash.
func computeCost(u agentcore.Usage, e models.ModelEntry) float64 {
	nonCachedInput := u.Input - u.CacheRead
	if nonCachedInput < 0 {
		nonCachedInput = u.Input
	}
	c := 0.0
	c += float64(nonCachedInput) * e.InputCostPer1M / 1_000_000
	c += float64(u.Output) * e.OutputCostPer1M / 1_000_000
	c += float64(u.CacheRead) * e.CacheReadCostPer1M / 1_000_000
	c += float64(u.CacheWrite) * e.CacheWriteCostPer1M / 1_000_000
	return c
}

// computeSaved estimates the USD saved by CacheRead hits relative to "billing at the normal input price".
// Note that the CacheWrite premium is not deducted — it is a necessary investment "to pave the way for later hits",
// and the real gain is recovered through later CacheRead accumulation.
func computeSaved(u agentcore.Usage, e models.ModelEntry) float64 {
	if u.CacheRead <= 0 || e.InputCostPer1M <= 0 {
		return 0
	}
	delta := e.InputCostPer1M - e.CacheReadCostPer1M
	if delta <= 0 {
		return 0
	}
	return float64(u.CacheRead) * delta / 1_000_000
}
