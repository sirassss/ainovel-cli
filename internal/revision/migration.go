package revision

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/chapterfacts"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

type legacyCommit struct {
	chapter    int
	facts      domain.ChapterFacts
	acceptedAt time.Time
}

type legacyCommitArgs struct {
	Chapter int `json:"chapter"`
	domain.ChapterFacts
}

// MigrateLegacyBaseline 为 chapter_records 出现前创建的作品补齐接纳基线。
// 成功提交会话保存完整章节事实，草稿保存当时的正文；只使用这两份历史事实，
// 不会把可能已被用户修改的 chapters/*.md 静默当成已接纳版本。
func MigrateLegacyBaseline(st *store.Store) error {
	progress, err := st.Progress.Load()
	if err != nil {
		return fmt.Errorf("Đọc tiến độ: %w", err)
	}
	if progress == nil || len(progress.CompletedChapters) == 0 {
		return nil
	}

	chapters := slices.Clone(progress.CompletedChapters)
	slices.Sort(chapters)
	missing := false
	for _, chapter := range chapters {
		record, err := st.ChapterRecords.Load(chapter)
		if err != nil {
			return err
		}
		missing = missing || record == nil
	}
	if !missing {
		records, err := st.ChapterRecords.LoadCompleted(chapters)
		if err != nil {
			return err
		}
		return ValidateRecords(records)
	}

	commits, err := loadLegacyCommits(st.Dir())
	if err != nil {
		return err
	}
	records := make([]domain.ChapterRecord, 0, len(chapters))
	for _, chapter := range chapters {
		commit, ok := commits[chapter]
		if !ok {
			commit, ok, err = loadLegacyImportCommit(st.Dir(), chapter)
			if err != nil {
				return err
			}
		}
		if !ok {
			return fmt.Errorf("Chương %d thiếu bản nộp thành công hoặc bản ghi phân tích nhập có thể xác minh, không thể lập đường cơ sở sửa đổi", chapter)
		}
		if err := chapterfacts.Validate(commit.facts); err != nil {
			return fmt.Errorf("Sự thật bản nộp lịch sử của chương %d không hợp lệ: %w", chapter, err)
		}
		draft, err := st.Drafts.LoadDraft(chapter)
		if err != nil {
			return fmt.Errorf("Đọc bản nháp lịch sử của chương %d: %w", chapter, err)
		}
		if strings.TrimSpace(draft) == "" {
			return fmt.Errorf("Chương %d thiếu bản nháp lịch sử, không thể xác nhận chính văn đã tiếp nhận", chapter)
		}
		if err := verifyLegacySummary(st, chapter, commit.facts); err != nil {
			return err
		}
		acceptedAt := commit.acceptedAt
		if acceptedAt.IsZero() {
			if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "commit"); cp != nil {
				acceptedAt = cp.OccurredAt
			}
		}
		if acceptedAt.IsZero() {
			return fmt.Errorf("Bản ghi nộp thành công của chương %d thiếu thời gian, không thể lập đường cơ sở sửa đổi", chapter)
		}
		content := domain.NormalizeChapterContent(draft)
		records = append(records, domain.ChapterRecord{
			Version: domain.ChapterRecordVersion, Chapter: chapter, Revision: 1,
			Origin: domain.ChapterOriginGenerated, Content: content,
			ContentSHA256: domain.ChapterContentSHA256(content), Facts: commit.facts,
			AcceptedAt: acceptedAt,
		})
	}
	if err := ValidateRecords(records); err != nil {
		return fmt.Errorf("Sự thật chương lịch sử không tạo được đường cơ sở nhất quán: %w", err)
	}

	for _, expected := range records {
		existing, err := st.ChapterRecords.Load(expected.Chapter)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameLegacyRecord(*existing, expected) {
				return fmt.Errorf("Bản ghi tiếp nhận hiện có của chương %d xung đột với đường cơ sở di chuyển", expected.Chapter)
			}
			continue
		}
		if err := st.ChapterRecords.Save(expected); err != nil {
			return fmt.Errorf("Lưu đường cơ sở sửa đổi của chương %d: %w", expected.Chapter, err)
		}
	}
	return nil
}

func verifyLegacySummary(st *store.Store, chapter int, facts domain.ChapterFacts) error {
	summary, err := st.Summaries.LoadSummary(chapter)
	if err != nil {
		return fmt.Errorf("Đọc tóm tắt chương %d: %w", chapter, err)
	}
	if summary == nil {
		return fmt.Errorf("Chương %d thiếu tóm tắt, không thể kiểm tra bản nộp lịch sử", chapter)
	}
	if summary.Title != facts.Title || summary.Summary != facts.Summary ||
		!slices.Equal(summary.Characters, facts.Characters) || !slices.Equal(summary.KeyEvents, facts.KeyEvents) {
		return fmt.Errorf("Tóm tắt chương %d không khớp với bản ghi nộp thành công, từ chối đoán khi di chuyển", chapter)
	}
	return nil
}

func sameLegacyRecord(a, b domain.ChapterRecord) bool {
	return a.Version == b.Version && a.Chapter == b.Chapter && a.Revision == b.Revision &&
		a.Origin == b.Origin && a.Content == b.Content && a.ContentSHA256 == b.ContentSHA256 &&
		reflect.DeepEqual(a.Facts, b.Facts) && reflect.DeepEqual(a.StyleDelta, b.StyleDelta) &&
		a.AcceptedAt.Equal(b.AcceptedAt)
}

func loadLegacyCommits(dir string) (map[int]legacyCommit, error) {
	sessionsDir := filepath.Join(dir, "meta", "sessions", "agents")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]legacyCommit{}, nil
		}
		return nil, fmt.Errorf("Đọc phiên Worker lịch sử: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	commits := make(map[int]legacyCommit)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "writer-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name())
		if err := readLegacyCommits(path, commits); err != nil {
			return nil, err
		}
	}
	return commits, nil
}

func loadLegacyImportCommit(dir string, chapter int) (legacyCommit, bool, error) {
	path := filepath.Join(dir, "meta", "import", "analyses", fmt.Sprintf("%06d.json", chapter))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return legacyCommit{}, false, nil
		}
		return legacyCommit{}, false, fmt.Errorf("Đọc phân tích nhập lịch sử của chương %d: %w", chapter, err)
	}
	var artifact struct {
		Payload struct {
			Facts legacyCommitArgs `json:"facts"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		return legacyCommit{}, false, fmt.Errorf("Phân tích phân tích nhập lịch sử của chương %d: %w", chapter, err)
	}
	if artifact.Payload.Facts.Chapter != chapter {
		return legacyCommit{}, false, fmt.Errorf("Số chương trong phân tích nhập lịch sử của chương %d là %d", chapter, artifact.Payload.Facts.Chapter)
	}
	return legacyCommit{chapter: chapter, facts: artifact.Payload.Facts.ChapterFacts}, true, nil
}

func readLegacyCommits(path string, commits map[int]legacyCommit) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	pending := make(map[string]legacyCommitArgs)
	reader := bufio.NewReader(f)
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			var msg agentcore.Message
			if err := json.Unmarshal(line, &msg); err != nil {
				return fmt.Errorf("Phân tích phiên lịch sử %s:%d: %w", filepath.Base(path), lineNo, err)
			}
			for _, call := range msg.ToolCalls() {
				if call.Name != "commit_chapter" || call.ArgsInvalid || call.ID == "" {
					continue
				}
				var args legacyCommitArgs
				if err := json.Unmarshal(call.Args, &args); err != nil {
					return fmt.Errorf("Phân tích commit_chapter của phiên lịch sử %s:%d: %w", filepath.Base(path), lineNo, err)
				}
				pending[call.ID] = args
			}
			if msg.Role == agentcore.RoleTool {
				id, _ := msg.Metadata["tool_call_id"].(string)
				args, ok := pending[id]
				if ok {
					delete(pending, id)
					failed, _ := msg.Metadata["is_error"].(bool)
					if !failed && toolResultCommitted(msg.TextContent()) {
						candidate := legacyCommit{chapter: args.Chapter, facts: args.ChapterFacts, acceptedAt: msg.Timestamp}
						previous, exists := commits[args.Chapter]
						if !exists || previous.acceptedAt.IsZero() || !candidate.acceptedAt.Before(previous.acceptedAt) {
							commits[args.Chapter] = candidate
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("Đọc phiên lịch sử %s: %w", filepath.Base(path), readErr)
		}
	}
}

func toolResultCommitted(text string) bool {
	var result struct {
		Committed bool `json:"committed"`
	}
	if json.Unmarshal([]byte(text), &result) == nil {
		return result.Committed
	}
	var quoted string
	return json.Unmarshal([]byte(text), &quoted) == nil && json.Unmarshal([]byte(quoted), &result) == nil && result.Committed
}
