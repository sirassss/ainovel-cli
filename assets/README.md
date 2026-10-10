# Bản đồ nội dung assets

Trước khi thêm cho hệ thống "một đoạn lời / một tài liệu / một quy tắc", hãy tra bảng dưới để xác định nó thuộc đâu, rồi mới xem cách nối dây.

Bản mặc định là tiếng Việt, nằm ngay ở thư mục gốc của mỗi mục (`prompts/`, `references/`, `styles/`, `voice.md`). Bản tiếng Trung cho chế độ `language=zh` nằm ở thư mục con `zh/` (hoặc `voice_zh.md`) và có cấu trúc tương ứng.

| Thư mục | Chứa gì | Ai dùng | Cách nối dây |
|---|---|---|---|
| `prompts/` | system prompt của Worker (writer / editor / architect×2), prompt của Trọng tài (Arbiter) và prompt tác vụ một lần (import / simulation / revision) | `agents/build.go`, `internal/arbiter`, các runner imp / sim / revision | trường Prompts trong `load.go`. Lưu ý: simulation_guidance được `load.go` chèn vào khi nạp, không thấy trong tệp md |
| `references/` | Tài liệu kiến thức viết không phụ thuộc thể loại. Không vào system prompt, do novel_context cắt theo vai trò / chương rồi chèn vào `reference_pack` | writer / editor / architect | **Nối ba chỗ**: thêm trường vào `tools.References` + `load.go` loadReferences đọc + `novel_context.go` writerReferences / architectReferences chèn. Bỏ vào thư mục sẽ không tự động được nạp |
| `references/genres/<style>/` | Kiến thức riêng của thể loại (style-references / arc-templates) | như trên, nạp khi `style != default` | `load.go` loadReferences |
| `rules/` | Thư mục quy tắc tích hợp kiểu cũ đã bị loại bỏ; đường cơ sở cơ học đã chuyển vào mã, quy tắc người dùng đến từ snapshot ngôn ngữ tự nhiên của `~/.ainovel/rules/*.md` / `./.ainovel/rules/*.md` | `userrules.Service` chuẩn hóa thành `meta/user_rules.json`; `novel_context` chèn; `commit_chapter` kiểm tra | Đường cơ sở tích hợp xem `SystemDefaults()` trong `internal/rules/snapshot.go`; tệp `.md` của người dùng không cần định dạng, không cần YAML, được chuẩn hóa theo ngôn ngữ tự nhiên |
| `styles/<style>.md` | Chỉ thị văn phong theo thể loại | ghép vào system prompt của **writer** (`agents/build.go`) | Tên tệp chính là giá trị `config.style`. Cùng khái niệm thể loại với `references/genres/<style>/` nhưng là hai loại vật mang: loại trước là chỉ thị văn phong, loại sau là tài liệu kiến thức |

## Phán đoán nội dung mới thuộc đâu (năm câu hỏi)

1. Quy trình này bắt buộc phải được **đảm bảo**? → Đừng viết prompt, hãy viết ràng buộc trong mã (StopAfterTools / bộ chặn công cụ / Flow Router)
2. Đây là căn cứ phân xử? → Quy trình dạng tra bảng viết trong `internal/flow/router.go`; phán đoán ngữ nghĩa viết trong `prompts/arbiter-*.md`
3. Đây là tiêu chuẩn thẩm mỹ / thực thi của một vai trò? → `prompts/<role>.md`
4. Đây là quy tắc mặc định có thể liệt kê cơ học (từ cấm / ngưỡng)? → `SystemDefaults()` trong `internal/rules/snapshot.go`; quy tắc tự định nghĩa của người dùng viết vào `.ainovel/rules/*.md`, do snapshot chuẩn hóa tiêu thụ (số chữ/dung lượng là ràng buộc mềm về ngữ nghĩa, đi qua preferences, không làm thành quy tắc cơ học)
5. Đây là tài liệu kiến thức viết? → `references/` (nhớ nối ba chỗ)

## Đảm bảo nhất quán

**Tên trường trong prompt phải khớp JSON thật.** Prompt hay viết những cụm như `working_memory.chapter_plan`. Cách đọc: `novel_context` trả về một object JSON gồm vài khối lớn (`working_memory`, `episodic_memory`, `foundation_memory`, `planning_memory`, `reference_pack`…); `working_memory.chapter_plan` nghĩa là trường `chapter_plan` nằm bên trong khối `working_memory`. Những tên này phải trùng từng chữ với JSON mà `novel_context` thực sự trả về — đổi tên trường trong code thì phải sửa prompt.

**Tham số tool chỉ định nghĩa ở Schema của tool.** Prompt chỉ bổ sung ngữ nghĩa nghiệp vụ mà Schema không biểu đạt được (khi nào gọi, thứ tự, điều kiện), không sao chép lại danh sách tham số JSON hay ví dụ hình dạng. Nếu có ví dụ lời gọi, tên tool, tên tham số và giá trị enum (như `type="update_compass"`) phải đúng với Schema.

**Bản Việt phải đầy đủ như bản `zh/`.** `assets/load_test.go` kiểm tra mỗi prompt tiếng Việt có cùng số tiêu đề mục và đủ các tên tool/trường (đặt trong dấu ``) như bản `prompts/zh/`. Sửa một bản thì sửa bản kia.

Prompt có thể mô tả cách thực thi của một Worker, nhưng định tuyến toàn cục, chuyển trạng thái và logic khôi phục chỉ lấy mã làm chuẩn. Bước nào xác định được từ sự thật trong Store thì đưa vào Router/Tool; chỉ những phán đoán cần hiểu nội dung tiểu thuyết hoặc ý định người dùng mới để lại cho model.

## Thuật ngữ (cùng một từ cho cùng một khái niệm)

Prompt, mô tả tool và thông báo trong code dùng thống nhất các từ sau. Thêm nội dung mới thì giữ đúng từ, đừng đặt từ đồng nghĩa khác.

| Thuật ngữ tiếng Việt | Định danh trong code / ý nghĩa |
|---|---|
| tập (volume), cung (arc), chương | `volume`, `arc`, `chapter`: một tập gồm nhiều cung, một cung gồm nhiều chương |
| cung phác thảo | cung mới có `title`, `goal`, `estimated_chapters`, chưa có chương chi tiết; mở rộng bằng `save_foundation(type="expand_arc")` |
| dàn ý (phẳng / phân tầng) | `outline` (truyện ngắn) / `layered_outline` (truyện dài, tập → cung → chương) |
| la bàn | `compass` (`ending_direction`, `open_threads`, `estimated_scale`); cập nhật bằng `type="update_compass"` |
| nội dung chương | văn bản chương gửi cho người đọc (`chapter` text, bản chính thức); phân biệt với tóm tắt, dàn ý |
| lập kế hoạch | bước lên khung truyện trước khi viết chương (`save_foundation`: premise, dàn ý, nhân vật, quy tắc thế giới) |
| điểm đã | điểm thỏa mãn người đọc (tình huống được đền đáp, cú lật, phần thưởng) mà chương cần trao đúng lúc |
| trau chuốt | sửa câu chữ, giữ nguyên cốt truyện; `verdict` = `polish`, chương vào `PendingRewrites`, luồng `polishing` |
| tập cuối | tập kết thúc cả truyện, `append_volume` với `"final": true`; viết xong tự kết thúc truyện |
| kết thúc truyện | `complete_book`, `phase=complete` |
| yêu cầu chương | `chapter_contract` (`required_beats`, `forbidden_moves`, `continuity_checks`…) |
| chi tiết gài trước | `foreshadow`: `plant` (gieo) / `advance` (đẩy tiếp) / `resolve` (thu) |
| hàng đợi viết lại | `PendingRewrites`: các chương chờ viết lại hoặc trau chuốt |
| đánh giá | `save_review`, vai trò editor; kết luận `verdict` = `accept` / `polish` / `rewrite` |
| duyệt từng chương | chế độ chương chỉ được viết khi người dùng cho phép (`/next`), tên nội bộ "advance gate" |
| hook | điểm kéo người đọc đọc tiếp, thường đặt ở cuối chương (trường `hook` trong dàn ý chương) |
| Trọng tài | Arbiter: model phân loại can thiệp / sự cố / khởi động rồi giao việc |
| sự thật | dữ liệu có cấu trúc đã lưu (facts) lấy từ store, không phải suy đoán của model |
