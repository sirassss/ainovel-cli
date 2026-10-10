package host

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/revision"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

func upgradeProject(st *storepkg.Store) error {
	version, err := st.LoadProjectFormatVersion()
	if err != nil {
		return fmt.Errorf("Đọc phiên bản định dạng dự án: %w", err)
	}
	if version > storepkg.CurrentProjectFormatVersion {
		return fmt.Errorf("Phiên bản định dạng dự án v%d cao hơn v%d mà chương trình hiện tại hỗ trợ, hãy nâng cấp ainovel-cli", version, storepkg.CurrentProjectFormatVersion)
	}
	for version < storepkg.CurrentProjectFormatVersion {
		next := version + 1
		switch version {
		case storepkg.LegacyProjectFormatVersion:
			if err := migrateLegacyBook(st); err != nil {
				return fmt.Errorf("Nâng cấp dữ liệu dự án v%d→v%d: %w", version, next, err)
			}
			if err := revision.MigrateLegacyBaseline(st); err != nil {
				return fmt.Errorf("Nâng cấp dữ liệu dự án v%d→v%d: %w", version, next, err)
			}
		default:
			return fmt.Errorf("Không hỗ trợ nâng cấp từ định dạng dự án v%d", version)
		}
		if err := st.SaveProjectFormatVersion(next); err != nil {
			return fmt.Errorf("Lưu phiên bản định dạng dự án v%d: %w", next, err)
		}
		slog.Info("project data migration completed", "module", "migration", "from", version, "to", next)
		version = next
	}
	return nil
}

func migrateLegacyBook(st *storepkg.Store) error {
	book, err := st.Book.Load()
	if err != nil {
		return err
	}
	if book == nil {
		book, err = loadLegacyBook(st)
		if err != nil || book == nil {
			return err
		}
	}
	if err := st.Book.Save(*book); err != nil {
		return fmt.Errorf("Lưu thông tin tác phẩm cũ: %w", err)
	}
	if _, err := st.Checkpoints.AppendArtifact(domain.GlobalScope(), "book", "meta/book.json"); err != nil {
		return fmt.Errorf("Ghi nhận thông tin tác phẩm cũ: %w", err)
	}
	return nil
}

func loadLegacyBook(st *storepkg.Store) (*domain.BookMetadata, error) {
	data, err := os.ReadFile(filepath.Join(st.Dir(), "meta", "progress.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Đọc tiến độ tác phẩm cũ: %w", err)
	}
	var legacy struct {
		NovelName string `json:"novel_name"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("Phân tích tiến độ tác phẩm cũ: %w", err)
	}
	legacy.NovelName = strings.TrimSpace(legacy.NovelName)
	if legacy.NovelName == "" {
		return nil, nil
	}
	premise, err := st.Outline.LoadPremise()
	if err != nil {
		return nil, fmt.Errorf("Đọc tiền đề truyện cũ: %w", err)
	}
	title := legacyPremiseTitle(premise)
	if title == "" {
		return nil, fmt.Errorf("Tiền đề truyện cũ thiếu tiêu đề tên sách")
	}
	if title != legacy.NovelName {
		return nil, fmt.Errorf("Tên sách tác phẩm cũ xung đột: progress=%q, premise=%q", legacy.NovelName, title)
	}
	// han:keep legacy premise heading (zh data)
	synopsis := legacyPremiseSection(premise, "核心冲突")
	if synopsis == "" {
		return nil, fmt.Errorf("Tiền đề truyện cũ thiếu mục \"Xung đột cốt lõi\", không thể tạo giới thiệu tác phẩm")
	}
	return &domain.BookMetadata{Title: title, Synopsis: synopsis}, nil
}

func legacyPremiseTitle(premise string) string {
	for _, line := range strings.Split(premise, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			// han:keep book-title brackets in the legacy premise
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "# ")), "《》\"")
		}
	}
	return ""
}

func legacyPremiseSection(premise, heading string) string {
	var body []string
	matched := false
	for _, line := range strings.Split(premise, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if matched {
				break
			}
			matched = strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")) == heading
			continue
		}
		if matched {
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// resumeLabel generates the UI label for Resume from facts.
// An empty label means there is no recoverable state (a fresh start should be taken). Resuming itself needs no prompt:
// the Engine only restores facts: it recomputes the route from the store and continues (docs/engine-rfc.md §6).
func resumeLabel(store *storepkg.Store) (string, error) {
	progress, err := store.Progress.Load()
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if progress == nil || progress.Phase == domain.PhaseComplete {
		return "", nil
	}
	return describeResume(store, progress)
}

// describeResume generates a human-readable resume label; it does not affect Engine routing.
// All execution routes are derived by the Flow Router from facts; this is only for the UI's "Resume: xxx".
func describeResume(store *storepkg.Store, progress *domain.Progress) (string, error) {
	switch progress.Phase {
	case domain.PhasePremise, domain.PhaseOutline:
		return fmt.Sprintf("Khôi phục: giai đoạn lập kế hoạch (%s)", progress.Phase), nil
	case domain.PhaseWriting:
		// Priority is aligned with the Router's decision priority, so the label matches the instruction about to be dispatched.
		pending, err := store.Signals.LoadPendingCommit()
		if err != nil {
			return "", fmt.Errorf("Đọc bản nộp chờ khôi phục: %w", err)
		}
		if pending != nil {
			return fmt.Sprintf("Khôi phục: nộp chương %d bị gián đoạn", pending.Chapter), nil
		}
		if len(progress.PendingRewrites) > 0 {
			verb := "viết lại"
			if progress.Flow == domain.FlowPolishing {
				verb = "trau chuốt"
			}
			return fmt.Sprintf("Khôi phục %s: %d chương chờ xử lý", verb, len(progress.PendingRewrites)), nil
		}
		if progress.Flow == domain.FlowReviewing {
			return "Khôi phục: đánh giá bị gián đoạn", nil
		}
		if progress.InProgressChapter > 0 {
			return fmt.Sprintf("Khôi phục: chương %d đang tiến hành", progress.InProgressChapter), nil
		}
		label, err := describeArcEndLabel(store, progress)
		if err != nil {
			return "", err
		}
		if label != "" {
			return label, nil
		}
		return fmt.Sprintf("Khôi phục: tiếp tục từ chương %d", progress.NextChapter()), nil
	}
	return "Khôi phục", nil
}

// describeArcEndLabel generates UI-friendly labels for the various intermediate states at an arc end / volume end.
// It keeps the same order as the arc-end branch of flow.Route, ensuring the label lines up with the Router's first instruction.
func describeArcEndLabel(store *storepkg.Store, progress *domain.Progress) (string, error) {
	if !progress.Layered || len(progress.CompletedChapters) == 0 {
		return "", nil
	}
	lastCh := progress.CompletedChapters[len(progress.CompletedChapters)-1]
	boundary, err := store.Outline.CheckArcBoundary(lastCh)
	if err != nil {
		return "", fmt.Errorf("Kiểm tra biên cung: %w", err)
	}
	if boundary == nil || !boundary.IsArcEnd {
		return "", nil
	}
	vol, arc := boundary.Volume, boundary.Arc
	hasArcReview, err := store.World.HasArcReview(lastCh)
	if err != nil {
		return "", fmt.Errorf("Đọc đánh giá cung: %w", err)
	}
	hasArcSummary, err := store.Summaries.HasArcSummary(vol, arc)
	if err != nil {
		return "", fmt.Errorf("Đọc tóm tắt cung: %w", err)
	}
	hasVolumeSummary := false
	if boundary.IsVolumeEnd {
		hasVolumeSummary, err = store.Summaries.HasVolumeSummary(vol)
		if err != nil {
			return "", fmt.Errorf("Đọc tóm tắt tập: %w", err)
		}
	}
	switch {
	case !hasArcReview:
		return fmt.Sprintf("Khôi phục: đánh giá cuối cung đang chờ (tập %d, cung %d)", vol, arc), nil
	case !hasArcSummary:
		return fmt.Sprintf("Khôi phục: tóm tắt cung chờ tạo (tập %d, cung %d)", vol, arc), nil
	case boundary.IsVolumeEnd && !hasVolumeSummary:
		return fmt.Sprintf("Khôi phục: tóm tắt tập chờ tạo (tập %d)", vol), nil
	case boundary.NeedsExpansion && boundary.NextArc > 0:
		return fmt.Sprintf("Khôi phục: cung kế chờ mở rộng (tập %d, cung %d)", boundary.NextVolume, boundary.NextArc), nil
	case boundary.NeedsNewVolume:
		return fmt.Sprintf("Khôi phục: chờ quyết định tập kế (cuối tập %d)", vol), nil
	}
	return "", nil
}
