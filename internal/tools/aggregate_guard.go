package tools

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/store"
)

// requireAggregateTarget binds Editor's new aggregate write to the single artifact the Router currently needs.
// The target is derived entirely from facts already on disk; it does not rely on the task text and does not trust model-supplied chapter/volume/arc numbers.
// Idempotent finishing for content already on disk is detected by each tool before calling this function.
func requireAggregateTarget(st *store.Store, kind flow.AggregateKind, volume, arc, endChapter int) error {
	state, err := flow.LoadState(st)
	if err != nil {
		return fmt.Errorf("load aggregate state: %w: %w", errs.ErrStoreRead, err)
	}
	due := state.AggregateRefresh
	if due == nil {
		return fmt.Errorf("Hiện không có sản phẩm %s nào cần xử lý: %w", kind, errs.ErrToolPrecondition)
	}
	targetMismatch := due.Kind != kind
	switch kind {
	case flow.AggregateArcReview, flow.AggregateArcSummary:
		targetMismatch = targetMismatch || due.Volume != volume || due.Arc != arc
	case flow.AggregateVolumeSummary:
		targetMismatch = targetMismatch || due.Volume != volume
	case flow.AggregateGlobalReview:
		// A global review has no volume/arc coordinates; it is located only by kind and the cutoff chapter.
	}
	endMismatch := endChapter > 0 && due.EndChapter != endChapter
	if targetMismatch || endMismatch {
		return fmt.Errorf(
			"Mục tiêu ghi tổng hợp không khớp: hiện cần xử lý kind=%s volume=%d arc=%d end_chapter=%d, nhận được kind=%s volume=%d arc=%d end_chapter=%d: %w",
			due.Kind, due.Volume, due.Arc, due.EndChapter,
			kind, volume, arc, endChapter, errs.ErrToolConflict,
		)
	}
	return nil
}
