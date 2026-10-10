// Package store provides file-system-based persistent storage.
//
// Architecture: 1 IO base + multiple sub-stores + 1 composition root.
// Each sub-store holds its own IO instance and its own sync.RWMutex.
// Reads and writes of the major domains (Progress, Outline, Drafts, Summaries, etc.) do not block each other;
// WorldStore merges several low-frequency small domains under one shared lock.
//
// The composition root Store holds references to all sub-stores and serially coordinates cross-domain operations
// (ExpandArc, AppendVolume, ClearHandledSteer); multiple files do not form an atomic transaction,
// so callers rely on safe write ordering, explicit errors, and idempotent replay with the same arguments for recovery.
//
// Sub-store breakdown:
//   - ProgressStore: main progress state (meta/progress.json)
//   - OutlineStore: premise, outline (flat/layered), compass
//   - DraftStore: chapter plans, drafts, final text
//   - SummaryStore: chapter/arc/volume summaries
//   - RunMetaStore: run metadata (model, intervention history)
//   - SignalStore: one-shot signal files (PendingCommit recovery)
//   - CheckpointStore: step-level checkpoints (meta/checkpoints.jsonl)
//   - RuntimeStore: runtime event queue (meta/runtime/*.jsonl)
//   - CharacterStore: character profiles, state snapshots
//   - WorldStore: timeline, foreshadowing, relationships, state changes, world rules, style rules, reviews
package store
