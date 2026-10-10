package chapterfacts

import (
	"fmt"
	"strings"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

// Properties returns the JSON Schema fields shared by complete chapter facts.
func Properties(includeFeedback bool) []schema.Prop {
	textList := func(description string) map[string]any {
		return schema.Array(description, schema.String(description))
	}
	timeline := schema.Object(
		schema.Property("time", schema.String("Thời gian trong truyện")).Required(),
		schema.Property("event", schema.String("Sự kiện")).Required(),
		schema.Property("characters", textList("Nhân vật liên quan")).Required(),
	)
	foreshadow := schema.Object(
		schema.Property("id", schema.String("ID chi tiết gài trước")).Required(),
		schema.Property("action", schema.Enum("Thao tác", "plant", "advance", "resolve")).Required(),
		schema.Property("description", llmcontract.Nullable(schema.String("Mô tả khi plant, các thao tác khác là null"))).Required(),
	)
	relationship := schema.Object(
		schema.Property("character_a", schema.String("Nhân vật A")).Required(),
		schema.Property("character_b", schema.String("Nhân vật B")).Required(),
		schema.Property("relation", schema.String("Quan hệ khi kết thúc chương")).Required(),
	)
	stateChange := schema.Object(
		schema.Property("entity", schema.String("Thực thể")).Required(),
		schema.Property("field", schema.String("Thuộc tính")).Required(),
		schema.Property("old_value", llmcontract.Nullable(schema.String("Giá trị trước thay đổi"))).Required(),
		schema.Property("new_value", schema.String("Giá trị sau thay đổi")).Required(),
		schema.Property("reason", llmcontract.Nullable(schema.String("Nguyên nhân"))).Required(),
	)
	props := []schema.Prop{
		schema.Property("title", schema.String("Tiêu đề cuối cùng")).Required(),
		schema.Property("summary", schema.String("Tóm tắt chương")).Required(),
		schema.Property("characters", textList("Nhân vật xuất hiện")).Required(),
		schema.Property("key_events", textList("Sự kiện then chốt")).Required(),
		schema.Property("timeline_events", schema.Array("Sự kiện dòng thời gian", timeline)).Required(),
		schema.Property("foreshadow_updates", schema.Array("Thao tác chi tiết gài trước", foreshadow)).Required(),
		schema.Property("relationship_changes", schema.Array("Biến đổi quan hệ", relationship)).Required(),
		schema.Property("state_changes", schema.Array("Biến đổi trạng thái", stateChange)).Required(),
		schema.Property("cast_intros", schema.Array("Nhân vật phụ mới", schema.Object(
			schema.Property("name", schema.String("Họ tên")).Required(),
			schema.Property("brief_role", schema.String("Vai trò")).Required(),
		))).Required(),
		schema.Property("hook_type", llmcontract.Nullable(schema.Enum("Hook cuối chương", domain.HookTypes()...))).Required(),
		schema.Property("dominant_strand", llmcontract.Nullable(schema.Enum("Tuyến truyện chủ đạo", domain.DominantStrands()...))).Required(),
	}
	if includeFeedback {
		feedback := schema.Object(
			schema.Property("deviation", schema.String("Mô tả phần lệch khỏi dàn ý")).Required(),
			schema.Property("suggestion", schema.String("Gợi ý điều chỉnh dàn ý phía sau")).Required(),
		)
		feedback["description"] = "Đối tượng gợi ý cho dàn ý phía sau; phải truyền trực tiếp JSON object, không truyền JSON dạng chuỗi"
		props = append(props, schema.Property("feedback", llmcontract.Nullable(feedback)).Required())
	}
	return props
}

// Validate checks the deterministic constraints shared by normal commits and manual revisions.
func Validate(facts domain.ChapterFacts) error {
	if strings.TrimSpace(facts.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if strings.TrimSpace(facts.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if len(facts.KeyEvents) == 0 {
		return fmt.Errorf("key_events must contain at least one event")
	}
	if err := validateTextItems("characters", facts.Characters); err != nil {
		return err
	}
	if err := validateTextItems("key_events", facts.KeyEvents); err != nil {
		return err
	}
	for i, event := range facts.TimelineEvents {
		if strings.TrimSpace(event.Time) == "" || strings.TrimSpace(event.Event) == "" {
			return fmt.Errorf("timeline_events[%d] requires time and event", i)
		}
		if err := validateTextItems(fmt.Sprintf("timeline_events[%d].characters", i), event.Characters); err != nil {
			return err
		}
	}
	for i, update := range facts.ForeshadowUpdates {
		if strings.TrimSpace(update.ID) == "" {
			return fmt.Errorf("foreshadow_updates[%d].id is required", i)
		}
		if placeholderForeshadowID(update.ID) {
			return fmt.Errorf(
				"foreshadow_updates[%d].id = %q là placeholder, không phải định danh. Mỗi chi tiết gài trước cần một id duy nhất và ổn định"+
					"(như FORESHADOW_OBSERVER, jade-pendant), các chương sau dựa vào nó để advance/resolve; "+
					"dùng lại placeholder sẽ khiến chi tiết gài trước mới đụng chi tiết gài trước cũ",
				i, update.ID)
		}
		switch update.Action {
		case "plant":
			if strings.TrimSpace(update.Description) == "" {
				return fmt.Errorf("foreshadow_updates[%d] plant requires description", i)
			}
		case "advance", "resolve":
		default:
			return fmt.Errorf("foreshadow_updates[%d].action invalid: %q", i, update.Action)
		}
	}
	for i, change := range facts.RelationshipChanges {
		if strings.TrimSpace(change.CharacterA) == "" || strings.TrimSpace(change.CharacterB) == "" || strings.TrimSpace(change.Relation) == "" {
			return fmt.Errorf("relationship_changes[%d] requires character_a, character_b and relation", i)
		}
		if change.CharacterA == change.CharacterB {
			return fmt.Errorf("relationship_changes[%d] cannot relate a character to itself", i)
		}
	}
	for i, change := range facts.StateChanges {
		if strings.TrimSpace(change.Entity) == "" || strings.TrimSpace(change.Field) == "" || strings.TrimSpace(change.NewValue) == "" {
			return fmt.Errorf("state_changes[%d] requires entity, field and new_value", i)
		}
	}
	for i, intro := range facts.CastIntros {
		if strings.TrimSpace(intro.Name) == "" || strings.TrimSpace(intro.BriefRole) == "" {
			return fmt.Errorf("cast_intros[%d] requires name and brief_role", i)
		}
	}
	if facts.HookType != "" && !domain.ValidHookType(facts.HookType) {
		return fmt.Errorf("invalid hook_type %q", facts.HookType)
	}
	if facts.DominantStrand != "" && !domain.ValidDominantStrand(facts.DominantStrand) {
		return fmt.Errorf("invalid dominant_strand %q", facts.DominantStrand)
	}
	if facts.Feedback != nil && (strings.TrimSpace(facts.Feedback.Deviation) == "" || strings.TrimSpace(facts.Feedback.Suggestion) == "") {
		return fmt.Errorf("feedback requires deviation and suggestion")
	}
	return nil
}

func validateTextItems(name string, items []string) error {
	for i, item := range items {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("%s[%d] cannot be empty", name, i)
		}
	}
	return nil
}

// placeholderForeshadowIDs are placeholder words models often write into the id field. They carry no identity,
// while a foreshadowing id must be stable across chapters - in practice the model named every new foreshadowing "new", and from the second one on it collided with
// the first and was silently dropped: 22 chapters planted 6 times, yet only 2 were recorded.
var placeholderForeshadowIDs = map[string]struct{}{
	"new": {}, "none": {}, "null": {}, "nil": {}, "tbd": {}, "todo": {},
	"auto": {}, "id": {}, "-": {}, "n/a": {}, "na": {}, "unknown": {}, "?": {},
}

func placeholderForeshadowID(id string) bool {
	_, ok := placeholderForeshadowIDs[strings.ToLower(strings.TrimSpace(id))]
	return ok
}
