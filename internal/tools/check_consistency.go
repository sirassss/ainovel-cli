package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// CheckConsistencyTool returns chapter content and all state data for the Agent to cross-check on its own.
// Pure IO tool: it only loads data and injects no instructions.
type CheckConsistencyTool struct {
	store *store.Store
}

func NewCheckConsistencyTool(store *store.Store) *CheckConsistencyTool {
	return &CheckConsistencyTool{store: store}
}

func (t *CheckConsistencyTool) Name() string { return "check_consistency" }
func (t *CheckConsistencyTool) Description() string {
	return "Nạp bản nháp đã viết và dữ liệu đối chiếu (quy tắc thế giới, chi tiết gài trước, quan hệ, biệt danh, tóm tắt gần nhất) để bạn kiểm tra tính nhất quán. Phải gọi sau draft_chapter"
}
func (t *CheckConsistencyTool) Label() string { return "Kiểm tra nhất quán" }

// Read-only tool (only appends a checkpoint event, does not change state); may be scheduled concurrently.
func (t *CheckConsistencyTool) ReadOnly(_ json.RawMessage) bool        { return true }
func (t *CheckConsistencyTool) ConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *CheckConsistencyTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("Số chương cần kiểm tra")).Required(),
	)
}

func (t *CheckConsistencyTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Chapter int `json:"chapter"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}

	result := map[string]any{"chapter": a.Chapter}
	var warnings []string
	warn := func(scope string, err error) {
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Đọc %s thất bại: %v", scope, err))
		}
	}

	// Chapter content
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	result["content"] = content
	result["word_count"] = wordCount

	// Cross-check data: keep the global consistency-check data and avoid reloading the window data novel_context already has
	if rules, err := t.store.World.LoadWorldRules(); len(rules) > 0 {
		result["world_rules"] = rules
	} else {
		warn("world_rules", err)
	}
	if foreshadow, err := t.store.World.LoadActiveForeshadow(); len(foreshadow) > 0 {
		result["foreshadow_ledger"] = foreshadow
	} else {
		warn("foreshadow_ledger", err)
	}
	if relationships, err := t.store.World.LoadRelationships(); len(relationships) > 0 {
		result["relationships"] = relationships
	} else {
		warn("relationships", err)
	}
	if chars, err := t.store.Characters.Load(); len(chars) > 0 {
		aliasMap := make(map[string]string)
		for _, c := range chars {
			for _, alias := range c.Aliases {
				aliasMap[alias] = c.Name
			}
		}
		if len(aliasMap) > 0 {
			result["alias_map"] = aliasMap
		}
	} else {
		warn("characters", err)
	}
	if summaries, err := t.store.Summaries.LoadRecentSummaries(a.Chapter, 2); len(summaries) > 0 {
		result["recent_summaries"] = summaries
	} else {
		warn("recent_summaries", err)
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(a.Chapter), "consistency_check",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint consistency check: %w", err)
	}
	if len(warnings) > 0 {
		result["status"] = "partial"
		result["_warnings"] = warnings
	}

	return json.Marshal(result)
}
