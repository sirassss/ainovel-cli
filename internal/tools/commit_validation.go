package tools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/voocel/ainovel-cli/internal/chapterfacts"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// validateCommitArgs validates the full semantic payload submitted by the model before a PendingCommit is created.
// Errors are returned straight to the model to correct; no half-finished state is produced and no missing value is guessed.
func (t *CommitChapterTool) validateCommitArgs(a commitArgs) error {
	if err := chapterfacts.Validate(a.ChapterFacts); err != nil {
		return fmt.Errorf("%v: %w", err, errs.ErrToolArgs)
	}

	if len(a.ForeshadowUpdates) > 0 {
		ledger, err := t.store.World.LoadForeshadowLedger()
		if err != nil {
			return fmt.Errorf("load foreshadow ledger: %w: %w", errs.ErrStoreRead, err)
		}
		// The ledger is a whole-book projection, while the Projector replays chapter records in chapter order. When an early chapter is rewritten, the ledger
		// still holds foreshadowing planted only by later chapters; letting those through would make the pre-commit check contradict the replay result,
		// leaving the model no way to fix it and locking the rewrite queue. So always use "visible to this chapter" as the criterion.
		plantedAt := make(map[string]int, len(ledger))
		for _, entry := range ledger {
			plantedAt[entry.ID] = entry.PlantedAt
		}
		declared := make(map[string]struct{}, len(a.ForeshadowUpdates))
		for i, update := range a.ForeshadowUpdates {
			switch update.Action {
			case "plant":
				declared[update.ID] = struct{}{}
			case "advance", "resolve":
				if _, ok := declared[update.ID]; ok {
					continue
				}
				at, known := plantedAt[update.ID]
				if !known {
					return fmt.Errorf("foreshadow_updates[%d] references unknown id %q (%s): %w", i, update.ID, describeKnownForeshadowIDs(plantedAt), errs.ErrToolPrecondition)
				}
				if at > a.Chapter {
					return fmt.Errorf("foreshadow_updates[%d] chi tiết gài trước %q được gieo ở chương %d, không thể advance hoặc resolve ở chương %d: %w",
						i, update.ID, at, a.Chapter, errs.ErrToolPrecondition)
				}
			}
		}
	}
	return nil
}

const maxListedForeshadowIDs = 20

// describeKnownForeshadowIDs gives the model a handle for self-correction: it lists the ids already in the ledger.
func describeKnownForeshadowIDs(plantedAt map[string]int) string {
	if len(plantedAt) == 0 {
		return `no foreshadows planted yet; to introduce a new foreshadow use action "plant"`
	}
	ids := make([]string, 0, len(plantedAt))
	for id := range plantedAt {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := strings.Join(ids, ", ")
	if len(ids) > maxListedForeshadowIDs {
		list = strings.Join(ids[:maxListedForeshadowIDs], ", ") + ", …"
	}
	return `known ids: ` + list + `; to introduce a new foreshadow use action "plant"`
}
