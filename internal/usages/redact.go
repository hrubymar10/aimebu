package usages

// ForAdmin is the snapshot as an admin who is not the owner may read it: how much of each provider's limits is used and when it resets, and nothing that
// names an account or says how it signs in. The owner's view has the e-mail address of each login, the host of a GitHub Enterprise sign-in, the
// error text a provider gave (which can quote a header), the credential alerts (which name profiles) and the token-refresh state of each tool; none
// of those is in this one. It never held a token, a cookie, an API key or a refresh-token hash. The list is of what is kept, not of what is dropped, so a field
// added to the snapshot later is not shown to an admin until it is added here.
func (r Response) ForAdmin() Response {
	out := Response{SwitcherEnabled: false}
	out.Settings.PercentDisplay = r.Settings.PercentDisplay
	out.Settings.RefreshIntervalSec = r.Settings.RefreshIntervalSec
	out.Providers = make([]Provider, 0, len(r.Providers))
	for _, provider := range r.Providers {
		shown := Provider{ProviderName: provider.ProviderName, Label: provider.Label, OrderNumber: provider.OrderNumber, Enabled: provider.Enabled, Available: provider.Available,
			Profiles: make([]Profile, 0, len(provider.Profiles))}
		for _, profile := range provider.Profiles {
			shown.Profiles = append(shown.Profiles, Profile{ProfileName: profile.ProfileName, Active: profile.Active, HasCredentials: profile.HasCredentials, Status: profile.Status, Plan: profile.Plan,
				Windows: profile.Windows, Credits: profile.Credits, LastRefreshAt: profile.LastRefreshAt, Stale: profile.Stale})
		}
		out.Providers = append(out.Providers, shown)
	}
	return out
}
