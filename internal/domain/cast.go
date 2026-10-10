package domain

// CastEntry is one supporting-character record in the cast roster.
//
// It is decoupled from Character (characters.json, the core profile maintained by the Architect):
//   - CastEntry is accumulated automatically by the commit_chapter tool and records "named minor characters who have appeared"
//   - Character is designed explicitly by the Architect and records the protagonist's and key supporting characters' personality arc / traits / tier
//
// When names collide, Character wins (core characters do not enter cast_ledger), which avoids duplicates.
type CastEntry struct {
	Name string `json:"name"`
	// Aliases currently has no write path; it is reserved for a future "user steer merges aliases" tool
	// (e.g. declaring 'Boss Li' and 'Old Li' to be the same person). MergeAppearances already supports alias lookup.
	Aliases          []string `json:"aliases,omitempty"`
	BriefRole        string   `json:"brief_role,omitempty"` // One-line positioning (filled in by the Writer on first appearance, can be completed later; never overwritten)
	FirstSeenChapter int      `json:"first_seen_chapter"`
	LastSeenChapter  int      `json:"last_seen_chapter"`
	// AppearanceCount is derived from len(AppearanceChapters) and kept in sync on merge.
	// The explicit field is kept so the UI/JSON can read it directly without recomputing every time.
	AppearanceCount    int   `json:"appearance_count"`
	AppearanceChapters []int `json:"appearance_chapters"`
	// Promoted marks that this entry has been promoted to characters.json. RecentActive skips these entries
	// to avoid recalling them twice alongside the core profile. The promotion path is not implemented yet; the field is a reserved hook.
	Promoted bool `json:"promoted,omitempty"`
}

// CastIntro is the Writer's introduction of a newly appearing character, declared at commit_chapter time.
// It is adopted only when the name appears for the first time or BriefRole is still empty in the ledger.
type CastIntro struct {
	Name      string `json:"name"`
	BriefRole string `json:"brief_role"`
}
