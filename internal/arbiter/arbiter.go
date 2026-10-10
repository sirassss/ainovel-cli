// Package arbiter is the semantic adjudication layer: an on-demand LLM-as-function.
//
// The two planes are symmetric (docs/engine-arbiter.md §2):
//
//	Deterministic plane: flow.LoadState   → flow.Route     → Instruction
//	Semantic plane:      arbiter.Collect* → arbiter.Decide* → XxxDecision
//
// Discipline: Collect centralizes IO (reads all facts from the store); Decide has no IO apart from the model request managed by the shared executor,
// and can be replayed offline from historical facts; execution belongs to the Engine. Each scenario has one pair of functions plus a dedicated Decision type,
// so an action that does not match the scenario cannot be expressed in the type; the remaining validity is rejected by each type's Validate -
// Arbiter output is as untrusted as any LLM output, and fact validation is the last gate.
package arbiter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

// decideMaxTokens is the output cap of a single adjudication; the adjudication JSON is tiny, so most of the budget is left for the reasoning model's thinking
// (same rationale as userrules.normalizeMaxTokens).
const decideMaxTokens = 8192

// decide hands the scenario contract and business validation to the shared structured executor. No IO apart from the model call.
func decide[T any](ctx context.Context, model agentcore.ChatModel, contract llmcontract.Contract, systemPrompt, payload string, validate func(*T) error) (T, error) {
	out, err := llmcontract.Execute(ctx, model, llmcontract.Request[T]{
		Contract:     contract,
		SystemPrompt: systemPrompt,
		Payload:      payload,
		Options:      []agentcore.CallOption{agentcore.WithMaxTokens(decideMaxTokens)},
		Validate:     validate,
		Agent:        "arbiter",
		Hooks: llmcontract.Hooks{
			Resolved: func(res llmcontract.Resolution) {
				slog.Debug("arbiter structured mode resolved", "module", "arbiter",
					"contract", contract.Name, "structured_mode", res.Mode,
					"capability_source", res.Source, "provider", res.Provider,
					"model", res.Model, "schema_fingerprint", contract.Fingerprint())
			},
			Correction: func(ev llmcontract.Correction) {
				slog.Warn("arbiter output self-correction", "module", "arbiter", "attempt", ev.Attempt,
					"layer", ev.Layer, "structured_mode", ev.Mode, "err", ev.Err)
			},
		},
	})
	if err != nil {
		return out, fmt.Errorf("arbiter: %w", err)
	}
	return out, nil
}

// DispatchOp is the dispatch action shared by all scenarios.
type DispatchOp struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// workerNames are the legal dispatch targets (consistent with those registered by agents.BuildWorkers). An ordered slice:
// it doubles as the schema enum (a deterministic order keeps the fingerprint stable) and the validation allowlist.
var workerNames = []string{"architect_long", "architect_short", "writer", "editor"}

func (d *DispatchOp) validate() error {
	if d == nil {
		return nil
	}
	if !slices.Contains(workerNames, d.Agent) {
		return fmt.Errorf("dispatch.agent không hợp lệ: %q", d.Agent)
	}
	if strings.TrimSpace(d.Task) == "" {
		return fmt.Errorf("dispatch.task không được để trống")
	}
	return nil
}

// dispatchSchema is the nullable schema slot for DispatchOp: only actions that need dispatching supply the object,
// otherwise it is null (strict mode requires all fields, so optional semantics are expressed with null).
func dispatchSchema(desc string) map[string]any {
	return llmcontract.Nullable(schema.Object(
		schema.Property("agent", schema.Enum(desc, workerNames...)).Required(),
		schema.Property("task", schema.String("Mô tả nhiệm vụ đầy đủ giao cho worker đó")).Required(),
	))
}

// marshalPayload serializes the fact pack; a failure is a programming error and must surface - silently fabricating empty facts
// would make the model misjudge on false input.
func marshalPayload(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("arbiter: tuần tự hóa gói sự thật thất bại: %w", err)
	}
	return string(data), nil
}
