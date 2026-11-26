package supervisor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestWarmOrphanSessionEndProbe(t *testing.T) {
	for _, tc := range []struct {
		name, answer, state string
		keep                bool
	}{
		{"ended", "ended", "running", false},
		{"alive", "busy", "running", true},
		{"probe error", "error", "running", true},
		{"probe error with stopped container", "error", "exited", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			journal, err := OpenJournal(ctx, filepath.Join(root, "journal.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			launcher := filepath.Join(root, "launcher")
			body := fmt.Sprintf("#!/bin/sh\nif [ %q = error ]; then exit 1; fi\nprintf '%%s\\n' %q\n", tc.answer, tc.answer)
			if err := os.WriteFile(launcher, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			report := true
			process := WarmProcess{PID: 999999, AgentID: "agent", Started: "old", ExecToken: containerOne + ":session", LauncherID: "launcher", SessionEndReport: &report}
			if err := journal.RecordWarmProcess(ctx, process); err != nil {
				t.Fatal(err)
			}
			client := &Client{Journal: journal, harnessProbeDir: root, Launchers: []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "launcher"}, ResolvedCommand: launcher, Available: true}}, ContainerState: func(string) (string, error) { return tc.state, nil }}
			client.SweepWarmOrphans(ctx)
			left, err := journal.WarmProcesses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if (len(left) != 0) != tc.keep {
				t.Fatalf("remaining rows = %d, want keep %t", len(left), tc.keep)
			}
			if tc.name == "probe error" {
				if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf 'ended\\n'\n"), 0700); err != nil {
					t.Fatal(err)
				}
				client.SweepWarmOrphans(ctx)
				left, err = journal.WarmProcesses(ctx)
				if err != nil || len(left) != 0 {
					t.Fatalf("retry rows: %v, %v", left, err)
				}
			}
		})
	}
}

func TestWarmOrphanLegacyRowStillForgets(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: 999999, AgentID: "agent", Started: "old"}); err != nil {
		t.Fatal(err)
	}
	client := &Client{Journal: journal}
	client.SweepWarmOrphans(ctx)
	if left, err := journal.WarmProcesses(ctx); err != nil || len(left) != 0 {
		t.Fatalf("legacy rows: %v, %v", left, err)
	}
}

func TestWarmJournalRecordsReportOff(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	off := false
	if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: 17, AgentID: "agent", Started: "old", LauncherID: "launcher", SessionEndReport: &off}); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionEndReport == nil || *rows[0].SessionEndReport || rows[0].LauncherID != "launcher" {
		t.Fatalf("report-off row: %+v", rows)
	}
}

func TestWarmOrphanJournalMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE warm_process(pid INTEGER PRIMARY KEY,agent_id TEXT NOT NULL,started TEXT NOT NULL,recorded_at TEXT NOT NULL); INSERT INTO warm_process VALUES (17,'agent','old','old')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionEndReport != nil || rows[0].ExecToken != "" || rows[0].LauncherID != "" {
		t.Fatalf("migrated rows: %+v", rows)
	}
}
