package imp

import (
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

func nullableString(description string) map[string]any {
	return llmcontract.Nullable(schema.String(description))
}

func stringList(description string) map[string]any {
	return schema.Array(description, schema.String(description))
}

var segmentContract = llmcontract.Contract{
	Name:        "import_segment",
	Description: "Nhận diện ranh giới chương, tập và văn bản phụ trong văn bản nhập",
	Schema: schema.Object(
		schema.Property("boundaries", schema.Array("Các ranh giới theo thứ tự nguyên văn", schema.Object(
			schema.Property("unit_id", schema.String("unit id nằm trong khoảng owned")).Required(),
			schema.Property("anchor", nullableString("Đoạn định vị nguyên văn khi một unit có nhiều ranh giới; nếu không thì là null")).Required(),
			schema.Property("kind", schema.Enum("Loại ranh giới", kindChapter, kindGroup, kindFrontMatter, kindBackMatter)).Required(),
			schema.Property("title", nullableString("Nguyên văn tiêu đề; không có tiêu đề thì là null")).Required(),
			schema.Property("uncertain", schema.Bool("Có cần người dùng xác nhận hay không")).Required(),
			schema.Property("reason", nullableString("Lý do không chắc chắn; không cần giải thích thì là null")).Required(),
		))).Required(),
	),
}

var analysisContract = llmcontract.Contract{
	Name:        "import_chapter_analysis",
	Description: "Trích xuất sự thật truyện có thể truy vết của các chương liên tiếp",
	Schema: schema.Object(
		schema.Property("chapters", schema.Array("Sự thật từng chương, cùng thứ tự với số chương đầu vào", chapterFactsSchema())).Required(),
	),
}

func chapterFactsSchema() map[string]any {
	characterEvidence := schema.Object(
		schema.Property("chapter", schema.Int("Chương chứa bằng chứng")).Required(),
		schema.Property("name", schema.String("Tên nhân vật")).Required(),
		schema.Property("note", nullableString("Sự thật về nhân vật; không có thì là null")).Required(),
	)
	worldEvidence := schema.Object(
		schema.Property("chapter", schema.Int("Chương chứa bằng chứng")).Required(),
		schema.Property("category", nullableString("Loại sự thật thế giới; không phân loại được thì là null")).Required(),
		schema.Property("fact", schema.String("Sự thật thế giới được nội dung chương tiết lộ rõ ràng")).Required(),
	)
	timelineEvent := schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("time", schema.String("Thời gian trong truyện")).Required(),
		schema.Property("event", schema.String("Sự kiện")).Required(),
		schema.Property("characters", stringList("Nhân vật liên quan")).Required(),
	)
	foreshadow := schema.Object(
		schema.Property("id", schema.String("Dùng lại ID chi tiết gài trước trong ledger")).Required(),
		schema.Property("action", schema.Enum("Hành động chi tiết gài trước", "plant", "advance", "resolve")).Required(),
		schema.Property("description", nullableString("Mô tả chi tiết gài trước khi plant; các trường hợp khác có thể là null")).Required(),
	)
	relationship := schema.Object(
		schema.Property("character_a", schema.String("Nhân vật A")).Required(),
		schema.Property("character_b", schema.String("Nhân vật B")).Required(),
		schema.Property("relation", schema.String("Biến đổi quan hệ")).Required(),
		schema.Property("chapter", schema.Int("Số chương")).Required(),
	)
	stateChange := schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("entity", schema.String("Nhân vật hoặc thực thể")).Required(),
		schema.Property("field", schema.String("Thuộc tính thay đổi")).Required(),
		schema.Property("old_value", nullableString("Trạng thái trước thay đổi; lần xuất hiện đầu tiên thì là null")).Required(),
		schema.Property("new_value", schema.String("Trạng thái sau thay đổi")).Required(),
		schema.Property("reason", nullableString("Nguyên nhân thay đổi; nội dung chương không nói rõ thì là null")).Required(),
	)
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương")).Required(),
		schema.Property("title", schema.String("Tiêu đề chương")).Required(),
		schema.Property("summary", schema.String("Tóm lược chương")).Required(),
		schema.Property("key_events", stringList("Sự kiện then chốt")).Required(),
		schema.Property("core_event", schema.String("Việc quan trọng nhất của chương")).Required(),
		schema.Property("hook", nullableString("Hook cuối chương; không có thì là null")).Required(),
		schema.Property("scenes", stringList("Chuỗi cảnh")).Required(),
		schema.Property("characters", stringList("Nhân vật xuất hiện")).Required(),
		schema.Property("character_evidence", schema.Array("Bằng chứng nhân vật", characterEvidence)).Required(),
		schema.Property("world_evidence", schema.Array("Bằng chứng sự thật thế giới", worldEvidence)).Required(),
		schema.Property("timeline_events", schema.Array("Sự kiện dòng thời gian", timelineEvent)).Required(),
		schema.Property("foreshadow_updates", schema.Array("Chi tiết gài trước tăng thêm", foreshadow)).Required(),
		schema.Property("relationship_changes", schema.Array("Biến đổi quan hệ", relationship)).Required(),
		schema.Property("state_changes", schema.Array("Biến đổi trạng thái", stateChange)).Required(),
		schema.Property("hook_type", schema.Enum("Loại hook cuối chương", domain.HookTypes()...)).Required(),
		schema.Property("dominant_strand", schema.Enum("Tuyến truyện chủ đạo", domain.DominantStrands()...)).Required(),
	)
}

var rangeContract = llmcontract.Contract{
	Name:        "import_range_digest",
	Description: "Tổng hợp cốt truyện và sự thật của một khoảng chương liên tiếp",
	Schema: schema.Object(
		schema.Property("start_chapter", schema.Int("Chương đầu của khoảng")).Required(),
		schema.Property("end_chapter", schema.Int("Chương cuối của khoảng")).Required(),
		schema.Property("plot", schema.String("Tiến triển cốt truyện chính xuyên chương")).Required(),
		schema.Property("characters", stringList("Nhân vật có tiến triển thực chất")).Required(),
		schema.Property("world_facts", stringList("Sự thật thế giới đã được xác lập")).Required(),
		schema.Property("opened_threads", stringList("Tuyến dài mới mở trong khoảng này")).Required(),
		schema.Property("resolved_threads", stringList("Tuyến dài đã thu trong khoảng này")).Required(),
	),
}

var synthesisContract = llmcontract.Contract{
	Name:        "import_book_synthesis",
	Description: "Tổng hợp sự thật toàn sách và đưa ra phạm vi tập/cung liên tục, đầy đủ",
	Schema: schema.Object(
		schema.Property("title", nullableString("Tên sách chính thức trong nội dung chương; không xác nhận được thì là null")).Required(),
		schema.Property("synopsis", schema.String("Lời giới thiệu truyện dành cho độc giả, không tiết lộ nội dung")).Required(),
		schema.Property("premise", schema.String("Mô tả tiền đề truyện bằng Markdown")).Required(),
		schema.Property("characters", schema.Array("Nhân vật chính", schema.Object(
			schema.Property("name", schema.String("Tên nhân vật")).Required(),
			schema.Property("aliases", stringList("Biệt danh và danh hiệu")).Required(),
			schema.Property("role", schema.String("Vai trò tự sự")).Required(),
			schema.Property("description", schema.String("Mô tả nhân vật")).Required(),
			schema.Property("arc", schema.String("Cung nhân vật")).Required(),
			schema.Property("traits", stringList("Đặc điểm nhân vật")).Required(),
			schema.Property("tier", nullableString("Cấp bậc nhân vật; không phán đoán được thì là null")).Required(),
		))).Required(),
		schema.Property("world_rules", schema.Array("Quy tắc thế giới do nội dung chương xác lập", schema.Object(
			schema.Property("category", schema.String("Loại quy tắc")).Required(),
			schema.Property("rule", schema.String("Mô tả quy tắc")).Required(),
			schema.Property("boundary", schema.String("Ranh giới không được vi phạm")).Required(),
		))).Required(),
		schema.Property("structure", schema.Array("Khoảng chương liên tục của tập và cung", schema.Object(
			schema.Property("title", schema.String("Tiêu đề tập")).Required(),
			schema.Property("theme", schema.String("Xung đột cốt lõi hoặc chủ đề của tập")).Required(),
			schema.Property("arcs", schema.Array("Các cung truyện trong tập", schema.Object(
				schema.Property("title", schema.String("Tiêu đề cung")).Required(),
				schema.Property("goal", schema.String("Mục tiêu cung")).Required(),
				schema.Property("start_chapter", schema.Int("Chương bắt đầu")).Required(),
				schema.Property("end_chapter", schema.Int("Chương kết thúc")).Required(),
			))).Required(),
		))).Required(),
		schema.Property("compass", schema.Object(
			schema.Property("ending_direction", schema.String("Hướng kết cục")).Required(),
			schema.Property("open_threads", stringList("Các tuyến dài chưa thu")).Required(),
			schema.Property("estimated_scale", nullableString("Quy mô ước chừng; không phán đoán được thì là null")).Required(),
			schema.Property("last_updated", llmcontract.Nullable(schema.Int("Số chương mới nhất làm căn cứ; không cần điền thì là null"))).Required(),
		)).Required(),
		schema.Property("planning_tier", schema.Enum("Cấp lập kế hoạch", "short", "mid", "long")).Required(),
		schema.Property("story_status", schema.Enum("Truyện đã kết thúc hay chưa", storyOpen, storyClosed, storyUncertain)).Required(),
		schema.Property("status_reason", nullableString("Lý do phán đoán trạng thái")).Required(),
	),
}
