package switcher

import "github.com/hrubymar10/aimebu/internal/usages"

// UsageProfiles lists the profiles the usage reader should poll: every stored
// profile with its credential copy path, or nothing while the switcher is
// disabled. Read-only, like List.
func (m *Manager) UsageProfiles() ([]usages.StoredProfile, error) {
	enabled, err := m.Enabled()
	if err != nil || !enabled {
		return nil, err
	}
	profiles, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make([]usages.StoredProfile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, usages.StoredProfile{
			Tool: p.Tool, Name: p.Name, Active: p.Active,
			CredPath: m.StoredCredPath(p.Tool, p.Name), HasCredentials: p.HasCreds,
			Email: p.Email, ExpiresAt: p.ExpiresAt, SwitchedAwayAt: p.SwitchedAwayAt,
		})
	}
	return out, nil
}
