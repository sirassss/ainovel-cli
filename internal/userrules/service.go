package userrules

import (
	"context"
	"log/slog"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// Service orchestrates generation and update of the user-rule snapshot: normalize each source → deterministic merge → persist.
//
// Two callers share the same logic:
//   - Book start/refresh: Build / GetOrBuild, called deterministically by the Host.
//   - Update during a run: after the Arbiter extracts rules, the Host calls AddRuntimeRule.
type Service struct {
	store     *store.Store
	norm      *Normalizer
	rulesOpts rules.LoadOptions
}

// NewService constructs the service. model is used for normalization (should be a more capable model); when model is nil,
// all sources degrade to raw preferences (a snapshot can still be produced, with mechanical checks covered by system_defaults).
func NewService(st *store.Store, model agentcore.ChatModel, opts rules.LoadOptions) *Service {
	return &Service{store: st, norm: NewNormalizer(model), rulesOpts: opts}
}

// normalizeOrDegrade normalizes one source; on failure it logs the real error and degrades to raw preferences
// (snapshot Status=degraded, original text preserved) - degradation is a visible fact, and the cause goes to the log.
func (s *Service) normalizeOrDegrade(ctx context.Context, source, text string) rules.Candidate {
	cand, err := s.norm.Normalize(ctx, source, text)
	if err != nil {
		slog.Warn("rules normalization failed, falling back to verbatim preferences", "module", "rules", "source", source, "err", err)
		return degraded(source, text)
	}
	return cand
}

// Build normalizes the static sources (system_defaults + rules files + startup prompt) into a snapshot and persists it.
// Called at book start/refresh. startupPrompt may be empty.
func (s *Service) Build(ctx context.Context, startupPrompt string) (*rules.Snapshot, error) {
	cands := []rules.Candidate{rules.SystemDefaults()}
	for _, rs := range rules.RawFileSources(s.rulesOpts) {
		cands = append(cands, s.normalizeOrDegrade(ctx, rs.Label, rs.Text))
	}
	if strings.TrimSpace(startupPrompt) != "" {
		cands = append(cands, s.normalizeOrDegrade(ctx, "startup_prompt", startupPrompt))
	}
	snap := rules.BuildSnapshot(cands)
	if err := s.store.UserRules.Save(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetOrBuild returns the current snapshot; when missing, it initializes from system_defaults + the rules files.
// The runtime read path goes through here uniformly.
func (s *Service) GetOrBuild(ctx context.Context) (*rules.Snapshot, error) {
	cur, err := s.store.UserRules.Load()
	if err != nil {
		return nil, err
	}
	if cur != nil {
		return cur, nil
	}
	return s.Build(ctx, "")
}

// AddRuntimeRule normalizes one long-term rule added during a run, overlays it onto the current snapshot at the highest priority and persists it.
// It never errors because normalization failed - on failure the rule degrades to raw preferences.
// It returns the overlaid snapshot and this round's normalization candidate.
func (s *Service) AddRuntimeRule(ctx context.Context, text string) (*rules.Snapshot, rules.Candidate, error) {
	cur, err := s.GetOrBuild(ctx)
	if err != nil {
		return nil, rules.Candidate{}, err
	}
	cand := s.normalizeOrDegrade(ctx, "runtime_update", text)
	merged := rules.OverlaySnapshot(*cur, cand)
	if err := s.store.UserRules.Save(&merged); err != nil {
		return nil, cand, err
	}
	return &merged, cand, nil
}
