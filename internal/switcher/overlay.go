package switcher

import "github.com/hrubymar10/aimebu/internal/usages"

// OverlayUsages shows the configured account profiles on a usages response:
// each Claude Code and Codex provider lists every profile, the active one
// carrying the live snapshot and the others their read-only stored snapshot,
// or not_selected when the reader has none yet. It also sets
// SwitcherEnabled. Providers with no configured profile keep their single
// local entry. Read-only, like List.
func (m *Manager) OverlayUsages(response *usages.Response) error {
	profiles, err := m.List()
	if err != nil {
		return err
	}
	enabled, err := m.Enabled()
	if err != nil {
		return err
	}
	response.SwitcherEnabled = enabled
	for i := range response.Providers {
		tool := response.Providers[i].ProviderName
		if tool == "claude-code" {
			tool = "claude"
		}
		if tool != "claude" && tool != "codex" {
			continue
		}
		// The expiry badge and the expiry alert apply where the token-copy
		// model applies: there the server refreshes the stored logins and
		// their short-lived tokens are its business. On the link model the
		// CLI renews its own login.
		migrated := m.TokenCopiesFor(tool)
		base := response.Providers[i].Profiles[0]
		configured := []usages.Profile{}
		for _, profile := range profiles {
			if profile.Tool != tool {
				continue
			}
			item := usages.Profile{ProfileName: profile.Name, AlertIdentity: profile.AlertIdentity, Active: profile.Active, HasCredentials: profile.HasCreds, Email: profile.Email, ExpiresAt: profile.ExpiresAt, Status: "not_selected"}
			if !profile.Active {
				// A read-only snapshot of the stored login, when the reader has one.
				for _, stored := range response.Providers[i].Profiles[1:] {
					if stored.ProfileName == profile.Name {
						item = stored
						item.Active, item.HasCredentials, item.Email, item.ExpiresAt, item.AlertIdentity = false, profile.HasCreds, profile.Email, profile.ExpiresAt, profile.AlertIdentity
					}
				}
			}
			if profile.Active {
				item = base
				item.ProfileName, item.Active, item.HasCredentials, item.Email, item.ExpiresAt, item.AlertIdentity = profile.Name, true, profile.HasCreds, profile.Email, profile.ExpiresAt, profile.AlertIdentity
			}
			item.ToolMigrated = migrated
			configured = append(configured, item)
		}
		if len(configured) > 0 {
			response.Providers[i].Profiles = configured
		}
	}
	return nil
}
