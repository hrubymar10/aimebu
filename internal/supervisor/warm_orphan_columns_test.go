package supervisor

import (
	"context"
	"path/filepath"
	"testing"
)

// This uses only the pre-migration Journal API so it can run on the base tag.
func TestWarmOrphanJournalColumns(t *testing.T) {
	journal, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	rows, err := journal.db.Query(`PRAGMA table_info(warm_process)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := make(map[string]bool)
	for rows.Next() {
		var index, required, primary int
		var name, kind string
		var fallback any
		if err := rows.Scan(&index, &name, &kind, &required, &fallback, &primary); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"exec_token", "launcher_id", "session_end_report", "orphan_since", "orphan_warned", "container_seen", "docker_daemon_id"} {
		if !found[name] {
			t.Errorf("missing warm journal column %s", name)
		}
	}
}
