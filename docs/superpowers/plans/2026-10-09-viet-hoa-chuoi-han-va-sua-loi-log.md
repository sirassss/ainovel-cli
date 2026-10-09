# Việt hóa chuỗi chữ Hán và sửa lỗi từ tui.log — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Chuỗi chạy thật trong Go (giai đoạn 1) hết chữ Hán, 6 vấn đề trong `tui.log` được xử lý, có test chống tái phát.

**Architecture:** Dịch thẳng tại chỗ, không thêm lớp i18n. Một test quét AST (`internal/hancheck`) là cổng chất lượng: mỗi package có một dòng trong map `pending`; task dịch package nào thì xóa dòng đó và test bắt buộc package ấy sạch chữ Hán. Bug hành vi (pause, id phục bút, premise, giới hạn cung) sửa bằng TDD riêng trước khi dịch.

**Tech Stack:** Go 1.25 (máy chưa cài Go, chạy qua Docker), `go/parser` + `go/ast`, `log/slog`.

**Spec:** `docs/superpowers/specs/2026-10-09-viet-hoa-chuoi-han-va-sua-loi-log-design.md`

## Global Constraints

- Chỉ dịch **chữ Hán → tiếng Việt**. Chuỗi tiếng Anh kiểu system log giữ nguyên (`InputValidationError…`, `tool precondition failed`, `context canceled`, `module=…`).
- Không đụng `assets/*/zh/`, `assets/voice_zh.md` và chế độ `language=zh`. Chuỗi Hán là *dữ liệu so khớp* hoặc dữ liệu cũ thì giữ và gắn marker `han:keep` (xem Task 1).
- Không tạo bảng thuật ngữ mới. Dùng đúng từ đang có trong prompt vi: 弧→**cung**, 骨架弧→**cung khung xương**, 卷→**tập**, 章→**chương**, 伏笔→**phục bút**, 裁定/Arbiter→**tài phán** (Bộ tài phán), 逐章验收→**nghiệm thu từng chương**, 评审/审阅→**thẩm duyệt**, 返工/重写→**viết lại** (hàng đợi viết lại), 摘要→**tóm tắt**, 大纲→**đại cương**, 前提→**tiền đề**, 派发→**phân công**. "advance gate", "Engine", "Worker", tên tool/field giữ nguyên.
- Khóa thuộc tính `slog` bằng chữ Hán (vd `"来源"`) đổi sang snake_case tiếng Anh (`"sources"`); message của `slog` dịch sang tiếng Việt.
- Chạy Go bằng `scripts/docker-go.sh` (tạo ở Task 1): `scripts/docker-go.sh test ./internal/host/...`. Baseline: toàn bộ `go test ./...` đang pass.
- Commit message tiếng Việt, kiểu `fix:`/`feat:`/`docs:`; mỗi task một commit; kết thúc bằng dòng `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Không `git add` `docker-compose.yml` (thay đổi dở của người dùng).

## Quy trình dịch một package (áp dụng Task 8–13)

1. Lấy danh sách cần dịch: `scripts/docker-go.sh test ./internal/hancheck -run 'TestNoHan/<dir>' -pending=<dir>` (cờ `-pending` ép chạy dir đang nằm trong `pending`, xem Task 1); output liệt kê `file:dòng: literal`.
2. Với mỗi chuỗi: `grep -rn` đoạn Hán đặc trưng của nó trên toàn repo. Nếu nó được **so khớp** ở nơi khác (`strings.Contains/HasPrefix`, `regexp`, `switch case`, khóa map, test của package khác) thì dịch **cả hai phía trong cùng commit**, kể cả phía nằm ở package phase 2. Nếu so khớp với đầu vào zh hoặc dữ liệu cũ thì giữ nguyên và thêm `// han:keep <lý do>`.
3. Dịch theo Global Constraints. Dấu câu full-width (`，：（）；！？“”`) đổi sang ASCII. Câu gửi cho LLM phải tự nhiên, đủ nghĩa, không dịch từng chữ.
4. Sửa các test đang khẳng định chuỗi Hán sang chuỗi mới (test dùng `strings.Contains` với đoạn Hán thì đổi sang đoạn Việt tương ứng).
5. Xóa dòng package khỏi `pending` trong `internal/hancheck/hancheck_test.go`.
6. Xác minh: `scripts/docker-go.sh test ./internal/hancheck ./<các package vừa sửa>/...` pass.
7. Commit.

## Review Focus

- Premise tiếng Việt có tiêu đề kèm đuôi mô tả mà model chép từ prompt (`## Hướng kết cục (Định hướng chủ đề)`, `## Móc câu khác biệt: …`) vẫn phải parse đúng (Task 4).
- Premise tiếng Trung của chế độ `zh` vẫn parse được sau khi đổi khóa chuẩn sang tiếng Việt (Task 4).
- Pause thủ công: lỗi **không phải** hủy xảy ra trong lúc đang `aborting` vẫn phải báo `error` (Task 2).
- Sổ phục bút rỗng: lỗi id lạ phải nói rõ "chưa có phục bút nào", không in danh sách rỗng (Task 3).
- Chuỗi bị dịch nhưng đang được so khớp ở nơi khác (vd `Reason` của flow, `Summary` của host mà diag/TUI đọc lại) làm hỏng logic im lặng (quy trình bước 2 + `go test ./...` toàn repo ở Task 14).

---

### Task 1: Script Docker và test quét chữ Hán

**Files:**
- Create: `scripts/docker-go.sh`
- Create: `internal/hancheck/hancheck_test.go`

**Interfaces:**
- Produces: `scripts/docker-go.sh <go args…>`; map `pending map[string]string` trong `hancheck_test.go` (khóa là thư mục tương đối so với gốc repo, vd `"internal/host"`; giá trị là lý do); marker `han:keep`.

- [ ] **Step 1: Tạo `scripts/docker-go.sh`** (executable): chạy `docker run --rm -v ainovel-gomod:/go/pkg/mod -v "$PWD":/src -w /src -e GOWORK=off -e GOFLAGS=-buildvcs=false golang:1.25 go "$@"` với `set -euo pipefail`; `docker volume create ainovel-gomod` trước (idempotent).
- [ ] **Step 2: Viết test `TestNoHan` trong `internal/hancheck/hancheck_test.go`.**
  - Duyệt `internal/`, `cmd/`, `assets/` (đường dẫn `../../…` vì test nằm ở `internal/hancheck`), gom thư mục có file `.go` không phải `_test.go`.
  - Mỗi thư mục là một subtest tên `TestNoHan/<dir>`. Dùng `go/parser.ParseFile` (`parser.ParseComments`), duyệt `*ast.BasicLit` kiểu `token.STRING`, giải `strconv.Unquote`, bỏ qua comment (AST không đưa vào).
  - Một literal vi phạm nếu chứa rune thỏa `unicode.Is(unicode.Han, r)` hoặc `0x3000<=r<=0x303F` hoặc `0xFF00<=r<=0xFFEF`.
  - Bỏ qua literal có `han:keep` trong dòng chứa literal hoặc dòng ngay trước nó (đọc dòng từ nguồn).
  - Subtest lỗi in `file:dòng: <literal cắt 80 ký tự>` rồi `t.Errorf`.
  - Nếu `dir` có trong `pending` và cờ `-pending` không khớp `dir` thì `t.Skip(pending[dir])`. Khai báo cờ bằng `flag.String("pending", "", …)` ở mức package.
  - `pending` ban đầu chứa **mọi** thư mục đang có chữ Hán: `internal/{host,host/imp,host/sim,host/exp,tools,flow,agents,agents/ctxpack,agents/guard,arbiter,llmcontract,chapterfacts,domain,userrules,rules,store,notify,entry/…,bootstrap,logger}` với lý do `"phase1: task N"` theo bảng ở cuối plan, và `internal/{diag,eval,revision,version,stylestat}` + mọi thư mục còn lại với lý do `"phase2"`. Thư mục nào sinh vi phạm mà chưa có trong `pending` thì phải thêm vào (đây là cách lấy danh sách chính xác).
  - Thêm `TestScannerDetectsHan`: viết một file `.go` tạm trong `t.TempDir()` chứa `"核心"`, `"ok：x"`, `"ok"` và `"数据" // han:keep` rồi gọi hàm quét dùng chung; kỳ vọng đúng 2 vi phạm (`核心`, `ok：x`). Để làm được, tách hàm `scanFile(path string) []violation`.
- [ ] **Step 3: Chạy** `scripts/docker-go.sh test ./internal/hancheck -v` — kỳ vọng PASS (mọi thư mục có Hán đều SKIP), `TestScannerDetectsHan` PASS.
- [ ] **Step 4: Xác nhận cờ ép chạy:** `scripts/docker-go.sh test ./internal/hancheck -run 'TestNoHan/internal/domain' -pending=internal/domain` — kỳ vọng FAIL và liệt kê literal của `domain`.
- [ ] **Step 5: Commit** `feat: Test quét chữ Hán trong chuỗi Go và script chạy Go qua Docker`.

---

### Task 2: Dừng thủ công không còn bị ghi là lỗi

**Files:**
- Modify: `internal/host/observer_tools.go:291` (`emitCallFinish`)
- Test: `internal/host/observer_test.go`

**Interfaces:**
- Consumes: `observer.aborting` (`atomic.Bool`, `observer.go:76`), `observer.setAborting(bool)`, `testObserver(*[]Event)`, `observer.dispatchFinish(agent string, runErr error)`.

- [ ] **Step 1: Viết hai test** trong `observer_test.go`:
  - `TestObserverDispatchFinishCanceledWhileAbortingIsNotFailure`: `o.setAborting(true)`; `o.dispatchStart("writer","task","reason")`; `o.dispatchFinish("writer", context.Canceled)`; sự kiện cuối có `Failed == false`, `Level == "warn"`, `Kind == "canceled"`, `Summary` không chứa `"错误"`/`"lỗi"`.
  - `TestObserverDispatchFinishRealErrorWhileAbortingStillFails`: cùng cấu hình nhưng `errors.New("boom")` → `Failed == true`, `Level == "error"`.
  - Cả hai có phiên bản `setAborting(false)` + `context.Canceled` → vẫn `Failed == true` (hành vi cũ giữ nguyên).
- [ ] **Step 2: Chạy** `scripts/docker-go.sh test ./internal/host -run 'TestObserverDispatchFinish' -v` — kỳ vọng FAIL ở test đầu.
- [ ] **Step 3: Sửa `emitCallFinish`:** nếu `callErr != nil && o.aborting.Load() && errors.Is(callErr, context.Canceled)` thì xem là hủy có chủ đích: `Failed=false`, `Level="warn"`, `Kind="canceled"`, `Detail=""`, summary là `call.summary + " (đã dừng theo yêu cầu)"`. Mọi trường hợp khác giữ nguyên nhánh lỗi hiện tại. Cập nhật comment `observer.aborting` cho đúng thực tế.
- [ ] **Step 4: Chạy** `scripts/docker-go.sh test ./internal/host -v 2>&1 | tail -20` — kỳ vọng PASS toàn package.
- [ ] **Step 5: Commit** `fix: Dừng thủ công không còn ghi ERROR context canceled`.

---

### Task 3: Lỗi id phục bút lạ kèm danh sách id đã biết

**Files:**
- Modify: `internal/tools/commit_validation.go:40`
- Test: `internal/tools/commit_chapter_test.go` (cạnh test dùng `"unknown id"` ở dòng ~70)

**Interfaces:**
- Consumes: biến `plantedAt map[string]int` ngay trong hàm validate.

- [ ] **Step 1: Viết hai test** gọi `newTestCommitChapterTool(s).Execute` như test hiện có:
  - Sổ có phục bút `a_id`, `b_id`; update `advance` id `ghost` → lỗi chứa `unknown id "ghost"` **và** `a_id` **và** `b_id`, danh sách sắp xếp tăng dần.
  - Sổ rỗng → lỗi chứa `unknown id "ghost"` và `no foreshadows planted yet` (không chứa `[]`).
- [ ] **Step 2: Chạy** `scripts/docker-go.sh test ./internal/tools -run 'ForeshadowUnknown' -v` — kỳ vọng FAIL.
- [ ] **Step 3: Sửa** thông báo thành: `foreshadow_updates[%d] references unknown id %q (known ids: %s; to introduce a new foreshadow use action "plant")` với `%s` là các id sắp xếp, nối `, `, cắt còn 20 id đầu và thêm `…` nếu dài hơn; sổ rỗng thì dùng `no foreshadows planted yet` thay cho phần `known ids`. Giữ nguyên `%w` và `errs.ErrToolPrecondition`; giữ tiếng Anh theo quy tắc log hệ thống.
- [ ] **Step 4: Chạy** `scripts/docker-go.sh test ./internal/tools -run 'CommitChapter|Foreshadow' -v` — kỳ vọng PASS (test cũ `unknown id` vẫn qua).
- [ ] **Step 5: Commit** `fix: Lỗi phục bút lạ liệt kê id đã biết để model tự sửa`.

---

### Task 4: Parser premise nhận tiêu đề tiếng Việt

**Files:**
- Modify: `internal/tools/premise_structure.go` (`premiseHeadingAliases`, `canonicalPremiseHeading`, `requiredPremiseHeadings`)
- Modify: `internal/tools/premise_structure_test.go`, `internal/tools/novel_context_test.go`, `internal/tools/save_foundation_test.go` (test đang dùng khóa Hán)
- Check: `internal/host/resume.go:95` (`legacyPremiseSection`) — dữ liệu premise cũ, giữ Hán + `han:keep`.

**Interfaces:**
- Produces: khóa chuẩn là 15 tiêu đề tiếng Việt: `Thể loại và giọng điệu`, `Định vị thể loại`, `Xung đột cốt lõi`, `Mục tiêu nhân vật chính`, `Hướng kết cục`, `Vùng cấm sáng tác`, `Điểm bán hàng khác biệt`, `Móc câu khác biệt`, `Cam kết cốt lõi`, `Động cơ câu chuyện`, `Tuyến quan hệ/trưởng thành`, `Lộ trình nâng cấp`, `Chuyển hướng trung kỳ`, `Mệnh đề kết cục`, `Tính phù hợp với truyện ngắn`. Tiêu đề Hán hiện có (`核心冲突` …, kèm biến thể `结局方向`, `中期转向`, `本作为什么适合短篇/单卷收束`) là alias trỏ tới khóa Việt tương ứng (`题材和基调→Thể loại và giọng điệu`, `题材定位→Định vị thể loại`, `核心冲突→Xung đột cốt lõi`, `主角目标→Mục tiêu nhân vật chính`, `终局方向/结局方向→Hướng kết cục`, `写作禁区→Vùng cấm sáng tác`, `差异化卖点→Điểm bán hàng khác biệt`, `差异化钩子→Móc câu khác biệt`, `核心兑现承诺→Cam kết cốt lõi`, `故事引擎→Động cơ câu chuyện`, `关系/成长主线→Tuyến quan hệ/trưởng thành`, `升级路径→Lộ trình nâng cấp`, `中段转折/中期转向→Chuyển hướng trung kỳ`, `终局命题→Mệnh đề kết cục`, `短篇适配性/本作为什么适合短篇/单卷收束→Tính phù hợp với truyện ngắn`).

- [ ] **Step 1: Viết test trong `premise_structure_test.go`:**
  - `TestParsePremiseSectionsVietnamese`: premise đủ tiêu đề tầng long viết bằng tiếng Việt → `premiseStructure(p, domain.PlanningTierLong)["template_ready"] == true`, `missing` rỗng, `parsePremiseSections` có khóa `Xung đột cốt lõi`.
  - `TestParsePremiseSectionsTolerateTrailingNote`: `## Hướng kết cục (Định hướng chủ đề)` và `## Móc câu khác biệt: điểm độc đáo` đều ra khóa chuẩn `Hướng kết cục`, `Móc câu khác biệt`.
  - `TestParsePremiseSectionsChineseStillWorks`: premise tiêu đề Hán đủ tầng short → `template_ready == true`, khóa là tiếng Việt.
  - `TestPremiseStructureMissingIsVietnamese`: premise chỉ có `## Xung đột cốt lõi` → `missing` không chứa rune Hán.
- [ ] **Step 2: Chạy** `scripts/docker-go.sh test ./internal/tools -run 'PremiseSections|PremiseStructure' -v` — kỳ vọng FAIL.
- [ ] **Step 3: Cài đặt:** khóa chuẩn tiếng Việt, alias Hán gắn `// han:keep alias tiêu đề premise chế độ zh`; `canonicalPremiseHeading` chuẩn hóa tiêu đề trước khi tra: cắt tại ký tự đầu tiên trong `(`, `（`, `:`, `：` rồi `TrimSpace`; so khớp không phân biệt hoa thường (`strings.ToLower` cả khóa và tiêu đề). `requiredPremiseHeadings` trả khóa Việt (`han:keep` không cần).
- [ ] **Step 4: Sửa test cũ** đang khẳng định khóa Hán (`novel_context_test.go` ~167–343, `save_foundation_test.go:43`, `premise_structure_test.go`): input giữ nguyên (premise Hán), kỳ vọng đổi sang khóa Việt.
- [ ] **Step 5: Chạy** `scripts/docker-go.sh test ./internal/tools ./internal/host/... -v 2>&1 | tail -30` — kỳ vọng PASS.
- [ ] **Step 6: Commit** `fix: Parser premise nhận tiêu đề tiếng Việt, giữ alias chữ Hán cho zh`.

---

### Task 5: Giới hạn 8 chương/cung trong prompt và thông báo lỗi

**Files:**
- Modify: `assets/prompts/architect-long.md` (phần Layered Outline / expand_arc)
- Modify: `internal/domain/story.go:255` (`OversizedArc`)
- Test: `assets/load_test.go`, `internal/domain/story_test.go` (tạo nếu chưa có)

- [ ] **Step 1: Viết test:**
  - `assets/load_test.go`: `TestArchitectLongPromptStatesArcLimit` — `Load("default", LoadOptions{}).Prompts.ArchitectLong` chứa `"8 chương"`.
  - `domain/story_test.go`: `TestOversizedArcMessageIsVietnamese` — `OversizedArc("Tập 1 cung 1", 10)` khác rỗng, không chứa rune Hán, chứa `"10"` và `"8"`; `OversizedArc("x", 8) == ""`.
- [ ] **Step 2: Chạy** `scripts/docker-go.sh test ./assets ./internal/domain -run 'ArcLimit|OversizedArc' -v` — kỳ vọng FAIL.
- [ ] **Step 3: Sửa `OversizedArc`:** dịch sang tiếng Việt giữ đúng ý và số liệu: một cung chỉ được tối đa `maxArcChapters` chương chi tiết vì thẩm duyệt cuối cung phải đọc trọn cung; yêu cầu tách thành nhiều cung có mục tiêu riêng, lần gọi này chỉ mở rộng cung đầu tiên trong 8 chương, các cung còn lại để dạng khung xương rồi mở rộng khi viết tới biên.
- [ ] **Step 4: Thêm vào `architect-long.md`** (tiếng Việt, đặt ở nơi mô tả cung/`expand_arc`) một quy tắc: mỗi cung tối đa 8 chương chi tiết (kể cả `estimated_chapters` của cung khung xương), vì thẩm duyệt cuối cung đọc trọn cung; cung dài hơn phải tách thành nhiều cung có mục tiêu riêng.
- [ ] **Step 5: Chạy lại bước 2** — kỳ vọng PASS.
- [ ] **Step 6: Commit** `fix: Nêu giới hạn 8 chương mỗi cung trong prompt kiến trúc sư và lỗi tool`.

---

### Task 6: Cảnh báo cache-break và thông báo notify-send

**Files:**
- Modify: `internal/host/usage.go:200` (`noteCacheBreak`, cả chuỗi `hint` ở ~195)
- Modify: `internal/notify/notify.go:160,165`
- Test: `internal/notify/notify_test.go`, `internal/host/usage_test.go` (`TestCacheBreakDetection` ~457 nếu có khẳng định `hint`)

- [ ] **Step 1: Viết test** `TestRunSystemLinuxWithoutNotifySendLogsDebug` trong `notify_test.go`: bỏ qua nếu `runtime.GOOS != "linux"`; `t.Setenv("PATH", t.TempDir())`; thay `slog.Default()` bằng logger ghi vào `bytes.Buffer` với `Level: slog.LevelDebug` (khôi phục bằng `t.Cleanup`); gọi `runSystem(ctx, Notification{Title:"t", Body:"b"})` → `err == nil`; bản ghi có `level=DEBUG`, không chứa rune Hán, vẫn chứa `module=notify`. Thêm một lần chạy với handler mức `Info` khẳng định buffer rỗng.
- [ ] **Step 2: Chạy** `scripts/docker-go.sh test ./internal/notify -run RunSystem -v` — kỳ vọng FAIL.
- [ ] **Step 3: Sửa `notify.go`:** hai message dịch sang tiếng Việt (vd `Thông báo hạ xuống log (không có notify-send)`), `slog.Info` → `slog.Debug`. Sửa `usage.go`: message `Chuỗi cache bị đứt: tiền tố không ngắn đi nhưng lượng cache hit sụt mạnh`; các `hint` Hán dịch sang `nghi ngờ TTL 1h đã hết hạn` / `nghi ngờ TTL 5m đã hết hạn` / `nghi ngờ máy chủ loại bỏ cache hoặc định tuyến lệch (relay xoay vòng upstream là nguyên nhân thường gặp)`; khóa attr giữ nguyên (đã là tiếng Anh).
- [ ] **Step 4: Sửa test** `usage_test.go` nếu đang so khớp `hint` Hán. Chạy `scripts/docker-go.sh test ./internal/notify ./internal/host -run 'RunSystem|CacheBreak' -v` — kỳ vọng PASS.
- [ ] **Step 5: Commit** `fix: Việt hóa cảnh báo cache-break, hạ log thiếu notify-send xuống DEBUG`.

---

### Task 7: Dịch `flow`, `llmcontract`, `agents/guard`, `chapterfacts`

**Files:** mọi `.go` trong `internal/flow`, `internal/llmcontract`, `internal/agents/guard`, `internal/chapterfacts` và test đi kèm. Ví dụ phải dịch: `flow/router.go` (`Reason: "卷摘要缺失"` → `"Thiếu tóm tắt tập"`, `Task: "写第 %d 章"` → `"Viết chương %d"`, `"PendingRewrites 队列剩余 %d 章"`), `llmcontract/execute.go:77` (`semanticCorrection`), `llmcontract/validate.go` (lỗi validate), `guard/subagent_guards.go`.

- [ ] Làm theo **Quy trình dịch một package** cho 4 thư mục: `internal/flow`, `internal/llmcontract`, `internal/agents/guard`, `internal/chapterfacts`.
- [ ] **Test bổ sung** `flow/router_test.go`: `TestRouterInstructionsHaveNoHan` — với các tình huống đã có trong test router (thiếu tóm tắt tập, cung chờ mở rộng, hàng đợi viết lại, viết chương kế), mọi `Instruction.Task`/`Reason` không chứa rune Hán.
- [ ] Xóa 4 dòng khỏi `pending`, chạy `scripts/docker-go.sh test ./internal/hancheck ./internal/flow/... ./internal/llmcontract/... ./internal/agents/... ./internal/chapterfacts/...` — kỳ vọng PASS.
- [ ] Commit `feat: Việt hóa chuỗi flow, llmcontract, guard, chapterfacts`.

---

### Task 8: Dịch `tools`

**Files:** mọi `.go` không-test trong `internal/tools` (~300 chuỗi: `Description()`, `schema.Property(... mô tả)`, lỗi `fmt.Errorf`, khóa `_usage`, nhãn `items`), cộng test của package.

- [ ] Làm theo **Quy trình dịch một package** cho `internal/tools`. Chú ý riêng:
  - Mô tả tool/schema là *hướng dẫn cho LLM*: dịch đủ ý, giữ nguyên tên tham số/giá trị enum (`expand_arc`, `update_compass`, `plant|advance|resolve`…) và các con số (giới hạn 8 chương, `scale` short/mid/long).
  - Nhãn dùng làm khóa so khớp hoặc tiền tố được parse lại (vd `novel_context.go` các nhãn `items`, `_usage`) phải kiểm tra bằng grep trước khi dịch.
  - Alias tiêu đề premise đã xử lý ở Task 4.
- [ ] Sau khi dịch, chạy `scripts/docker-go.sh test ./internal/tools/... ./internal/host/...` — kỳ vọng PASS (host dùng tools).
- [ ] Xóa `internal/tools` khỏi `pending`; commit `feat: Việt hóa mô tả tool, schema và lỗi tool`.

---

### Task 9: Dịch `domain`, `store`, `userrules`, `rules`, `arbiter`, `agents` (phần còn lại)

**Files:** `internal/{domain,store,userrules,rules,arbiter}`, `internal/agents`, `internal/agents/ctxpack`.

- [ ] Làm theo **Quy trình dịch một package** lần lượt cho từng thư mục trên. Chú ý riêng: `agents/ctxpack/restore.go`, `builder.go` và `agents/context_manager.go` sinh văn bản đưa thẳng cho LLM (gói ngữ cảnh khôi phục); `userrules/normalize.go` chứa hợp đồng cho LLM chuẩn hóa luật; `domain/*.go` có nhãn enum/String() hiển thị.
- [ ] Chạy `scripts/docker-go.sh test ./internal/domain/... ./internal/store/... ./internal/userrules/... ./internal/rules/... ./internal/arbiter/... ./internal/agents/... ./internal/hancheck` — kỳ vọng PASS.
- [ ] Xóa các dòng tương ứng khỏi `pending`; commit `feat: Việt hóa chuỗi domain, store, userrules, rules, arbiter, agents`.

---

### Task 10: Dịch `host` (lõi) và `entry`, `bootstrap`, `logger`, `notify`, `assets`

**Files:** `internal/host/*.go` (không gồm thư mục con; ~230 chuỗi gồm `host.go`, `engine.go`, `resume.go`, `advance_gate.go`, `cocreate*.go`, `events.go`, `model_config.go`, `observer*.go`, `stream_extract.go`), `internal/entry/{headless,startup,tui}`, `internal/bootstrap`, `internal/logger`, `internal/notify`, `assets/load.go`, `assets/README.md`.

- [ ] Làm theo **Quy trình dịch một package** cho từng thư mục. Chú ý riêng:
  - `Summary`/`Detail` của `Event` hiển thị trên TUI và ghi vào `tui.log`; `emitCallFinish` có `"%s 错误: %s"` → `"%s lỗi: %s"`. Grep `Summary` được TUI/diag so khớp lại trước khi dịch.
  - `host/resume.go:95` và `legacyPremiseTitle` (bỏ `《》`) là dữ liệu premise cũ: giữ Hán + `// han:keep dữ liệu premise phiên bản cũ`.
  - `host/host.go` các khóa `slog` Hán (`"来源"`, `"禁用短语"`, `"疲劳词"`) → `"sources"`, `"forbidden_phrases"`, `"fatigue_words"`.
  - `assets/README.md`: dịch toàn bộ 17 dòng Hán sang tiếng Việt; `assets/load.go`: 3 dòng còn lại.
- [ ] Chạy `scripts/docker-go.sh test ./internal/host ./internal/entry/... ./internal/bootstrap/... ./internal/logger/... ./internal/notify/... ./assets/... ./internal/hancheck` — kỳ vọng PASS.
- [ ] Xóa các dòng khỏi `pending`; commit `feat: Việt hóa sự kiện host, TUI, bootstrap và assets README`.

---

### Task 11: Dịch `host/imp`

**Files:** mọi `.go` trong `internal/host/imp` (~330 chuỗi: `contracts.go` mô tả schema cho LLM nhập truyện, `runner.go`, `segment.go`, `analyze.go`, `synthesize.go`, `state.go`, `publish.go`, `call.go`) và test.

- [ ] Làm theo **Quy trình dịch một package**. Chú ý riêng: `contracts.go` là schema/hợp đồng LLM phân tích truyện nhập vào (có thể là truyện tiếng Trung hoặc Việt): dịch mô tả, **không** đổi tên field/enum; regex nhận dạng đầu chương (`第N章`, `Chương N`) là dữ liệu so khớp zh → `han:keep`.
- [ ] Chạy `scripts/docker-go.sh test ./internal/host/imp/... ./internal/hancheck` — kỳ vọng PASS.
- [ ] Xóa `internal/host/imp` khỏi `pending`; commit `feat: Việt hóa pipeline nhập truyện (host/imp)`.

---

### Task 12: Dịch `host/sim`, `host/exp` và `internal/revision`

**Files:** `internal/host/sim`, `internal/host/exp` (xuất EPUB/TXT), `internal/revision` (tiền xử lý/phân tích sửa đổi gửi cho LLM; thuộc giai đoạn 1 vì gửi LLM, bổ sung so với danh sách trong spec).

- [ ] Làm theo **Quy trình dịch một package** cho từng thư mục. `exp/epub.go` có nhãn metadata/tiêu đề mục lục tiếng Hán: dịch nhưng kiểm tra bằng test xuất EPUB/TXT hiện có.
- [ ] Chạy `scripts/docker-go.sh test ./internal/host/... ./internal/revision/... ./internal/hancheck` — kỳ vọng PASS.
- [ ] Xóa các dòng khỏi `pending`; commit `feat: Việt hóa host/sim, host/exp, revision`.

---

### Task 13: Rà tối nghĩa trong prompt và reference bản vi

**Files:** `assets/prompts/*.md` (14), `assets/references/*.md` + `references/genres/*/*.md`, `assets/styles/*.md`, `assets/voice.md`. Không đụng `zh/`.
- Test: `assets/load_test.go`

- [ ] **Step 1: Viết test** `TestVietnamesePromptsHaveNoHan`: với `Load("default", …)` và mỗi style (`default fantasy romance suspense wuxia`), mọi chuỗi trong `Prompts`, `Voice`, `Styles` và nội dung reference không chứa rune Hán. Chạy → FAIL (3 dòng của `arbiter-intervention.md`).
- [ ] **Step 2: Sửa** các cụm Hán/Anh chèn thẳng: `arbiter-intervention.md` dòng 9–10 `(最小充分范围 / Minimum sufficient scope)` → bỏ phần Hán, giữ tiếng Việt thuần (`phạm vi tối thiểu đủ dùng`); tương tự `(分析范围不等于修改范围…)`. Dòng còn lại do `grep -nP '[\x{4e00}-\x{9fff}]'` chỉ ra.
- [ ] **Step 3: Rà tối nghĩa:** đọc từng prompt vi (14 file) và sửa các câu dịch máy khó hiểu: câu mà người đọc tiếng Việt không suy ra được hành động (vd "phạm vi tối thiểu đủ dùng" nên kèm giải nghĩa ngắn "chỉ gồm các chương thật sự cần để đáp ứng yêu cầu"; "Cung khung xương" nên kèm "(cung mới chỉ có mục tiêu, chưa chia chương chi tiết)" ở lần xuất hiện đầu mỗi file). Giữ nguyên tên field, tên tool, ví dụ JSON và các từ vựng đã chốt ở Global Constraints. Ghi vào thân commit danh sách file đã sửa.
- [ ] **Step 4: Chạy** `scripts/docker-go.sh test ./assets/...` — kỳ vọng PASS.
- [ ] **Step 5: Commit** `docs: Rà soát prompt tiếng Việt, bỏ chữ Hán chèn và câu tối nghĩa`.

---

### Task 14: Xác minh toàn bộ

- [ ] **Step 1: Chạy** `scripts/docker-go.sh vet ./... && scripts/docker-go.sh test ./...` — kỳ vọng toàn bộ PASS, không còn dòng `FAIL`.
- [ ] **Step 2: Kiểm `pending`:** chỉ còn các thư mục phase 2 (`diag`, `eval`, `version`, `stylestat`, `host/…` nếu còn) với lý do `"phase2"`; chạy `scripts/docker-go.sh test ./internal/hancheck -v | grep SKIP` để liệt kê.
- [ ] **Step 3: Đếm lại** chữ Hán còn trong chuỗi chạy thật của phase 1: các thư mục không còn trong `pending` có 0 vi phạm (đã được test bảo đảm); ghi số dòng còn lại của phase 2 vào thân commit.
- [ ] **Step 4: Dò thủ công đường chạy:** build `scripts/docker-go.sh build ./cmd/ainovel-cli` và xác nhận biên dịch qua; (tùy chọn) chạy một vòng ngắn với model mock nếu repo có sẵn chế độ headless test.
- [ ] **Step 5: Cập nhật spec**: đổi dòng "Trạng thái" thành `đã triển khai giai đoạn 1`; commit `docs: Đánh dấu hoàn tất giai đoạn 1 Việt hóa`.

---

## Bảng gán `pending` → task (dùng ở Task 1)

| Thư mục | Task |
|---|---|
| `internal/flow`, `internal/llmcontract`, `internal/agents/guard`, `internal/chapterfacts` | 7 |
| `internal/tools` | 8 |
| `internal/domain`, `internal/store`, `internal/userrules`, `internal/rules`, `internal/arbiter`, `internal/agents`, `internal/agents/ctxpack` | 9 |
| `internal/host`, `internal/entry/*`, `internal/bootstrap`, `internal/logger`, `internal/notify`, `assets` | 10 |
| `internal/host/imp` | 11 |
| `internal/host/sim`, `internal/host/exp`, `internal/revision` | 12 |
| `internal/diag`, `internal/eval`, `internal/version`, `internal/stylestat`, còn lại | phase2 |
