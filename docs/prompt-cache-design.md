# Prompt Cache Design: litellm / agentcore / ainovel Working Together in Three Layers

> This document is explanatory material: it describes how we designed end-to-end LLM prompt-cache support
> (prompt caching) across three cooperating repositories, covering the design rationale, a real troubleshooting case and the matching source locations.
>
> - **litellm** — the LLM gateway: protocol translation and capability declaration
> - **agentcore** — the Agent framework: cache placement and cache identity
> - **ainovel-cli** — the application layer: plugged in with one line of configuration (codebot likewise)

---

## 1. Why it is worth doing: the cost model and a real case

Agent system requests have a structural trait: **every request carries the entire history**. In a 30-round tool loop,
the request body of round 30 contains all the messages of the previous 29 rounds. Without caching, the same prefix bytes are billed over and over.

Cache pricing of the two major vendors (taking Anthropic as the example):

| Item | Relative to the normal input price |
|---|---|
| Cache write (5-minute TTL) | 1.25x |
| Cache write (1-hour TTL) | 2x |
| **Cache read** | **0.1x (saves 90%)** |

A real case: one 33-chapter long-form novel generation cost $58. Post-hoc analysis of `meta/usage.json` showed
an **overall cache hit rate of only 8.5%** (coordinator only 2.7%, architect 0). Comparing the usage sequence
(input vs cache_read) request by request located three root causes:

1. **tools byte jitter**: the Description/Schema of the subagent tool was rebuilt every round by iterating a Go map directly,
   in random order → the request body differed from the previous round from byte 0 → the prefix cache was invalidated entirely;
2. **No routing affinity**: OpenAI-family endpoints were not sent `prompt_cache_key`, so even byte-identical requests could be
   load-balanced to instances with no cache (hard evidence: of 33 sessions, only 12 of the byte-identical first requests hit);
3. **Zero breakpoints on the Claude family**: Anthropic caching is explicit, and no `cache_control` breakpoint = no cache at all.

These three root causes map respectively to the three pieces of design below: **prefix stability discipline**, **cache identity** and **breakpoint orchestration**.

---

## 2. Background: a mental model of the two cache protocols

### 2.1 OpenAI: automatic prefix caching (implicit)

- The server automatically caches prefixes of **≥1024 tokens**, with no client declaration needed;
- Hits grow in 128-token aligned increments;
- A request may carry `prompt_cache_key` (an official field) for **routing affinity**: requests with the same key
  land on the same cache shard as far as possible;
- `cached_tokens` in usage reports the hit volume; **cache writes are never reported** (`cache_write` being constantly 0
  is normal, not a bug).

### 2.2 Anthropic: explicit breakpoints (cache_control)

- The client places `cache_control` breakpoints on content blocks, and **a breakpoint covers everything before it**
  (the order is fixed as tools → system → messages);
- **At most 4 breakpoints** per request;
- Write price 1.25x (5m) / 2x (1h), read price 0.1x;
- `cache_control` **must not be placed on a thinking block** (it is rejected with a 400).

### 2.3 Common premise

Whether implicit or explicit, the cache recognizes only **byte-level prefix equality**. So the foundation of every design is one sentence:

> **Order the entire request from low to high change frequency: put the static parts first and the dynamic parts last,
> and never change a single byte of the history already sent.**

---

## 3. Overall architecture: a three-layer division of labor

```
┌────────────────────────────────────────────────────────┐
│ Application layer (ainovel-cli / codebot)              │
│  decides the "cache identity": book base + role name   │
│  integration cost = two lines of config per agent      │
├────────────────────────────────────────────────────────┤
│ agentcore (Agent framework)                            │
│  decides where breakpoints go, when the key derives:   │
│  system floor + rolling tip; spawn appends #seq;       │
│  gated by provider capability, dropped if unsupported  │
├────────────────────────────────────────────────────────┤
│ litellm (LLM gateway)                                  │
│  pure translation: cache_control ↔ vendor fields,      │
│  prompt_cache_key passthrough, Capabilities declaration│
│  makes no "whether to cache" decision                  │
└────────────────────────────────────────────────────────┘
```

The separation principle: **litellm only answers "what does this endpoint support", agentcore only answers "where do cache points go",
and the application layer only answers "what is the identity"**. Each layer is testable on its own, and when an application is swapped (codebot reuses the same
agentcore/litellm) the cache logic need not be rewritten.

---

## 4. Foundation: the three disciplines of prefix byte stability

The payoff of caching presupposes a stable prefix. Each of the three disciplines corresponds to a real incident.

### Discipline 1: tools serialization must be byte-deterministic

Incident: the `subagent` tool embedded the list of registered agents in its own Description/Schema, and that list came from
Go map iteration, so the order was random on every call, the tools bytes changed every round, and the coordinator hit rate was therefore only 2.7%.
(The Claude Code team was bitten by the same problem: their whole fleet once paid 10.2% extra in cache writes because of it.)

Fix (agentcore `subagent/subagent.go`):

```go
// sortedAgentNames returns registered agent names in deterministic order.
// Description and Schema are rebuilt on every LLM call; iterating the map
// directly would shuffle their bytes across requests and defeat provider
// prefix caching (tools serialize into the cached prompt prefix).
func (t *Tool) sortedAgentNames() []string {
	return slices.Sorted(maps.Keys(t.agents))
}
```

> The general form of the lesson: **any collection that goes into the request body must be sorted before serialization**. Go's map iteration
> randomization hides this bug very deeply: functionality is completely normal, and only the bill is abnormal.

### Discipline 2: history must be append-only (compaction must "commit")

Incident: the writer's context compaction strategy was a "projection" (temporarily rewriting the history view on every call without writing it back to the
baseline). Once past the threshold, **every round rewrote the entire prefix** → a full miss every round.

Fix: commit after projection (`CommitOnProject: true`), so the rewrite happens only once and the history then returns to
append-only until the threshold is crossed again.

> The general form: context compaction is a **planned one-time break** (reset the prefix, pay full price once),
> which is fine; what is unacceptable is **breaking every round**. Either do not compact, or solidify after compacting.

### Discipline 3: dynamic content goes at the tail

Things that change every round (the world-state envelope, per-round reminders, the latest tool results) may only be **appended at the tail of the messages**,
and never modify the middle retroactively. ainovel's `novel_context` envelope is designed as tail-append: it changes every chapter,
but its changing does not affect the cache of the hundreds of thousands of tokens before it.

---

## 5. Cache identity: one base per book, one name per role, one key per session

The `prompt_cache_key` of the OpenAI family solves a **routing problem**: if byte-identical requests are load-balanced to
different instances, they miss all the same. The design goal of the key is "requests of the same cache lineage always carry the same key".

Our three-level identity (ainovel `internal/agents/build.go`):

```go
// promptCacheBase derives a stable short hash from the book directory as the prompt-cache identity prefix: the same book
// shares a routing bucket across process restarts, and no local path is leaked to the provider. The caller appends the role suffix,
// and each subagent spawn appends "#seq" (one key per session).
func promptCacheBase(bookDir string) string {
	sum := sha256.Sum256([]byte(bookDir))
	return "nvl-" + hex.EncodeToString(sum[:6])
}
```

The application layer plugs in with just two lines per agent:

```go
writer := subagent.Config{
	// ...
	CacheLastMessage: "ephemeral",                // Claude breakpoint switch (see §6)
	PromptCacheKey:   cacheBase + "-writer",      // OpenAI routing identity (role level)
}
// the coordinator (top-level Agent) likewise:
agentcore.WithCacheLastMessage("ephemeral"),
agentcore.WithPromptCacheKey(cacheBase+"-coordinator"),
```

The third level (session level) is derived automatically by agentcore: every spawn of a new session is a new
cache lineage (agentcore `subagent/subagent.go`):

```go
runSeq := t.runSeq.Add(1)

// One conversation, one cache key: suffix the per-run sequence so each
// spawn forms its own cache lineage instead of piling every run of this
// agent into a single routing bucket.
promptCacheKey := cfg.PromptCacheKey
if promptCacheKey != "" {
	promptCacheKey = fmt.Sprintf("%s#%d", promptCacheKey, runSeq)
}
```

End form: `nvl-a1b2c3-writer#17` = this book, the writer role, the session of the 17th spawn.

> Why not one global key? Different sessions have different prefixes, and mixing them in one routing bucket dilutes the hit rate.
> Why no timestamp/random number? The key must be **stable across requests**, identical on every round within a session.

codebot's corresponding design: the key semantics = SessionID (switching session = switching lineage), a teammate appends its name
as a suffix, and when the host reuses the same Agent instance to switch sessions it calls `Agent.SetPromptCacheKey` to re-point the identity.

---

## 6. Claude breakpoint orchestration: floor + rolling tip

On Anthropic, no breakpoints = zero cache. Our budget allocation (limit of 4 breakpoints per request):

```
[tools][system ←breakpoint ① "floor"][...history messages...][latest message ←breakpoint ② "rolling tip"]
```

### 6.1 Floor: pin the static prefix

The system prompt is the largest static block. Give it a dedicated breakpoint to guarantee that **when a new session / the tail cache has been evicted,
the system+tools prefix is still read from cache** (agentcore `loop.go`):

```go
} else if agentCtx.SystemPrompt != "" {
	m := SystemMsg(agentCtx.SystemPrompt)
	if config.CacheLastMessage != "" {
		// Cache floor: pin the static system prompt with its own
		// breakpoint so a fresh session — or a turn whose tail entry was
		// evicted — still reads the system+tools prefix from cache.
		m.Metadata = map[string]any{"cache_control": config.CacheLastMessage}
	}
	prefix = append(prefix, m)
}
```

### 6.2 Rolling tip: advance the coverage every round

Place one breakpoint on the **last non-system message**. In a tool loop, every LLM call writes a cache entry
covering up to the latest tool_use+tool_result, and the next round reads it directly with no retransmission:

```go
// markLastMessageForCache returns a copy of messages with cache_control attached
// to the metadata of the last non-system message. System messages are skipped so
// trailing per-turn reminders (which change every turn) don't end up carrying
// the breakpoint.
func markLastMessageForCache(messages []Message, cacheControl string) []Message {
	idx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != RoleSystem {
			idx = i
			break
		}
	}
	// ...
}
```

Note that the trailing system reminder is skipped: it changes every round, and putting the breakpoint on it means writing, every round, a cache entry
that will never be reused.

### 6.3 Last-block semantics: one message burns only one breakpoint

The message-level `cache_control` semantics is "write a breakpoint after this message". When translated to block level it may only
land on the **last cacheable block**: tagging every block would burn through the budget of 4 breakpoints; and Anthropic
rejects a thinking block carrying `cache_control`, so we scan from the tail and skip reasoning
(agentcore `llm/litellm.go`):

```go
if cache != nil {
	// Anthropic rejects cache_control on thinking blocks — land the
	// breakpoint on the last cacheable block instead.
	for i := len(blocks) - 1; i >= 0; i-- {
		if _, isReasoning := blocks[i].(litellm.ReasoningBlock); isReasoning {
			continue
		}
		blocks[i] = withBlockCache(blocks[i], cache)
		break
	}
}
```

### 6.4 TTL pipeline

The config value is conventionally a `"type[:ttl]"` string, such as `"ephemeral"` (default 5m) or `"ephemeral:1h"`:

```go
func cacheControlFromMetadata(metadata map[string]any) *litellm.CacheControl {
	value, _ := metadata["cache_control"].(string)
	if value == "" {
		return nil
	}
	if typ, ttl, ok := strings.Cut(value, ":"); ok {
		return &litellm.CacheControl{Type: typ, TTL: ttl}
	}
	return &litellm.CacheControl{Type: value}
}
```

Whether to upgrade to 1h should be decided by data: the write price rises from 1.25x to 2x, so it is only worth it if the measured call interval often exceeds 5 minutes
(we measured a coordinator median interval of 172s and did not upgrade).

---

## 7. Safe sending: capability gating + official endpoint detection

### 7.1 Capability gating: unsupported fields do not leave the building

Each litellm provider validates `ProviderOptions` **strictly** (an unknown key is an immediate error), so
agentcore gates by the capability declaration before sending (agentcore `llm/litellm.go`):

```go
// Prompt-cache routing identity. Capability-gated: litellm providers
// validate provider options strictly, so an unsupported key must be
// dropped here rather than rejected there.
if callCfg.PromptCacheKey != "" && caps.Cache.PromptKey == litellm.SupportYes {
	req.ProviderOptions["prompt_cache_key"] = callCfg.PromptCacheKey
}
```

### 7.2 Official endpoint detection: the compatible ecosystem has no unknown-field contract

`prompt_cache_key` is an official OpenAI field, but the behavior of "OpenAI-compatible" endpoints has no uniform contract.
Online verification (2026-07):

- **Strict endpoints reject it outright**: Groq, Cerebras, Volcano Engine (火山引擎) and Fireworks return 400/422 for this field <!-- han:keep -->
  (Zed #36215 and OpenClaw #48155 both changed to conditional sending because of this);
- **Re-marshaling relays silently drop it**: on the non-passthrough path, one-api/new-api/sub2api parse the request body into
  a struct and re-marshal it, so unknown fields vanish without a trace (sent for nothing);
- **Lenient endpoints ignore it**: Ollama, the current vLLM, MiniMax.

So the capability declaration of the litellm openai provider is judged **dynamically** by BaseURL
(litellm `provider/openai/capabilities.go`):

```go
// promptCacheParamsSupport reports whether this endpoint is trusted to accept
// OpenAI's prompt cache params (prompt_cache_key / prompt_cache_retention).
// Only the official endpoint guarantees the field contract.
func (p *Provider) promptCacheParamsSupport() litellm.Support {
	if p.cfg.PromptCacheParams || isOfficialBaseURL(p.cfg.BaseURL) {
		return litellm.SupportYes
	}
	return litellm.SupportUnknown
}

func isOfficialBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.openai.com")
}
```

Official `api.openai.com` → `SupportYes` (send); a third-party BaseURL → `SupportUnknown`
(the gating of §7.1 automatically does not send, so **by default it never blows up any endpoint**); a user who has confirmed that their relay passes through unchanged
opts in explicitly in the provider config:

```jsonc
"my-relay": {
  "type": "openai",
  "base_url": "https://relay.example.com/v1",
  "extra": { "prompt_cache_params": true }   // I confirm this relay passes the request body through
}
```

> Why is the switch placed in litellm's capability layer rather than the application config layer? Because switching provider at runtime via `/model`
> swaps the client, and the capability declaration switches automatically along with the client; a decision made when the application is constructed cannot cover a runtime switch.

---

## 8. Observability: cache chain break detection

The cache is an "invisible feature": when broken it raises no error, it just gets more expensive. So observability is needed (borrowing from Claude Code's
promptCacheBreakDetection, as a lightweight version).

Definition of the judgment (ainovel `internal/host/usage.go`):

```go
// Within the same session (role+task): the prefix has not shortened, while the hit volume dropped >5% from last time and the drop is ≥2000 tokens
broke := prevPrefix > 0 && prefix >= prevPrefix &&
	float64(u.CacheRead) < float64(prevRead)*cacheBreakKeepRatio &&
	prevRead-u.CacheRead >= cacheBreakMinDropTokens
```

Four key design points, each corresponding to one class of false positive:

| Design | The false positive it prevents |
|---|---|
| **Dual thresholds** (relative 5% and absolute 2000) | A single relative threshold is drowned by noise on small prefixes; a single absolute threshold misses degradation on large prefixes |
| **Baseline follows the session (role+task)** | The detection dimension must align with the session granularity of `prompt_cache_key` (`#seq`); comparing across sessions by role alone gives a false positive when "the previous session was very short and the new session's first request has an even longer prefix" (a real gap caught by the Codex review) |
| **Shortened prefix = legitimate reset** | Context compaction is a planned break; the baseline is reset without an alert |
| **No detection on replay** | Replaying history at startup would turn stale breaks into fresh alerts |

When alerting, an attribution hint is given by the time interval: interval >1h → suspected 1h TTL expiry; >5m → suspected 5m TTL expiry;
very short → suspected server-side eviction / routing drift (**a relay polling upstream accounts is the most common cause**). The count is persisted to
`usage.json` and shown on the "chain breaks" row of the TUI cache panel.

---

## 9. The latch red line: the session-monotonic principle

A constitutional-level constraint on future features:

> **Everything that enters the cache prefix (system prompt, tools, thinking parameters, sampling parameters),
> once first computed within a session, must be frozen — better stale than a broken cache.**

Example: for a feature like "adjust the thinking intensity at runtime", if the new intensity took effect immediately on an in-progress session,
every adjustment would rewrite the prefix and invalidate the whole cache. The correct approach is for the new value to apply only to **newly spawned sessions**.
In the review of any "X adjustable at runtime" request, the first question is: is X in the cache prefix?

---

## 10. Common misjudgments and ceilings

1. **OpenAI's `cache_write` being constantly 0 is normal** — the API does not report write volume; do not investigate it as a bug.
2. **The relay ceiling**: if a relay polls several upstream accounts, a miss happens no matter how stable the client bytes are (the cache of upstream account A
   is invisible to account B). This explains the puzzle of "byte-identical requests hit only 12/33".
   **This is not a problem the client can solve** — the Claude Code team's data also shows that about 90% of the "break despite no client change"
   cases have server-side causes.
3. **Basis for verification**: the session JSONL contains neither the system prompt nor the full request body, so the **per-request usage sequence
   (input vs cache_read) is the gold standard for diagnosis**. A practical fingerprint: if the hit volume is pinned at exactly
   "the system prompt token count rounded down to a multiple of 128", only the system segment hits and the message segment misses entirely.
4. **Payoff accounting**: read price 0.1x and write price 1.25x mean a cache entry pays for itself once it is read just once.
   In a multi-round agent session a breakpoint is almost always net positive, so `CacheLastMessage` has no switch and is on by default.

---

## 11. Integration quick reference

**ainovel-cli** (built in): configure each agent with `CacheLastMessage: "ephemeral"` +
`PromptCacheKey: promptCacheBase(bookDir) + "-<role>"`; everything else is automatic.

**codebot** (built in): key = SessionID; on `Reset`/`SwitchSession`,
`agent.SetPromptCacheKey(newSessionID)`; a teammate uses `sessionID + "-" + name`.

The minimal checklist for **a new application integrating agentcore**:

```go
agentcore.NewAgent(
	agentcore.WithCacheLastMessage("ephemeral"),   // Claude breakpoints: floor + rolling tip
	agentcore.WithPromptCacheKey(stableIdentity),  // OpenAI routing: stable, unique per session
	// ...
)
```

Plus three self-check questions (corresponding to the three disciplines):

1. Is my tools serialization byte-deterministic? (Are all collections sorted?)
2. Is my history append-only? (Does compaction commit?)
3. Is everything that changes each round at the tail?

---

## 12. Checklist of lessons for learners

- The essence of cache optimization is **byte discipline**, not parameter tuning: first guarantee prefix stability, then talk about keys and breakpoints.
- Diagnosis always starts from the **per-request usage sequence**; do not guess from the code.
- Go map iteration randomization + request body serialization = the most insidious cache killer, which functional tests can never find.
- "OpenAI-compatible" is a marketing term, not a contract: before sending an official field to a third-party endpoint, first find primary evidence
  (source code / issues / fixes already landed in comparable clients); "it will generally be ignored" is a dangerous inference.
- With observability, avoid false positives first: the detection dimension must align with the granularity of the cache lineage; prefer a missed alert to a false one,
  otherwise alerts soon get ignored.
- The test of good layering: when a different application (codebot) integrates, not a single line of cache logic needs to be rewritten.

---

### Appendix: source index

| Topic | Location |
|---|---|
| Deterministic tools ordering | agentcore `subagent/subagent.go` `sortedAgentNames` |
| Session-level key derivation (#seq) | agentcore `subagent/subagent.go` `runAgent` |
| system floor + rolling tip | agentcore `loop.go` `callLLM` / `markLastMessageForCache` |
| Last-block breakpoint + skip thinking | agentcore `llm/litellm.go` `convertAgentBlocks` |
| TTL parsing ("ephemeral:1h") | agentcore `llm/litellm.go` `cacheControlFromMetadata` |
| Capability gating | agentcore `llm/litellm.go` `applyCallConfig` |
| Official endpoint detection + opt-in | litellm `provider/openai/capabilities.go` / `provider.go Config` |
| Cache identity (one base per book) | ainovel `internal/agents/build.go` `promptCacheBase` |
| Break detection | ainovel `internal/host/usage.go` `noteCacheBreak` |
| Architecture positioning | ainovel `docs/architecture.md` §6.6 |
