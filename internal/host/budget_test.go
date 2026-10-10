package host

import (
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

type budgetRecorder struct {
	cost    float64
	aborts  []string
	reports []string
}

func (r *budgetRecorder) sentinel(cfg bootstrap.BudgetConfig) *BudgetSentinel {
	return NewBudgetSentinel(cfg,
		func() float64 { return r.cost },
		func(reason string) { r.aborts = append(r.aborts, reason) },
		func(level, summary string) { r.reports = append(r.reports, level+": "+summary) },
	)
}

func subagentEndEvent() agentcore.Event {
	return agentcore.Event{Type: agentcore.EventToolExecEnd, Tool: "subagent"}
}

func TestBudgetSentinelDisabled(t *testing.T) {
	r := &budgetRecorder{}
	if s := r.sentinel(bootstrap.BudgetConfig{}); s != nil {
		t.Fatal("disabled budget should return nil sentinel")
	}
	// nil safety
	var s *BudgetSentinel
	s.OnCost(100)
	s.HandleEvent(subagentEndEvent())
	if err := s.Refuse(); err != nil {
		t.Errorf("nil sentinel Refuse should pass: %v", err)
	}
	if s.Limit() != 0 {
		t.Error("nil sentinel Limit should be 0")
	}
}

func TestBudgetSentinelWarnOnceThenBoundaryStop(t *testing.T) {
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})

	// Below the warning level: no side effects
	s.OnCost(5)
	if len(r.reports) != 0 {
		t.Fatalf("below warn ratio should be silent, got %v", r.reports)
	}

	// Crossing the warning level: exactly one warn, repeated callbacks do not emit again
	s.OnCost(8.5)
	s.OnCost(9)
	if len(r.reports) != 1 || !strings.HasPrefix(r.reports[0], "warn:") {
		t.Fatalf("expected exactly one warn, got %v", r.reports)
	}

	// Crossing the limit: enters stopPending and emits error, but does not stop immediately (by default waits for the boundary)
	s.OnCost(10.5)
	if len(r.reports) != 2 || !strings.HasPrefix(r.reports[1], "error:") {
		t.Fatalf("expected error report on exceeding, got %v", r.reports)
	}
	if len(r.aborts) != 0 {
		t.Fatalf("default mode should not abort before boundary, got %v", r.aborts)
	}

	// Non-boundary events do not trigger
	s.HandleEvent(agentcore.Event{Type: agentcore.EventToolExecEnd, Tool: "novel_context"})
	if len(r.aborts) != 0 {
		t.Fatal("non-subagent boundary should not trigger stop")
	}

	// Sub-agent boundary: exactly one stop, repeated boundaries do not stop again
	r.cost = 10.5
	if !s.HandleBoundary() {
		t.Fatal("pending budget stop should be handled at boundary")
	}
	if s.HandleBoundary() {
		t.Fatal("stopped budget should not report another handled boundary")
	}
	if len(r.aborts) != 1 {
		t.Fatalf("expected exactly one abort at boundary, got %v", r.aborts)
	}
}

func TestBudgetSentinelJumpStraightPastLimit(t *testing.T) {
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})

	// One callback crosses both the warning level and the limit: warn and error each fire exactly once
	s.OnCost(12)
	if len(r.reports) != 2 {
		t.Fatalf("expected warn+error in single jump, got %v", r.reports)
	}
}

func TestBudgetSentinelHardStop(t *testing.T) {
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8, HardStop: true})

	s.OnCost(11)
	if len(r.aborts) != 1 {
		t.Fatalf("hard_stop should abort immediately, got %v", r.aborts)
	}
	// Later boundaries do not repeat the stop
	r.cost = 11
	s.HandleEvent(subagentEndEvent())
	if len(r.aborts) != 1 {
		t.Fatalf("stopped state should not abort again, got %v", r.aborts)
	}
}

func TestBudgetSentinelRefuse(t *testing.T) {
	r := &budgetRecorder{cost: 9.99}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})

	if err := s.Refuse(); err != nil {
		t.Errorf("below limit should pass: %v", err)
	}
	r.cost = 10 // exactly equal to the limit → refused
	if err := s.Refuse(); err == nil {
		t.Error("at limit should refuse")
	} else if !strings.Contains(err.Error(), "book_usd") {
		t.Errorf("refuse error should mention how to recover, got %v", err)
	}
}

func TestBudgetSentinelZeroCostBlindWarning(t *testing.T) {
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})

	// Consecutive zero-cost records: at blindZeroStreak records exactly one blind-spot warning fires, then silence
	for range blindZeroStreak + 3 {
		s.OnCost(0)
	}
	if len(r.reports) != 1 || !strings.Contains(r.reports[0], "Điểm mù ngân sách") {
		t.Fatalf("expected exactly one blind warning, got %v", r.reports)
	}
	if len(r.aborts) != 0 {
		t.Fatal("blind warning must not abort")
	}

	// A normally priced model must not false-alarm: the total grows with each record
	r2 := &budgetRecorder{}
	s2 := r2.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})
	for i := range blindZeroStreak + 3 {
		s2.OnCost(0.1 * float64(i+1))
	}
	for _, rep := range r2.reports {
		if strings.Contains(rep, "Điểm mù ngân sách") {
			t.Fatalf("priced model should not trigger blind warning: %v", r2.reports)
		}
	}
}

func TestBudgetSentinelBlindWarningAfterModelSwitch(t *testing.T) {
	// /model switches mid-run to an unpriced model: total stays at a nonzero historical value but stops growing, which must also warn
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 100, WarnRatio: 0.8})

	for i := range 5 {
		s.OnCost(1.0 * float64(i+1)) // pricing phase: total grows up to $5
	}
	for range blindZeroStreak {
		s.OnCost(5.0) // switched to an unpriced model: total pinned
	}
	if len(r.reports) != 1 || !strings.Contains(r.reports[0], "Điểm mù ngân sách") {
		t.Fatalf("expected blind warning after switch to unpriced model, got %v", r.reports)
	}
}

func TestBudgetSentinelWarnMessageKeepsFiguresInOrder(t *testing.T) {
	r := &budgetRecorder{}
	s := r.sentinel(bootstrap.BudgetConfig{BookUSD: 10, WarnRatio: 0.8})
	s.OnCost(8.5)
	if len(r.reports) != 1 {
		t.Fatalf("reports = %v", r.reports)
	}
	msg := r.reports[0]
	for _, want := range []string{"đã chi $8.50", "ngân sách $10.00", "80%"} {
		if !strings.Contains(msg, want) {
			t.Errorf("thông báo thiếu %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "$80.00") || strings.Contains(msg, "10%") {
		t.Errorf("số liệu bị đảo vị trí: %s", msg)
	}
}
