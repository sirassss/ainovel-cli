package store

import (
	"os"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// UsageStore persists cumulative token / cost usage to meta/usage.json.
// Writes go through IO's atomic write (tmp + rename); each Save fully overwrites the whole state.
type UsageStore struct{ io *IO }

func NewUsageStore(io *IO) *UsageStore { return &UsageStore{io: io} }

// Load reads usage.json. Returns (nil, nil) when the file does not exist or the schema version does not match,
// leaving the caller to decide whether to backfill once via session replay.
func (s *UsageStore) Load() (*domain.UsageState, error) {
	var state domain.UsageState
	if err := s.io.ReadJSON("meta/usage.json", &state); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if state.Schema != domain.UsageSchemaVersion {
		return nil, nil
	}
	return &state, nil
}

// Save fully overwrites the state on disk. The caller is responsible for debounce / throttling.
func (s *UsageStore) Save(state domain.UsageState) error {
	state.Schema = domain.UsageSchemaVersion
	return s.io.WriteJSON("meta/usage.json", state)
}
