package tui

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/host"
)

type configStep int

const (
	configStepProvider configStep = iota
	configStepAddPicker
	configStepCustomName
	configStepHub
	configStepProtocol
	configStepAPI
	configStepModels
)

const (
	configModelNameField   = "model_name"
	configModelWindowField = "model_window"
)

type configProviderChoice struct {
	label    string
	existing *host.ProviderSnapshot
	preset   *bootstrap.ProviderPreset
	custom   bool
	add      bool
}

type modelConfigBaseline struct {
	providerType string
	api          string
	baseURL      string
	models       []bootstrap.ModelConfig
}

type modelConfigState struct {
	snapshot   host.ModelConfigurationSnapshot
	step       configStep
	cursor     int
	message    string
	input      textinput.Model
	saving     bool
	testing    bool
	testCancel context.CancelFunc

	providerChoices []configProviderChoice
	presetChoices   []configProviderChoice
	provider        string
	providerType    string
	api             string
	baseURL         string
	models          []bootstrap.ModelConfig
	currentModel    string
	existing        bool
	hasAPIKey       bool
	apiKeyHint      string
	apiKeyOptional  bool
	apiKeyAction    host.APIKeyAction
	apiKey          string
	editingField    string
	baseline        *modelConfigBaseline

	modelOrigins []string
	modelColumn  int
	editModelIdx int
	addingModel  bool
}

func newModelConfigState(rt *host.Host) *modelConfigState {
	state := &modelConfigState{snapshot: rt.ModelConfiguration(), editModelIdx: -1}
	state.buildProviderMenus()
	return state
}

func (s *modelConfigState) buildProviderMenus() {
	configured := make(map[string]bool, len(s.snapshot.Providers))
	for i := range s.snapshot.Providers {
		provider := s.snapshot.Providers[i]
		configured[provider.Name] = true
		copyProvider := provider
		s.providerChoices = append(s.providerChoices, configProviderChoice{
			label: provider.Name, existing: &copyProvider,
		})
	}
	s.providerChoices = append(s.providerChoices, configProviderChoice{
		label: "+ Thêm provider mới…", add: true,
	})

	for _, presetValue := range bootstrap.ProviderPresets() {
		if configured[presetValue.Name] && !presetValue.NeedType {
			continue
		}
		preset := presetValue
		choice := configProviderChoice{label: preset.Label, preset: &preset}
		if preset.NeedType {
			choice.custom = true
		}
		s.presetChoices = append(s.presetChoices, choice)
	}
}

func (s *modelConfigState) applyProviderChoice(choice configProviderChoice) {
	s.cursor = 0
	s.message = ""
	s.editingField = ""
	s.editModelIdx = -1
	s.modelColumn = 0
	s.addingModel = false
	if choice.existing != nil {
		p := choice.existing
		s.provider = p.Name
		s.providerType = p.Type
		s.api = p.API
		s.baseURL = p.BaseURL
		s.models = append([]bootstrap.ModelConfig(nil), p.Models...)
		s.existing = true
		s.hasAPIKey = p.HasAPIKey
		s.apiKeyHint = p.APIKeyHint
		s.apiKeyOptional = !p.RequiresAPIKey
		s.apiKeyAction = host.APIKeyKeep
		s.apiKey = ""
		s.modelOrigins = make([]string, len(s.models))
		for i, model := range s.models {
			s.modelOrigins[i] = model.Name
		}
		s.currentModel = ""
		if s.snapshot.DefaultProvider == s.provider {
			s.currentModel = s.snapshot.DefaultModel
		}
		s.captureBaseline()
		s.step = configStepHub
		return
	}

	// Add new
	s.existing = false
	s.hasAPIKey = false
	s.apiKeyHint = ""
	s.apiKeyAction = host.APIKeyReplace
	s.apiKey = ""
	s.baseline = nil
	s.api = ""
	s.models = nil
	s.modelOrigins = nil
	s.currentModel = ""
	if choice.custom {
		s.apiKeyOptional = true
		s.providerType = "openai"
		s.baseURL = ""
		s.step = configStepCustomName
		s.startTextInput("", "Tên provider", false)
		return
	}
	s.provider = choice.preset.Name
	s.providerType = ""
	s.baseURL = choice.preset.BaseURL
	s.apiKeyOptional = choice.preset.APIKeyOptional
	s.step = configStepHub
}

func (s *modelConfigState) captureBaseline() {
	s.baseline = &modelConfigBaseline{
		providerType: s.providerType,
		api:          s.api,
		baseURL:      s.baseURL,
		models:       append([]bootstrap.ModelConfig(nil), s.models...),
	}
}

func (s *modelConfigState) isDirty() bool {
	if !s.existing || s.baseline == nil {
		return true
	}
	if s.apiKeyAction != host.APIKeyKeep {
		return true
	}
	baseURL := s.baseURL
	if s.editingField == "baseurl" {
		baseURL = strings.TrimSpace(s.input.Value())
	}
	if s.editingField == "key" && strings.TrimSpace(s.input.Value()) != "" {
		return true
	}
	return s.providerType != s.baseline.providerType ||
		s.api != s.baseline.api ||
		baseURL != s.baseline.baseURL ||
		!slices.Equal(s.models, s.baseline.models)
}

type hubField struct {
	id    string
	label string
	value string
}

func (s *modelConfigState) hubFields() []hubField {
	var fields []hubField
	if s.providerType != "" {
		fields = append(fields, hubField{"protocol", "Giao thức", s.providerType})
	}
	if s.isOpenAIEndpoint() {
		api := s.api
		if api == "" {
			api = "chat"
		}
		fields = append(fields, hubField{"api", "Endpoint", api})
	}
	fields = append(fields, hubField{"key", "API key", s.keyStatus()})
	base := s.baseURL
	if base == "" {
		base = "Mặc định"
	}
	fields = append(fields, hubField{"baseurl", "Base URL", base})
	fields = append(fields, hubField{"models", "Model", fmt.Sprintf("%d model", len(s.models))})
	testModel := s.testModelName()
	if testModel == "" {
		testModel = "Chưa có model"
	}
	fields = append(fields, hubField{"test", "Thử kết nối", testModel})
	fields = append(fields, hubField{"save", "Lưu cấu hình", ""})
	return fields
}

func (s *modelConfigState) testModelName() string {
	for _, model := range s.models {
		if model.Name == s.currentModel {
			return model.Name
		}
	}
	if len(s.models) > 0 {
		return s.models[0].Name
	}
	return ""
}

func (s *modelConfigState) isOpenAIEndpoint() bool {
	return s.providerType == "openai" || (s.providerType == "" && s.provider == "openai")
}

func (s *modelConfigState) keyStatus() string {
	switch s.apiKeyAction {
	case host.APIKeyClear:
		return "Đã xóa"
	case host.APIKeyReplace:
		if s.apiKey != "" {
			return host.MaskAPIKey(s.apiKey)
		}
	}
	if s.apiKeyHint != "" {
		return s.apiKeyHint
	}
	return "Chưa đặt"
}

func (s *modelConfigState) enterHubField(id string) (save bool, cmd tea.Cmd) {
	s.message = ""
	switch id {
	case "protocol":
		s.step = configStepProtocol
		s.cursor = protocolIndex(s.providerType)
	case "api":
		s.step = configStepAPI
		s.cursor = 0
		if s.api == "responses" {
			s.cursor = 1
		}
	case "key":
		return false, s.beginInlineEdit("key")
	case "baseurl":
		return false, s.beginInlineEdit("baseurl")
	case "models":
		s.ensureModelOrigins()
		s.step = configStepModels
		s.cursor = 0
		s.modelColumn = 0
	case "test":
		return false, nil
	case "save":
		return true, nil
	}
	return false, nil
}

func (s *modelConfigState) beginInlineEdit(field string) tea.Cmd {
	s.editingField = field

	switch field {
	case "key":
		placeholder := "Nhập API key"
		if s.hasEffectiveAPIKey() {
			placeholder = "Nhập key mới (để trống để giữ nguyên)"
		}
		return s.startTextInput("", placeholder, true)
	case "baseurl":
		return s.startTextInput(s.baseURL, "Để trống để dùng địa chỉ mặc định", false)
	}
	return nil
}

func (s *modelConfigState) startTextInput(value, placeholder string, secret bool) tea.Cmd {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = 0
	input.Width = 36
	input.TextStyle = lipgloss.NewStyle().Foreground(bodyTextColor).Underline(true)
	input.PlaceholderStyle = lipgloss.NewStyle().Foreground(colorDim).Underline(true)
	input.Cursor.Style = lipgloss.NewStyle().Foreground(colorAccent)
	if secret {
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '•'
	}
	input.SetValue(value)
	input.CursorEnd()
	s.input = input
	return s.input.Focus()
}

func (s *modelConfigState) hasEffectiveAPIKey() bool {
	switch s.apiKeyAction {
	case host.APIKeyClear:
		return false
	case host.APIKeyReplace:
		return strings.TrimSpace(s.apiKey) != ""
	default:
		return s.hasAPIKey
	}
}

func (s *modelConfigState) finishInlineEdit() bool {
	value := strings.TrimSpace(s.input.Value())
	switch s.editingField {
	case "key":
		if value == "" {
			if !s.apiKeyOptional && !s.hasEffectiveAPIKey() {
				s.message = "Provider này bắt buộc phải có API key"
				return false
			}
		} else {
			s.apiKey = value
			s.apiKeyAction = host.APIKeyReplace
		}
	case "baseurl":
		s.baseURL = value
	}
	s.input.Blur()
	s.editingField = ""
	s.message = ""
	return true
}

func (s *modelConfigState) escapeBack() (configStep, bool) {
	switch s.step {
	case configStepAddPicker, configStepHub:
		return configStepProvider, true
	case configStepCustomName:
		return configStepAddPicker, true
	case configStepProtocol, configStepAPI, configStepModels:
		return configStepHub, true
	default:
		return 0, false
	}
}

func (s *modelConfigState) ensureModelOrigins() {
	if len(s.modelOrigins) == len(s.models) {
		return
	}
	s.modelOrigins = make([]string, len(s.models))
	for i, model := range s.models {
		s.modelOrigins[i] = model.Name
	}
}

func (s *modelConfigState) beginModelEdit(idx, column int) tea.Cmd {
	if idx < 0 || idx >= len(s.models) {
		return nil
	}
	s.editModelIdx = idx
	s.modelColumn = column
	s.message = ""
	if column == 0 {
		s.editingField = configModelNameField
		return s.startTextInput(s.models[idx].Name, "Tên/ID model", false)
	}
	s.editingField = configModelWindowField
	value := ""
	if window := s.models[idx].ContextWindow; window > 0 {
		value = strconv.Itoa(window)
	}
	return s.startTextInput(value, "auto / 128K / 1M", false)
}

func (s *modelConfigState) finishModelEdit() (tea.Cmd, bool) {
	idx := s.editModelIdx
	if idx < 0 || idx >= len(s.models) {
		return nil, false
	}
	switch s.editingField {
	case configModelNameField:
		name := strings.TrimSpace(s.input.Value())
		if name == "" {
			s.message = "Tên model không được để trống"
			return nil, false
		}
		for i, model := range s.models {
			if i != idx && model.Name == name {
				s.message = "Model này đã tồn tại"
				return nil, false
			}
		}
		s.models[idx].Name = name
		if s.addingModel {
			s.modelColumn = 1
			s.editingField = configModelWindowField
			s.message = ""
			return s.startTextInput("", "auto / 128K / 1M", false), true
		}
		s.input.Blur()
		s.editingField = ""
		s.message = ""
		origin := s.modelOrigins[idx]
		if origin != "" && origin != name {
			if refs := s.snapshot.ReferencesFor(s.provider, origin); len(refs) > 0 {
				s.message = "Khi lưu sẽ đồng bộ cập nhật gán vai trò: " + strings.Join(refs, ", ")
			}
		}
		return nil, true
	case configModelWindowField:
		window, err := parseContextWindowInput(s.input.Value())
		if err != nil {
			s.message = err.Error()
			return nil, false
		}
		s.models[idx].ContextWindow = window
		s.input.Blur()
		s.editingField = ""
		s.addingModel = false
		s.message = ""
		return nil, true
	}
	return nil, false
}

func (s *modelConfigState) cancelModelEdit() {
	if s.addingModel && s.editingField == configModelNameField &&
		s.editModelIdx >= 0 && s.editModelIdx < len(s.models) {
		idx := s.editModelIdx
		s.models = append(s.models[:idx], s.models[idx+1:]...)
		s.modelOrigins = append(s.modelOrigins[:idx], s.modelOrigins[idx+1:]...)
		s.cursor = len(s.models)
	}
	s.input.Blur()
	s.editingField = ""
	s.editModelIdx = -1
	s.addingModel = false
	s.message = ""
}

func (s *modelConfigState) deleteModel(idx int) bool {
	if idx < 0 || idx >= len(s.models) {
		return false
	}
	s.ensureModelOrigins()
	model := s.models[idx]
	identity := s.modelOrigins[idx]
	if identity == "" {
		identity = model.Name
	}
	if identity == s.currentModel {
		s.message = "Model này đang được sử dụng mặc định, vui lòng chuyển qua model khác trước khi xóa"
		return false
	}
	for _, ref := range s.snapshot.ReferencesFor(s.provider, identity) {
		if ref == "default" {
			continue
		}
		s.message = fmt.Sprintf("Model đang được gán cho vai trò %s, vui lòng chuyển vai trò sang model khác trước khi xóa", ref)
		return false
	}
	s.models = append(s.models[:idx], s.models[idx+1:]...)
	s.modelOrigins = append(s.modelOrigins[:idx], s.modelOrigins[idx+1:]...)
	s.cursor = idx
	if s.cursor > len(s.models) {
		s.cursor = len(s.models)
	}
	s.message = ""
	return true
}

func (s *modelConfigState) draft() host.ModelConfigurationDraft {
	s.ensureModelOrigins()
	renames := make([]host.ModelRename, 0)
	for i, model := range s.models {
		if origin := s.modelOrigins[i]; origin != "" && origin != model.Name {
			renames = append(renames, host.ModelRename{From: origin, To: model.Name})
		}
	}
	return host.ModelConfigurationDraft{
		Provider: s.provider, Type: s.providerType, API: s.api, BaseURL: s.baseURL,
		Models:       append([]bootstrap.ModelConfig(nil), s.models...),
		Renames:      renames,
		APIKeyAction: s.apiKeyAction, APIKey: s.apiKey,
	}
}

type modelConfigSavedMsg struct{ err error }

type modelConfigConnectionMsg struct {
	model string
	err   error
}

func saveModelConfiguration(rt *host.Host, draft host.ModelConfigurationDraft) tea.Cmd {
	return func() tea.Msg { return modelConfigSavedMsg{err: rt.ConfigureModels(draft)} }
}

func testModelConnection(ctx context.Context, rt *host.Host, draft host.ModelConfigurationDraft, model string) tea.Cmd {
	return func() tea.Msg {
		return modelConfigConnectionMsg{model: model, err: rt.TestModelConnection(ctx, draft, model)}
	}
}

func (m Model) handleModelConfigKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.modelConfig
	if state == nil {
		return m, nil
	}
	if msg.Type == tea.KeyEsc {
		if state.testing {
			if state.testCancel != nil {
				state.testCancel()
			}
			state.message = "Đang hủy kiểm tra kết nối..."
			return m, nil
		}
		if state.editingField != "" && (state.step == configStepHub || state.step == configStepModels) {
			if state.step == configStepModels {
				state.cancelModelEdit()
			} else {
				state.input.Blur()
				state.editingField = ""
				state.message = ""
			}
			return m, nil
		}
		if target, ok := state.escapeBack(); ok {
			state.input.Blur()
			state.step = target
			state.cursor = 0
			state.message = ""
			return m, nil
		}
		m.modelConfig = nil
		return m, m.textarea.Focus()
	}
	if state.saving || state.testing {
		return m, nil
	}

	switch state.step {
	case configStepProvider:
		moveConfigCursor(state, msg, len(state.providerChoices))
		if msg.Type == tea.KeyEnter && state.cursor >= 0 && state.cursor < len(state.providerChoices) {
			choice := state.providerChoices[state.cursor]
			if choice.add {
				state.step = configStepAddPicker
				state.cursor = 0
				state.message = ""
			} else {
				state.applyProviderChoice(choice)
			}
		}
	case configStepAddPicker:
		moveConfigCursor(state, msg, len(state.presetChoices))
		if msg.Type == tea.KeyEnter && state.cursor >= 0 && state.cursor < len(state.presetChoices) {
			state.applyProviderChoice(state.presetChoices[state.cursor])
		}
	case configStepCustomName:
		if msg.Type == tea.KeyEnter {
			name := strings.TrimSpace(state.input.Value())
			if name == "" {
				state.message = "Tên provider không được để trống"
				break
			}
			for _, provider := range state.snapshot.Providers {
				if provider.Name == name {
					state.message = "Provider này đã tồn tại, vui lòng quay lại để chỉnh sửa"
					return m, nil
				}
			}
			state.provider = name
			state.step = configStepHub
			state.cursor = 0
			state.message = ""
			return m, nil
		}
		var cmd tea.Cmd
		state.input, cmd = state.input.Update(msg)
		return m, cmd
	case configStepHub:
		fields := state.hubFields()
		if state.editingField != "" {
			if msg.Type == tea.KeyEnter {
				state.finishInlineEdit()
				return m, nil
			}
			var cmd tea.Cmd
			state.input, cmd = state.input.Update(msg)
			return m, cmd
		}
		moveConfigCursor(state, msg, len(fields))
		if msg.Type == tea.KeyDelete && state.cursor >= 0 && state.cursor < len(fields) && fields[state.cursor].id == "key" {
			if !state.apiKeyOptional {
				state.message = "Provider này bắt buộc phải có API key, không thể xóa"
				break
			}
			state.apiKeyAction = host.APIKeyClear
			state.apiKey = ""
			state.message = "API key đã đánh dấu xóa, sẽ có hiệu lực sau khi lưu"
			break
		}
		if msg.Type == tea.KeyEnter && state.cursor >= 0 && state.cursor < len(fields) {
			fieldID := fields[state.cursor].id
			if fieldID == "test" {
				model := state.testModelName()
				if model == "" {
					state.message = "Vui lòng thêm ít nhất một model trước khi thử kết nối"
					break
				}
				if !state.apiKeyOptional && !state.hasEffectiveAPIKey() {
					state.message = "Provider này bắt buộc phải cấu hình API key"
					break
				}
				state.testing = true
				state.message = fmt.Sprintf("Đang thử kết nối: %s/%s...", state.provider, model)
				ctx, cancel := context.WithCancel(context.Background())
				state.testCancel = cancel
				return m, testModelConnection(ctx, m.runtime, state.draft(), model)
			}
			save, cmd := state.enterHubField(fieldID)
			if save {
				if len(state.models) == 0 {
					state.message = "Vui lòng thêm ít nhất một model"
					break
				}
				if !state.apiKeyOptional && !state.hasEffectiveAPIKey() {
					state.message = "Provider này bắt buộc phải cấu hình API key"
					break
				}
				state.saving = true
				state.message = "Đang kiểm tra và lưu cấu hình..."
				return m, saveModelConfiguration(m.runtime, state.draft())
			}
			return m, cmd
		}
	case configStepProtocol:
		moveConfigCursor(state, msg, len(configProtocols))
		if msg.Type == tea.KeyEnter {
			state.providerType = configProtocols[state.cursor]
			if state.providerType != "openai" {
				state.api = ""
			}
			state.step = configStepHub
			state.cursor = 0
		}
	case configStepAPI:
		moveConfigCursor(state, msg, len(configAPIs))
		if msg.Type == tea.KeyEnter {
			state.api = configAPIs[state.cursor]
			state.step = configStepHub
			state.cursor = 0
		}
	case configStepModels:
		state.ensureModelOrigins()
		if state.editingField != "" {
			if msg.Type == tea.KeyEnter {
				cmd, _ := state.finishModelEdit()
				return m, cmd
			}
			var cmd tea.Cmd
			state.input, cmd = state.input.Update(msg)
			return m, cmd
		}
		moveConfigCursor(state, msg, len(state.models)+1)
		if state.cursor < len(state.models) {
			switch msg.Type {
			case tea.KeyLeft:
				state.modelColumn = 0
			case tea.KeyRight:
				state.modelColumn = 1
			case tea.KeyDelete:
				state.deleteModel(state.cursor)
				return m, nil
			}
		}
		if msg.Type == tea.KeyEnter {
			if state.cursor == len(state.models) {
				state.models = append(state.models, bootstrap.ModelConfig{})
				state.modelOrigins = append(state.modelOrigins, "")
				state.cursor = len(state.models) - 1
				state.addingModel = true
				state.message = ""
				return m, state.beginModelEdit(state.cursor, 0)
			} else if state.cursor >= 0 && state.cursor < len(state.models) {
				return m, state.beginModelEdit(state.cursor, state.modelColumn)
			}
		}
	}
	return m, nil
}

var configProtocols = []string{"openai", "anthropic", "gemini"}
var configAPIs = []string{"chat", "responses"}

func protocolIndex(protocol string) int {
	for i, item := range configProtocols {
		if item == protocol {
			return i
		}
	}
	return 0
}

func moveConfigCursor(state *modelConfigState, msg tea.KeyMsg, total int) {
	if total <= 0 {
		state.cursor = 0
		return
	}
	switch msg.Type {
	case tea.KeyUp:
		state.cursor = (state.cursor - 1 + total) % total
	case tea.KeyDown:
		state.cursor = (state.cursor + 1) % total
	}
}

func parseContextWindowInput(input string) (int, error) {
	value := strings.ToLower(strings.TrimSpace(input))
	if value == "" || value == "0" || value == "auto" {
		return 0, nil
	}
	multiplier := float64(1)
	if strings.HasSuffix(value, "k") {
		multiplier = 1000
		value = strings.TrimSpace(strings.TrimSuffix(value, "k"))
	} else if strings.HasSuffix(value, "m") {
		multiplier = 1_000_000
		value = strings.TrimSpace(strings.TrimSuffix(value, "m"))
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("Context window phải là số nguyên dương, 128K, 1M hoặc để trống để tự động")
	}
	result := number * multiplier
	if result > float64(math.MaxInt) || math.Trunc(result) != result {
		return 0, fmt.Errorf("Context window vượt quá giới hạn số nguyên")
	}
	return int(result), nil
}

func renderModelConfigModal(width int, state *modelConfigState) string {
	if state == nil {
		return ""
	}
	boxW := min(max(60, width*3/5), 76, width-4)
	contentW := paddedModalContentWidth(boxW)
	var lines []string
	title := "/config Cấu hình model & provider"
	hint := "↑↓ Chọn · Enter Xác nhận · Esc Thoát"

	switch state.step {
	case configStepProvider:
		lines = append(lines, configHeading("Chọn provider cần chỉnh sửa hoặc thêm mới"))
		lines = append(lines, renderConfigChoices(labelsForProviderChoices(state.providerChoices), state.cursor, contentW, 12)...)
	case configStepAddPicker:
		lines = append(lines, configHeading("Chọn provider muốn thêm"))
		lines = append(lines, renderConfigChoices(labelsForProviderChoices(state.presetChoices), state.cursor, contentW, 12)...)
	case configStepCustomName:
		lines = append(lines, configHeading("Đặt tên provider tùy chỉnh"), renderConfigTextInput(&state.input, contentW))
		hint = configInputHint
	case configStepHub:
		heading := state.provider
		if !state.existing {
			heading += " (Thêm mới)"
		}
		lines = append(lines, configHeading(heading))
		lines = append(lines, renderProviderHubFields(state, contentW)...)
		if state.snapshot.ConfigPath != "" {
			advanced := "Cấu hình nâng cao (extra / extra_body / stream_idle_timeout): " + state.snapshot.ConfigPath
			lines = append(lines, "")
			lines = appendWrappedConfigText(lines, advanced, contentW, lipgloss.NewStyle().Foreground(colorDim))
		}
		if state.editingField != "" {
			hint = "Nhập dữ liệu · Enter Xác nhận · Esc Hủy"
		} else {
			hint = "↑↓ Chọn · Enter Sửa/Vào · Esc Quay lại"
			fields := state.hubFields()
			if state.apiKeyOptional && state.cursor >= 0 && state.cursor < len(fields) && fields[state.cursor].id == "key" {
				hint += " · Delete Xóa key"
			}
			if state.cursor >= 0 && state.cursor < len(fields) && fields[state.cursor].id == "test" {
				lines = append(lines, lipgloss.NewStyle().Foreground(colorDim).Render("Thử nghiệm sẽ gửi request tối thiểu để kiểm tra"))
			}
		}
	case configStepProtocol:
		lines = append(lines, configHeading("Loại giao thức API"))
		lines = append(lines, renderConfigChoices(configProtocols, state.cursor, contentW, 8)...)
	case configStepAPI:
		lines = append(lines, configHeading("OpenAI Endpoint"))
		lines = append(lines, renderConfigChoices([]string{"chat · /v1/chat/completions", "responses · /v1/responses"}, state.cursor, contentW, 8)...)
	case configStepModels:
		lines = append(lines, configHeading("Quản lý danh sách model"))
		lines = append(lines, renderModelConfigRows(state, contentW)...)
		if state.editingField != "" {
			hint = "Nhập dữ liệu · Enter Xác nhận · Esc Hủy"
		} else {
			hint = "↑↓ Dòng · ←→ Cột · Enter Sửa · Delete Xóa · Esc Quay lại"
		}
	}

	if state.message != "" {
		color := colorError
		if strings.HasPrefix(state.message, "Kiểm tra kết nối thành công") || strings.HasPrefix(state.message, "Kết nối thành công") {
			color = colorSuccess
		} else if state.saving || state.testing || strings.HasPrefix(state.message, "Đã chọn") ||
			strings.HasPrefix(state.message, "API key đã") || strings.HasPrefix(state.message, "Đang") {
			color = colorAccent
		} else if strings.HasPrefix(state.message, "Khi lưu sẽ") {
			color = colorAccent
		}
		lines = append(lines, "")
		lines = appendWrappedConfigText(lines, state.message, contentW, lipgloss.NewStyle().Foreground(color))
	}
	return renderPaddedModalFrame(boxW, len(lines)+2, title, hint, lines)
}

const configInputHint = "Nhập dữ liệu · Enter Xác nhận · Ctrl+U Xóa trắng · Esc Hủy"

func configHeading(text string) string {
	return lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(text)
}

func appendWrappedConfigText(lines []string, text string, width int, style lipgloss.Style) []string {
	for _, line := range strings.Split(wrapText(text, width), "\n") {
		lines = append(lines, style.Render(line))
	}
	return lines
}

func renderModelConfigRows(state *modelConfigState, contentW int) []string {
	state.ensureModelOrigins()
	contextW := 16
	refsW := 16
	nameW := contentW - 2 - contextW - refsW - 4
	if nameW < 20 {
		refsW = 0
		nameW = max(12, contentW-2-contextW-2)
	}

	header := "  " + padConfigCell("Tên/ID model", nameW) + "  " + padConfigCell("Context window", contextW)
	if refsW > 0 {
		header += "  " + padConfigCell("Vai trò gán", refsW)
	}
	lines := []string{lipgloss.NewStyle().Foreground(colorDim).Render(header)}

	total := len(state.models) + 1
	start, end := configWindow(total, state.cursor, 10)
	for i := start; i < end; i++ {
		selected := i == state.cursor
		marker := "  "
		if selected {
			marker = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("› ")
		}
		if i == len(state.models) {
			style := lipgloss.NewStyle().Foreground(bodyTextColor)
			if selected {
				style = style.Foreground(colorAccent).Bold(true)
			}
			lines = append(lines, marker+style.Render("+ Thêm model mới…"))
			continue
		}

		model := state.models[i]
		name := padConfigCell(model.Name, nameW)
		window := "Tự động"
		if model.ContextWindow > 0 {
			window = formatContextWindow(model.ContextWindow)
		}
		window = padConfigCell(window, contextW)

		nameCell := lipgloss.NewStyle().Foreground(bodyTextColor).Render(name)
		windowCell := lipgloss.NewStyle().Foreground(colorDim).Render(window)
		if selected && state.modelColumn == 0 {
			nameCell = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(name)
		}
		if selected && state.modelColumn == 1 {
			windowCell = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(window)
		}
		if state.editModelIdx == i && state.editingField == configModelNameField {
			nameCell = renderConfigInputCell(&state.input, nameW)
		}
		if state.editModelIdx == i && state.editingField == configModelWindowField {
			windowCell = renderConfigInputCell(&state.input, contextW)
		}

		line := marker + nameCell + "  " + windowCell
		if refsW > 0 {
			identity := state.modelOrigins[i]
			if identity == "" {
				identity = model.Name
			}
			refs := strings.Join(state.snapshot.ReferencesFor(state.provider, identity), ", ")
			line += "  " + lipgloss.NewStyle().Foreground(colorDim).Render(padConfigCell(refs, refsW))
		}
		lines = append(lines, truncateStyledWidth(line, contentW))
	}
	return lines
}

func padConfigCell(value string, width int) string {
	value = truncateWidth(value, width)
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func renderConfigInputCell(input *textinput.Model, width int) string {
	input.Width = max(1, width-1)
	view := truncateStyledWidth(input.View(), width)
	return view + strings.Repeat(" ", max(0, width-lipgloss.Width(view)))
}

func renderProviderHubFields(state *modelConfigState, contentW int) []string {
	fields := state.hubFields()
	lines := make([]string, 0, len(fields))
	dirty := state.isDirty()
	for i, f := range fields {
		marker := "  "
		labelStyle := lipgloss.NewStyle().Foreground(bodyTextColor)
		primarySave := f.id == "save" && dirty
		if primarySave {
			labelStyle = labelStyle.Foreground(colorSuccess)
		}
		if i == state.cursor {
			selectedColor := colorAccent
			if primarySave {
				selectedColor = colorSuccess
			}
			marker = lipgloss.NewStyle().Foreground(selectedColor).Bold(true).Render("› ")
			labelStyle = labelStyle.Foreground(selectedColor).Bold(true)
		}
		pad := max(1, 14-lipgloss.Width(f.label))
		if state.editingField == f.id {
			state.input.Width = max(8, contentW-2-lipgloss.Width(f.label)-pad)
			line := marker + labelStyle.Render(f.label) + strings.Repeat(" ", pad) + state.input.View()
			lines = append(lines, truncateStyledWidth(line, contentW))
			continue
		}
		var line string
		if f.value == "" {
			line = marker + labelStyle.Render(f.label)
		} else {
			line = marker + labelStyle.Render(f.label) + strings.Repeat(" ", pad) +
				lipgloss.NewStyle().Foreground(colorDim).Render(f.value)
		}
		lines = append(lines, truncateStyledWidth(line, contentW))
	}
	return lines
}

func labelsForProviderChoices(choices []configProviderChoice) []string {
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		out = append(out, choice.label)
	}
	return out
}

func renderConfigChoices(labels []string, cursor, width, limit int) []string {
	if len(labels) == 0 {
		return []string{lipgloss.NewStyle().Foreground(colorDim).Render("Không có lựa chọn nào khả dụng")}
	}
	start, end := configWindow(len(labels), cursor, limit)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(bodyTextColor)
		if i == cursor {
			prefix = "› "
			style = style.Foreground(colorAccent).Bold(true)
		}
		lines = append(lines, prefix+style.Render(truncateWidth(labels[i], max(8, width-2))))
	}
	return lines
}

func configWindow(total, cursor, limit int) (int, int) {
	if total <= limit {
		return 0, total
	}
	start := max(0, cursor-limit/2)
	end := min(total, start+limit)
	if end-start < limit {
		start = max(0, end-limit)
	}
	return start, end
}

func renderConfigTextInput(input *textinput.Model, width int) string {
	input.Width = max(8, width-4)
	return lipgloss.NewStyle().Foreground(colorAccent).Render("› ") + input.View()
}
