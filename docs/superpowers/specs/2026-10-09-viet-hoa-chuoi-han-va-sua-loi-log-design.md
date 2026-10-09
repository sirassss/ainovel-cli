# Việt hóa chuỗi chữ Hán trong Go và sửa lỗi từ tui.log — Thiết kế

Ngày: 2026-10-09 · Trạng thái: chờ duyệt

## Mục tiêu
- Người dùng tiếng Việt thấy TUI, thông báo, lỗi tool hoàn toàn tiếng Việt; LLM không nhận chữ Hán trong task/lỗi tool khi viết truyện tiếng Việt.
- Xử lý các vấn đề thật trong `workspace/output/novel/logs/tui.log`.
- Có unit test, gồm test chống tái phát chữ Hán.

## Nguyên tắc dịch
- **Chỉ dịch chữ Hán → tiếng Việt.** Chuỗi tiếng Anh kiểu system log giữ nguyên (`InputValidationError…`, `tool precondition failed`, `context canceled`, tên module/field).
- **Không đổi thuật ngữ đã có trong prompt:** cung, khung xương, Bộ tài phán (Arbiter), nghiệm thu từng chương, phục bút. Chuỗi Go gửi cho LLM phải dùng đúng các từ này. Không tạo bảng thuật ngữ mới; "advance gate" không vào prompt nên giữ nguyên là tên nội bộ.
- Dịch thẳng tại chỗ, không thêm lớp i18n (YAGNI). `assets/*/zh` và chế độ `language=zh` không đụng tới. Các chuỗi Hán cố tình phục vụ zh (ví dụ regex `第N章`) vào allowlist.

## Giai đoạn 1 — phạm vi (~1.450 chuỗi)
Packages: `host` (gồm `imp`, `sim`, `exp`), `tools`, `flow`, `agents` (gồm `guard`, `ctxpack`), `arbiter`, `llmcontract`, `chapterfacts`, `domain`, `userrules`, `rules`, `store`, `notify`, `entry`, `bootstrap`, `logger`.
Cộng: 3 dòng Hán lẫn trong `assets/prompts/arbiter-intervention.md`, `assets/README.md` (17 dòng), và rà tối nghĩa các prompt `.md` bản vi (thuật ngữ Hán/Anh chèn thẳng như `(最小充分范围 / …)`, câu dịch máy khó hiểu).

Thứ tự theo rủi ro, mỗi lô: sửa chuỗi → sửa test đang khẳng định chữ Hán → `go test`.
1. Gửi cho LLM: `tools/*`, `flow/router.go` (`Task`/`Reason` dispatch), `agents/guard`, `llmcontract`, `chapterfacts`.
2. Hiển thị TUI: `host/*` (Summary, notify), `entry`.
3. Phần còn lại của giai đoạn 1.

## Sửa lỗi từ tui.log (TDD: test fail trước)
| # | Vấn đề | Cách sửa |
|---|---|---|
| 1 | `save_foundation` thiếu `type` hoặc `type="compass"` | Suy luận `type` từ các trường có mặt khi không mơ hồ; alias `compass → update_compass`; lỗi liệt kê giá trị hợp lệ bằng tiếng Việt |
| 2 | Cung 10 chương vượt giới hạn 8 | Nêu rõ giới hạn 8 chương/cung trong `assets/prompts/architect-long.md` (vi); lỗi tool bằng tiếng Việt |
| 3 | `commit_chapter`: id phục bút lạ | Lỗi kèm danh sách id hợp lệ để model tự sửa |
| 4 | Pause thủ công ghi `ERROR … context canceled`, `failed=true` | Phân loại là hủy có chủ đích: không ERROR, không `failed` |
| 5 | Cảnh báo "缓存链断裂" | Chỉ dịch |
| 6 | "通知降级为日志（无 notify-send）" | Dịch, hạ xuống DEBUG |

Việc #1–#3 cần đọc code tool thực tế để xác nhận hành vi trước khi sửa; nếu #1 suy luận `type` không an toàn thì chỉ làm alias + lỗi rõ.

## Test
- Test từng bug ở bảng trên (#1–#4).
- **Test quét chữ Hán** (`internal/...`): parse AST bằng `go/parser`, duyệt string literal (bỏ comment, bỏ `_test.go`), báo lỗi nếu có chữ Hán ngoài allowlist. Áp cho các package giai đoạn 1; giai đoạn 2 mở rộng danh sách.
- Chạy: `docker run --rm -v "$PWD":/src -w /src -e GOWORK=off golang:1.25 go test ./...` (máy chưa cài Go). Baseline hiện tại: toàn bộ pass.

## Giai đoạn 2 (ngoài phạm vi lần này)
`diag`, `eval`, `revision`, `version`, `stylestat`, `slog`, comment Go, `docs/*.md`.

## Rủi ro
- Merge upstream sẽ xung đột ở các file đã dịch (đã chấp nhận).
- Test hiện khẳng định chuỗi Hán ở nhiều nơi (vd `commit_chapter_test.go` ~300 dòng): phải sửa cùng lúc.
- Một số chuỗi Hán là *dữ liệu so khớp* (khóa map, regex, tiền tố được parse lại), dịch sẽ làm hỏng logic: đánh dấu khi rà, không dịch mù.
