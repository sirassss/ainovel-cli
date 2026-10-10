# Việt hóa phase 2 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mỗi đoạn chữ trong repo dùng ngôn ngữ hợp với người đọc (comment/log/docs = Anh, TUI = Việt hoặc Anh, LLM chế độ vi = Việt), thuật ngữ Hán-Việt khó hiểu được thay, và test chặn tái phát.

**Architecture:** Dịch tại chỗ, không thêm lớp i18n. `internal/hancheck` là cổng chất lượng: ba test mới (comment, thuật ngữ cũ, Title Case ở TUI) mỗi cái có một map/biến `pending`; task nào dọn xong thư mục nào thì xóa dòng của thư mục đó (hoặc lật biến), để test bắt buộc sạch.

**Tech Stack:** Go 1.25 (máy không có Go, chạy qua `scripts/docker-go.sh`), `go/parser` + `go/ast`, Python 3 cho script thay thế một lần (để trong scratchpad, không commit).

**Spec:** `docs/superpowers/specs/2026-10-10-viet-hoa-phase2-design.md` (kế thừa `2026-10-09-viet-hoa-chuoi-han-va-sua-loi-log-design.md`).

## Global Constraints

- Chạy Go: `scripts/docker-go.sh test ./<pkg>/...`; toàn repo: `scripts/docker-go.sh test ./...`.
- Không đổi hành vi: chỉ đổi chữ. Tên tool, tên trường JSON, định danh Go (`save_review`, `foreshadow`, `chapter_contract`, `PhaseComplete`…) **giữ nguyên**.
- Không đụng `assets/**/zh/`, `assets/voice_zh.md`, `evals/cases/**/*.json`, `docs/superpowers/`.
- Dữ liệu máy so khớp (regex, khóa map, tiền tố parse lại) giữ Hán và gắn `han:keep` trên cùng dòng hoặc dòng ngay trên.
- Comment Go: **tiếng Anh**, kể cả `_test.go`. Chuỗi log/`slog`, chuỗi trong `diag`/`eval`/`models`: **tiếng Anh**. Chuỗi người dùng thấy: **tiếng Việt** (R1–R5 của spec).
- Bảng thuật ngữ (đã duyệt), áp theo đúng chữ này: đại cương→dàn ý; phục bút→chi tiết gài trước; thẩm duyệt→đánh giá; chính văn→nội dung chương; hoàn kết→kết thúc truyện; (Bộ) tài phán→Trọng tài; quy hoạch→lập kế hoạch; tập khép lại→tập cuối; nghiệm thu từng chương→duyệt từng chương; cung khung xương→cung phác thảo; hợp đồng chương→yêu cầu chương; điểm móc/móc câu→hook; sảng điểm→điểm đã; gọt giũa→trau chuốt. **Giữ** `khoái cảm`, `thực lực`, `cẩu đạo`, `vô địch lưu`, `lực cản`, `đỉnh điểm`, `thân phận`.
- Commit nhỏ, mỗi task một commit, thông điệp tiếng Việt kiểu `type: …` như lịch sử repo, kết thúc bằng `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Không `git add -A` (file `docker-compose.yml` đang sửa dở, không thuộc việc này).

## Review Focus

1. **Dịch nhầm dữ liệu so khớp** (regex `stylestat`, `chapterHeaderRe`, khóa map): hành vi âm thầm đổi. Test ở Task 4 (văn mẫu tiếng Trung vẫn khớp mọi mẫu) và Task 5/6.
2. **Rule `diag`/`eval` hết khớp vì chuỗi nguồn đã đổi** ở phase 1: không báo lỗi, chỉ im lặng. Test ở Task 5, 6.
3. **Thay thuật ngữ máy móc ra câu gượng** (`bản thảo nội dung chương`, `bản lập kế hoạch`): test `TestNoLegacyTerms` kèm danh sách `awkwardPhrases` ở Task 1.
4. **Thay thuật ngữ trúng tên tool/trường JSON**: `assets/load_test.go` (khớp tên trong dấu `` với `zh/`) phải vẫn xanh sau Task 7, 8.
5. **Dịch comment lỡ sửa code** hoặc làm rơi nội dung: sau mỗi task comment chạy lệnh kiểm diff ở Task 10; với docs so số tiêu đề (Task 17).

---

### Task 1: Cổng kiểm tra trong `hancheck`

**Files:**
- Modify: `internal/hancheck/hancheck_test.go`
- Create: `internal/hancheck/terms_test.go`

**Interfaces:**
- Produces (dùng ở mọi task sau):
  - `var pendingComments = map[string]string{}` — thư mục (tương đối repo, như `pending`) còn comment sai ngôn ngữ; task dọn xong xóa dòng.
  - `var termsPending = true`, `var titleCasePending = true` — lật thành `false` ở Task 8 và Task 9.
  - `func hasVietnamese(s string) bool` — true nếu có rune trong `ăâđêôơưĂÂĐÊÔƠƯ`, hoặc `U+1EA0..U+1EF9`, hoặc `àáãèéìíòóõùúýÀÁÃÈÉÌÍÒÓÕÙÚÝ`.
  - `func scanComments(path string) ([]violation, error)` — mỗi dòng comment (`//` và `/* */`, cả `_test.go`) có `hasCJK` hoặc `hasVietnamese` mà không có `han:keep` trên cùng dòng/dòng trên thì thành `violation`.
  - `var legacyTerms []string` (15 thuật ngữ cũ trong bảng, chữ thường), `var awkwardPhrases = []string{"bản thảo nội dung chương", "bản lập kế hoạch", "việc lập kế hoạch lập kế hoạch", "nội dung chương chương"}`.

- [ ] **Step 1: Viết test thất bại** trong `hancheck_test.go`:
  `TestScannerDetectsComments` — file mẫu có `// 中文`, `// tiếng Việt`, `// plain english`, `x := 1 // 保留 han:keep`, kỳ vọng đúng 2 violation (dòng 1, 2). `TestCommentsEnglish` — duyệt mọi thư mục có `.go` (gồm `internal/hancheck`, `cmd`, `assets`; **gồm** `_test.go`), bỏ qua thư mục trong `pendingComments` trừ khi `*pendingFlag` trùng.
  Trong `terms_test.go`: `TestNoLegacyTerms` — nếu `termsPending` thì `t.Skip`; ngược lại quét (không phân biệt hoa thường) mọi `.md` dưới `assets/` trừ thư mục `zh/`, `testdata/`; `README.md`, `HUONG_DAN_SU_DUNG.md`, `config.example.jsonc`, `config/config.example.jsonc` ở gốc; và mọi string literal Go (cả `_test.go`) trong `internal/`, `cmd/`, `assets/` trừ `internal/hancheck`. Báo `file:line: term`, kiểm cả `awkwardPhrases`. `TestTUINotTitleCase` — nếu `titleCasePending` thì skip; quét string literal (không `_test.go`) trong `internal/entry`, `internal/host`, `internal/bootstrap`, `internal/notify`; chuỗi có `hasVietnamese` hoặc chữ có dấu và chứa từ viết hoa liên tiếp ≥2 từ (trừ tên phím `Enter Esc Tab Ctrl Shift PgUp PgDn`) là lỗi; marker `case:ok` ở cùng dòng bỏ qua. Test đơn vị của hàm lọc: `"Tài Nguyên Đã Dùng"` bị bắt; `"Enter Gửi"`, `"Esc Thoát"`, `"Tổng quan"` không.
- [ ] **Step 2: Chạy, xác nhận thất bại**: `scripts/docker-go.sh test ./internal/hancheck/ -run 'Scanner|Comments'` → FAIL (`scanComments` chưa có).
- [ ] **Step 3: Cài `hasVietnamese`, `scanComments`, hàm lọc Title Case**; dùng `go/parser` với `parser.ParseComments`, kiểm theo từng dòng của `ast.Comment` để số dòng đúng.
- [ ] **Step 4: Gieo `pendingComments`**: chạy `scripts/docker-go.sh test ./internal/hancheck/ -run TestCommentsEnglish`, thêm mọi thư mục báo lỗi vào `pendingComments` với giá trị `"phase2"`. Dịch 8 dòng comment tiếng Việt trong chính `internal/hancheck` sang tiếng Anh (không đưa nó vào pending).
- [ ] **Step 5: Chạy cả package**: `scripts/docker-go.sh test ./internal/hancheck/...` → PASS (`TestNoLegacyTerms`, `TestTUINotTitleCase` hiện skip).
- [ ] **Step 6: Commit** `test: Thêm cổng quét comment, thuật ngữ cũ và Title Case cho hancheck`.

### Task 2: `internal/version`

**Files:** Modify `internal/version/update.go`, `internal/version/update_test.go`; Modify `internal/hancheck/hancheck_test.go` (xóa `internal/version` khỏi `pending` và `pendingComments`).

- [ ] **Step 1: Xóa dòng `internal/version` khỏi cả hai map; chạy** `scripts/docker-go.sh test ./internal/hancheck/ -pending=internal/version` → FAIL, liệt kê ~13 vị trí.
- [ ] **Step 2: Dịch.** `fmt.Errorf` trả về cho người dùng (tự cập nhật) → **tiếng Việt** ngắn, giữ nguyên `%s`/`%w` và thứ tự đối số (vd `"Windows không hỗ trợ tự cập nhật tại chỗ, hãy tải bản mới tại https://github.com/%s/releases"`). Comment (gồm `update_test.go:127-129`) → tiếng Anh.
- [ ] **Step 3:** `scripts/docker-go.sh test ./internal/version/... ./internal/hancheck/...` → PASS.
- [ ] **Step 4: Commit** `refactor: Dọn chữ Hán và comment ở internal/version`.

### Task 3: `internal/models`

**Files:** Modify `internal/models/{pricing,gen_models,registry,model_lookup}.go`; Modify `internal/hancheck/hancheck_test.go`.

- [ ] **Step 1:** xóa `internal/models` khỏi hai map; chạy hancheck với `-pending=internal/models` → FAIL.
- [ ] **Step 2: Dịch** hai thông điệp `slog` ở `pricing.go:80,87` sang **tiếng Anh** (`"model metadata refresh failed"`, `"model metadata ready"`, giữ nguyên khóa `module`/`err`/`count`); mọi comment (gồm chú thích trường `json` ở `registry.go:14-21`) sang tiếng Anh.
- [ ] **Step 3:** `scripts/docker-go.sh test ./internal/models/... ./internal/hancheck/...` → PASS.
- [ ] **Step 4: Commit** `refactor: Dọn chữ Hán và comment ở internal/models`.

### Task 4: `internal/stylestat`

**Files:** Modify `internal/stylestat/{stylestat,tracker}.go` và hai file `_test.go`; Modify `internal/hancheck/hancheck_test.go`.

- [ ] **Step 1: Viết test giữ hành vi** trong `stylestat_test.go`: `TestZhPatternsStillMatch` — với mỗi mẫu trong bảng ở `stylestat.go:78-85` một câu tiếng Trung mẫu (vd `"他心想这不对"` khớp mẫu "思维标记"), kỳ vọng `regexp.MatchString` đúng; thêm kiểm `openingTimeRe`, `titlePrefixRe`, `sentenceSplit`. Chạy → PASS (khóa hành vi trước khi sửa).
- [ ] **Step 2:** xóa `internal/stylestat` khỏi hai map; chạy hancheck với `-pending=internal/stylestat` → FAIL.
- [ ] **Step 3: Dịch.** Regex và hằng dữ liệu (`stylestat.go:78-93,193`) giữ nguyên, gắn `han:keep`. **Nhãn** (phần tử đầu mỗi mẫu, vd `矫正句『不是…(而)是…』`) → tiếng Việt, giữ ví dụ Hán trong dấu `『』` nếu cần để tra, hoặc dịch ví dụ (`câu chỉnh lý『không phải…mà là…』`); cập nhật các test đang khẳng định nhãn cũ. Comment → tiếng Anh.
- [ ] **Step 4:** `scripts/docker-go.sh test ./internal/stylestat/... ./internal/hancheck/...` → PASS; `grep -rn "style_stats\|stylestat" internal --include=*.go -l` rồi chạy test các package đó để chắc nhãn mới không làm gãy test khác.
- [ ] **Step 5: Commit** `refactor: Dịch nhãn stylestat, giữ regex tiếng Trung`.

### Task 5: `internal/eval`

**Files:** Modify `internal/eval/{eval,case,collect,runner,grade,report}.go` và 6 file `_test.go`; Modify `internal/hancheck/hancheck_test.go`.

- [ ] **Step 1: Rà so khớp.** Chạy `grep -nE 'Contains|HasPrefix|regexp|== "' internal/eval/*.go | grep -v _test` và liệt kê mọi chỗ so khớp văn bản đến từ package khác (kết quả tool, log, bản thảo). Với mỗi chỗ, tìm chuỗi nguồn hiện tại (sau phase 1) và viết/sửa test trong `grade_test.go`/`collect_test.go` dùng đúng chuỗi nguồn đó; chuỗi so khớp tiếng Trung chỉ dành cho bản thảo tiếng Trung giữ `han:keep`.
- [ ] **Step 2:** xóa `internal/eval` khỏi hai map; chạy hancheck với `-pending=internal/eval` → FAIL.
- [ ] **Step 3: Dịch** chuỗi và báo cáo sang **tiếng Anh**, cập nhật test khẳng định chuỗi cũ (`grade_test.go` 56 dòng, `collect_test.go`, `runner_test.go`, `case_test.go`); comment → tiếng Anh. Không đổi `evals/cases/**/*.json`.
- [ ] **Step 4:** `scripts/docker-go.sh test ./internal/eval/... ./internal/hancheck/...` → PASS.
- [ ] **Step 5: Commit** `refactor: Dọn chữ Hán và comment ở internal/eval`.

### Task 6: `internal/diag`

**Files:** Modify 12 file nguồn và 4 file `_test.go` trong `internal/diag/`; Modify `internal/hancheck/hancheck_test.go`.

- [ ] **Step 1: Rà so khớp.** Với `runtime.go`, `runtime_rules.go`, `rules_*.go`, `redact.go`: liệt kê mọi so khớp văn bản log/sự kiện/tool result (vd `runtime_rules.go:55` `" · err: "` và `"(args invalid)"` do chính `runtime.go:204-211` sinh ra, nên không đổi). Chỗ nào so khớp chuỗi do package khác sinh ra thì dùng chuỗi hiện tại của package đó. Thêm test `TestRuntimeRulesFireOnCurrentStrings` (trong `runtime_rules_test.go`) dựng `RuntimeCapture` với các `Repeats` mẫu và khẳng định `repeatedErrors`, `stuckStep`, `streamIdleStorm` vẫn ra `Finding` đúng `Rule`.
- [ ] **Step 2:** xóa `internal/diag` khỏi hai map; chạy hancheck với `-pending=internal/diag` → FAIL (~96 vị trí).
- [ ] **Step 3: Dịch** `Title`/`Evidence`/`Suggestion`/nhãn báo cáo và `export.go` sang **tiếng Anh**; cập nhật `export_test.go` (38 dòng), `runtime_rules_test.go`, `rules_quality_test.go`, `planner_test.go`; comment → tiếng Anh. Chuỗi trong `redact.go` là mẫu so khớp tiếng Trung thì `han:keep`.
- [ ] **Step 4:** `scripts/docker-go.sh test ./internal/diag/... ./internal/hancheck/...` → PASS.
- [ ] **Step 5: Commit** `refactor: Dọn chữ Hán và comment ở internal/diag`.

### Task 7: Đổi thuật ngữ trong `assets/` (bản Việt)

**Files:** Modify `assets/prompts/*.md`, `assets/voice.md`, `assets/styles/*.md`, `assets/references/*.md`, `assets/references/genres/*/*.md`, `assets/README.md`, `assets/load_test.go`.

**Interfaces:** Produces — bảng thuật ngữ trong `assets/README.md` dùng đúng từ mới (Task 8 và 9 bám theo).

- [ ] **Step 1: Script thay thế một lần** (`$SCRATCH/rename_terms.py`, không commit): nhận danh sách file, thay theo bảng Global Constraints, giữ hoa/thường (`Đại cương`→`Dàn ý`), có `--dry-run` in `file:line` + 40 ký tự ngữ cảnh. Thứ tự thay: cụm dài trước (`cung khung xương`, `tập khép lại`, `nghiệm thu từng chương`, `hợp đồng chương`, `Bộ tài phán`) rồi từ đơn.
- [ ] **Step 2: Chạy `--dry-run`, rà tay** các câu gượng sau thay: `bản thảo chính văn`→`bản thảo chương`; danh từ `quy hoạch`→`kế hoạch`, động từ→`lập kế hoạch`; `hoàn kết` đứng sau "tự/sau khi" → `kết thúc truyện`; `điểm móc`/`móc câu`→`hook` (không viết hoa giữa câu). Ghi quyết định ngoại lệ vào chính script.
- [ ] **Step 3: Chạy thật, cập nhật** bảng "Thuật ngữ" ở `assets/README.md` và `assets/load_test.go` nếu nó khẳng định từ cũ.
- [ ] **Step 4:** `scripts/docker-go.sh test ./assets/...` → PASS (kiểm tên trường/tiêu đề còn khớp `zh/`).
- [ ] **Step 5:** `grep -rniE "đại cương|phục bút|thẩm duyệt|chính văn|hoàn kết|tài phán|quy hoạch|tập khép|nghiệm thu|khung xương|hợp đồng chương|điểm móc|móc câu|sảng điểm|gọt giũa" assets --include='*.md' | grep -v '/zh/' | grep -v voice_zh` → không còn dòng nào.
- [ ] **Step 6: Commit** `refactor: Đổi thuật ngữ Hán-Việt khó hiểu trong prompt tiếng Việt`.

### Task 8: Đổi thuật ngữ trong chuỗi Go, test và tài liệu người dùng

**Files:** Modify các file Go có chuỗi chứa thuật ngữ cũ (dự kiến `internal/tools`, `internal/flow`, `internal/agents`, `internal/arbiter`, `internal/llmcontract`, `internal/host`, `internal/entry`, `internal/bootstrap` và `_test.go` tương ứng); `README.md`, `HUONG_DAN_SU_DUNG.md`, `config.example.jsonc`, `config/config.example.jsonc` nếu có; Modify `internal/hancheck/terms_test.go` (đặt `termsPending = false`).

- [ ] **Step 1: Viết test đỏ.** Đặt `termsPending = false`, chạy `scripts/docker-go.sh test ./internal/hancheck/ -run TestNoLegacyTerms` → FAIL, liệt kê `file:line: term`. Ghi tổng số hit.
- [ ] **Step 2: Thay** bằng script của Task 7 (`--dry-run` trước, rà tay các câu gượng) trên đúng các file test báo; chỉ sửa string literal và Markdown, không đụng định danh Go. Chuỗi LLM nhận phải dùng đúng từ như prompt Task 7.
- [ ] **Step 3: Sửa test khẳng định từ cũ** (`commit_chapter_test.go`, `novel_context_test.go`, `panels_test.go`…): chạy `scripts/docker-go.sh test ./...` và sửa đến khi xanh.
- [ ] **Step 4:** `scripts/docker-go.sh test ./internal/hancheck/ -run TestNoLegacyTerms` → PASS.
- [ ] **Step 5: Commit** `refactor: Đổi thuật ngữ Hán-Việt trong chuỗi Go, test và tài liệu`.

### Task 9: Quy tắc TUI R1–R5

**Files:** Modify `internal/entry/**`, `internal/host/**` (thông báo hiển thị), `internal/bootstrap/*.go`, `internal/notify/*.go` và các `_test.go` liên quan; Modify `internal/hancheck/hancheck_test.go` (`titleCasePending = false`).

- [ ] **Step 1: Viết test đỏ.** Đặt `titleCasePending = false`; chạy `scripts/docker-go.sh test ./internal/hancheck/ -run TestTUINotTitleCase` → FAIL, liệt kê chuỗi vi phạm R1.
- [ ] **Step 2: Áp R1–R5** theo từng file, bắt đầu từ nhãn tĩnh (`panels_sidebar.go` 84, `panels.go`, `panels_outline.go`, `model.go`, `cocreate.go`, `commands.go`, `command_help.go`, `command_palette.go`, `statusbar.go`), sau đó thông báo động ở `internal/host`. Quy đổi bắt buộc: `Bộ Nhớ Đệm (Cache)`→`Cache`; `Cửa sổ ngữ cảnh`→`Context window`; `hồ sơ mô phỏng`→`style profile`; `Architect quy hoạch / Writer sáng tác / Editor thẩm duyệt`→`Architect lập kế hoạch / Writer sáng tác / Editor đánh giá`; `Khởi động xuất bản thất bại:`→`Không xuất được:`; `Đang tiến hành xuất bản...`→`Đang xuất...`; `Premise` hiển thị→`Tiền đề`; `Đồng sáng tác` giữ nguyên.
- [ ] **Step 3: Sửa test** (`panels_test.go`, `commands_*_test.go`, `dispatch_summary_test.go`…) khẳng định nhãn cũ.
- [ ] **Step 4:** `scripts/docker-go.sh test ./internal/entry/... ./internal/host/... ./internal/bootstrap/... ./internal/notify/... ./internal/hancheck/...` → PASS.
- [ ] **Step 5: Commit** `refactor: Chuẩn hóa chữ TUI theo R1-R5 (sentence case, thuật ngữ kỹ thuật tiếng Anh)`.

### Task 10: Comment → tiếng Anh: `internal/host` (kể cả `exp`, `sim`)

**Files:** Modify mọi `.go` (cả `_test.go`) trong `internal/host/` và `internal/host/{exp,sim}/` (~1.190 dòng comment); Modify `internal/hancheck/hancheck_test.go` (xóa các thư mục này khỏi `pendingComments`).

**Interfaces:** Produces — quy trình cho Task 11–16 (cùng bước): dịch comment, không sửa code.

- [ ] **Step 1:** xóa các thư mục khỏi `pendingComments`; `scripts/docker-go.sh test ./internal/hancheck/ -run TestCommentsEnglish` → FAIL, liệt kê file:dòng.
- [ ] **Step 2: Dịch** mọi comment sang tiếng Anh tự nhiên, giữ nguyên nghĩa, định danh và con số; chuỗi minh họa dữ liệu zh trong comment gắn `han:keep`. Chỉ đổi comment — không sửa code, không format lại.
- [ ] **Step 3: Kiểm diff chỉ chứa comment:** `git diff -U0 -- internal/host | grep -E '^[+-][^+-]' | grep -vE '^[+-]\s*(//|/\*|\*)'` — chỉ được còn các dòng có comment đuôi (`code // comment`); mở từng dòng để chắc phần code không đổi.
- [ ] **Step 4:** `scripts/docker-go.sh build ./... && scripts/docker-go.sh test ./internal/host/... ./internal/hancheck/...` → PASS.
- [ ] **Step 5: Commit** `refactor: Dịch comment internal/host sang tiếng Anh`.

### Task 11: Comment → tiếng Anh: `internal/host/imp` (~587 dòng)
Các bước và kiểm tra như Task 10 (Step 1–5), phạm vi `internal/host/imp/`. Commit `refactor: Dịch comment internal/host/imp sang tiếng Anh`.

### Task 12: Comment → tiếng Anh: `internal/store`, `internal/tools` (~1.024 dòng)
Như Task 10, phạm vi hai thư mục. Chạy thêm `scripts/docker-go.sh test ./internal/store/... ./internal/tools/...`. Commit `refactor: Dịch comment store và tools sang tiếng Anh`.

### Task 13: Comment → tiếng Anh: `internal/entry/**`, `cmd/ainovel-cli` (~390 dòng)
Như Task 10, phạm vi `internal/entry/tui`, `entry/startup`, `entry/headless`, `cmd/ainovel-cli`. Commit `refactor: Dịch comment entry và cmd sang tiếng Anh`.

### Task 14: Comment → tiếng Anh: `internal/domain`, `internal/bootstrap` (~511 dòng)
Như Task 10. Commit `refactor: Dịch comment domain và bootstrap sang tiếng Anh`.

### Task 15: Comment → tiếng Anh: `internal/rules`, `internal/flow`, `internal/agents` (+ `guard`, `ctxpack`) (~500 dòng)
Như Task 10. Commit `refactor: Dịch comment rules, flow, agents sang tiếng Anh`.

### Task 16: Comment → tiếng Anh: các package nhỏ còn lại
Phạm vi: `internal/{arbiter,userrules,llmcontract,utils,notify,logger,llmretry,chapterfacts,revision}`, `assets` (file `.go`). Như Task 10. Cuối task `pendingComments` phải **rỗng**: `grep -n 'phase2' internal/hancheck/hancheck_test.go` không còn dòng thuộc `pendingComments`. Commit `refactor: Dịch comment các package nhỏ sang tiếng Anh`.

### Task 17: `docs/*.md` → tiếng Anh

**Files:** Modify 12 file: `architecture`, `observability`, `context-management`, `import-pipeline`, `refactor-flow-driven`, `evaluation-system`, `prompt-cache-design`, `user-rules-runtime`, `chapter-advance-gate`, `engine-arbiter`, `voice-layer`, `engine-rfc`. Không đụng `docs/superpowers/`.

- [ ] **Step 1: Ghi baseline** mỗi file: `grep -c '^#' f` (số tiêu đề), `grep -c '```' f` (số khối code), `wc -l f` vào `$SCRATCH/docs_baseline.txt`.
- [ ] **Step 2: Dịch** từng file sang tiếng Anh (giao theo cặp file cho subagent Sonnet; ưu tiên `architecture`, `observability` trước). Giữ nguyên cấu trúc, tiêu đề, khối code, bảng, tên file/định danh; dịch cả chú thích bên trong khối code. Link nội bộ giữ nguyên.
- [ ] **Step 3: Kiểm parity:** số tiêu đề và số khối code mỗi file bằng baseline; `grep -cP '\p{Han}' f` = 0 trừ dòng cố ý trích dữ liệu tiếng Trung.
- [ ] **Step 4: Commit** từng 3–4 file một: `docs: Dịch <tên file> sang tiếng Anh`.

### Task 18: Hoàn tất

**Files:** Modify `config.example.jsonc` (dòng 3, bỏ "指针"); Modify `internal/hancheck/hancheck_test.go` (không còn bản ghi `phase2`).

- [ ] **Step 1:** sửa `config.example.jsonc` dòng 3 thành `trỏ tới (pointer)` hoặc bỏ chú thích Hán; đối chiếu `config/config.example.jsonc`.
- [ ] **Step 2: Kiểm toàn cục:**
  - `scripts/docker-go.sh vet ./... && scripts/docker-go.sh test ./...` → PASS.
  - `grep -rlP '\p{Han}' --include='*.go' . | xargs grep -nP '\p{Han}' | grep -v 'han:keep'` → chỉ còn dòng dữ liệu cố ý (khớp `han:keep` dòng trên) hoặc không còn gì.
  - `grep -rlP '\p{Han}' docs README.md HUONG_DAN_SU_DUNG.md config.example.jsonc | grep -v superpowers` → không còn file nào.
- [ ] **Step 3:** cập nhật dòng trạng thái đầu `docs/superpowers/specs/2026-10-10-viet-hoa-phase2-design.md` (đã triển khai) và `.../2026-10-09-...-design.md` (phase 2 xong).
- [ ] **Step 4: Commit** `docs: Hoàn tất Việt hóa phase 2`.
