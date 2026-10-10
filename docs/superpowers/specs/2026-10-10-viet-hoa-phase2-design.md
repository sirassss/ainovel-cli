# Việt hóa phase 2: ngôn ngữ theo đối tượng đọc — Thiết kế

Ngày: 2026-10-10 · Trạng thái: đã triển khai (kế hoạch `docs/superpowers/plans/2026-10-10-viet-hoa-phase2.md`) · Tiếp nối `2026-10-09-viet-hoa-chuoi-han-va-sua-loi-log-design.md` (phase 1 đã xong, nhánh `feat/viet-hoa-chuoi-han`).

## Mục tiêu
Mỗi đoạn chữ trong repo dùng ngôn ngữ hợp với **người đọc nó**; truyện tiếng Việt không bị lẫn chữ Hán hay từ Hán-Việt khó hiểu.

## Ba quy tắc của người dùng (nguồn gốc của thiết kế)
1. Comment code: **tiếng Anh**.
2. Văn bản nạp cho LLM: giữ chữ Hán nếu tốt hơn; điều người dùng không muốn là truyện Việt có từ Hán-Việt khó hiểu.
3. Chữ hiển thị trên TUI: **tiếng Việt, hoặc tiếng Anh nếu tự nhiên hơn**.

## Chính sách theo đối tượng đọc
| Đối tượng đọc | Ngôn ngữ | Áp dụng cho |
|---|---|---|
| Lập trình viên | **Anh** | Mọi comment Go (kể cả ~164 dòng comment tiếng Việt phase 1 đã thêm), chuỗi `slog`/log, chuỗi và báo cáo trong `diag`, `eval`, `models`, `version` (log, lỗi nội bộ), `docs/*.md` (trừ `docs/superpowers/`, là tài liệu làm việc, giữ tiếng Việt) |
| Người dùng cuối | **Việt** (Anh cho thuật ngữ kỹ thuật, xem R1–R5) | TUI, wizard, thông báo, thông báo tự cập nhật của `version` |
| LLM, chế độ vi | **Việt** | `assets/*` bản Việt, chuỗi Go gửi LLM (kết quả/lỗi tool, `Task`/`Reason`). Chưa có bằng chứng Hán tốt hơn; ngữ cảnh lẫn Hán dễ làm model nhả từ Hán-Việt vào truyện |
| Máy so khớp | **Giữ Hán** (`han:keep`) | Regex, khóa map, dữ liệu chế độ `zh`, `assets/*/zh` |

## Thuật ngữ (đã duyệt): đổi đồng bộ prompt, chuỗi Go, TUI, test
| Cũ | Mới |
|---|---|
| đại cương | dàn ý |
| phục bút | chi tiết gài trước |
| thẩm duyệt | đánh giá |
| chính văn | nội dung chương |
| hoàn kết | kết thúc truyện |
| Bộ tài phán / tài phán | Trọng tài (giữ `Arbiter` trong ngoặc lần đầu) |
| quy hoạch | lập kế hoạch |
| tập khép lại | tập cuối |
| nghiệm thu từng chương | duyệt từng chương |
| cung khung xương | cung phác thảo |
| hợp đồng chương | yêu cầu chương (tên trường `chapter_contract` giữ nguyên) |
| điểm móc / móc câu | hook |
| sảng điểm | điểm đã |
| gọt giũa | trau chuốt |

Giữ nguyên: `khoái cảm`, `thực lực`, `cẩu đạo`, `vô địch lưu`, `lực cản`/`đỉnh điểm`/`thân phận`, và mọi từ Hán-Việt đã thành tiếng Việt thường dùng (`nhân vật`, `xung đột`, `cảm xúc`…). Tên tool và tên trường JSON (`save_review`, `foreshadow`…) không đổi.

Cập nhật: bảng "Thuật ngữ" trong `assets/README.md`, `assets/load_test.go` (khớp tiêu đề/tên trường với `zh/`). Quyết định phase 1 "không đổi thuật ngữ" bị thay thế bằng bảng này.

## Quy tắc TUI (R1–R5, đã duyệt)
- R1 Sentence case, không viết hoa từng chữ (`Tài Nguyên Đã Dùng` → `Tài nguyên đã dùng`).
- R2 Giữ tiếng Anh cho thuật ngữ kỹ thuật: Agent, Model, Provider, Cache, Prompt cache, Context window, Token, Stream, style profile.
- R3 Giữ tiếng Anh cho tên lệnh và tên vai trò (`/review`, Architect, Writer, Editor, Arbiter).
- R4 Từ Hán-Việt theo bảng thuật ngữ trên.
- R5 Thông báo ngắn, chủ động, bỏ câu kiểu dịch từ Trung (`Khởi động xuất bản thất bại:` → `Không xuất được:`).
- `Đồng sáng tác` giữ tiếng Việt. `Premise`/`Tiền đề` thống nhất là `Tiền đề`.
- Áp cho `internal/entry` (452 chuỗi), `internal/host` (727), `bootstrap`, `notify`. Nhãn tĩnh (~250) sửa tay; thông báo động theo cùng quy tắc.

## Xử lý riêng
- **`stylestat`**: regex bắt văn Trung giữ nguyên (`han:keep`); nhãn đưa vào `style_stats` dịch sang Việt. Hệ quả đã biết: với truyện Việt, `stylestat` không bắt được gì. Viết bộ mẫu tiếng Việt là việc riêng, ngoài phase 2.
- **`diag`**: trước khi dịch, rà các rule so khớp text log (vd `runtime_rules.go:55` tách theo `" · err: "`); không để rule so khớp chuỗi mà phase 1 đã đổi.
- **`evals/cases/smoke/*.json`**: prompt tiếng Trung của case chạy thật, **ngoài phạm vi**. `config.example.jsonc` dòng lẫn "指针": sửa.

## Kiểm soát (mở rộng `internal/hancheck`)
- Gỡ 5 package khỏi `pending`; test phải xanh.
- Thêm quét **comment**: không chữ Hán (cho phép `han:keep`) và không ký tự có dấu tiếng Việt trong comment Go (comment là tiếng Anh).
- Chạy `scripts/docker-go.sh test ./...` sau mỗi lô.

## Thứ tự thực thi
1. `version` → `models` → `stylestat` → `eval` → `diag` (chuỗi và comment cùng lúc, mỗi package một commit).
2. Đổi thuật ngữ (prompt + Go + TUI + test), kèm R1–R5 cho TUI.
3. Comment Go còn lại sang tiếng Anh, chia theo package (giao subagent Sonnet; việc cơ học).
4. `docs/*.md` (12 file, ~2.900 dòng Hán) sang tiếng Anh; ưu tiên `architecture`, `observability`.
5. Mở rộng `hancheck` quét comment khi bước 3 xong.

## Rủi ro
- Đổi thuật ngữ sót một nơi → prompt và chuỗi Go lệch nhau, model thấy hai từ cho một khái niệm. Giảm thiểu: grep từng từ cũ về 0 sau khi đổi (trừ `zh/`).
- Dịch comment bằng subagent có thể bóp méo ý. Giảm thiểu: chỉ dịch, không sửa code; build + test sau mỗi lô; xem lại mẫu ngẫu nhiên.
- Không có bằng chứng cho giả định "Việt tốt hơn Hán" với LLM; xác nhận cần chạy `evals/` với API key, ngoài phạm vi.
