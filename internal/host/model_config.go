package host

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

type APIKeyAction string

const (
	APIKeyKeep    APIKeyAction = "keep"
	APIKeyReplace APIKeyAction = "replace"
	APIKeyClear   APIKeyAction = "clear"
)

// ProviderSnapshot is the redacted provider configuration for the TUI.
type ProviderSnapshot struct {
	Name           string
	Type           string
	API            string
	BaseURL        string
	Models         []bootstrap.ModelConfig
	HasAPIKey      bool
	APIKeyHint     string
	RequiresAPIKey bool
}

type ModelConfigurationSnapshot struct {
	Providers       []ProviderSnapshot
	DefaultProvider string
	DefaultModel    string
	ConfigPath      string
	References      map[string][]string
}

func (s ModelConfigurationSnapshot) ReferencesFor(provider, model string) []string {
	return append([]string(nil), s.References[modelReferenceKey(provider, model)]...)
}

// ModelConfigurationDraft is a single provider configuration draft that /config submits to the Host.
// It describes only that provider's definition (protocol/credentials/model library), not "which one is currently in use"; switching belongs to /model.
type ModelConfigurationDraft struct {
	Provider     string
	Type         string
	API          string
	BaseURL      string
	Models       []bootstrap.ModelConfig
	Renames      []ModelRename
	APIKeyAction APIKeyAction
	APIKey       string
}

// ModelRename describes an ID change of the same model configuration. It is not a guess of "delete the old, add a new one";
// the Host migrates default, role and fallback references only when the TUI explicitly submits this relation.
type ModelRename struct {
	From string
	To   string
}

type ConfiguredModel struct {
	Name          string
	ContextWindow int
	ContextSource bootstrap.ContextWindowSource
}

func modelReferenceKey(provider, model string) string {
	return strings.TrimSpace(provider) + "\x00" + strings.TrimSpace(model)
}

// MaskAPIKey keeps only enough leading and trailing fragments to identify the credential; short credentials are hidden entirely.
// The TUI receives only this result and never holds the full API Key from the configuration.
func MaskAPIKey(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) < 16 {
		return "******"
	}
	return string(runes[:4]) + "******" + string(runes[len(runes)-4:])
}

// ModelConfiguration returns the redacted configuration, writable targets and model references, and never exposes the existing API Key.
func (h *Host) ModelConfiguration() ModelConfigurationSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	providers := make([]ProviderSnapshot, 0, len(h.cfg.Providers))
	for name, pc := range h.cfg.Providers {
		providers = append(providers, ProviderSnapshot{
			Name: name, Type: pc.Type, API: pc.API, BaseURL: pc.BaseURL,
			Models:    modelConfigurations(h.cfg, name, pc),
			HasAPIKey: pc.APIKey != "", APIKeyHint: MaskAPIKey(pc.APIKey),
			RequiresAPIKey: pc.RequiresAPIKey(name),
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })

	refs := make(map[string][]string)
	refs[modelReferenceKey(h.cfg.Provider, h.cfg.ModelName)] = append(
		refs[modelReferenceKey(h.cfg.Provider, h.cfg.ModelName)], "default")
	for role, rc := range h.cfg.Roles {
		key := modelReferenceKey(rc.Provider, rc.Model)
		refs[key] = append(refs[key], role)
		for i, fallback := range rc.Fallbacks {
			key = modelReferenceKey(fallback.Provider, fallback.Model)
			refs[key] = append(refs[key], fmt.Sprintf("%s fallback[%d]", role, i))
		}
	}
	for key := range refs {
		sort.Strings(refs[key])
	}

	return ModelConfigurationSnapshot{
		Providers: providers, DefaultProvider: h.cfg.Provider, DefaultModel: h.cfg.ModelName,
		ConfigPath: h.configPath, References: refs,
	}
}

func modelConfigurations(cfg bootstrap.Config, provider string, pc bootstrap.ProviderConfig) []bootstrap.ModelConfig {
	models := make([]bootstrap.ModelConfig, 0)
	for _, modelName := range cfg.CandidateModels(provider) {
		model, ok := pc.ModelConfig(modelName)
		if !ok {
			model = bootstrap.ModelConfig{Name: modelName}
		}
		models = append(models, model)
	}
	return models
}

func (h *Host) ConfiguredModelOptions(provider string) []ConfiguredModel {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := h.cfg.CandidateModels(provider)
	out := make([]ConfiguredModel, 0, len(names))
	for _, name := range names {
		window, source := h.cfg.ResolveContextWindow(provider, name)
		out = append(out, ConfiguredModel{Name: name, ContextWindow: window, ContextSource: source})
	}
	return out
}

type preparedProviderDraft struct {
	draft     ModelConfigurationDraft
	candidate bootstrap.Config
	provider  bootstrap.ProviderConfig
	oldModels []bootstrap.ModelConfig
}

// prepareProviderDraftLocked normalizes the TUI draft and merges it into a copy of the configuration; saving and connection testing share this one validation chain.
func (h *Host) prepareProviderDraftLocked(draft ModelConfigurationDraft) (preparedProviderDraft, error) {
	draft.Provider = strings.TrimSpace(draft.Provider)
	draft.Type = strings.ToLower(strings.TrimSpace(draft.Type))
	draft.API = strings.ToLower(strings.TrimSpace(draft.API))
	draft.BaseURL = strings.TrimSpace(draft.BaseURL)
	draft.APIKey = strings.TrimSpace(draft.APIKey)
	if draft.Provider == "" {
		return preparedProviderDraft{}, fmt.Errorf("provider không được để trống")
	}
	if len(draft.Models) == 0 {
		return preparedProviderDraft{}, fmt.Errorf("Hãy cấu hình ít nhất một model")
	}

	candidate := bootstrap.CloneConfig(h.cfg)
	pc := candidate.Providers[draft.Provider]
	oldModels := modelConfigurations(candidate, draft.Provider, pc)
	pc.Type = draft.Type
	pc.API = draft.API
	pc.BaseURL = draft.BaseURL
	configuredModels := make([]bootstrap.ModelConfig, 0, len(draft.Models))
	seen := make(map[string]bool, len(draft.Models))
	for _, model := range draft.Models {
		model.Name = strings.TrimSpace(model.Name)
		if model.Name == "" {
			return preparedProviderDraft{}, fmt.Errorf("Tên model không được để trống")
		}
		if model.ContextWindow < 0 {
			return preparedProviderDraft{}, fmt.Errorf("Context window của model %q không được âm", model.Name)
		}
		if seen[model.Name] {
			return preparedProviderDraft{}, fmt.Errorf("Model %q bị trùng", model.Name)
		}
		seen[model.Name] = true
		configuredModels = append(configuredModels, model)
	}
	pc.Models = configuredModels

	switch draft.APIKeyAction {
	case "", APIKeyKeep:
		// Keep the existing values in the candidate configuration; they are naturally empty when adding a new provider.
	case APIKeyReplace:
		pc.APIKey = draft.APIKey
	case APIKeyClear:
		pc.APIKey = ""
	default:
		return preparedProviderDraft{}, fmt.Errorf("Thao tác API key không xác định %q", draft.APIKeyAction)
	}
	if pc.RequiresAPIKey(draft.Provider) && pc.APIKey == "" {
		return preparedProviderDraft{}, fmt.Errorf("Provider %q bắt buộc phải cấu hình API key", draft.Provider)
	}

	if candidate.Providers == nil {
		candidate.Providers = make(map[string]bootstrap.ProviderConfig)
	}
	candidate.Providers[draft.Provider] = pc
	return preparedProviderDraft{draft: draft, candidate: candidate, provider: pc, oldModels: oldModels}, nil
}

// ConfigureModels validates, persists and hot-applies the model library of one provider.
func (h *Host) ConfigureModels(draft ModelConfigurationDraft) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	preparedDraft, err := h.prepareProviderDraftLocked(draft)
	if err != nil {
		return err
	}
	draft = preparedDraft.draft
	candidate := preparedDraft.candidate
	pc := preparedDraft.provider
	renames, err := validateModelRenames(draft.Renames, preparedDraft.oldModels, pc.Models)
	if err != nil {
		return err
	}
	renameModelReferences(&candidate, draft.Provider, renames)

	newNames := make(map[string]bool, len(pc.Models))
	for _, model := range pc.Models {
		newNames[model.Name] = true
	}
	// Check references before deleting a model: a model pointed to by the top-level default or any role/fallback cannot be deleted;
	// the user must switch away in /model first, since /config no longer switches the default on their behalf.
	for _, old := range preparedDraft.oldModels {
		if newNames[old.Name] {
			continue
		}
		if _, renamed := renames[old.Name]; renamed {
			continue
		}
		if refs := h.modelReferencesLocked(draft.Provider, old.Name); len(refs) > 0 {
			return fmt.Errorf("Model %q vẫn đang được %s tham chiếu, hãy chuyển trong /model rồi mới xóa", old.Name, strings.Join(refs, ", "))
		}
	}

	// A plain edit does not change "which one is currently in use"; an explicit rename migrates only the reference identity of the same model.
	if err := candidate.ValidateBase(); err != nil {
		return err
	}
	prepared, err := bootstrap.NewModelSet(candidate)
	if err != nil {
		return fmt.Errorf("Không tạo được client model: %w", err)
	}

	if h.configPath == "" {
		return fmt.Errorf("Không xác định được đường dẫn tệp cấu hình")
	}
	if err := h.saveModelConfigurationLocked(candidate, draft.Provider, pc, len(renames) > 0); err != nil {
		return fmt.Errorf("Không lưu được cấu hình: %w", err)
	}

	h.models.ApplyPrepared(prepared)
	h.cfg = candidate
	// After the model client is rebuilt, re-issue the reasoning effort: applyThinkingLocked clamps the effective value to each role's new model capability,
	// while the stored effort intent stays unchanged.
	h.applyThinkingLocked("default")
	summary := fmt.Sprintf("Đã lưu cấu hình provider: %s → %s", draft.Provider, h.configPath)
	if draft.Provider != h.cfg.Provider {
		summary += "; dùng /model để chuyển"
	}
	h.emitEvent(Event{
		Time: time.Now(), Category: "SYSTEM", Level: "info",
		Summary: summary,
	})
	return nil
}

func validateModelRenames(requested []ModelRename, oldModels, newModels []bootstrap.ModelConfig) (map[string]string, error) {
	oldNames := make(map[string]bool, len(oldModels))
	newNames := make(map[string]bool, len(newModels))
	for _, model := range oldModels {
		oldNames[model.Name] = true
	}
	for _, model := range newModels {
		newNames[model.Name] = true
	}
	renames := make(map[string]string, len(requested))
	targets := make(map[string]bool, len(requested))
	for _, rename := range requested {
		from := strings.TrimSpace(rename.From)
		to := strings.TrimSpace(rename.To)
		if from == "" || to == "" {
			return nil, fmt.Errorf("Tên cũ và tên mới khi đổi tên model không được để trống")
		}
		if from == to {
			continue
		}
		if !oldNames[from] {
			return nil, fmt.Errorf("Không thể đổi tên model không tồn tại %q", from)
		}
		if !newNames[to] {
			return nil, fmt.Errorf("Model đích đổi tên %q không có trong danh sách model hiện tại", to)
		}
		if _, exists := renames[from]; exists {
			return nil, fmt.Errorf("Model %q bị đổi tên trùng lặp", from)
		}
		if targets[to] {
			return nil, fmt.Errorf("Nhiều model không được cùng đổi tên thành %q", to)
		}
		renames[from] = to
		targets[to] = true
	}
	return renames, nil
}

func renameModelReferences(cfg *bootstrap.Config, provider string, renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	if cfg.Provider == provider {
		if renamed, ok := renames[cfg.ModelName]; ok {
			cfg.ModelName = renamed
		}
	}
	for role, roleConfig := range cfg.Roles {
		changed := false
		if roleConfig.Provider == provider {
			if renamed, ok := renames[roleConfig.Model]; ok {
				roleConfig.Model = renamed
				changed = true
			}
		}
		for i := range roleConfig.Fallbacks {
			fallback := &roleConfig.Fallbacks[i]
			if fallback.Provider != provider {
				continue
			}
			if renamed, ok := renames[fallback.Model]; ok {
				fallback.Model = renamed
				changed = true
			}
		}
		if changed {
			cfg.Roles[role] = roleConfig
		}
	}
}

func (h *Host) saveModelConfigurationLocked(candidate bootstrap.Config, provider string, pc bootstrap.ProviderConfig, renamed bool) error {
	if renamed {
		// References and the provider definition must land in the same file replacement, or a process restart might see only half.
		// /model also writes the effective configuration back via SaveConfig; a rename follows the same semantics.
		return bootstrap.SaveConfig(h.configPath, candidate)
	}
	return bootstrap.SaveProviderConfig(h.configPath, provider, pc)
}

// TestModelConnection builds a real model client from the current draft and sends a minimal request.
// It does not save the configuration, does not switch the runtime model, and does not fall back to another Provider on failure.
func (h *Host) TestModelConnection(ctx context.Context, draft ModelConfigurationDraft, modelName string) error {
	h.mu.Lock()
	preparedDraft, err := h.prepareProviderDraftLocked(draft)
	h.mu.Unlock()
	if err != nil {
		return err
	}

	modelName = strings.TrimSpace(modelName)
	found := false
	for _, model := range preparedDraft.provider.Models {
		if model.Name == modelName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("Model kiểm tra kết nối %q không có trong danh sách model hiện tại", modelName)
	}

	testConfig := preparedDraft.candidate
	testConfig.Provider = preparedDraft.draft.Provider
	testConfig.ModelName = modelName
	testConfig.Roles = nil
	if err := testConfig.ValidateBase(); err != nil {
		return err
	}
	models, err := bootstrap.NewModelSet(testConfig)
	if err != nil {
		return fmt.Errorf("Không tạo được client model để kiểm tra: %w", err)
	}
	if _, err := models.Default.Generate(ctx, []agentcore.Message{agentcore.UserMsg("Reply OK.")}, nil); err != nil {
		return fmt.Errorf("Không kết nối được (%s/%s): %w", preparedDraft.draft.Provider, modelName, err)
	}
	return nil
}

func (h *Host) modelReferencesLocked(provider, model string) []string {
	var refs []string
	if h.cfg.Provider == provider && h.cfg.ModelName == model {
		refs = append(refs, "default")
	}
	for role, rc := range h.cfg.Roles {
		if rc.Provider == provider && rc.Model == model {
			refs = append(refs, role)
		}
		for i, fallback := range rc.Fallbacks {
			if fallback.Provider == provider && fallback.Model == model {
				refs = append(refs, fmt.Sprintf("%s fallback[%d]", role, i))
			}
		}
	}
	sort.Strings(refs)
	return refs
}
