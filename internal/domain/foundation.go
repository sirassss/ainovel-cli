package domain

// FoundationAuditIssue is a cross-file consistency issue the Architect raises about the foundation settings already persisted.
type FoundationAuditIssue struct {
	Artifact    string `json:"artifact"`
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
	Suggestion  string `json:"suggestion,omitempty"`
}

// FoundationAudit records one model review of the foundation settings for a specific version.
type FoundationAudit struct {
	Fingerprint string                 `json:"fingerprint"`
	Ready       bool                   `json:"ready"`
	Summary     string                 `json:"summary"`
	Issues      []FoundationAuditIssue `json:"issues"`
}
