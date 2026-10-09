# Bản đồ nội dung assets

Trước khi thêm cho hệ thống "một đoạn lời / một tài liệu / một quy tắc", hãy tra bảng dưới để xác định nó thuộc đâu, rồi mới xem cách nối dây.

Bản mặc định là tiếng Việt, nằm ngay ở thư mục gốc của mỗi mục (`prompts/`, `references/`, `styles/`, `voice.md`). Bản tiếng Trung cho chế độ `language=zh` nằm ở thư mục con `zh/` (hoặc `voice_zh.md`) và có cấu trúc tương ứng.

| Thư mục | Chứa gì | Ai dùng | Cách nối dây |
|---|---|---|---|
| `prompts/` | system prompt của Worker (writer / editor / architect×2), prompt tài phán của Arbiter và prompt tác vụ một lần (import / simulation / revision) | `agents/build.go`, `internal/arbiter`, các runner imp / sim / revision | trường Prompts trong `load.go`. Lưu ý: simulation_guidance được `load.go` chèn vào khi nạp, không thấy trong tệp md |
| `references/` | Tài liệu kiến thức viết không phụ thuộc thể loại. Không vào system prompt, do novel_context cắt theo vai trò / chương rồi chèn vào `reference_pack` | writer / editor / architect | **Nối ba chỗ**: thêm trường vào `tools.References` + `load.go` loadReferences đọc + `novel_context.go` writerReferences / architectReferences chèn. Bỏ vào thư mục sẽ không tự động được nạp |
| `references/genres/<style>/` | Kiến thức riêng của thể loại (style-references / arc-templates) | như trên, nạp khi `style != default` | `load.go` loadReferences |
| `rules/` | Thư mục quy tắc tích hợp kiểu cũ đã bị loại bỏ; đường cơ sở cơ học đã chuyển vào mã, quy tắc người dùng đến từ snapshot ngôn ngữ tự nhiên của `~/.ainovel/rules/*.md` / `./.ainovel/rules/*.md` | `userrules.Service` chuẩn hóa thành `meta/user_rules.json`; `novel_context` chèn; `commit_chapter` kiểm tra | Đường cơ sở tích hợp xem `SystemDefaults()` trong `internal/rules/snapshot.go`; tệp `.md` của người dùng không cần định dạng, không cần YAML, được chuẩn hóa theo ngôn ngữ tự nhiên |
| `styles/<style>.md` | Chỉ thị văn phong theo thể loại | ghép vào system prompt của **writer** (`agents/build.go`) | Tên tệp chính là giá trị `config.style`. Cùng khái niệm thể loại với `references/genres/<style>/` nhưng là hai loại vật mang: loại trước là chỉ thị văn phong, loại sau là tài liệu kiến thức |

## Phán đoán nội dung mới thuộc đâu (năm câu hỏi)

1. Quy trình này bắt buộc phải được **đảm bảo**? → Đừng viết prompt, hãy viết ràng buộc trong mã (StopAfterTools / bộ chặn công cụ / Flow Router)
2. Đây là căn cứ tài phán? → Quy trình dạng tra bảng viết trong `internal/flow/router.go`; phán đoán ngữ nghĩa viết trong `prompts/arbiter-*.md`
3. Đây là tiêu chuẩn thẩm mỹ / thực thi của một vai trò? → `prompts/<role>.md`
4. Đây là quy tắc mặc định có thể liệt kê cơ học (từ cấm / ngưỡng)? → `SystemDefaults()` trong `internal/rules/snapshot.go`; quy tắc tự định nghĩa của người dùng viết vào `.ainovel/rules/*.md`, do snapshot chuẩn hóa tiêu thụ (số chữ/dung lượng là ràng buộc mềm về ngữ nghĩa, đi qua preferences, không làm thành quy tắc cơ học)
5. Đây là tài liệu kiến thức viết? → `references/` (nhớ nối ba chỗ)

## Đảm bảo nhất quán

Đường dẫn phong bì mà prompt tham chiếu (`working_memory.*`, v.v.) phải khớp với `novel_context`. Hình dạng tham số công cụ chỉ được định nghĩa trong Schema của công cụ; prompt chỉ bổ sung ngữ nghĩa nghiệp vụ mà Schema không biểu đạt được, không sao chép lại danh sách tham số JSON hay ví dụ hình dạng.

Prompt có thể mô tả cách thực thi của một Worker, nhưng định tuyến toàn cục, chuyển trạng thái và logic khôi phục chỉ lấy mã làm chuẩn. Bước nào xác định được từ sự thật trong Store thì đưa vào Router/Tool; chỉ những phán đoán cần hiểu nội dung tiểu thuyết hoặc ý định người dùng mới để lại cho model.
