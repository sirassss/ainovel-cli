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
		slog.Info("Nâng cấp dữ liệu dự án hoàn tất", "module", "migration", "from", version, "to", next)
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
	// han:keep tiêu đề premise phiên bản cũ (dữ liệu zh)
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
			// han:keep dấu ngoặc tên sách trong premise phiên bản cũ
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

// resumeLabel 基于事实生成 Resume 的 UI 标签。
// label 为空表示无可恢复状态（应走新建）。恢复本身不需要任何 prompt——
// Engine 只恢复事实：从 store 重算路由续跑（docs/engine-rfc.md §6）。
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

// describeResume 生成人类可读的恢复标签；不影响 Engine 路由。
// 所有执行路由由 Flow Router 按事实推导；这里仅面向 UI 的 "恢复：xxx"。
func describeResume(store *storepkg.Store, progress *domain.Progress) (string, error) {
	switch progress.Phase {
	case domain.PhasePremise, domain.PhaseOutline:
		return fmt.Sprintf("Khôi phục: giai đoạn quy hoạch (%s)", progress.Phase), nil
	case domain.PhaseWriting:
		// 优先级与 Router 的决策优先级对齐，让 label 与即将派发的指令一致。
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
			return "Khôi phục: thẩm duyệt bị gián đoạn", nil
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

// describeArcEndLabel 为弧末/卷末的多种中间状态生成贴合 UI 的标签。
// 与 flow.Route 的弧末分支保持同序，保证 label 与 Router 首条指令对齐。
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
		return "", fmt.Errorf("Đọc thẩm duyệt cung: %w", err)
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
		return fmt.Sprintf("Khôi phục: thẩm duyệt cuối cung đang chờ (V%d A%d)", vol, arc), nil
	case !hasArcSummary:
		return fmt.Sprintf("Khôi phục: tóm tắt cung chờ tạo (V%d A%d)", vol, arc), nil
	case boundary.IsVolumeEnd && !hasVolumeSummary:
		return fmt.Sprintf("Khôi phục: tóm tắt tập chờ tạo (V%d)", vol), nil
	case boundary.NeedsExpansion && boundary.NextArc > 0:
		return fmt.Sprintf("Khôi phục: cung kế chờ mở rộng (V%d A%d)", boundary.NextVolume, boundary.NextArc), nil
	case boundary.NeedsNewVolume:
		return fmt.Sprintf("Khôi phục: chờ quyết định tập kế (cuối V%d)", vol), nil
	}
	return "", nil
}
