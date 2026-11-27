package supervisor

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestWarmOrphanSessionEndProbe(t *testing.T) {
	for _, tc := range []struct {
		name, answer, state string
		keep                bool
		stateError          bool
	}{
		{"ended", "ended", "running", false, false},
		{"alive", "busy", "running", true, false},
		{"missing container", "busy", "", true, false},
		{"Docker unavailable", "busy", "", true, true},
		{"probe error", "error", "running", true, false},
		{"probe error with missing container", "error", "", true, false},
		{"probe error with stopped container", "error", "exited", true, false},
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
			if err := testenv.WriteExecutable(launcher, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			report := true
			process := WarmProcess{PID: 999999, AgentID: "agent", Started: "old", ExecToken: containerOne + ":session", LauncherID: "launcher", SessionEndReport: &report}
			if err := journal.RecordWarmProcess(ctx, process); err != nil {
				t.Fatal(err)
			}
			client := &Client{Journal: journal, harnessProbeDir: root, Launchers: []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "launcher"}, ResolvedCommand: launcher, Available: true}}, ContainerState: func(string) (string, error) {
				if tc.stateError {
					return "", fmt.Errorf("Docker unavailable")
				}
				return tc.state, nil
			}}
			client.SweepWarmOrphans(ctx)
			left, err := journal.WarmProcesses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if (len(left) != 0) != tc.keep {
				t.Fatalf("remaining rows = %d, want keep %t", len(left), tc.keep)
			}
			if tc.name == "probe error" {
				if err := testenv.WriteExecutable(launcher, []byte("#!/bin/sh\nprintf 'ended\\n'\n"), 0700); err != nil {
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

func TestWarmOrphanMissingContainerNeedsPriorSight(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	report := true
	for _, pid := range []int{999998, 999999} {
		if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: pid, AgentID: "agent", Started: fmt.Sprint(pid), ExecToken: containerOne + ":session", LauncherID: "remote", SessionEndReport: &report}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := journal.db.ExecContext(ctx, `UPDATE warm_process SET container_seen=1,docker_daemon_id='daemon-a' WHERE pid=999999`); err != nil {
		t.Fatal(err)
	}
	client := &Client{Journal: journal, ContainerState: func(string) (string, error) { return "", nil }, DockerDaemonID: func() (string, error) { return "daemon-a", nil }}
	client.SweepWarmOrphans(ctx)
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PID != 999998 {
		t.Fatalf("remaining rows: %+v", rows)
	}
}

func TestWarmOrphanMissingContainerOnDifferentDaemonStays(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	report := true
	process := WarmProcess{PID: 999999, AgentID: "agent", Started: "old", ExecToken: containerOne + ":session", SessionEndReport: &report}
	if err := journal.RecordWarmProcess(ctx, process); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordWarmContainerSight(ctx, process.PID, process.Started, "daemon-a"); err != nil {
		t.Fatal(err)
	}
	client := &Client{Journal: journal, ContainerState: func(string) (string, error) { return "", nil }, DockerDaemonID: func() (string, error) { return "daemon-b", nil }}
	client.SweepWarmOrphans(ctx)
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("remaining rows: %+v", rows)
	}
}

func TestWarmOrphanPIDReuseKeepsBothIdentities(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	report := true
	old := WarmProcess{PID: cmd.Process.Pid, AgentID: "old", Started: "old-start", ExecToken: containerOne + ":old", SessionEndReport: &report}
	if err := journal.RecordWarmProcess(ctx, old); err != nil {
		t.Fatal(err)
	}
	client := &Client{Journal: journal, ContainerState: func(string) (string, error) { return "running", nil }}
	client.SweepWarmOrphans(ctx)
	fresh := WarmProcess{PID: old.PID, AgentID: "new", Started: processStart(cmd.Process.Pid)}
	if fresh.Started == "" {
		t.Fatal("missing live process start time")
	}
	if err := journal.RecordWarmProcess(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	client.SweepWarmOrphans(ctx)
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("sweep killed the reused PID: %v", err)
	}
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Started == rows[1].Started {
		t.Fatalf("PID reuse rows: %+v", rows)
	}
	if err := journal.ForgetWarmProcess(ctx, old.PID, old.Started); err != nil {
		t.Fatal(err)
	}
	rows, err = journal.WarmProcesses(ctx)
	if err != nil || len(rows) != 1 || rows[0].Started != fresh.Started {
		t.Fatalf("live row after orphan release: %+v, %v", rows, err)
	}
}

func TestWarmOrphanTokenlessReportReleasesAndLogsOnce(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	report := true
	if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: 999999, AgentID: "agent", Started: "old", SessionEndReport: &report}); err != nil {
		t.Fatal(err)
	}
	var logs []string
	client := &Client{Journal: journal, warmOrphanLog: func(s string) { logs = append(logs, s) }}
	client.SweepWarmOrphans(ctx)
	client.SweepWarmOrphans(ctx)
	rows, err := journal.WarmProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(logs) != 1 || !strings.Contains(logs[0], "no exec token") {
		t.Fatalf("rows=%+v logs=%v", rows, logs)
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
	if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: 17, AgentID: "new", Started: "new"}); err != nil {
		t.Fatalf("migration kept pid-only key: %v", err)
	}
}

func TestWarmOrphanOverdueLogsOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	journal, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	report := true
	if err := journal.RecordWarmProcess(ctx, WarmProcess{PID: 999999, AgentID: "agent", Started: "old", ExecToken: containerOne + ":session", SessionEndReport: &report}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordWarmContainerSight(ctx, 999999, "old", "daemon-a"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	client := &Client{Journal: journal, ContainerState: func(string) (string, error) { return "running", nil }, DockerDaemonID: func() (string, error) { return "daemon-a", nil }, warmOrphanLog: func(message string) { logs = append(logs, message) }}
	client.SweepWarmOrphans(ctx)
	if len(logs) != 0 {
		t.Fatalf("early logs: %v", logs)
	}
	_, err = journal.db.ExecContext(ctx, `UPDATE warm_process SET orphan_since=?`, time.Now().Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	client.SweepWarmOrphans(ctx)
	client.SweepWarmOrphans(ctx)
	if len(logs) != 1 || !strings.Contains(logs[0], "agent agent: warm orphan awaiting proof for 2h") {
		t.Fatalf("logs: %v", logs)
	}
	restarted := &Client{Journal: journal, ContainerState: client.ContainerState, DockerDaemonID: client.DockerDaemonID, warmOrphanLog: client.warmOrphanLog}
	restarted.SweepWarmOrphans(ctx)
	if len(logs) != 1 {
		t.Fatalf("warning repeated after restart: %v", logs)
	}
	alerts, err := journal.WarmOrphansAwaitingProof(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].AgentID != "agent" || alerts[0].Age < 2*time.Hour {
		t.Fatalf("alerts: %+v", alerts)
	}
	status, err := ReadWarmOrphanStatus(path)
	if err != nil || len(status.WarmOrphans) != 1 || status.WarmOrphans[0].AgentID != "agent" {
		t.Fatalf("published orphans: %+v, %v", status, err)
	}
	client.ContainerState = func(string) (string, error) { return "", nil }
	client.SweepWarmOrphans(ctx)
	status, err = ReadWarmOrphanStatus(path)
	if err != nil || len(status.WarmOrphans) != 0 {
		t.Fatalf("cleared orphans: %+v, %v", status, err)
	}
}
