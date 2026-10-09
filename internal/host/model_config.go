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

// ProviderSnapshot 是供 TUI 使用的脱敏 provider 配置。
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

// ModelConfigurationDraft 是 /config 提交给 Host 的单个 provider 配置草稿。
// 只描述该 provider 的定义（协议/凭证/模型库），不含“当前用哪个”——切换归 /model。
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

// ModelRename 描述同一条模型配置的 ID 变化。它不是“删旧增新”的猜测，
// Host 只在 TUI 明确提交该关系时迁移 default、角色和 fallback 引用。
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

// MaskAPIKey 仅保留足够识别凭证的首尾片段；短凭证全部隐藏。
// TUI 只接收这个结果，绝不持有配置中的完整 API Key。
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

// ModelConfiguration 返回脱敏配置、可写目标和模型引用，绝不暴露现有 API Key。
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

// prepareProviderDraftLocked 将 TUI 草稿规范化并合入配置副本，保存和连接测试共用同一条校验链路。
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
			return preparedProviderDraft{}, fmt.Errorf("Cửa sổ ngữ cảnh của model %q không được âm", model.Name)
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
		// 保留候选配置里的现有值；新增 provider 时自然为空。
	case APIKeyReplace:
		pc.APIKey = draft.APIKey
	case APIKeyClear:
		pc.APIKey = ""
	default:
		return preparedProviderDraft{}, fmt.Errorf("Thao tác API Key không xác định %q", draft.APIKeyAction)
	}
	if pc.RequiresAPIKey(draft.Provider) && pc.APIKey == "" {
		return preparedProviderDraft{}, fmt.Errorf("Provider %q bắt buộc phải cấu hình API Key", draft.Provider)
	}

	if candidate.Providers == nil {
		candidate.Providers = make(map[string]bootstrap.ProviderConfig)
	}
	candidate.Providers[draft.Provider] = pc
	return preparedProviderDraft{draft: draft, candidate: candidate, provider: pc, oldModels: oldModels}, nil
}

// ConfigureModels 校验、持久化并热应用一个 provider 的模型库。
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
	// 删除模型前先查引用：被顶层默认或任何角色/fallback 指向的模型不能删，
	// 让用户先去 /model 切走——/config 不再代切默认。
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

	// 普通编辑不改变“当前用哪个”；显式重命名只迁移同一模型的引用身份。
	if err := candidate.ValidateBase(); err != nil {
		return err
	}
	prepared, err := bootstrap.NewModelSet(candidate)
	if err != nil {
		return fmt.Errorf("Tạo client model thất bại: %w", err)
	}

	if h.configPath == "" {
		return fmt.Errorf("Không xác định được đường dẫn tệp cấu hình")
	}
	if err := h.saveModelConfigurationLocked(candidate, draft.Provider, pc, len(renames) > 0); err != nil {
		return fmt.Errorf("Lưu cấu hình thất bại: %w", err)
	}

	h.models.ApplyPrepared(prepared)
	h.cfg = candidate
	// 模型客户端被重建后重新下发推理强度：applyThinkingLocked 按各角色的新模型能力钳制生效值，
	// 存储的强度意图保持不变。
	h.applyThinkingLocked("default")
	summary := fmt.Sprintf("Đã lưu cấu hình Provider: %s → %s", draft.Provider, h.configPath)
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
		// 引用与 provider 定义必须在同一次文件替换中落盘，否则进程重启可能只看到一半。
		// /model 也使用 SaveConfig 写回有效配置；重命名沿用同一语义。
		return bootstrap.SaveConfig(h.configPath, candidate)
	}
	return bootstrap.SaveProviderConfig(h.configPath, provider, pc)
}

// TestModelConnection 使用当前草稿构造一个真实模型客户端并发送最小请求。
// 它不保存配置、不切换运行时模型，也不在失败时降级到其他 Provider。
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
		return fmt.Errorf("Tạo client model kiểm tra thất bại: %w", err)
	}
	if _, err := models.Default.Generate(ctx, []agentcore.Message{agentcore.UserMsg("Reply OK.")}, nil); err != nil {
		return fmt.Errorf("Kiểm tra kết nối thất bại (%s/%s): %w", preparedDraft.draft.Provider, modelName, err)
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
