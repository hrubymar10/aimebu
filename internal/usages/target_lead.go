package usages

import "time"

// Refresh lead: a profile is due when it is expired or expires within the lead.
// Claude tokens live 8 h, codex ~10 days.
const (
	TargetLeadClaude = 60 * time.Minute
	TargetLeadCodex  = 24 * time.Hour
)
