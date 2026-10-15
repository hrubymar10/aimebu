package usages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedProfileTombstonesAreBoundedAndHashed(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxRemovedProfiles+2; i++ {
		credentials := []byte(fmt.Sprintf(`{"tokens":{"refresh_token":"secret-%d"}}`, i))
		if err := RecordRemovedProfile(root, "codex", fmt.Sprintf("profile-%d", i), credentials); err != nil {
			t.Fatal(err)
		}
	}
	file, err := readRemovedProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != maxRemovedProfiles {
		t.Fatalf("tombstone count = %d", len(file.Entries))
	}
	if file.Entries[0].Name != "profile-2" {
		t.Fatalf("oldest retained = %q", file.Entries[0].Name)
	}
	data, err := os.ReadFile(filepath.Join(root, "removed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-") {
		t.Fatal("removed.json contains a refresh token")
	}
	if removed, err := WasRefreshTokenRemoved(root, "codex", "secret-51"); err != nil || !removed {
		t.Fatalf("recent removal = %v, %v", removed, err)
	}
	if removed, err := WasRefreshTokenRemoved(root, "codex", "secret-0"); err != nil || removed {
		t.Fatalf("oldest removal = %v, %v", removed, err)
	}
}
