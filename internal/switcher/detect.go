package switcher

import (
	"fmt"
	"os"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// Profile is the public view of one stored profile entry.
type Profile struct {
	Tool      string     `json:"tool"`
	Name      string     `json:"name"`
	Active    bool       `json:"active"`
	HasCreds  bool       `json:"has_credentials"`
	Email     string     `json:"email,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Eligibility reports whether a tool's live credentials are usable for switching.
// Absent distinguishes "no live file" (switching away captures nothing but is
// allowed) from "live file present but corrupt" (refused — capturing would
// destroy a good stored profile). Both report Eligible=false; Absent lets Switch
// tell them apart.
type Eligibility struct {
	Tool     string `json:"tool"`
	Eligible bool   `json:"eligible"`
	Absent   bool   `json:"absent,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Eligibility returns eligibility for all supported tools.
func (m *Manager) Eligibility() ([]Eligibility, error) {
	result := make([]Eligibility, 0, len(Tools()))
	for _, tool := range Tools() {
		el, err := m.eligibilityFor(tool)
		if err != nil {
			return nil, err
		}
		result = append(result, el)
	}
	return result, nil
}

// eligibilityFor checks whether tool's live credentials are usable for switching.
// Three cases from the spec:
//   - file exists, parses, expiry sane → eligible
//   - file absent → not eligible, "not logged in, or using the macOS Keychain"
//   - file present but invalid → not eligible, "credentials look corrupt"
func (m *Manager) eligibilityFor(tool string) (Eligibility, error) {
	path, err := m.liveCredPath(tool)
	if err != nil {
		return Eligibility{}, err
	}
	// Check for absence first. The usages validators convert os.ErrNotExist into
	// a non-wrapping error, so we must stat separately to distinguish absent from corrupt.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return Eligibility{Tool: tool, Eligible: false, Absent: true, Reason: fmt.Sprintf("no live credentials file at %s; switching away captures nothing (not logged in, or using the macOS Keychain)", path)}, nil
	}

	var valErr error
	switch tool {
	case ToolClaude:
		_, valErr = usages.ValidateClaudeCredentials(path)
	case ToolCodex:
		_, valErr = usages.ValidateCodexCredentials(path)
	default:
		return Eligibility{Tool: tool, Eligible: false, Reason: "unknown tool"}, nil
	}
	if valErr == nil {
		return Eligibility{Tool: tool, Eligible: true}, nil
	}
	return Eligibility{Tool: tool, Eligible: false, Reason: "credentials look corrupt"}, nil
}
