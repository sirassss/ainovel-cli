// Package notify provides the unattended alerting channel.
//
// Constitutional positioning (architecture.md §2.3): a pure observation-layer action - alerts never intervene in control flow
// (no retry, no reassignment, no halt); they only "shout" events already present in the TUI beyond the screen.
// Send runs asynchronously, never blocks the Host, and logs failures to slog only.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Notification carries all facts of one alert.
type Notification struct {
	Kind  string `json:"kind"`  // stable event name returned by Kinds
	Level string `json:"level"` // info / warn / error
	Title string `json:"title"`
	Body  string `json:"body"`
}

const (
	KindRunEnd        = "run_end"
	KindBudget        = "budget"
	KindAdvanceGate   = "advance_gate"
	KindStopGuard     = "stop_guard"
	KindPlanStart     = "plan_start"
	KindDeadlock      = "deadlock"
	KindWorkerFailure = "worker_failure"
)

// Kinds returns all event names usable in notify.events in the current version.
// This is the single source of truth for the notification event contract.
func Kinds() []string {
	return []string{
		KindRunEnd,
		KindBudget,
		KindAdvanceGate,
		KindStopGuard,
		KindPlanStart,
		KindDeadlock,
		KindWorkerFailure,
	}
}

func IsKnownKind(kind string) bool {
	for _, known := range Kinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// Notifier dispatches notifications per configuration. The zero value is unusable and must be created via New; nil-safe (Send is a no-op).
type Notifier struct {
	command string          // when non-empty, replaces the system channel (mobile push goes through here)
	events  map[string]bool // nil = all kinds allowed
	timeout time.Duration
}

// New creates a Notifier. An empty command uses the built-in system channel (Windows notification balloon /
// macOS osascript / Linux notify-send); when events is non-empty, only the listed kinds are allowed.
func New(command string, events []string) *Notifier {
	n := &Notifier{command: strings.TrimSpace(command), timeout: 10 * time.Second}
	if len(events) > 0 {
		n.events = make(map[string]bool, len(events))
		for _, ev := range events {
			n.events[ev] = true
		}
	}
	return n
}

// Send sends a notification asynchronously. Filtering, execution and failure handling never affect the caller.
func (n *Notifier) Send(nt Notification) {
	if !n.allows(nt.Kind) {
		return
	}
	go n.deliver(nt)
}

// allows reports whether the kind is allowed (blocked for a nil Notifier / when not listed in events).
func (n *Notifier) allows(kind string) bool {
	if n == nil {
		return false
	}
	return n.events == nil || n.events[kind]
}

// deliver synchronously performs one send and records failures; Send calls it in a goroutine.
func (n *Notifier) deliver(nt Notification) {
	if err := n.deliverError(nt); err != nil {
		slog.Warn("notification send failed", "module", "notify", "kind", nt.Kind, "err", err)
	}
}

// deliverError synchronously performs one send and returns the raw error. Send calls deliver in a goroutine
// to record failures; tests call this method directly so the error is not masked by a secondary symptom.
func (n *Notifier) deliverError(nt Notification) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
	defer cancel()

	if n.command != "" {
		return runCommand(ctx, n.command, nt)
	}
	return runSystem(ctx, nt)
}

// runCommand runs the user-configured command: fields are passed via environment variables (a one-line curl with zero dependencies and no injection
// risk), and the full JSON is also written to stdin (for complex dispatch scenarios to parse themselves). A timeout is enforced by killing via ctx.
func runCommand(ctx context.Context, command string, nt Notification) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		powershell, err := findPowerShell()
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Env = notificationEnv(nt)
	payload, _ := json.Marshal(nt)
	cmd.Stdin = strings.NewReader(string(payload))
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("Lệnh thông báo hết thời gian: %w", ctxErr)
		}
		return err
	}
	return nil
}

func notificationEnv(nt Notification) []string {
	return append(os.Environ(),
		"NOTIFY_KIND="+nt.Kind,
		"NOTIFY_LEVEL="+nt.Level,
		"NOTIFY_TITLE="+nt.Title,
		"NOTIFY_BODY="+nt.Body,
	)
}

// runSystem is the built-in desktop notification: it only covers the "person is at the computer" scenario, and silently degrades when no command is found.
func runSystem(ctx context.Context, nt Notification) error {
	switch runtime.GOOS {
	case "windows":
		return runWindowsNotification(ctx, nt)
	case "darwin":
		script := "display notification " + appleScriptString(nt.Body) + " with title " + appleScriptString(nt.Title)
		return exec.CommandContext(ctx, "osascript", "-e", script).Run()
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			slog.Debug("notification downgraded to log (no notify-send)", "module", "notify", "title", nt.Title, "body", nt.Body)
			return nil
		}
		return exec.CommandContext(ctx, "notify-send", nt.Title, nt.Body).Run()
	default:
		slog.Debug("notification downgraded to log (platform has no system channel)", "module", "notify", "title", nt.Title, "body", nt.Body)
		return nil
	}
}

// runWindowsNotification uses the system's own PowerShell + WinForms NotifyIcon.
// On Windows 10/11 the balloon appears in the upper-right corner and joins the system notification experience; there is no need to install a module, register an app
// or carry an extra binary. The caller already runs asynchronously, and the brief keep-alive only lets the system receive the balloon message.
func runWindowsNotification(ctx context.Context, nt Notification) error {
	powershell, err := findPowerShell()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, powershell,
		"-NoLogo", "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsNotificationScript)
	cmd.Env = notificationEnv(nt)
	return cmd.Run()
}

func findPowerShell() (string, error) {
	// Prefer PowerShell 7: on GitHub Windows runners and modern Windows environments, pwsh
	// behaves more stably with redirected stdin; Windows PowerShell 5.1 serves only as a compatibility fallback.
	for _, name := range []string{"pwsh.exe", "pwsh", "powershell.exe", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Thông báo Windows cần PowerShell, nhưng hệ thống không tìm thấy powershell.exe hoặc pwsh.exe")
}

const windowsNotificationScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$notify = New-Object System.Windows.Forms.NotifyIcon
$notify.Icon = [System.Drawing.SystemIcons]::Information
$notify.BalloonTipTitle = $env:NOTIFY_TITLE
$notify.BalloonTipText = $env:NOTIFY_BODY
$notify.BalloonTipIcon = switch ($env:NOTIFY_LEVEL) {
  'error' { [System.Windows.Forms.ToolTipIcon]::Error; break }
  'warn'  { [System.Windows.Forms.ToolTipIcon]::Warning; break }
  default { [System.Windows.Forms.ToolTipIcon]::Info }
}
$notify.Visible = $true
$notify.ShowBalloonTip(4000)
Start-Sleep -Milliseconds 4500
$notify.Dispose()`

// appleScriptString wraps arbitrary text as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
