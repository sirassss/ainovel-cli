package tui

import (
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/host"
	buildversion "github.com/voocel/ainovel-cli/internal/version"
)

// Run starts the TUI.
// Startup-mode layering convention:
// 1. Quick mode and co-create mode belong to "startup orchestration";
// 2. The actual creation session enters host.Host;
// 3. If a shared mode such as "continue an existing novel" is added later, it goes into internal/entry/startup.
func Run(cfg bootstrap.Config, bundle assets.Bundle, build buildversion.Info) error {
	rt, err := host.New(cfg, bundle, host.WithFileLog("tui.log", false,
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("built", build.Date),
	))
	if err != nil {
		return err
	}
	defer rt.Close()

	m := NewModel(rt, build.Version)
	if logErr := rt.FileLogError(); logErr != nil {
		logWarning := fmt.Errorf("Nhật ký tệp không dùng được, đã tiếp tục dùng nhật ký terminal: %w", logErr)
		m.err = logWarning
		m.applyEvent(host.Event{
			Time: time.Now(), Category: "SYSTEM", Level: "warn",
			Summary: logWarning.Error(), Detail: logWarning.Error(),
		})
	}
	// Mouse reporting is not enabled globally at startup: the welcome screen does not use the mouse, and keeping reporting off preserves the terminal's native
	// drag-to-select copy. When entering the creation workbench (modeRunning), enterRunning turns reporting on
	// to support click-to-switch panels / wheel scrolling / dragging the sidebar.
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}
