package domain

import "time"

// UsageSchemaVersion is the compatibility version number of meta/usage.json.
// If the semantics of the AgentUsageTotals fields change in the future, increment this value; when UsageStore.Load sees a different version it should ignore the file and trigger a replay rebuild.
const UsageSchemaVersion = 2

// UsageState is a persistable snapshot of cumulative token / cost usage.
// It is maintained in memory by UsageTracker and periodically written to meta/usage.json with a debounce.
//
// Note: the sliding-window samples inside UsageTracker ("hit rate of the last N calls") are **not persisted** -
// they serve only short-term UI diagnostics, and after a process restart the semantics recover once a few rounds have accumulated from empty.
// MissingAssistantUsage stays persisted, since accumulating across restarts has more diagnostic value.
type UsageState struct {
	Schema       int                         `json:"schema"`
	UpdatedAt    time.Time                   `json:"updated_at"`
	Overall      AgentUsageTotals            `json:"overall"`
	PerAgent     map[string]AgentUsageTotals `json:"per_agent"`
	PerModel     map[string]AgentUsageTotals `json:"per_model,omitempty"`
	MissingUsage int                         `json:"missing_assistant_usage"`
}

// AgentUsageTotals is the persistable form of the cumulative counts for a single role (or overall).
type AgentUsageTotals struct {
	Input        int     `json:"input"`
	Output       int     `json:"output"`
	CacheRead    int     `json:"cache_read"`
	CacheWrite   int     `json:"cache_write"`
	Cost         float64 `json:"cost_usd"`
	Saved        float64 `json:"saved_usd"`
	CacheCapable bool    `json:"cache_capable"`
	// CacheBreaks is the number of cache-chain breaks detected live (the prefix did not shrink but the hit rate dropped sharply).
	// Accumulated only on the live path; session replay does not replay the detection.
	CacheBreaks int `json:"cache_breaks,omitempty"`
}
