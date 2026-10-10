package assets

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/voocel/ainovel-cli/internal/tools"
)

//go:embed prompts
var promptsFS embed.FS

//go:embed references
var referencesFS embed.FS

//go:embed styles
var stylesFS embed.FS

//go:embed voice.md voice_zh.md
var voiceFS embed.FS

// Prompts holds the embedded set of prompts.
type Prompts struct {
	ArchitectShort   string
	ArchitectLong    string
	Writer           string // Template protocol with a {{VOICE}} placeholder; the final text is assembled by BuildWriterPrompt
	Editor           string
	ImportSegment    string // Semantic segmentation: recognizes chapter/volume/auxiliary-section boundaries
	ImportAnalyze    string // Per-chapter facts extraction
	ImportSynthesize string // Layered synthesis and volume/arc division of the whole book (BookSynthesis)
	ImportRange      string // Digest of a contiguous range in the Map phase (RangeDigest)
	SimulationSource string
	SimulationMerge  string
	RevisionAnalyze  string

	// Arbiter adjudication (LLM-as-function, not wrapped with simulation guidance)
	ArbiterPlanStart    string
	ArbiterIntervention string
	ArbiterFailure      string
}

// Bundle holds the static resources needed at runtime.
type Bundle struct {
	References tools.References
	Prompts    Prompts
	Styles     map[string]string
	Voice      string // Writing standard, assembled through 3 override tiers
	Language   string // "vi" or "zh"
}

// LoadOptions declares the override sources of the style tier.
type LoadOptions struct {
	BookStyleDir string // <outputDir>/style
	HomeStyleDir string // ~/.ainovel/style
}

// DefaultLoadOptions builds the override sources from the book directory.
func DefaultLoadOptions(outputDir string) LoadOptions {
	var opts LoadOptions
	if outputDir != "" {
		opts.BookStyleDir = filepath.Join(outputDir, "style")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		opts.HomeStyleDir = filepath.Join(home, ".ainovel", "style")
	}
	return opts
}

// Load returns the resources for the given style and story language (default vi).
func Load(style string, opts LoadOptions) Bundle {
	return LoadWithLanguage("vi", style, opts)
}

// LoadWithLanguage loads resources for the given language ("vi" or "zh") and style.
func LoadWithLanguage(language, style string, opts LoadOptions) Bundle {
	lang := strings.ToLower(strings.TrimSpace(language))
	if lang != "zh" && lang != "chinese" && lang != "cn" {
		lang = "vi"
	}
	return Bundle{
		References: loadReferences(lang, style, opts),
		Prompts:    loadPrompts(lang),
		Styles:     loadStyles(lang, opts),
		Voice:      resolveAppendable(loadVoice(lang), "voice.md", opts),
		Language:   lang,
	}
}

const voicePlaceholder = "{{VOICE}}"

// BuildWriterPrompt is the single prompt-assembly gate of the Writer.
func BuildWriterPrompt(writerPrompt, voice, style string) string {
	out := strings.Replace(writerPrompt, voicePlaceholder, strings.TrimSpace(voice), 1)
	if style != "" {
		out += "\n\n" + style
	}
	return out
}

// OverrideVoice replaces the assembled voice section (for A/B experiments).
func (b *Bundle) OverrideVoice(raw string) {
	b.Voice = raw
}

func resolveAppendable(builtin, name string, opts LoadOptions) string {
	out := builtin
	if s := readOverride(opts.HomeStyleDir, name); s != "" {
		out += "\n\n## Người dùng ghi đè văn phong toàn cục (User Global Style Override)\n\n" + s
	}
	if s := readOverride(opts.BookStyleDir, name); s != "" {
		out += "\n\n## Ghi đè văn phong cuốn sách này (Book Style Override)\n\n" + s
	}
	return out
}

func readOverride(dir, name string) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

var styleNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func loadVoice(language string) string {
	if language == "zh" {
		if data, err := voiceFS.ReadFile("voice_zh.md"); err == nil {
			return string(data)
		}
	}
	return mustRead(voiceFS, "voice.md")
}

func loadReferences(language, style string, opts LoadOptions) tools.References {
	if style == "" {
		style = "default"
	}
	prefix := "references/"
	if language == "zh" {
		prefix = "references/zh/"
	}
	readRef := func(rel string) string {
		if data, err := referencesFS.ReadFile(prefix + rel); err == nil {
			return string(data)
		}
		return mustRead(referencesFS, "references/"+rel)
	}

	refs := tools.References{
		ChapterGuide:      readRef("chapter-guide.md"),
		HookTechniques:    readRef("hook-techniques.md"),
		QualityChecklist:  readRef("quality-checklist.md"),
		OutlineTemplate:   readRef("outline-template.md"),
		CharacterTemplate: readRef("character-template.md"),
		ChapterTemplate:   readRef("chapter-template.md"),
		Consistency:       readRef("consistency.md"),
		ContentExpansion:  readRef("content-expansion.md"),
		DialogueWriting:   readRef("dialogue-writing.md"),
		LongformPlanning:  readRef("longform-planning.md"),
		Differentiation:   readRef("differentiation.md"),
		AntiAITone:        resolveAppendable(readRef("anti-ai-tone.md"), "anti-ai-tone.md", opts),
	}
	if style != "" && style != "default" {
		genreDir := prefix + "genres/" + style + "/"
		if data, err := referencesFS.ReadFile(genreDir + "style-references.md"); err == nil {
			refs.StyleReference = string(data)
		} else if data, err := referencesFS.ReadFile("references/genres/" + style + "/style-references.md"); err == nil {
			refs.StyleReference = string(data)
		}
		if data, err := referencesFS.ReadFile(genreDir + "arc-templates.md"); err == nil {
			refs.ArcTemplates = string(data)
		} else if data, err := referencesFS.ReadFile("references/genres/" + style + "/arc-templates.md"); err == nil {
			refs.ArcTemplates = string(data)
		}
		relPath := filepath.Join("genres", style, "style-references.md")
		for _, dir := range []string{opts.HomeStyleDir, opts.BookStyleDir} {
			if s := readOverride(dir, relPath); s != "" {
				refs.StyleReference = s
			}
		}
	}
	return refs
}

func loadPrompts(languages ...string) Prompts {
	language := "vi"
	if len(languages) > 0 && languages[0] != "" {
		language = languages[0]
	}
	prefix := "prompts/"
	if language == "zh" {
		prefix = "prompts/zh/"
	}
	readPrompt := func(filename string) string {
		if data, err := promptsFS.ReadFile(prefix + filename); err == nil {
			return string(data)
		}
		return mustRead(promptsFS, "prompts/"+filename)
	}

	return Prompts{
		ArchitectShort:   WithSimulationGuidance(readPrompt("architect-short.md"), "architect", language),
		ArchitectLong:    WithSimulationGuidance(readPrompt("architect-long.md"), "architect", language),
		Writer:           WithSimulationGuidance(readPrompt("writer.md"), "writer", language),
		Editor:           WithSimulationGuidance(readPrompt("editor.md"), "editor", language),
		ImportSegment:    readPrompt("import-segment.md"),
		ImportAnalyze:    readPrompt("import-analyze.md"),
		ImportSynthesize: readPrompt("import-synthesize.md"),
		ImportRange:      readPrompt("import-range.md"),
		SimulationSource: readPrompt("simulation-source.md"),
		SimulationMerge:  readPrompt("simulation-merge.md"),
		RevisionAnalyze:  readPrompt("revision-analyze.md"),

		ArbiterPlanStart:    readPrompt("arbiter-plan-start.md"),
		ArbiterIntervention: readPrompt("arbiter-intervention.md"),
		ArbiterFailure:      readPrompt("arbiter-failure.md"),
	}
}

// WithSimulationGuidance appends voice-simulation guidance by role and language.
func WithSimulationGuidance(prompt, role string, language ...string) string {
	lang := "vi"
	if len(language) > 0 && language[0] == "zh" {
		lang = "zh"
	}
	guidance := simulationGuidanceVI
	if lang == "zh" {
		guidance = simulationGuidanceZH
	}
	return prompt + "\n\n" + strings.ReplaceAll(guidance, "{{role}}", role)
}

// OverridePrompt overrides the prompt of a specific role.
func (b *Bundle) OverridePrompt(file, raw string) error {
	role, ok := promptRole[file]
	if !ok {
		return fmt.Errorf("không hỗ trợ ghi đè file prompt: %s (chỉ có thể ghi đè prompt vai trò cốt lõi)", file)
	}
	wrapped := WithSimulationGuidance(raw, role, b.Language)
	switch file {
	case "architect-short.md":
		b.Prompts.ArchitectShort = wrapped
	case "architect-long.md":
		b.Prompts.ArchitectLong = wrapped
	case "writer.md":
		b.Prompts.Writer = wrapped
	case "editor.md":
		b.Prompts.Editor = wrapped
	}
	return nil
}

var promptRole = map[string]string{
	"architect-short.md": "architect",
	"architect-long.md":  "architect",
	"writer.md":          "writer",
	"editor.md":          "editor",
}

const simulationGuidanceVI = `## Hồ sơ mô phỏng văn phong (Simulation Profile)

Khi trong planning_memory hoặc working_memory của novel_context xuất hiện simulation_profile, bắt buộc phải xem đó là ràng buộc định hướng mô phỏng của tác phẩm hiện tại. {{role}} cần đọc kỹ các trường style, lexicon, plot_design, hook_design, pacing_density, reader_engagement và role_guidance.

Nguyên tắc sử dụng: Học hỏi cấu trúc, nhịp điệu, hook, cách giải phóng thông tin và thủ pháp cuốn hút độc giả; tuyệt đối không sao chép câu văn nguyên văn, tên nhân vật, địa danh, thiết lập độc quyền hay phân đoạn cố định. Nếu simulation_profile xung đột với yêu cầu rõ ràng của người dùng, ưu tiên tuân thủ yêu cầu của người dùng.`

// Voice-simulation guidance for the zh edition (simulationGuidanceZH). han:keep
const simulationGuidanceZH = `## 仿写画像

当 novel_context 的 planning_memory 或 working_memory 中存在 simulation_profile 时，必须把它视为当前作品的仿写方向约束。{{role}} 应读取其中的 style、lexicon、plot_design、hook_design、pacing_density、reader_engagement 和 role_guidance。

使用原则：借鉴结构、节奏、钩子、信息释放和吸引读者的手法；不要复制原文句子、人物、地名、专有设定或固定桥段。若 simulation_profile 与用户显式要求冲突，优先服从用户要求。`

func loadStyles(language string, opts LoadOptions) map[string]string {
	styles := make(map[string]string)
	prefix := "styles"
	if language == "zh" {
		prefix = "styles/zh"
	}
	entries, err := stylesFS.ReadDir(prefix)
	if err != nil {
		prefix = "styles"
		entries, err = stylesFS.ReadDir(prefix)
	}
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			data, err := stylesFS.ReadFile(prefix + "/" + e.Name())
			if err != nil {
				continue
			}
			styles[name] = string(data)
		}
	}
	for _, dir := range []string{opts.HomeStyleDir, opts.BookStyleDir} {
		overlayStyles(styles, dir)
	}
	return styles
}

func overlayStyles(styles map[string]string, dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dir, "styles"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if !styleNameRe.MatchString(name) {
			slog.Warn("Bỏ qua tên file style không hợp lệ", "module", "assets", "dir", dir, "file", e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "styles", e.Name()))
		if err != nil {
			continue
		}
		styles[name] = string(data)
	}
}

func mustRead(fs embed.FS, path string) string {
	data, err := fs.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("embed read %s: %v", path, err))
	}
	return string(data)
}
