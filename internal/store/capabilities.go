package store

import (
	"context"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// RecordMachineCapabilities replaces startup discovery after mapped-folder
// launcher probes resolve versions that were unknown in the first hello.
func (s *Store) RecordMachineCapabilities(ctx context.Context, machineID string, reports []protocol.CapabilityReport) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	data, err := json.Marshal(reports)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE machine SET capabilities_json=? WHERE id=?`, string(data), machineID)
	return err
}

// TurnPolicy is the capability snapshot at dispatch. A missing or ambiguous
// supervisor report leaves every capability false.
type TurnPolicy struct {
	Tool                           string               `json:"tool,omitempty"`
	Version                        string               `json:"version,omitempty"`
	Environment                    adapter.Environment  `json:"environment"`
	Capabilities                   adapter.Capabilities `json:"capabilities"`
	AutoResumeAfterCleanDiagnostic bool                 `json:"auto_resume_after_clean_diagnostic"`
	AutoResumeActor                string               `json:"auto_resume_actor,omitempty"`
}

type machineCapability struct {
	Tool        string               `json:"tool"`
	Version     string               `json:"version"`
	Environment adapter.Environment  `json:"environment"`
	Flags       adapter.Capabilities `json:"flags"`
}

func (tx *Tx) snapshotTurnPolicy(projectID, machineID, tool, variant string) (string, error) {
	var raw string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT capabilities_json FROM machine WHERE id=?`, machineID).Scan(&raw); err != nil {
		return "", err
	}
	var reports []machineCapability
	if err := json.Unmarshal([]byte(raw), &reports); err != nil {
		return "", err
	}
	policy := TurnPolicy{Tool: tool, Environment: adapter.Environment{Variant: variant}}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT auto_resume_after_clean_diagnostic FROM settings_project WHERE project_id=?`, projectID).
		Scan(&policy.AutoResumeAfterCleanDiagnostic); err != nil {
		return "", err
	}
	matches := 0
	for _, report := range reports {
		if report.Tool == tool && report.Environment.Variant == variant && report.Version != "" {
			matches++
			policy.Version = report.Version
			policy.Environment = report.Environment
			policy.Capabilities = report.Flags
		}
	}
	if matches != 1 {
		policy.Version = ""
		policy.Environment = adapter.Environment{Variant: variant}
		policy.Capabilities = adapter.Capabilities{}
	}
	data, err := json.Marshal(policy)
	return string(data), err
}
