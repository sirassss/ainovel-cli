package headless

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

type Options struct {
	Prompt string
	Stdout io.Writer
	Stderr io.Writer
}

// Run runs the session kernel in UI-less mode, consuming Engine events and streaming output directly.
// If a shared startup path such as "continue an existing novel" is added later, it should not be
// piled in here; land it in internal/entry/startup first and let the headless entry call it.
func Run(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	eng, err := host.New(cfg, bundle, host.WithFileLog("headless.log", false))
	if err != nil {
		return err
	}
	defer eng.Close()
	if logErr := eng.FileLogError(); logErr != nil {
		fmt.Fprintf(stderr, "Cảnh báo: nhật ký tệp không dùng được, tiếp tục dùng nhật ký terminal: %v\n", logErr)
	}
	// When the run finishes or returns with an error, write a redacted diagnostic so headless users
	// can paste it into an issue. (A hang ended by an external kill skips defer; run /diag manually in the TUI.)
	defer func() {
		if _, err := diag.Export(store.NewStore(eng.Dir())); err != nil {
			fmt.Fprintf(stderr, "Cảnh báo: không xuất được báo cáo chẩn đoán: %v\n", err)
		}
	}()

	prompt := strings.TrimSpace(opts.Prompt)
	if prompt != "" {
		prompt, err = startup.PrepareQuick(prompt)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "headless khởi động: %s\n", eng.Dir())
		// Deterministically generate this book's user-rule snapshot on the startup side (normalized from the original prompt); must happen before StartPrepared.
		if err := eng.PrepareUserRules(prompt); err != nil {
			return err
		}
		if err := eng.StartPrepared(prompt); err != nil {
			return err
		}
	} else {
		items, err := eng.ReplayQueue(0)
		if err != nil {
			return err
		}
		replayQueue(items, stderr)
		label, err := eng.Resume()
		if err != nil {
			return err
		}
		if label == "" {
			return fmt.Errorf("Chế độ headless cần --prompt, hoặc thư mục đầu ra %q đã có phiên có thể khôi phục", eng.Dir())
		}
		fmt.Fprintf(stderr, "headless khôi phục: %s (%s)\n", eng.Dir(), label)
		return consume(eng, stdout, stderr, false)
	}

	return consume(eng, stdout, stderr, false)
}

func consume(eng *host.Host, stdout, stderr io.Writer, roundHasContent bool) error {
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				return nil
			}
			writeEvent(stderr, ev)
		case delta, ok := <-eng.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return err
					}
					roundHasContent = false
				}
				continue
			}
			if delta == "" {
				continue
			}
			if _, err := io.WriteString(stdout, delta); err != nil {
				return err
			}
			roundHasContent = true
		case _, ok := <-eng.Done():
			if !ok {
				return nil
			}
			return drainPending(eng, stdout, stderr, roundHasContent)
		}
	}
}

func drainPending(eng *host.Host, stdout, stderr io.Writer, roundHasContent bool) error {
	for {
		select {
		case ev, ok := <-eng.Events():
			if ok {
				writeEvent(stderr, ev)
			}
		case delta, ok := <-eng.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return err
					}
					roundHasContent = false
				}
				continue
			}
			if delta != "" {
				if _, err := io.WriteString(stdout, delta); err != nil {
					return err
				}
				roundHasContent = true
			}
		default:
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n"); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

func writeEvent(w io.Writer, ev host.Event) {
	if w == nil || strings.TrimSpace(ev.Summary) == "" {
		return
	}
	ts := ev.Time.Format("15:04:05")
	if ts == "00:00:00" {
		ts = "--:--:--"
	}
	fmt.Fprintf(w, "[%s] [%s] %s\n", ts, ev.Category, ev.Summary)
}

func replayQueue(items []domain.RuntimeQueueItem, stderr io.Writer) {
	for _, item := range items {
		writeEvent(stderr, host.Event{
			Time:     item.Time,
			Category: item.Category,
			Summary:  item.Summary,
		})
	}
}
