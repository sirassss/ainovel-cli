package host

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

// Budget state machine: advances monotonically, each transition fires its side effect exactly once, and never moves back.
// Raising the budget = the user re-authorizing = changing config and restarting / a new Host instance; state is not rolled back within this instance.
const (
	budgetNormal      int32 = iota // below the warning level
	budgetWarned                   // warning issued, limit not crossed
	budgetStopPending              // limit crossed, waiting for a sub-agent boundary to stop
	budgetStopped                  // stop executed
)

// BudgetSentinel watches cumulative cost and enforces the user's budget policy (the config budget block).
//
// Constitutional positioning (architecture.md §8.3/§10): it does not evaluate model behavior; a stop on crossing the limit is equivalent to the user
// manually Aborting at that moment, with the Host merely executing a pre-signed instruction on their behalf. It affects control flow, so it
// is not an observer; it is positioned as a Host policy component on par with flow.Dispatcher, and Route/the tool layer are unaware of it.
//
// Stop timing: by default at a sub-agent boundary (the Host calls HandleBoundary synchronously), so the in-flight chapter is not wasted;
// with hardStop=true it stops immediately on crossing the limit. Boundary handling runs before flow.Dispatcher dispatches the next step; Route/the tool layer are unaware of the budget.
type BudgetSentinel struct {
	limit     float64
	warnRatio float64
	hardStop  bool

	costNow func() float64              // current cumulative cost (wraps usage.Totals; a test stub can be injected)
	abort   func(reason string)         // Host stop wrapper (emits an event with the reason)
	report  func(level, summary string) // alert outlet (emitEvent + notify, injected by the Host)

	state atomic.Int32

	// Billing blind-spot detection: a model with no price in the registry whose provider does not self-report cost adds $0 per record,
	// so the budget silently fails. It is judged by "many consecutive zero increments" rather than total==0, since the latter cannot catch the case where
	// /model switches mid-run to an unpriced model (total stays at a nonzero historical value but stops growing).
	// Free models hit it too, and the hint "the budget will not trigger" holds for them equally.
	lastTotal   atomic.Uint64 // math.Float64bits(cumulative cost at the last callback)
	zeroStreak  atomic.Int32
	blindWarned atomic.Bool
}

// blindZeroStreak is how many consecutive zero-increment records trigger the warning. A normally priced model's increment per record is always > 0
// (cost is a float accumulation, not rounded); 5 is chosen only to avoid extreme glitches and is not a tunable policy threshold.
const blindZeroStreak = 5

// NewBudgetSentinel creates the budget sentinel; it returns nil when the policy is not enabled (all methods are nil-safe).
func NewBudgetSentinel(cfg bootstrap.BudgetConfig, costNow func() float64, abort func(reason string), report func(level, summary string)) *BudgetSentinel {
	if !cfg.Enabled() {
		return nil
	}
	return &BudgetSentinel{
		limit:     cfg.BookUSD,
		warnRatio: cfg.WarnRatio,
		hardStop:  cfg.HardStop,
		costNow:   costNow,
		abort:     abort,
		report:    report,
	}
}

// OnCost is called by UsageTracker after each record with the latest cumulative cost (outside the lock).
// One callback may cross two levels at once (normal→warned→stopPending), and each of the two side effects fires once.
func (s *BudgetSentinel) OnCost(total float64) {
	if s == nil {
		return
	}
	if prev := s.lastTotal.Swap(math.Float64bits(total)); total == math.Float64frombits(prev) {
		if s.zeroStreak.Add(1) >= blindZeroStreak && s.blindWarned.CompareAndSwap(false, true) {
			s.report("warn", fmt.Sprintf("Điểm mù ngân sách: chi phí vẫn được ghi nhận nhưng tổng dừng ở $%.2f không tăng nữa (bảng giá model chưa có giá và provider không tự báo cost, hoặc là model miễn phí) — giới hạn ngân sách sẽ không kích hoạt", total))
		}
	} else {
		s.zeroStreak.Store(0)
	}
	if total >= s.limit*s.warnRatio && s.state.CompareAndSwap(budgetNormal, budgetWarned) {
		s.report("warn", fmt.Sprintf("Cảnh báo ngân sách: đã chi $%.2f trên ngân sách $%.2f (đạt ngưỡng cảnh báo %.0f%%)", total, s.limit, s.warnRatio*100))
	}
	if total >= s.limit && s.state.CompareAndSwap(budgetWarned, budgetStopPending) {
		if s.hardStop {
			s.report("error", fmt.Sprintf("Hết ngân sách: đã chi $%.2f, vượt ngân sách $%.2f, dừng ngay", total, s.limit))
			s.stop(total)
			return
		}
		s.report("error", fmt.Sprintf("Hết ngân sách: đã chi $%.2f, vượt ngân sách $%.2f, sẽ dừng sau khi nhiệm vụ subagent hiện tại kết thúc", total, s.limit))
	}
}

// HandleEvent executes a pending stop at a sub-agent boundary. The subscription must come before the Dispatcher.
// It does not skip IsError: an error return is equally a boundary, and the stop should not be delayed because a sub-agent failed.
func (s *BudgetSentinel) HandleEvent(ev agentcore.Event) {
	if s == nil {
		return
	}
	if ev.Type != agentcore.EventToolExecEnd || ev.Tool != "subagent" {
		return
	}
	s.HandleBoundary()
}

func (s *BudgetSentinel) HandleBoundary() bool {
	if s == nil || s.state.Load() != budgetStopPending {
		return false
	}
	s.stop(s.costNow())
	return true
}

func (s *BudgetSentinel) stop(total float64) {
	if s.state.CompareAndSwap(budgetStopPending, budgetStopped) {
		s.abort(fmt.Sprintf("Dừng do ngân sách: đã chi $%.2f, vượt ngân sách $%.2f; tăng budget.book_usd để có thể chạy tiếp", total, s.limit))
	}
}

// Refuse is the pre-start check: if the budget is already exceeded it returns a refusal error (called on the Start/Resume/Continue recovery paths).
// A user raising the budget = re-authorization, so under the new config Refuse naturally lets it through.
func (s *BudgetSentinel) Refuse() error {
	if s == nil {
		return nil
	}
	if cost := s.costNow(); cost >= s.limit {
		return fmt.Errorf("Cuốn sách này đã chi $%.2f, đạt giới hạn ngân sách $%.2f; hãy tăng cấu hình budget.book_usd rồi thử lại", cost, s.limit)
	}
	return nil
}

// Limit returns the budget cap (for UI display); it returns 0 when not enabled.
func (s *BudgetSentinel) Limit() float64 {
	if s == nil {
		return 0
	}
	return s.limit
}
