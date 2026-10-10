package tui

import "github.com/charmbracelet/lipgloss"

// Theme palette -- warm, bookish tone
// AdaptiveColor: Light = light-background value, Dark = dark-background value
//
// Design principle: the Light tier stays stable and untouched (the light background was already tuned to a satisfying result); the Dark tier is uniformly
// ~25% lighter than Light with slightly higher saturation, ensuring enough contrast on dark backgrounds (colorDim used to be #6b6355
// and was almost invisible on a #1c1c1c black background, so separators / auxiliary text all vanished).
//
// colorAccent2 on dark changed from #7a9e7e to teal #5fb8a3 to set it apart from colorSuccess's "healthy green"
// -- the two used to be exactly the same color, confusing the architect agent's color tag with the joy of a "high hit rate".
// bodyTextColor is the foreground policy for "neutral body text":
//   - Dark terminal -> NoColor, inherit the terminal default foreground, to avoid us forcing #e8e0d0 off-white and clashing
//     with the user's own warm/cool background themes (users found the dark-background default color easier to read).
//   - Light terminal -> use the Light tier of colorText (dark brown #3d3529) to keep the brand's warm tone;
//     the default black on light backgrounds has too harsh a contrast, and the tuned dark brown looks softer on light backgrounds.
//
// AdaptiveColor requires color values at both ends and has no "no color" tier, so the background is detected once at startup here,
// and afterward all "neutral body text" such as overview values / chapter text / command descriptions uniformly references bodyTextColor.
var bodyTextColor lipgloss.TerminalColor = func() lipgloss.TerminalColor {
	if lipgloss.HasDarkBackground() {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color("#3d3529")
}()

var (
	colorText    = lipgloss.AdaptiveColor{Light: "#3d3529", Dark: "#e8e0d0"}
	colorDim     = lipgloss.AdaptiveColor{Light: "#8a7e6b", Dark: "#8a8175"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#7a7060", Dark: "#b8b09c"}
	colorAccent  = lipgloss.AdaptiveColor{Light: "#b8860b", Dark: "#e5b449"}
	colorAccent2 = lipgloss.AdaptiveColor{Light: "#3d7a42", Dark: "#5fb8a3"}
	colorRunning = lipgloss.AdaptiveColor{Light: "#6f8641", Dark: "#b5d075"}
	colorSuccess = lipgloss.AdaptiveColor{Light: "#3d7a42", Dark: "#7ec488"}
	colorError   = lipgloss.AdaptiveColor{Light: "#b5433a", Dark: "#e07060"}
	colorReview  = lipgloss.AdaptiveColor{Light: "#b07530", Dark: "#e09b5a"}
	colorContext = lipgloss.AdaptiveColor{Light: "#6b5a9e", Dark: "#a890d8"}
	colorTool    = lipgloss.AdaptiveColor{Light: "#3a7a8a", Dark: "#7ec5d8"}
)

// Status label color mapping
var statusColors = map[string]lipgloss.AdaptiveColor{
	"READY":    colorDim,
	"PAUSING":  colorAccent,
	"PAUSED":   colorAccent,
	"RUNNING":  colorRunning,
	"REVIEW":   colorReview,
	"REWRITE":  colorReview,
	"COMPLETE": colorSuccess,
	"ERROR":    colorError,
}

// Status display: icon + label. Consistent with the overall warm theme, avoiding jarring solid color blocks.
// The RUNNING icon is left empty and filled dynamically by the spinner frame, blending the sense of motion into the status indicator itself.
var statusDisplay = map[string]struct {
	icon  string
	label string
}{
	"READY":    {"○", "Sẵn sàng"},
	"RUNNING":  {"", "Đang chạy"},
	"REVIEW":   {"◆", "Đánh giá"},
	"REWRITE":  {"◆", "Viết lại"},
	"COMPLETE": {"●", "Hoàn tất"},
	"PAUSED":   {"⏸", "Tạm dừng"},
	"PAUSING":  {"⏸", "Đang tạm dừng"},
	"ERROR":    {"✕", "Lỗi"},
}

// Event category color mapping
var categoryColors = map[string]lipgloss.AdaptiveColor{
	"DISPATCH": colorAccent,
	"DECISION": colorContext,
	"TOOL":     colorTool,
	"SYSTEM":   colorAccent,
	"USER":     colorAccent2,
	"REVIEW":   colorReview,
	"CHECK":    colorSuccess,
	"ERROR":    colorError,
	"AGENT":    colorMuted,
	"CONTEXT":  colorContext,
	"COMPACT":  colorContext,
}

// Base styles
var (
	baseBorder = lipgloss.RoundedBorder()

	topBarStyle = lipgloss.NewStyle().
			Foreground(colorText).
			Padding(0, 1)

	statusIconStyle = lipgloss.NewStyle().
			Bold(true)

	statusLabelStyle = lipgloss.NewStyle().
				Foreground(colorText)

	panelTitleStyle = lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true)

	fieldLabelStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Width(10)

	// fieldValueStyle / cardContentStyle use bodyTextColor -- overview values (run state,
	// completed chapter count, word count, etc.), outline entries, character lists, chapter summaries and other "neutral body content"
	// follow the terminal default foreground on dark backgrounds (avoiding forcing off-white that clashes with themes) and use dark brown on light backgrounds to keep the warm tone.
	// Strongly semantic elements (titles, highlighted values, status, errors, hit-rate coloring, etc.) still use the theme colors such as colorAccent /
	// colorError.
	fieldValueStyle = lipgloss.NewStyle().Foreground(bodyTextColor)

	highlightValueStyle = lipgloss.NewStyle().
				Foreground(colorAccent).
				Bold(true)

	contextUsageMetaStyle = lipgloss.NewStyle().
				Foreground(colorDim)

	cardTitleStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)

	cardContentStyle = lipgloss.NewStyle().Foreground(bodyTextColor)
)
