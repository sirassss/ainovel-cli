package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/voocel/agentcore/schema"
	agentcoretools "github.com/voocel/agentcore/tools"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// EditChapterTool performs targeted string replacement on a chapter draft, suited to polishing.
// Compared with rewriting the whole chapter via draft_chapter, it saves 10x+ tokens.
//
// On-disk contract: only edits drafts/{ch:02d}.draft.md; directly editing chapters/ is forbidden (the final text is owned exclusively by commit_chapter).
// Seed semantics: if drafts is missing but chapters has the chapter, chapters is copied to drafts as the starting point.
// Ownership check: only chapters that are completed and in the PendingRewrites queue may be edited.
//
// This tool is a thin wrapper around agentcore.EditTool; the find-and-replace logic (multi-level fuzzy matching, diff output, line-ending/BOM preservation)
// is entirely reused from the upstream implementation.
type EditChapterTool struct {
	store *store.Store
	edit  *agentcoretools.EditTool
}

func NewEditChapterTool(s *store.Store) *EditChapterTool {
	return &EditChapterTool{
		store: s,
		edit:  agentcoretools.NewEdit(s.Dir(), nil),
	}
}

func (t *EditChapterTool) Name() string  { return "edit_chapter" }
func (t *EditChapterTool) Label() string { return "Sửa chương" }

// ReadOnly explicitly declares this a write tool (works with ConcurrencySafeTool to prevent concurrent scheduling).
func (t *EditChapterTool) ReadOnly(_ json.RawMessage) bool { return false }

// ConcurrencySafe explicitly forbids concurrency: parallel edit_chapter calls on the same chapter would race on read-modify-write,
// and even parallel calls on different chapters would interleave the checkpoint order. Serializing everything is the safest.
func (t *EditChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

// ActivityDescription provides the description of the tool's current activity for UI/log display.
func (t *EditChapterTool) ActivityDescription(_ json.RawMessage) string {
	return "Sửa bản nháp chương"
}

func (t *EditChapterTool) Description() string {
	return "Chỉ thay thế chuỗi tại điểm cụ thể trong bản nháp của chương đã hoàn thành và nằm trong hàng đợi PendingRewrites (ưu tiên cho trau chuốt, tiết kiệm token hơn viết lại cả chương bằng draft_chapter)." +
		"Cấm dùng công cụ này cho bản nháp đầu của chương mới; nếu bản nháp đầu có lỗi nặng hãy gọi draft_chapter(mode=\"write\") để ghi đè cả chương." +
		"Tìm old_string và thay bằng new_string, yêu cầu khớp chính xác và duy nhất (khớp nhiều chỗ thì cần replace_all=true)." +
		"old_string phải được sao chép từng chữ từ kết quả read_chapter(source=\"draft\") gần nhất, cấm dựng lại nguyên văn theo trí nhớ;" +
		"lưu ý giá trị trả về là chuỗi JSON, \\n phải được khôi phục thành xuống dòng thật. Sau khi draft_chapter đã viết lại bản nháp, phải read_chapter lại rồi mới được sửa." +
		"Khi khớp thất bại, thông báo lỗi sẽ kèm đoạn ứng viên gần nhất trong bản nháp; hãy sao chép từng chữ từ ứng viên rồi thử lại." +
		"Ghi vào drafts/{ch}.draft.md; nếu drafts chưa có thì tự gieo từ chapters." +
		"Từ chối thực hiện khi chương đã hoàn thành và không nằm trong hàng đợi PendingRewrites. Mỗi lần gọi chỉ sửa một chỗ, nhiều chỗ thì gọi nhiều lần."
}

func (t *EditChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("old_string", schema.String("Đoạn nguyên văn cần thay, phải chính xác; nhiều dòng thì phải gồm cả xuống dòng; nếu không đặt replace_all thì phải xuất hiện duy nhất trong bản nháp")).Required(),
		schema.Property("new_string", schema.String("Văn bản mới sau khi thay")).Required(),
		schema.Property("replace_all", schema.Bool("Thay tất cả chỗ khớp (mặc định false)")),
	)
}

func (t *EditChapterTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Chapter    int    `json:"chapter"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	if a.OldString == "" {
		return nil, fmt.Errorf("old_string không được để trống: %w", errs.ErrToolArgs)
	}
	if a.OldString == a.NewString {
		return nil, fmt.Errorf("old_string và new_string giống nhau, không cần sửa: %w", errs.ErrToolArgs)
	}
	if err := t.store.Progress.ValidateChapterWork(a.Chapter); err != nil {
		return nil, err
	}

	// Ownership check: mechanically enforce the writer protocol. A new chapter's first draft can only be overwritten as a whole chapter; we must not rely on the
	// model obeying the prompt and still exposing brittle exact edits as an executable path.
	completed, err := t.store.Progress.IsChapterCompleted(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if !completed {
		return nil, fmt.Errorf("Chương %d chưa hoàn thành, cấm dùng edit_chapter cho bản nháp đầu; nếu có lỗi nặng hãy gọi draft_chapter(mode=\"write\", chapter=%d) để ghi đè cả chương: %w", a.Chapter, a.Chapter, errs.ErrToolPrecondition)
	}
	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if progress == nil || !slices.Contains(progress.PendingRewrites, a.Chapter) {
		return nil, fmt.Errorf("Chương %d đã hoàn thành và không nằm trong hàng đợi PendingRewrites, không thể sửa; cần sửa thì để editor đánh giá và kích hoạt viết lại/trau chuốt trước: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	if err := EnsureChapterExpanded(t.store, a.Chapter); err != nil {
		return nil, err
	}

	// Seed: when drafts is missing, copy one from chapters as the starting point
	if err := t.ensureDraft(a.Chapter); err != nil {
		return nil, err
	}

	// Delegate the find-and-replace to agentcore.EditTool
	subArgs, _ := json.Marshal(map[string]any{
		"path":        fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		"file_path":   fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		"old_text":    a.OldString,
		"old_string":  a.OldString,
		"new_text":    a.NewString,
		"new_string":  a.NewString,
		"replace_all": a.ReplaceAll,
	})
	result, err := t.edit.Execute(ctx, subArgs)
	if err != nil {
		return nil, fmt.Errorf("apply edit: %w: %w", errs.ErrToolPrecondition, err)
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(a.Chapter), "edit",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint edit: %w: %w", errs.ErrStoreWrite, err)
	}

	// Append guidance: let the writer know the follow-up steps, to avoid missing check_consistency / commit_chapter
	var passthrough map[string]any
	if err := json.Unmarshal(result, &passthrough); err != nil {
		return result, nil
	}
	passthrough["chapter"] = a.Chapter
	passthrough["next_step"] = "edit đã được lưu. Nếu còn lỗi nặng có thể edit_chapter lần nữa; nếu không thì check_consistency rồi commit_chapter"
	return json.Marshal(passthrough)
}

// ensureDraft guarantees drafts/{ch}.draft.md exists:
//   - draft exists → return directly
//   - no draft but a final text exists → copy the final text to drafts as the starting point for edits (common in polishing)
//   - neither exists → error, telling the caller to create the first draft with draft_chapter first
func (t *EditChapterTool) ensureDraft(chapter int) error {
	draft, err := t.store.Drafts.LoadDraft(chapter)
	if err != nil {
		return fmt.Errorf("load draft: %w: %w", errs.ErrStoreRead, err)
	}
	if draft != "" {
		return nil
	}
	text, err := t.store.Drafts.LoadChapterText(chapter)
	if err != nil {
		return fmt.Errorf("load chapter: %w: %w", errs.ErrStoreRead, err)
	}
	if text == "" {
		return fmt.Errorf("Chương %d không có bản nháp cũng chưa có bản chính thức, hãy gọi draft_chapter(mode=write, chapter=%d) để tạo bản nháp đầu trước: %w", chapter, chapter, errs.ErrToolPrecondition)
	}
	if err := t.store.Drafts.SaveDraft(chapter, text); err != nil {
		return fmt.Errorf("seed draft from chapter: %w: %w", errs.ErrStoreWrite, err)
	}
	return nil
}
