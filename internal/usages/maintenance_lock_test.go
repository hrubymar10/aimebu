package usages

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func maintenanceFixture(t *testing.T, root, name, token string) MaintenanceProfile {
	t.Helper()
	path := filepath.Join(root, name+".json")
	data := []byte(`{"claudeAiOauth":{"accessToken":"test","refreshToken":"` + token + `"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return MaintenanceProfile{Name: name, CredPath: path}
}

func TestHoldMaintenanceProfilesReleasesFirstLockAndTracksRotatedSecond(t *testing.T) {
	root := t.TempDir()
	profiles := []MaintenanceProfile{
		maintenanceFixture(t, root, "a", "token-a"),
		maintenanceFixture(t, root, "b", "token-b"),
	}
	sort.Slice(profiles, func(i, j int) bool {
		return maintenanceIdentity("claude", profiles[i].Name, profiles[i].CredPath) < maintenanceIdentity("claude", profiles[j].Name, profiles[j].CredPath)
	})
	first := maintenanceIdentity("claude", profiles[0].Name, profiles[0].CredPath)
	second := maintenanceIdentity("claude", profiles[1].Name, profiles[1].CredPath)
	held := map[string]bool{}
	firstReleased := 0
	busyOnce := true
	try := func(_ string, id string) (func(), error) {
		if id == second && busyOnce {
			busyOnce = false
			if err := os.WriteFile(profiles[1].CredPath, []byte(`{"claudeAiOauth":{"accessToken":"test","refreshToken":"rotated"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		if held[id] {
			t.Fatalf("lock %s acquired twice", id)
		}
		held[id] = true
		return func() {
			delete(held, id)
			if id == first {
				firstReleased++
			}
		}, nil
	}
	release, err := holdMaintenanceProfiles(root, "claude", profiles, time.Second, try)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rotated := maintenanceIdentity("claude", profiles[1].Name, profiles[1].CredPath)
	if busyOnce || firstReleased == 0 || !held[rotated] || held[second] {
		t.Fatalf("retry did not release/reacquire rotated accounts: busy=%v released=%d held=%v", busyOnce, firstReleased, held)
	}
}

func TestHoldMaintenanceProfilesDeduplicatesOneAccount(t *testing.T) {
	root := t.TempDir()
	profiles := []MaintenanceProfile{
		maintenanceFixture(t, root, "a", "shared"),
		maintenanceFixture(t, root, "b", "shared"),
	}
	release, err := HoldMaintenanceProfiles(root, "claude", profiles, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if other, ok, err := AcquireMaintenance(root, "claude", profiles[1].Name, profiles[1].CredPath); err != nil || ok {
		if other != nil {
			other()
		}
		t.Fatalf("shared account was not held: ok=%v err=%v", ok, err)
	}
}

func TestHoldMaintenanceProfilesFailsClosedOnLockError(t *testing.T) {
	root := t.TempDir()
	profile := maintenanceFixture(t, root, "a", "token-a")
	if err := os.WriteFile(filepath.Join(root, maintenanceDirName), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if release, err := HoldMaintenanceProfiles(root, "claude", []MaintenanceProfile{profile}, time.Second); err == nil || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("lock directory error was not refused: release=%v err=%v", release != nil, err)
	}
	profiles := []MaintenanceProfile{profile, maintenanceFixture(t, root, "b", "token-b")}
	sort.Slice(profiles, func(i, j int) bool {
		return maintenanceIdentity("claude", profiles[i].Name, profiles[i].CredPath) < maintenanceIdentity("claude", profiles[j].Name, profiles[j].CredPath)
	})
	first := maintenanceIdentity("claude", profiles[0].Name, profiles[0].CredPath)
	second := maintenanceIdentity("claude", profiles[1].Name, profiles[1].CredPath)
	released := false
	try := func(_ string, id string) (func(), error) {
		if id == second {
			return nil, errors.New("lock failed")
		}
		if id != first {
			t.Fatalf("unexpected identity %s", id)
		}
		return func() { released = true }, nil
	}
	if release, err := holdMaintenanceProfiles(root, "claude", profiles, time.Second, try); err == nil || release != nil || !released {
		if release != nil {
			release()
		}
		t.Fatalf("second-lock error did not release first: released=%v err=%v", released, err)
	}
}
