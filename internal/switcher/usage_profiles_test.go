package switcher

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages"
)

func TestUsageProfilesListsStoredCopiesOnlyWhenEnabled(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.SetEnabled(true))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "work"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "work"), []byte(futureClaude("tok-w")), 0o600))

	got, err := m.UsageProfiles()
	must(t, err)
	byName := map[string]usages.StoredProfile{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if len(byName) != 2 || !byName["main"].Active || byName["work"].Active || !byName["work"].HasCredentials || byName["work"].CredPath != m.profileCredPath(ToolClaude, "work") {
		t.Fatalf("profiles = %+v", got)
	}

	must(t, m.SetEnabled(false))
	got, err = m.UsageProfiles()
	if err != nil || len(got) != 0 {
		t.Fatalf("disabled switcher must list nothing, got %+v %v", got, err)
	}
}

func TestOverlayShowsStoredSnapshotsForInactiveProfiles(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.SetEnabled(true))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "work"))
	must(t, m.Add(ToolClaude, "spare"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "work"), []byte(futureClaude("tok-w")), 0o600))

	response := usages.Response{Providers: []usages.Provider{{
		ProviderName: "claude-code",
		Profiles: []usages.Profile{
			{ProfileName: "local", Active: true, Status: "ok", Windows: []usages.Window{{Key: "session", PercentUsed: 10}}},
			{ProfileName: "work", Status: "ok", Windows: []usages.Window{{Key: "session", PercentUsed: 70}}},
		},
	}}}
	must(t, m.OverlayUsages(&response))
	got := map[string]usages.Profile{}
	for _, p := range response.Providers[0].Profiles {
		got[p.ProfileName] = p
	}
	if len(got) != 3 {
		t.Fatalf("profiles = %+v", response.Providers[0].Profiles)
	}
	if main := got["main"]; !main.Active || len(main.Windows) != 1 || main.Windows[0].PercentUsed != 10 {
		t.Fatalf("active = %+v", main)
	}
	if work := got["work"]; work.Active || work.Status != "ok" || len(work.Windows) != 1 || work.Windows[0].PercentUsed != 70 || !work.HasCredentials {
		t.Fatalf("stored snapshot lost: %+v", work)
	}
	if spare := got["spare"]; spare.Status != "not_selected" || len(spare.Windows) != 0 {
		t.Fatalf("profile without a snapshot = %+v", spare)
	}
}
