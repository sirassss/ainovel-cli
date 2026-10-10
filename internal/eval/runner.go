package eval

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
)

// RunOptions controls a single case run.
type RunOptions struct {
	OutputDir string        // isolated output directory (required)
	Timeout   time.Duration // wall-clock limit per case; 0 means unlimited
	Progress  io.Writer     // progress line output (optional; nil prints nothing)
}

// RunCase drives one case: assemble the host -> start -> advance up to the chapter cap -> Abort
// when the cap is reached. The caller has already applied any variant override to bundle. The
// returned error is the "runtime error" (the basis for a hard fail); a normal finish or a normal
// cap stop both return nil.
//
// RunCase takes exclusive ownership of OutputDir and resets it: StartPrepared only resets
// progress/checkpoints and leaves artifacts such as chapters/ and foundation untouched, so reusing
// an old directory would let leftovers pollute diag and novel_context. The directory is therefore
// cleared before the run to guarantee isolation.
func RunCase(cfg bootstrap.Config, bundle assets.Bundle, c Case, opts RunOptions) error {
	if strings.TrimSpace(opts.OutputDir) == "" {
		return fmt.Errorf("RunCase: missing OutputDir")
	}
	if err := os.RemoveAll(opts.OutputDir); err != nil {
		return fmt.Errorf("clean output directory: %w", err)
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	cfg.OutputDir = opts.OutputDir
	if c.Style != "" {
		cfg.Style = c.Style
	}

	eng, err := host.New(cfg, bundle, host.WithFileLog("headless.log", false))
	if err != nil {
		return fmt.Errorf("assemble host: %w", err)
	}
	defer eng.Close()
	if logErr := eng.FileLogError(); logErr != nil {
		return fmt.Errorf("evaluation file log unavailable: %w", logErr)
	}

	prompt, err := startup.PrepareQuick(c.Prompt)
	if err != nil {
		return err
	}
	if err := eng.PrepareUserRules(prompt); err != nil {
		return fmt.Errorf("prepare user rules: %w", err)
	}
	if err := eng.StartPrepared(prompt); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	return drive(eng, c.MaxChapters, opts)
}

// driveEngine is the minimal engine interface drive consumes (*host.Host satisfies it naturally).
// It is extracted so the drain-to-Done discipline can be tested deterministically; this
// concurrency logic once hit a send-on-closed-channel bug.
type driveEngine interface {
	Events() <-chan host.Event
	Stream() <-chan string
	Done() <-chan struct{}
	Snapshot() host.UISnapshot
	Abort() bool
}

// drive consumes the engine event stream, calls Abort on reaching the chapter cap or timing out,
// and waits for Done to wind down.
//
// Key discipline: whether the run completes normally, stops at the chapter cap or times out, it
// must drain to Done before returning. The host's background waitDone sends on done once, while
// eng.Close() (RunCase's defer) closes done; returning early and triggering Close would race with
// waitDone's send on the channel and panic (send on closed channel). Headless relies on the same
// "Done first, then Close" order. Events and Stream must also be drained so the engine is never
// blocked.
func drive(eng driveEngine, maxChapters int, opts RunOptions) error {
	var timeoutCh <-chan time.Time
	if opts.Timeout > 0 {
		t := time.NewTimer(opts.Timeout)
		defer t.Stop()
		timeoutCh = t.C
	}

	aborted, timedOut := false, false
	// finish is called after draining to Done (or the channel closing): it returns an error on
	// timeout and ends normally otherwise.
	finish := func() error {
		if timedOut {
			return fmt.Errorf("run timed out (%s)", opts.Timeout)
		}
		return nil
	}
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				return finish()
			}
			if opts.Progress != nil && strings.TrimSpace(ev.Summary) != "" {
				fmt.Fprintf(opts.Progress, "    [%s] %s\n", ev.Category, ev.Summary)
			}
			if !aborted && capReached(eng.Snapshot(), maxChapters) {
				eng.Abort()
				aborted = true
				timeoutCh = nil // the stop condition is met; move on to a normal wind-down that is no longer bound by the
				// timeout (so a successful cap stop is not misreported as a timeout)
			}
		case <-eng.Stream():
			// Drain streaming deltas without consuming the content: eval ignores the text stream and only
			// looks at what was persisted.
		case _, ok := <-eng.Done():
			if !ok {
				return finish()
			}
			return finish()
		case <-timeoutCh:
			eng.Abort() // aborted is always false here (a cap stop sets timeoutCh to nil)
			aborted, timedOut = true, true
			timeoutCh = nil // disable the timer, keep draining until Done, then finish returns the timeout error
		}
	}
}

// capReached reports whether the stop condition is met. With maxChapters>0 it counts completed
// chapters; <=0 is treated as "planning only" and stops once planning finishes (entering writing
// or already complete).
func capReached(snap host.UISnapshot, maxChapters int) bool {
	if maxChapters <= 0 {
		return snap.Phase == string(domain.PhaseWriting) || snap.Phase == string(domain.PhaseComplete)
	}
	return snap.CompletedCount >= maxChapters
}
