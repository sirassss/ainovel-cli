package rules

import (
	"os"
	"path/filepath"
)

// LoadOptions enumerates the source directories of rules files, for RawFileSources to scan and normalize.
//
// A missing directory is not an error; it is silently skipped during scanning.
type LoadOptions struct {
	// HomeRulesDir is the ~/.ainovel/rules/ directory; all top-level .md files under it are scanned (merged in filename lexicographic order). Empty means skip.
	HomeRulesDir string

	// ProjectRulesDir is the ./.ainovel/rules/ directory (mirrors the global one; all top-level .md files under it are scanned as well). Empty means skip.
	ProjectRulesDir string
}

// ainovelDirName is the dotdir name that ainovel shares at both the user and project levels.
// The global ~/.ainovel/rules/ and the project ./.ainovel/rules/ are symmetric because of it.
const ainovelDirName = ".ainovel"

// DefaultProjectRulesDir builds the absolute path of ./.ainovel/rules/ (based on the given project directory).
// The caller passes in the project root so the loader does not depend on cwd internally; mirrors DefaultHomeRulesDir.
func DefaultProjectRulesDir(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	return filepath.Join(projectDir, ainovelDirName, "rules")
}

// DefaultHomeRulesDir builds the absolute path of the ~/.ainovel/rules/ directory.
// Returns an empty string when the home directory cannot be resolved (callers skip this source accordingly).
func DefaultHomeRulesDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ainovelDirName, "rules")
}

// homeRulesReadme is the guide written to ~/.ainovel/rules/README.txt on first-run onboarding.
// It deliberately uses the .txt suffix instead of .md: scanning only recognizes .md, so this guide is never normalized as a rule.
const homeRulesReadme = `Đặt ở đây các sở thích viết toàn cục, có hiệu lực với mọi cuốn sách.

Tạo một tệp .md mới (như my-style.md) và viết yêu cầu bằng lời thường là được —
không cần định dạng nào, không cần YAML:

    # Nhân vật
    - Nhân vật chính Lâm Trần đừng viết thành người hiền lành quá mức, lạnh ngoài nóng trong là được
    # Văn phong
    - Dùng nhiều cảm nhận cơ thể (khớp ngón tay trắng bệch) thay cho nhãn cảm xúc (căng thẳng)
    - Đối thoại đừng quá sách vở, mỗi chương khoảng 3000 chữ
    - Đừng xuất hiện kiểu giọng AI như "ở một mức độ nào đó"

Viết xong không cần lo định dạng: hệ thống dùng model chuẩn hóa các yêu cầu bằng ngôn ngữ tự nhiên này
thành ràng buộc có cấu trúc (khoảng số chữ, từ cấm, ngưỡng từ gây nhàm, v.v.), tự động tuân theo khi viết
và tự kiểm tra khi nộp chương.

Nhiều tệp .md được gộp theo thứ tự từ điển của tên tệp; tệp ẩn bắt đầu bằng dấu chấm và tệp không phải .md đều bị bỏ qua
(vì vậy README.txt này sẽ không bị coi là quy tắc).

Đường cơ sở cơ học về các câu sáo của AI và từ gây nhàm đã được tích hợp sẵn, dùng ngay được, không viết cũng không sao.

Thứ tự ưu tiên nạp (cao -> thấp): ./.ainovel/rules/*.md (cuốn sách này) > ~/.ainovel/rules/*.md (ở đây) > mặc định tích hợp`

// EnsureHomeRulesDir makes a best effort to create the ~/.ainovel/rules/ directory and write the README.txt guide into it,
// so users discover this global-preference extension point and know how to write rules.
// Nice-to-have, not on the critical path: failure to resolve home or to write is silently swallowed and never blocks startup.
func EnsureHomeRulesDir() {
	if dir := DefaultHomeRulesDir(); dir != "" {
		_ = ensureRulesDirAt(dir)
	}
}

// ensureRulesDirAt creates the directory and writes README.txt as the current onboarding template; it is the testable core of EnsureHomeRulesDir.
// README.txt is a system-generated onboarding file (user preferences go in *.md; it is not scanned or loaded) and is overwritten with
// the latest template every time - old content is not preserved, so no version-compatibility logic is needed.
func ensureRulesDirAt(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "README.txt"), []byte(homeRulesReadme), 0o644)
}

// DefaultOptions builds the commonly used LoadOptions from the current working directory.
//
// Suited to being called once at Host startup so the user-rules service reuses the same source configuration.
// When cwd cannot be resolved, ProjectRulesDir is left empty (scanning skips that source).
//
// Path semantics: ProjectRulesDir is bound to the **current working directory (cwd)**, not outputDir.
// When the user cd's into different directories to start different books, ./.ainovel/rules/ naturally follows cwd; to share across books,
// put the rules in the global ~/.ainovel/rules/ directory (all .md files under it are loaded).
func DefaultOptions() LoadOptions {
	cwd, _ := os.Getwd()
	return LoadOptions{
		HomeRulesDir:    DefaultHomeRulesDir(),
		ProjectRulesDir: DefaultProjectRulesDir(cwd),
	}
}
