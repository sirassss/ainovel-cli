package bootstrap

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/utils"
)

// exampleConfig is the annotated config template written to ~/.ainovel/config.example.jsonc.
//
//go:embed config.example.jsonc
var exampleConfig string

// NeedsSetup checks whether the first-run setup wizard needs to run.
func NeedsSetup() bool {
	if p := DefaultConfigPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return false
		}
	}
	if _, err := os.Stat(projectConfigPath()); err == nil {
		return false
	}
	return true
}

type setupProvider struct {
	name           string
	label          string
	baseURL        string // base_url pre-filled
	needType       bool   // A custom proxy needs extra questions for type and base_url
	apiKeyOptional bool   // true if the API Key may be left empty
}

// ProviderPreset is a Provider config entry shared by Setup and the /config command.
type ProviderPreset struct {
	Name           string
	Label          string
	BaseURL        string
	NeedType       bool
	APIKeyOptional bool
}

var setupProviders = []setupProvider{
	{name: "ollama", label: "Ollama (cục bộ / offline, miễn phí)", baseURL: "http://localhost:11434/v1", apiKeyOptional: true},
	{name: "openrouter", label: "OpenRouter (Claude, Gemini, DeepSeek, Qwen...)", baseURL: "https://openrouter.ai/api/v1"},
	{name: "gemini", label: "Google Gemini", baseURL: ""},
	{name: "anthropic", label: "Anthropic Claude", baseURL: ""},
	{name: "deepseek", label: "DeepSeek", baseURL: "https://api.deepseek.com/v1"},
	{name: "openai", label: "OpenAI", baseURL: ""},
	{name: "qwen", label: "Alibaba Qwen (DashScope)", baseURL: ""},
	{name: "glm", label: "Zhipu GLM", baseURL: ""},
	{name: "grok", label: "xAI Grok", baseURL: ""},
	{name: "bedrock", label: "AWS Bedrock", apiKeyOptional: true},
	{name: "custom", label: "Custom proxy (proxy / API tùy chỉnh)", needType: true, apiKeyOptional: true},
}

// ProviderPresets returns the list of preset Provider settings.
func ProviderPresets() []ProviderPreset {
	out := make([]ProviderPreset, 0, len(setupProviders))
	for _, preset := range setupProviders {
		out = append(out, ProviderPreset{
			Name: preset.name, Label: preset.label, BaseURL: preset.baseURL,
			NeedType: preset.needType, APIKeyOptional: preset.apiKeyOptional,
		})
	}
	return out
}

type setupLanguageOption struct {
	code  string
	label string
}

var languageOptions = []setupLanguageOption{
	{code: "vi", label: "Tiếng Việt (mặc định, sáng tác văn phong Việt tự nhiên)"}, // case:ok (proper noun)
	{code: "zh", label: "Tiếng Trung (nguyên bản, sáng tác bằng tiếng Trung)"},     // case:ok (proper noun)
}

// RunSetup runs the first-run setup wizard and returns the config it produced.
func RunSetup() (Config, error) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).
		Render("Chưa có tệp cấu hình, bắt đầu thiết lập lần đầu..."))
	fmt.Fprintf(os.Stderr, "  Đường dẫn tệp cấu hình: %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(DefaultConfigPath()))
	fmt.Fprintf(os.Stderr, "  Sau khi xong, bạn có thể sửa tệp này để tùy chỉnh nâng cao.\n")
	fmt.Fprintln(os.Stderr)

	// Step 1: Choose the novel writing language
	selectedLang, err := runLanguageSelect()
	if err != nil {
		return Config{}, err
	}
	printStepDone("Ngôn ngữ sáng tác", selectedLang.label)

	// Step 2: Choose the AI provider (Provider)
	sp, err := runProviderSelect()
	if err != nil {
		return Config{}, err
	}

	providerName := sp.name
	var pc ProviderConfig
	printStepDone("Provider AI", sp.label)

	// Custom proxy: ask additionally for the name and the API protocol
	if sp.needType {
		providerName, err = runTextInput("Tên provider", "my-proxy")
		if err != nil {
			return Config{}, err
		}
		providerType, err := runTypeSelect()
		if err != nil {
			return Config{}, err
		}
		pc.Type = providerType
	}

	// Step 3: Enter the API Key
	var apiKey string
	if sp.apiKeyOptional {
		apiKey, err = runOptionalTextInput("[3/5] API key (Nhấn Enter để bỏ qua nếu dùng Ollama/Local)", "Để trống nếu không cần API key")
	} else {
		apiKey, err = runTextInput("[3/5] API key", "sk-xxx...")
	}
	if err != nil {
		return Config{}, err
	}
	pc.APIKey = apiKey
	if apiKey == "" {
		printStepDone("API key", "Không dùng (mặc định cho Ollama/Local)")
	} else {
		printStepDone("API key", maskKey(apiKey))
	}

	// Step 4: Base URL (press Enter to use the default)
	baseDefault := sp.baseURL
	baseHint := "Để trống dùng địa chỉ mặc định"
	if baseDefault != "" {
		baseHint = baseDefault
	}
	baseURL, err := runTextInputWithDefault("[4/5] Base URL (Nhấn Enter để dùng địa chỉ mặc định, hoặc nhập địa chỉ proxy/Ollama)", baseHint, baseDefault)
	if err != nil {
		return Config{}, err
	}
	pc.BaseURL = baseURL
	if baseURL != "" {
		printStepDone("Base URL", baseURL)
	} else {
		printStepDone("Base URL", "Mặc định")
	}

	// Step 5: Model name (required)
	modelPlaceholder := "Ví dụ: qwen2.5:14b / ainovel-qwen / google/gemini-2.5-flash / claude-3-5-sonnet"
	if providerName == "ollama" {
		modelPlaceholder = "Ví dụ: qwen2.5:14b / ainovel-qwen / qwen3:14b"
	}
	modelName, err := runTextInput("[5/5] Tên model chính", modelPlaceholder)
	if err != nil {
		return Config{}, err
	}
	printStepDone("Model", modelName)
	pc.Models = []ModelConfig{{Name: modelName}}

	cfg := Config{
		Provider:  providerName,
		ModelName: modelName,
		Providers: map[string]ProviderConfig{providerName: pc},
		Roles:     map[string]RoleConfig{},
		Style:     "default",
		Language:  selectedLang.code,
	}

	// Save the config
	path := DefaultConfigPath()
	if err := SaveConfig(path, cfg); err != nil {
		return cfg, fmt.Errorf("không lưu được cấu hình: %w", err)
	}

	// Create the example file
	saveExampleConfig()

	rulesDir := rules.DefaultHomeRulesDir()

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "%s Đã lưu cấu hình tại: %s\n",
		lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓"), path)
	fmt.Fprintf(os.Stderr, "  Ngôn ngữ truyện: %s\n", selectedLang.label)
	fmt.Fprintf(os.Stderr, "  Provider mặc định: %s\n", providerName)
	fmt.Fprintf(os.Stderr, "  Model mặc định: %s\n", modelName)
	fmt.Fprintln(os.Stderr, "  Bạn có thể dùng lệnh /config hoặc /model trong TUI để thay đổi bất cứ lúc nào.")
	if rulesDir != "" {
		fmt.Fprintf(os.Stderr, "  Các quy tắc và phong cách viết cá nhân có thể đặt tại thư mục: %s\n", rulesDir)
	}
	fmt.Fprintln(os.Stderr)

	return cfg, nil
}

func saveExampleConfig() {
	dir, err := configDir()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "config.example.jsonc"), []byte(exampleConfig), 0o644)
}

// printStepDone prints a confirmation line that a step is done.
func printStepDone(label, value string) {
	fmt.Fprintf(os.Stderr, "  %s %s: %s\n",
		lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓"),
		label,
		lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(value))
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// ---------- TUI Components ----------

func runLanguageSelect() (setupLanguageOption, error) {
	items := make([]setupProvider, len(languageOptions))
	for i, opt := range languageOptions {
		items[i] = setupProvider{name: opt.code, label: opt.label}
	}
	m := setupSelectModel{
		title: "[1/5] Chọn ngôn ngữ sáng tác nội dung truyện (giao diện luôn là tiếng Việt)",
		items: items,
	}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return setupLanguageOption{}, err
	}
	result := final.(setupSelectModel)
	if result.cancelled {
		return setupLanguageOption{}, fmt.Errorf("đã hủy khởi tạo")
	}
	return languageOptions[result.cursor], nil
}

func runProviderSelect() (setupProvider, error) {
	m := setupSelectModel{
		title: "[2/5] Chọn provider AI",
		items: setupProviders,
	}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return setupProvider{}, err
	}
	result := final.(setupSelectModel)
	if result.cancelled {
		return setupProvider{}, fmt.Errorf("đã hủy khởi tạo")
	}
	return result.items[result.cursor], nil
}

var apiTypeOptions = []setupProvider{
	{name: "openai", label: "Chuẩn OpenAI (tương thích hầu hết các bên)"}, // case:ok (product name)
	{name: "anthropic", label: "Chuẩn Anthropic"},                         // case:ok (product name)
	{name: "gemini", label: "Chuẩn Google Gemini"},                        // case:ok (product name)
}

func runTypeSelect() (string, error) {
	m := setupSelectModel{
		title: "Loại giao thức API",
		items: apiTypeOptions,
	}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	result := final.(setupSelectModel)
	if result.cancelled {
		return "", fmt.Errorf("đã hủy khởi tạo")
	}
	return result.items[result.cursor].name, nil
}

func runTextInput(label, placeholder string) (string, error) {
	return runTextInputWithDefault(label, placeholder, "")
}

func runOptionalTextInput(label, placeholder string) (string, error) {
	m := setupInputModel{label: label, placeholder: placeholder, allowEmpty: true}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	result := final.(setupInputModel)
	if result.cancelled {
		return "", fmt.Errorf("đã hủy khởi tạo")
	}
	return utils.CleanInputLine(result.value), nil
}

func runTextInputWithDefault(label, placeholder, defaultValue string) (string, error) {
	m := setupInputModel{label: label, placeholder: placeholder, defaultValue: defaultValue}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	result := final.(setupInputModel)
	if result.cancelled {
		return "", fmt.Errorf("đã hủy khởi tạo")
	}
	if result.value == "" && result.defaultValue != "" {
		return result.defaultValue, nil
	}
	return utils.CleanInputLine(result.value), nil
}

// ---------- Select Component ----------

var (
	setupCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	setupDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	setupHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	setupInputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
)

type setupSelectModel struct {
	title     string
	items     []setupProvider
	cursor    int
	cancelled bool
}

func (m setupSelectModel) Init() tea.Cmd { return nil }

func (m setupSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter":
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m setupSelectModel) View() string {
	var b strings.Builder
	b.WriteString(setupHeaderStyle.Render(m.title))
	b.WriteString("\n\n")
	for i, item := range m.items {
		cursor := "  "
		label := item.label
		if i == m.cursor {
			cursor = setupCursorStyle.Render("❯ ")
			label = setupCursorStyle.Render(label)
		}
		b.WriteString(cursor + label + "\n")
	}
	b.WriteString(setupDimStyle.Render("\n  ↑↓ Chọn  Enter Xác nhận  Esc Hủy"))
	return b.String()
}

// ---------- Input Component ----------

type setupInputModel struct {
	label        string
	placeholder  string
	defaultValue string
	allowEmpty   bool
	value        string
	cancelled    bool
}

func (m setupInputModel) Init() tea.Cmd { return nil }

func (m setupInputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.String() {
		case "enter":
			if utils.CleanInputLine(m.value) != "" || m.defaultValue != "" || m.allowEmpty {
				return m, tea.Quit
			}
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "backspace":
			if len(m.value) > 0 {
				runes := []rune(m.value)
				m.value = string(runes[:len(runes)-1])
			}
		default:
			if msg.Type == tea.KeyRunes {
				m.value += utils.CleanInputRunes(msg.Runes)
			} else if msg.Type == tea.KeySpace {
				m.value += " "
			}
		}
	}
	return m, nil
}

func (m setupInputModel) View() string {
	var b strings.Builder
	b.WriteString(setupHeaderStyle.Render(m.label))
	b.WriteString("\n\n")
	b.WriteString(setupInputStyle.Render("❯ "))
	if m.value == "" {
		b.WriteString(setupCursorStyle.Render("▌"))
		b.WriteString(setupDimStyle.Render(m.placeholder))
	} else {
		b.WriteString(m.value)
		b.WriteString(setupCursorStyle.Render("▌"))
	}
	b.WriteString(setupDimStyle.Render("  (Enter Xác nhận, Esc Hủy)"))
	b.WriteString("\n")
	return b.String()
}
