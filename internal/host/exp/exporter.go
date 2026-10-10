package exp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// Run performs one export. It returns synchronously; the IO volume is small (local file reads and writes).
//
// Failure semantics:
//   - invalid deps/opts → configuration error returned immediately
//   - no completed chapters at all → return an error (so the caller is explicit)
//   - a chapter in range with chapters/{ch}.md missing → return an error (progress being inconsistent with the filesystem is a fact-layer bug, and the user should see it)
//   - output path already exists and Overwrite is not set → return an error
//
// Skipped is for the case "legal within range but not yet completed" (the user passes to=100 but only 80 are written).
func Run(ctx context.Context, deps Deps, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("exp: deps.Store is nil")
	}

	if opts.Format == "" {
		f, err := inferFormat(opts.OutPath)
		if err != nil {
			return nil, err
		}
		opts.Format = f
	}
	if opts.Format != FormatTXT && opts.Format != FormatEPUB {
		return nil, fmt.Errorf("exp: định dạng chưa được hỗ trợ %q", opts.Format)
	}

	progress, err := deps.Store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("Không tải được progress: %w", err)
	}
	if progress == nil || len(progress.CompletedChapters) == 0 {
		return nil, fmt.Errorf("Chưa có chương nào hoàn thành, không có nội dung để xuất")
	}
	book, err := deps.Store.Book.Load()
	if err != nil {
		return nil, fmt.Errorf("Không tải được thông tin tác phẩm: %w", err)
	}
	if book == nil {
		return nil, fmt.Errorf("Thông tin tác phẩm không tồn tại, không thể xuất")
	}

	completed := make(map[int]struct{}, len(progress.CompletedChapters))
	maxCh := 0
	for _, c := range progress.CompletedChapters {
		completed[c] = struct{}{}
		if c > maxCh {
			maxCh = c
		}
	}

	from := opts.From
	if from <= 0 {
		from = 1
	}
	to := opts.To
	if to <= 0 {
		to = maxCh
	}
	if from > to {
		return nil, fmt.Errorf("Khoảng chương không hợp lệ: from=%d > to=%d", from, to)
	}

	var chapters, skipped []int
	for ch := from; ch <= to; ch++ {
		if _, ok := completed[ch]; ok {
			chapters = append(chapters, ch)
		} else {
			skipped = append(skipped, ch)
		}
	}
	if len(chapters) == 0 {
		return nil, fmt.Errorf("Không có chương hoàn thành nào trong khoảng %d..%d", from, to)
	}

	bodies := make(map[int]string, len(chapters))
	for _, ch := range chapters {
		text, err := deps.Store.Drafts.LoadChapterText(ch)
		if err != nil {
			return nil, fmt.Errorf("Không đọc được chương %d: %w", ch, err)
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("progress đánh dấu chương %d đã hoàn thành, nhưng chapters/%02d.md thiếu hoặc rỗng", ch, ch)
		}
		bodies[ch] = text
	}

	outline, _ := deps.Store.Outline.LoadOutline()
	var volumes []domain.VolumeOutline
	if progress.Layered {
		volumes, _ = deps.Store.Outline.LoadLayeredOutline()
	}

	outPath := opts.OutPath
	if outPath == "" {
		outPath = filepath.Join(deps.Store.Dir(), sanitizeFileName(book.Title)+"."+string(opts.Format))
	}

	if !opts.Overwrite {
		if _, err := os.Stat(outPath); err == nil {
			return nil, fmt.Errorf("Tệp đã tồn tại: %s (thêm --overwrite để ghi đè)", outPath)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("Không kiểm tra được đường dẫn đầu ra: %w", err)
		}
	}

	titleIdx := buildTitleIndex(outline)
	for _, ch := range chapters {
		summary, err := deps.Store.Summaries.LoadSummary(ch)
		if err != nil {
			return nil, fmt.Errorf("Không đọc được tóm tắt chương %d: %w", ch, err)
		}
		if summary != nil && strings.TrimSpace(summary.Title) != "" {
			titleIdx[ch] = summary.Title
		}
	}
	var locations map[int]chapterLocation
	if len(volumes) > 0 {
		locations = buildLocations(volumes)
	}

	var data []byte
	switch opts.Format {
	case FormatTXT:
		data = []byte(renderTXT(labelsFor(deps.Store.Language()), book.Title, chapters, titleIdx, locations, bodies))
	case FormatEPUB:
		buf, err := renderEPUB(labelsFor(deps.Store.Language()), *book, chapters, titleIdx, locations, bodies)
		if err != nil {
			return nil, fmt.Errorf("Không tạo được EPUB: %w", err)
		}
		data = buf
	}

	if err := atomicWrite(outPath, data); err != nil {
		return nil, fmt.Errorf("Không ghi được tệp: %w", err)
	}

	return &Result{
		Path:     outPath,
		Chapters: len(chapters),
		Bytes:    len(data),
		Skipped:  skipped,
	}, nil
}

// inferFormat infers the format from the output path suffix. An empty path falls back to TXT; an unknown suffix is an error (to avoid silent mistakes).
func inferFormat(path string) (Format, error) {
	if path == "" {
		return FormatTXT, nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case "", ".txt":
		return FormatTXT, nil
	case ".epub":
		return FormatEPUB, nil
	default:
		return "", fmt.Errorf("Không suy ra được định dạng từ phần mở rộng %q (hỗ trợ .txt / .epub)", filepath.Ext(path))
	}
}

// atomicWrite has the same shape as WriteFile in store/io.go: tmp + sync + rename.
// It does not reuse store.IO because the output path may be outside store.Dir().
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// sanitizeFileName replaces characters in a file name that are disallowed or confusable on most filesystems.
// It does no aggressive transcoding and only blocks path separators and control characters.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "novel"
	}
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		"\x00", "_",
	)
	return replacer.Replace(name)
}
