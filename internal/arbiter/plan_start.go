package arbiter

import (
	"context"
	"fmt"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

// PlanStartDecision is the start adjudication: picks the planner and produces the task text (expanded if necessary).
type PlanStartDecision struct {
	Planner string `json:"planner"` // architect_long | architect_short
	Task    string `json:"task"`    // the complete task handed to the planner (including the expanded requirements)
	Reason  string `json:"reason"`
}

func (d *PlanStartDecision) Validate() error {
	if d.Planner != "architect_long" && d.Planner != "architect_short" {
		return fmt.Errorf("planner không hợp lệ: %q (chọn architect_long / architect_short)", d.Planner)
	}
	if strings.TrimSpace(d.Task) == "" {
		return fmt.Errorf("task không được để trống")
	}
	if strings.TrimSpace(d.Reason) == "" {
		return fmt.Errorf("reason không được để trống")
	}
	return nil
}

// planStartContract sits next to PlanStartDecision: all fields required, planner is a closed enum.
var planStartContract = llmcontract.Contract{
	Name:        "arbiter_plan_start",
	Description: "Phân xử khởi động: chọn kiến trúc sư và tạo văn bản nhiệm vụ đầy đủ",
	Schema: schema.Object(
		schema.Property("planner", schema.Enum("Kiến trúc sư", "architect_long", "architect_short")).Required(),
		schema.Property("task", schema.String("Nhiệm vụ đầy đủ giao cho kiến trúc sư (gồm yêu cầu đã mở rộng)")).Required(),
		schema.Property("reason", schema.String("Lý do lựa chọn")).Required(),
	),
}

// planStartPayload is the user payload of plan_start (the facts are the input, no store state - a new book).
type planStartPayload struct {
	Requirement string `json:"requirement"`
	Style       string `json:"style,omitempty"`
}

// DecidePlanStart is the start adjudication: picks a planner from the user's requirements; when the requirements are too short (<20 characters), it autonomously adds in the task
// a differentiated direction, the target readers and core consumption points, and at least one unconventional hook.
// Failure semantics: returning an error → the caller explicitly reports the error and aborts the start (the user is present at startup, so an error beats a guess).
func DecidePlanStart(ctx context.Context, model agentcore.ChatModel, systemPrompt, requirement, style string) (PlanStartDecision, error) {
	payload, err := marshalPayload(planStartPayload{Requirement: requirement, Style: style})
	if err != nil {
		return PlanStartDecision{}, err
	}
	return decide(ctx, model, planStartContract, systemPrompt, payload, (*PlanStartDecision).Validate)
}
