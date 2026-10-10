package chapterfacts

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// Reproduces a real incident: the model named every new foreshadowing "new", so from the second one on
// they collided with the old record and were silently swallowed - 22 chapters planted 6 times, 2 recorded.
func TestRejectsPlaceholderForeshadowID(t *testing.T) {
	for _, id := range []string{"new", "New", " none ", "TBD", "-", "n/a", "auto"} {
		f := domain.ChapterFacts{
			Title:     "Chương 1: Dược Nông",
			Summary:   "Lê An lần đầu cảm nhận khí trời đất qua cây cỏ",
			KeyEvents: []string{"Lê An chạm vào cây thông già"},
			ForeshadowUpdates: []domain.ForeshadowUpdate{
				{ID: id, Action: "plant", Description: "Ánh mắt vô hình soi xét từ xa"},
			}}
		err := Validate(f)
		if err == nil {
			t.Errorf("id %q là占位符, phải bị bác", id)
			continue
		}
		if !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("id %q: thông báo phải giải thích lý do, nhận %v", id, err)
		}
	}
}

func TestAcceptsRealForeshadowID(t *testing.T) {
	for _, id := range []string{"FORESHADOW_OBSERVER", "jade-pendant", "f1", "quan-sat-vien"} {
		f := domain.ChapterFacts{
			Title:     "Chương 1: Dược Nông",
			Summary:   "Lê An lần đầu cảm nhận khí trời đất qua cây cỏ",
			KeyEvents: []string{"Lê An chạm vào cây thông già"},
			ForeshadowUpdates: []domain.ForeshadowUpdate{
				{ID: id, Action: "plant", Description: "Ánh mắt vô hình soi xét từ xa"},
			}}
		if err := Validate(f); err != nil {
			t.Errorf("id %q hợp lệ nhưng bị bác: %v", id, err)
		}
	}
}
