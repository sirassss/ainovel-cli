package bootstrap

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/voocel/agentcore/llm"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/models"
	"github.com/voocel/ainovel-cli/internal/notify"
	"github.com/voocel/ainovel-cli/internal/utils"
)

// DefaultContextWindow is the fallback window size used when a model is not registered in the registry.
const DefaultContextWindow = 200000

// CompactRatio is the relative threshold that triggers context compaction: compact when tokens >= window * CompactRatio.
// 0.85 is an empirical value that leaves 15% headroom for "the next prompt + large tool results", while also letting large-window
// models compact proactively at 85% instead of waiting until a nominal 1M window is full (the attention-decay zone).
//
// The compaction ratio is not exposed as user configuration; users only configure each model's real context_window.
const CompactRatio = 0.85

// MinCompactReserve is the lower bound of ReserveTokens. For small-window models (e.g. a 32k local qwen3:8b),
// a reserve computed at the 0.15 ratio is only 4800, yet a single commit_chapter tool response can take 5-8k
// and one chapter's content 8-15k - causing "over the limit again right after compacting". The 8000 floor guarantees half a round of buffer even in the worst case.
const MinCompactReserve = 8000

// CompactReserveTokens derives ReserveTokens back from CompactRatio and applies the MinCompactReserve floor:
//
//	threshold = window - reserve = window * CompactRatio
//	reserve   = max(MinCompactReserve, window * (1 - CompactRatio))
//
// Used for EngineConfig.ReserveTokens of agentcore.context.Engine.
func CompactReserveTokens(window int) int {
	if window <= 0 {
		return 0
	}
	reserve := window - int(float64(window)*CompactRatio)
	if reserve < MinCompactReserve {
		return MinCompactReserve
	}
	return reserve
}

// ProviderConfig defines the credentials of a single LLM provider.
type ProviderConfig struct {
	Type    string        `json:"type,omitempty"`     // API protocol type (openai/anthropic/gemini), specified for custom proxies
	API     string        `json:"api,omitempty"`      // OpenAI protocol endpoint: chat (default) / responses
	APIKey  string        `json:"api_key,omitempty"`  // API Key
	BaseURL string        `json:"base_url,omitempty"` // API Base URL
	Models  []ModelConfig `json:"models,omitempty"`   // Optional model list, shown when switching in the TUI
	// ExtraBody is extra parameters passed through on every request to this provider (such as temperature/top_p/min_p/
	// presence_penalty, or vendor-specific keys such as nvidia's chat_template_kwargs for enabling think).
	// OpenAI-compatible endpoints merge it verbatim into the request body (the extra_body convention); the values are the user's own responsibility.
	ExtraBody map[string]any `json:"extra_body,omitempty"`
	// Extra is passed through to the provider-level config (litellm.ProviderConfig.Extra), for client / transport-layer options such as HTTP
	// headers, user_agent and anthropic_beta.
	Extra map[string]any `json:"extra,omitempty"`
	// StreamIdleTimeout is the streaming idle watchdog: the stream is cut if no chunk is received for longer than this duration
	// (a Go duration string, e.g. "900s" / "15m"). Empty defaults to 5m - a reasonable upper bound for cloud services;
	// self-hosted slow inference such as LocalAI/ollama can take far longer than 5 minutes for the first chunk, so relax it per provider,
	// without weakening hang detection on other channels (#79).
	StreamIdleTimeout string `json:"stream_idle_timeout,omitempty"`
}

// ModelConfig describes a switchable model under a provider and its optional context window.
// For compatibility with old configs, it can be read from either a JSON string ("model-name") or an object;
// when written back it is always normalized to the object form.
type ModelConfig struct {
	Name          string `json:"name"`
	ContextWindow int    `json:"context_window,omitempty"`
	// JSONSchema is the tri-state declaration for native structured output (response_format json_schema):
	// unset = decide from the provider adapter's model-level capabilities; true = the user declares that this endpoint/model
	// supports it (a rejected request is surfaced as is, with no silent downgrade); false = force the prompt contract.
	// For custom proxies and aggregating gateways the capability is whatever the user declares; the program does not probe.
	JSONSchema *bool `json:"json_schema,omitempty"`
}

func (m *ModelConfig) UnmarshalJSON(data []byte) error {
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		m.Name = legacy
		m.ContextWindow = 0
		m.JSONSchema = nil
		return nil
	}
	type modelConfigAlias ModelConfig
	var decoded modelConfigAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("model config must be a string or object: %w", err)
	}
	*m = ModelConfig(decoded)
	return nil
}

// ModelConfig returns the explicit config of the given model.
func (pc ProviderConfig) ModelConfig(name string) (ModelConfig, bool) {
	name = strings.TrimSpace(name)
	for _, model := range pc.Models {
		if strings.TrimSpace(model.Name) == name {
			return model, true
		}
	}
	return ModelConfig{}, false
}

// ModelJSONSchema returns the model's json_schema tri-state declaration; when the model is not listed in models or is unset,
// it returns nil (decide by adapter capability).
func (c Config) ModelJSONSchema(provider, model string) *bool {
	if pc, ok := c.Providers[provider]; ok {
		if mc, ok := pc.ModelConfig(model); ok {
			return mc.JSONSchema
		}
	}
	return nil
}

// defaultStreamIdleTimeout: in long-output + long-context scenarios, when a reasoning-aware provider
// (mimo / deepseek-r1, etc.) does not stream reasoning deltas from the server during the thinking phase,
// the SSE stream stays silent for the whole stretch. litellm's default watchdog is 2 minutes, which often
// kills 8000-character chapter writes by mistake; 5 minutes covers the vast majority of measured cases (see the plan->draft thinking-duration statistics in tasks/todo.md).
const defaultStreamIdleTimeout = 5 * time.Minute

// StreamIdleTimeoutValue parses this provider's streaming idle timeout; empty falls back to the default.
func (pc ProviderConfig) StreamIdleTimeoutValue() (time.Duration, error) {
	s := strings.TrimSpace(pc.StreamIdleTimeout)
	if s == "" {
		return defaultStreamIdleTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use Go duration like \"900s\" / \"15m\")", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %q", s)
	}
	return d, nil
}

// RequiresAPIKey returns whether this provider must have api_key configured explicitly.
// Conventions:
// 1. ollama / bedrock may have no key;
// 2. a config with an explicit Type is treated as a custom proxy and may have no key;
// 3. other providers require a key by default, keeping conservative validation for official hosted APIs.
func (pc ProviderConfig) RequiresAPIKey(name string) bool {
	switch name {
	case "ollama", "bedrock":
		return false
	}
	return pc.Type == ""
}

// ProviderType returns the effective API protocol type.
// An explicit Type takes priority; otherwise the provider name itself must already be in the litellm registry.
func (pc ProviderConfig) ProviderType(name string) (string, error) {
	if pc.Type != "" {
		return pc.Type, nil
	}
	if llm.IsProviderRegistered(name) {
		return name, nil
	}
	return "", fmt.Errorf("provider %q thiếu type và không nằm trong danh sách provider litellm đã biết: %w", name, errs.ErrConfig)
}

// ModelRef denotes a provider/model combination.
type ModelRef struct {
	Provider string `json:"provider"` // Provider name (a key in the Providers map)
	Model    string `json:"model"`    // Model name (passed through as is, without any parsing)
}

// RoleConfig defines the model override of a single role.
type RoleConfig struct {
	Provider  string     `json:"provider"`            // Primary provider name (a key in the Providers map)
	Model     string     `json:"model"`               // Primary model name (passed through as is, without any parsing)
	Fallbacks []ModelRef `json:"fallbacks,omitempty"` // Explicit fallback provider/model list
	// ReasoningEffort is this role's reasoning effort (off/low/medium/high/xhigh/max); empty = inherit the top-level default.
	// Validated and applied by agents.ParseThinkingLevel; an out-of-range value is treated as empty.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// knownRoles are the supported configurable role names. The Arbiter currently has no role-level config
// and uniformly uses the top-level default model (host.arbiterModel uses models.Default).
// import_* are the model-tier knobs of the import semantic functions (docs/import-pipeline.md §13.1):
// when unset they fall to architect; once configured, the more mechanical functions can be pointed at a cheaper tier.
var knownRoles = map[string]bool{
	"architect":         true,
	"writer":            true,
	"editor":            true,
	"import_segment":    true,
	"import_analyze":    true,
	"import_synthesize": true,
}

// Config is the novel application configuration.
type Config struct {
	// Runtime fields (not serialized to JSON)
	OutputDir string `json:"-"` // Output root directory

	// Default LLM configuration
	Provider  string `json:"provider"` // Default provider (a key in the Providers map)
	ModelName string `json:"model"`    // Default model name
	// ReasoningEffort is the top-level default reasoning effort (off/low/medium/high/xhigh/max); empty = no override (keep the model/provider default).
	// A role falls back to this value when it has no reasoning_effort configured of its own.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Provider credential store
	Providers map[string]ProviderConfig `json:"providers,omitempty"`

	// Role-level model overrides
	Roles map[string]RoleConfig `json:"roles,omitempty"`

	// Writing parameters
	Style    string `json:"style,omitempty"`
	Language string `json:"language,omitempty"` // Novel writing language: "vi" (Vietnamese, the default) or "zh" (Chinese)

	// ContextWindow is the legacy global context window, kept for compatibility.
	ContextWindow int `json:"context_window,omitempty"`

	// Budget is the cost budget policy for a single book; it is enabled only when book_usd > 0.
	Budget BudgetConfig `json:"budget,omitzero"`

	// Notify is the unattended alert configuration; enabled by default (the system channel is the fallback).
	Notify NotifyConfig `json:"notify,omitzero"`
}

// BudgetConfig is the user's policy declaration for a single book's wallet. Stopping when the line is crossed is equivalent to the user
// manually Aborting at that moment - the Host only carries it out on their behalf and does not evaluate model behavior (architecture §10, the constitutional boundary).
type BudgetConfig struct {
	BookUSD   float64 `json:"book_usd,omitempty"`   // Required to enable; 0 / unset = unlimited
	WarnRatio float64 `json:"warn_ratio,omitempty"` // Alert watermark, default 0.8
	HardStop  bool    `json:"hard_stop,omitempty"`  // true = stop immediately when the line is crossed; by default wait until the current sub-agent task finishes
}

// Enabled returns whether the budget policy is enabled.
func (b BudgetConfig) Enabled() bool { return b.BookUSD > 0 }

// NotifyConfig is the unattended alert channel configuration.
type NotifyConfig struct {
	Enabled *bool    `json:"enabled,omitempty"` // Defaults to true (the system channel works with zero configuration)
	Command string   `json:"command,omitempty"` // Optional; once set it replaces the system channel (mobile push goes here)
	Events  []string `json:"events,omitempty"`  // Optional, filtered by notify.Kinds; all enabled by default
}

// IsEnabled returns whether alerts are enabled (default true).
func (n NotifyConfig) IsEnabled() bool { return n.Enabled == nil || *n.Enabled }

// ValidateBase validates the base configuration.
func (c *Config) ValidateBase() error {
	if err := validateConfigText("provider", c.Provider); err != nil {
		return err
	}
	if err := validateConfigText("model", c.ModelName); err != nil {
		return err
	}

	if c.Provider == "" {
		return fmt.Errorf("provider is required: %w", errs.ErrConfig)
	}
	if c.ModelName == "" {
		return fmt.Errorf("model is required: %w", errs.ErrConfig)
	}

	// The default provider must have credentials
	pc, ok := c.Providers[c.Provider]
	if !ok {
		return fmt.Errorf("provider %q chưa cấu hình thông tin xác thực trong providers; nếu đã ghi đè provider trong ./.ainovel/config.json thì cũng phải khai báo providers.%s (gồm api_key/base_url), không thể chỉ đổi provider ở cấp cao nhất: %w", c.Provider, c.Provider, errs.ErrConfig)
	}
	if pc.RequiresAPIKey(c.Provider) && pc.APIKey == "" {
		return fmt.Errorf("provider %q has no api_key configured: %w", c.Provider, errs.ErrConfig)
	}
	if err := validateProviderConfigText(c.Provider, pc); err != nil {
		return err
	}
	if err := c.validateProviderAPI("default", c.Provider, pc); err != nil {
		return err
	}
	for name, provider := range c.Providers {
		if err := validateConfigText("provider name", name); err != nil {
			return err
		}
		if err := validateProviderConfigText(name, provider); err != nil {
			return err
		}
		if err := c.validateProviderAPI(fmt.Sprintf("provider %q", name), name, provider); err != nil {
			return err
		}
	}

	// Validate role overrides
	for role, rc := range c.Roles {
		if err := validateConfigText("role name", role); err != nil {
			return err
		}
		if err := validateConfigText(fmt.Sprintf("role %q provider", role), rc.Provider); err != nil {
			return err
		}
		if err := validateConfigText(fmt.Sprintf("role %q model", role), rc.Model); err != nil {
			return err
		}
		if !knownRoles[role] {
			return fmt.Errorf("unknown role %q in roles config (valid: architect/writer/editor/import_segment/import_analyze/import_synthesize): %w", role, errs.ErrConfig)
		}
		if rc.Provider == "" || rc.Model == "" {
			return fmt.Errorf("role %q must have both provider and model: %w", role, errs.ErrConfig)
		}
		if err := c.validateModelRef(
			fmt.Sprintf("role %q", role),
			ModelRef{Provider: rc.Provider, Model: rc.Model},
		); err != nil {
			return err
		}
		for i, fallback := range rc.Fallbacks {
			if err := validateConfigText(fmt.Sprintf("role %q fallback[%d] provider", role, i), fallback.Provider); err != nil {
				return err
			}
			if err := validateConfigText(fmt.Sprintf("role %q fallback[%d] model", role, i), fallback.Model); err != nil {
				return err
			}
			if err := c.validateModelRef(
				fmt.Sprintf("role %q fallback[%d]", role, i),
				fallback,
			); err != nil {
				return err
			}
		}
	}

	// Validate the budget policy
	if c.Budget.BookUSD < 0 {
		return fmt.Errorf("budget.book_usd must be >= 0: %w", errs.ErrConfig)
	}
	if c.Budget.Enabled() && (c.Budget.WarnRatio <= 0 || c.Budget.WarnRatio >= 1) {
		return fmt.Errorf("budget.warn_ratio must be in (0, 1): %w", errs.ErrConfig)
	}

	// Validate the alert configuration
	if err := validateConfigText("notify.command", c.Notify.Command); err != nil {
		return err
	}
	for _, ev := range c.Notify.Events {
		if !notify.IsKnownKind(ev) {
			return fmt.Errorf("unknown notify event %q (valid: %s): %w", ev, strings.Join(notify.Kinds(), "/"), errs.ErrConfig)
		}
	}

	return nil
}

func validateProviderConfigText(name string, pc ProviderConfig) error {
	fields := []struct {
		label string
		value string
	}{
		{label: fmt.Sprintf("provider %q type", name), value: pc.Type},
		{label: fmt.Sprintf("provider %q api", name), value: pc.API},
		{label: fmt.Sprintf("provider %q api_key", name), value: pc.APIKey},
		{label: fmt.Sprintf("provider %q base_url", name), value: pc.BaseURL},
	}
	for _, field := range fields {
		if err := validateConfigText(field.label, field.value); err != nil {
			return err
		}
	}
	seenModels := make(map[string]bool, len(pc.Models))
	for i, model := range pc.Models {
		modelName := strings.TrimSpace(model.Name)
		if err := validateConfigText(fmt.Sprintf("provider %q models[%d].name", name, i), model.Name); err != nil {
			return err
		}
		if modelName == "" {
			return fmt.Errorf("provider %q models[%d].name is required: %w", name, i, errs.ErrConfig)
		}
		if seenModels[modelName] {
			return fmt.Errorf("provider %q has duplicate model %q: %w", name, modelName, errs.ErrConfig)
		}
		seenModels[modelName] = true
		if model.ContextWindow < 0 {
			return fmt.Errorf("provider %q model %q context_window must be >= 0: %w", name, modelName, errs.ErrConfig)
		}
	}
	switch pc.API {
	case "", "chat", "responses":
	default:
		return fmt.Errorf("provider %q api must be chat or responses: %w", name, errs.ErrConfig)
	}
	if _, err := pc.StreamIdleTimeoutValue(); err != nil {
		return fmt.Errorf("provider %q stream_idle_timeout: %w: %w", name, err, errs.ErrConfig)
	}
	return nil
}

func validateConfigText(name, value string) error {
	if utils.ContainsControl(value) {
		return fmt.Errorf("%s contains control character: %w", name, errs.ErrConfig)
	}
	return nil
}

// DefaultProviderConfig returns the credential config of the default provider.
func (c *Config) DefaultProviderConfig() ProviderConfig {
	if c.Providers == nil {
		return ProviderConfig{}
	}
	return c.Providers[c.Provider]
}

// FillDefaults fills in default values.
func (c *Config) FillDefaults() {
	if c.OutputDir == "" {
		c.OutputDir = filepath.Join("output", "novel")
	}
	if c.Providers == nil {
		c.Providers = make(map[string]ProviderConfig)
	}
	if c.Roles == nil {
		c.Roles = make(map[string]RoleConfig)
	}
	if c.Style == "" {
		c.Style = "default"
	}
	if c.Language == "" {
		c.Language = "vi"
	} else {
		c.Language = strings.ToLower(strings.TrimSpace(c.Language))
	}
	if c.Budget.Enabled() && c.Budget.WarnRatio == 0 {
		c.Budget.WarnRatio = 0.8
	}
}

// NormalizedLanguage returns the normalized novel content language ("vi" or "zh").
func (c Config) NormalizedLanguage() string {
	lang := strings.ToLower(strings.TrimSpace(c.Language))
	if lang == "zh" || lang == "chinese" || lang == "cn" {
		return "zh"
	}
	return "vi"
}

// ContextWindowSource marks where the window value came from, for logging / diagnostics.
type ContextWindowSource string

const (
	CtxWindowModelConfig ContextWindowSource = "model_config" // Explicitly specified by the provider's model entry
	CtxWindowConfig      ContextWindowSource = "config"       // Explicitly specified by the legacy top-level context_window
	CtxWindowRegistry    ContextWindowSource = "registry"     // OpenRouter baseline hit
	CtxWindowDefault     ContextWindowSource = "default"      // Fallback (custom proxy / unknown model)
)

// ResolveContextWindow resolves the effective window used for context compaction, in priority order:
//  1. providers.<provider>.models[].context_window
//  2. the legacy top-level ContextWindow (compatible with existing configs)
//  3. models.DefaultRegistry lookup by model name (OpenRouter baseline + 24h refresh)
//  4. fallback DefaultContextWindow (custom proxy / unknown model)
//
// Note: the return value is used only for compaction-threshold calculation and does not shrink the real request length the LLM API can accept.
func (c Config) ResolveContextWindow(provider, modelName string) (int, ContextWindowSource) {
	if pc, ok := c.Providers[strings.TrimSpace(provider)]; ok {
		if model, found := pc.ModelConfig(modelName); found && model.ContextWindow > 0 {
			return model.ContextWindow, CtxWindowModelConfig
		}
	}
	if c.ContextWindow > 0 {
		return c.ContextWindow, CtxWindowConfig
	}
	if rw := models.DefaultRegistry().ResolveContextWindow(modelName); rw > 0 {
		return rw, CtxWindowRegistry
	}
	return DefaultContextWindow, CtxWindowDefault
}

// ResolveReasoningEffort returns the effective raw reasoning-effort string for a role (off/low/medium/high/xhigh/max, or empty).
// Priority: role-level Roles[role].ReasoningEffort -> top-level default ReasoningEffort -> "" (no override, keep the model/provider default).
// When role is empty or "default" the top-level default is used directly. The validity of the value is guarded by agents.ParseThinkingLevel.
func (c Config) ResolveReasoningEffort(role string) string {
	if role != "" && role != "default" {
		if rc, ok := c.Roles[role]; ok && rc.ReasoningEffort != "" {
			return rc.ReasoningEffort
		}
	}
	return c.ReasoningEffort
}

// LogContextWindowChoice logs the window decision for a role. When source=default it emits a Warn saying
// the model was not found in the registry (and OpenRouter does not list it either), so later context compaction will trigger on the fallback window
// - if the model's real window is larger, specify it explicitly with context_window in the config file, to avoid being compacted too early and losing history.
func LogContextWindowChoice(role, model string, window int, source ContextWindowSource) {
	attrs := []any{"module", "context", "role", role, "model", model, "window", window, "source", source}
	switch source {
	case CtxWindowModelConfig:
		slog.Info("context window (from provider model config)", attrs...)
	case CtxWindowDefault:
		slog.Warn("model not recognized, using fallback window (can be set explicitly at providers.<name>.models[].context_window)", attrs...)
	case CtxWindowConfig:
		slog.Info("context window (from context_window in config file)", attrs...)
	default:
		slog.Info("context window", attrs...)
	}
}

// CandidateModels returns the list of models that can be switched to under a provider.
// Models explicitly declared by the provider take priority; models of that provider that already appear in the current config are added as well.
func (c Config) CandidateModels(provider string) []string {
	if provider == "" {
		return nil
	}

	seen := make(map[string]bool)
	models := make([]string, 0, 4)
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			return
		}
		seen[model] = true
		models = append(models, model)
	}

	if pc, ok := c.Providers[provider]; ok {
		for _, model := range pc.Models {
			add(model.Name)
		}
	}
	if c.Provider == provider {
		add(c.ModelName)
	}
	for _, rc := range c.Roles {
		if rc.Provider == provider {
			add(rc.Model)
		}
		for _, fallback := range rc.Fallbacks {
			if fallback.Provider == provider {
				add(fallback.Model)
			}
		}
	}
	return models
}

func (c Config) validateModelRef(owner string, ref ModelRef) error {
	if ref.Provider == "" || ref.Model == "" {
		return fmt.Errorf("%s must have both provider and model: %w", owner, errs.ErrConfig)
	}

	pc, ok := c.Providers[ref.Provider]
	if !ok {
		return fmt.Errorf("%s references provider %q which is not configured: %w", owner, ref.Provider, errs.ErrConfig)
	}
	if pc.RequiresAPIKey(ref.Provider) && pc.APIKey == "" {
		return fmt.Errorf("%s references provider %q which has no api_key: %w", owner, ref.Provider, errs.ErrConfig)
	}
	if err := c.validateProviderAPI(owner, ref.Provider, pc); err != nil {
		return err
	}
	return nil
}

func (c Config) validateProviderAPI(owner, providerName string, pc ProviderConfig) error {
	if pc.API == "" {
		return nil
	}
	providerType, err := pc.ProviderType(providerName)
	if err != nil {
		return fmt.Errorf("%s provider %q: cấu hình api không phân tích được loại giao thức: %w", owner, providerName, err)
	}
	if strings.ToLower(strings.TrimSpace(providerType)) != "openai" {
		return fmt.Errorf("%s provider %q: api chỉ hỗ trợ provider theo giao thức OpenAI: %w", owner, providerName, errs.ErrConfig)
	}
	return nil
}
