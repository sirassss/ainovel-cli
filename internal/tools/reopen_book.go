package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ReopenBookTool reopens a finished book into the rework state; the Engine calls it at an intervention-action boundary.
// After completion, completePhaseGate hard-blocks every subagent dispatch, so the user cannot rework chapters already written.
// This tool is not a subagent and is callable in the complete phase: it atomically switches phase back to writing, puts the target chapters into
// PendingRewrites and sets flow=rewriting; the Flow Router then dispatches the writer chapter by chapter through the existing rewrite queue,
// and when the queue finishes, commit_chapter automatically wraps up the book again. No heavy changes to Gate / Router / edit / commit logic are needed.
type ReopenBookTool struct {
	store *store.Store
}

func NewReopenBookTool(s *store.Store) *ReopenBookTool {
	return &ReopenBookTool{store: s}
}

func (t *ReopenBookTool) Name() string  { return "reopen_book" }
func (t *ReopenBookTool) Label() string { return "Mở lại để viết lại" }

func (t *ReopenBookTool) Description() string {
	return "Mở lại cả truyện đã kết thúc (phase=complete) sang trạng thái viết lại, dùng khi người dùng sau khi kết thúc truyện yêu cầu viết lại/trau chuốt vài chương." +
		"chapters là các số chương đã hoàn thành cần viết lại; sau khi gọi, các chương này vào hàng đợi viết lại, Host sẽ lần lượt giao writer viết lại từng chương, sửa xong hết sẽ lại tự động kết thúc truyện." +
		"Chỉ dùng khi cả truyện đã kết thúc và người dùng yêu cầu rõ ràng sửa chương đã viết; người dùng muốn thêm tình tiết/kéo dài dung lượng không thuộc viết lại, đừng dùng công cụ này."
}

// Write tool; concurrency is forbidden.
func (t *ReopenBookTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *ReopenBookTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *ReopenBookTool) ActivityDescription(_ json.RawMessage) string {
	return "Mở lại cả truyện để viết lại"
}

func (t *ReopenBookTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapters", schema.Array("Danh sách số chương đã hoàn thành cần viết lại (ít nhất một chương)", schema.Int(""))).Required(),
		schema.Property("reason", schema.String("Lý do viết lại (tùy chọn, ví dụ \"dọn ký tự đặc biệt\")")),
	)
}

func (t *ReopenBookTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Chapters []int  `json:"chapters"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if len(a.Chapters) == 0 {
		return nil, fmt.Errorf("chapters không được để trống, cần chỉ rõ các chương cần viết lại: %w", errs.ErrToolArgs)
	}

	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if progress == nil {
		return nil, fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
	}
	// Only chapters already written can be reworked; a chapter number outside the completed set is a continuation/out of range, so reject it explicitly and guide the user to adjust the length instead.
	var invalid []int
	for _, ch := range a.Chapters {
		if !slices.Contains(progress.CompletedChapters, ch) {
			invalid = append(invalid, ch)
		}
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("Chương %v chưa viết xong, reopen chỉ viết lại được chương đã hoàn thành (thêm/kéo dài tình tiết hãy đi qua điều chỉnh dung lượng): %w", invalid, errs.ErrToolPrecondition)
	}

	// The phase precondition is enforced as a backstop inside store.Reopen (callable only in complete).
	if err := t.store.Progress.Reopen(a.Chapters, a.Reason); err != nil {
		return nil, fmt.Errorf("reopen: %w: %w", errs.ErrStoreWrite, err)
	}

	// checkpoint: symmetric with complete_book (GlobalScope + meta/progress.json).
	if _, err := t.store.Checkpoints.AppendArtifact(domain.GlobalScope(), "reopen", "meta/progress.json"); err != nil {
		return nil, fmt.Errorf("checkpoint reopen: %w: %w", errs.ErrStoreWrite, err)
	}

	return json.Marshal(map[string]any{
		"reopened":         true,
		"phase":            string(domain.PhaseWriting),
		"pending_rewrites": a.Chapters,
		"next_step":        "Đã mở lại và đưa các chương mục tiêu vào hàng đợi. Hãy chờ Host chỉ thị giao writer viết lại từng chương; sửa xong hết sẽ lại tự động kết thúc truyện.",
	})
}
