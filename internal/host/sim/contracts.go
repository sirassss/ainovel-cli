package sim

import (
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

func textList(description string) map[string]any {
	return schema.Array(description, schema.String(description))
}

var sourceReportContract = llmcontract.Contract{
	Name:        "simulation_source_report",
	Description: "Rút ra phương pháp viết có thể tái sử dụng từ một bài ngữ liệu mà không sao chép nguyên văn",
	Schema: schema.Object(
		schema.Property("title", llmcontract.Nullable(schema.String("Tiêu đề tùy chọn; không xác nhận được thì là null"))).Required(),
		schema.Property("summary", schema.String("Tóm lược giá trị lối viết của văn bản mẫu")).Required(),
		schema.Property("style_observations", textList("Quan sát về góc nhìn kể, cấu trúc câu và kết cấu miêu tả")).Required(),
		schema.Property("common_words", textList("Nhóm từ tần suất cao, hình ảnh và từ chuyển cảnh")).Required(),
		schema.Property("plot_patterns", textList("Mô thức đẩy cốt truyện, bước ngoặt và leo thang xung đột")).Required(),
		schema.Property("hook_patterns", textList("Mô thức mở đầu, cuối chương và hook chênh lệch thông tin")).Required(),
		schema.Property("pacing_notes", textList("Mật độ cảnh và nhịp giải phóng thông tin")).Required(),
		schema.Property("reader_appeal", textList("Phương pháp giữ độc giả đọc tiếp")).Required(),
		schema.Property("reusable_techniques", textList("Thủ pháp cấu trúc có thể học hỏi")).Required(),
		schema.Property("warnings", textList("Rủi ro sao chép và rập khuôn phải tránh")).Required(),
	),
}

var synthesisContract = llmcontract.Contract{
	Name:        "simulation_synthesis",
	Description: "Tổng hợp chân dung hiện có và báo cáo ngữ liệu thành chân dung phương pháp mô phỏng có thể thực thi",
	Schema: schema.Object(
		schema.Property("style", schema.Object(
			schema.Property("narrative_voice", textList("Ngôi kể, khoảng cách và kiểm soát thông tin")).Required(),
			schema.Property("sentence_rhythm", textList("Nhịp điệu câu")).Required(),
			schema.Property("prose_texture", textList("Chất cảm miêu tả")).Required(),
			schema.Property("perspective", textList("Quy tắc góc nhìn")).Required(),
			schema.Property("mood", textList("Sắc thái cảm xúc")).Required(),
			schema.Property("do_not_copy", textList("Nội dung cấm sao chép")).Required(),
		)).Required(),
		schema.Property("lexicon", schema.Object(
			schema.Property("common_words", textList("Nhóm từ thường dùng")).Required(),
			schema.Property("emotion_words", textList("Nhóm từ cảm xúc")).Required(),
			schema.Property("scene_words", textList("Nhóm từ cảnh")).Required(),
			schema.Property("transition_words", textList("Nhóm từ chuyển cảnh")).Required(),
			schema.Property("signature_phrases", textList("Đặc trưng giọng điệu đã trừu tượng hóa, không chứa câu gốc")).Required(),
		)).Required(),
		schema.Property("plot_design", schema.Object(
			schema.Property("opening_patterns", textList("Cách mở đầu")).Required(),
			schema.Property("escalation_patterns", textList("Cách leo thang xung đột")).Required(),
			schema.Property("turning_point_patterns", textList("Thiết kế bước ngoặt")).Required(),
			schema.Property("payoff_patterns", textList("Cách thu hồi và đáp ứng")).Required(),
		)).Required(),
		schema.Property("hook_design", schema.Object(
			schema.Property("hook_types", textList("Loại hook")).Required(),
			schema.Property("placement", textList("Vị trí hook")).Required(),
			schema.Property("cliffhanger_patterns", textList("Cách dừng hồi hộp")).Required(),
			schema.Property("payoff_rules", textList("Quy tắc đáp ứng hook")).Required(),
		)).Required(),
		schema.Property("pacing_density", schema.Object(
			schema.Property("scene_density", textList("Mật độ thông tin mỗi cảnh")).Required(),
			schema.Property("information_release", textList("Nhịp giải phóng thông tin")).Required(),
			schema.Property("dialogue_action_ratio", textList("Tỷ lệ đối thoại, hành động và tâm lý")).Required(),
			schema.Property("compression_rules", textList("Quy tắc triển khai và nén nội dung")).Required(),
		)).Required(),
		schema.Property("reader_engagement", schema.Object(
			schema.Property("methods", textList("Phương pháp thu hút độc giả")).Required(),
			schema.Property("emotional_drivers", textList("Động lực cảm xúc")).Required(),
			schema.Property("progression_rewards", textList("Phần thưởng tiến triển theo giai đoạn")).Required(),
			schema.Property("anti_patterns", textList("Phản mẫu làm giảm sức hút")).Required(),
		)).Required(),
		schema.Property("role_guidance", schema.Object(
			schema.Property("architect", textList("Quy tắc Architect sử dụng chân dung")).Required(),
			schema.Property("writer", textList("Quy tắc Writer học hỏi nhưng không sao chép")).Required(),
			schema.Property("editor", textList("Quy tắc Editor kiểm tra hướng đi và rủi ro xâm phạm")).Required(),
		)).Required(),
	),
}
