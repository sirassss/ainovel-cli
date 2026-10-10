package store

import (
	"os"
	"slices"
	"sort"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// CastStore manages the supporting-cast ledger (meta/cast_ledger.json).
//
// The cast ledger records "named minor characters who have appeared" and is orthogonal to characters.json (the core character profiles):
//   - characters.json: protagonists + key supporting characters explicitly designed by the Architect, not modified during writing
//   - cast_ledger.json: accumulated automatically by the commit_chapter tool, all named non-core supporting characters
//
// MergeAppearances is idempotent: committing the same chapter repeatedly does not double-count AppearanceCount.
type CastStore struct{ io *IO }

func NewCastStore(io *IO) *CastStore { return &CastStore{io: io} }

const castLedgerPath = "meta/cast_ledger.json"

// Load reads the cast ledger. Returns an empty slice if the file does not exist.
func (s *CastStore) Load() ([]domain.CastEntry, error) {
	var entries []domain.CastEntry
	if err := s.io.ReadJSON(castLedgerPath, &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// Save saves the whole cast ledger (atomic write).
func (s *CastStore) Save(entries []domain.CastEntry) error {
	return s.io.WriteJSON(castLedgerPath, entries)
}

// MergeAppearances merges this chapter's appearance records into the ledger.
//
// Parameters:
//   - chapter: this chapter's number
//   - characters: array of names appearing in this chapter (from commit_chapter.Characters)
//   - intros: new-character introductions explicitly declared by the Writer (first appearance, or filling in BriefRole)
//   - knownCore: set of core character names already in characters.json (these are skipped for ledger writes)
//
// Behavior:
//   - Name is in knownCore: skip (the core character profile is its only record entry)
//   - Name is already in the ledger and chapter is already in AppearanceChapters: skip entirely (idempotent)
//   - Name is already in the ledger but chapter is new: update LastSeenChapter + append chapter + count++
//   - Name is not in the ledger: add a new entry
//   - BriefRole from intros is adopted only if the ledger entry's BriefRole is still empty, to avoid overwriting an earlier introduction
func (s *CastStore) MergeAppearances(
	chapter int,
	characters []string,
	intros []domain.CastIntro,
	knownCore map[string]bool,
) error {
	if chapter <= 0 || len(characters) == 0 {
		return nil
	}
	return s.io.WithWriteLock(func() error {
		var entries []domain.CastEntry
		if err := s.io.ReadJSONUnlocked(castLedgerPath, &entries); err != nil && !os.IsNotExist(err) {
			return err
		}

		introMap := make(map[string]string, len(intros))
		for _, in := range intros {
			if in.Name != "" {
				introMap[in.Name] = in.BriefRole
			}
		}

		index := make(map[string]int, len(entries))
		for i, e := range entries {
			index[e.Name] = i
			for _, alias := range e.Aliases {
				index[alias] = i
			}
		}

		seen := make(map[string]bool, len(characters))
		for _, name := range characters {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if knownCore[name] {
				continue
			}
			if i, ok := index[name]; ok {
				entry := &entries[i]
				if !slices.Contains(entry.AppearanceChapters, chapter) {
					entry.AppearanceChapters = append(entry.AppearanceChapters, chapter)
					entry.AppearanceCount = len(entry.AppearanceChapters)
					if chapter > entry.LastSeenChapter {
						entry.LastSeenChapter = chapter
					}
					if chapter < entry.FirstSeenChapter || entry.FirstSeenChapter == 0 {
						entry.FirstSeenChapter = chapter
					}
				}
				if entry.BriefRole == "" {
					if br, ok := introMap[name]; ok && br != "" {
						entry.BriefRole = br
					}
				}
				continue
			}
			entries = append(entries, domain.CastEntry{
				Name:               name,
				BriefRole:          introMap[name],
				FirstSeenChapter:   chapter,
				LastSeenChapter:    chapter,
				AppearanceCount:    1,
				AppearanceChapters: []int{chapter},
			})
		}
		return s.io.WriteJSONUnlocked(castLedgerPath, entries)
	})
}

// RecentActive returns the N most recently active supporting-cast entries (ordered by LastSeenChapter descending).
// Used by novel_context to recall the "recently appearing supporting characters" the Writer may need when writing the next chapter.
//
// Entries already promoted to characters.json (Promoted=true) are skipped, to avoid recalling duplicates of the core profiles.
func (s *CastStore) RecentActive(limit int) ([]domain.CastEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	entries, err := s.Load()
	if err != nil {
		return nil, err
	}
	active := entries[:0:0]
	for _, e := range entries {
		if e.Promoted {
			continue
		}
		active = append(active, e)
	}
	if len(active) == 0 {
		return nil, nil
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].LastSeenChapter != active[j].LastSeenChapter {
			return active[i].LastSeenChapter > active[j].LastSeenChapter
		}
		return active[i].AppearanceCount > active[j].AppearanceCount
	})
	if len(active) > limit {
		active = active[:limit]
	}
	return active, nil
}
