package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCrashLog(t *testing.T, root, content string) {
	t.Helper()
	path := CrashLogPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func rotatedCrashLogs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, "crash-") && strings.HasSuffix(name, ".log") {
			names = append(names, name)
		}
	}
	return names
}

func TestInstallCrashOutputRotatesAndReports(t *testing.T) {
	_, transport := startTest(t)
	root := t.TempDir()
	home, _ := os.UserHomeDir()
	// A runtime report like the one a fatal error writes, with a path of the runner's home in a frame.
	writeCrashLog(t, root, "unexpected fault address 0x1068eea38\nfatal error: fault\n[signal SIGBUS ...]\n\ngoroutine 1 [running]:\nruntime.throw(...)\n\t"+home+"/go/pkg/mod/modernc.org/sqlite/lib/sqlite.go:456 +0x170\n")
	previous, err := InstallCrashOutput(root)
	if err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	if previous == "" {
		t.Fatal("a non-empty crash.log was not rotated")
	}
	if info, err := os.Stat(CrashLogPath(root)); err != nil || info.Size() != 0 {
		t.Fatalf("the fresh crash.log = (%v, %v), want an empty file", info, err)
	}
	if info, err := os.Stat(CrashLogPath(root)); err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("crash.log mode = %v, want 0600", info.Mode().Perm())
	}
	if len(rotatedCrashLogs(t, root)) != 1 {
		t.Fatal("the rotated crash file is missing")
	}
	if err := ReportCrashFile(context.Background(), previous); err != nil {
		t.Fatalf("ReportCrashFile: %v", err)
	}
	events := transport.sent()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	// The title is the report's first line, with its address normalised away.
	if event.Level != "fatal" || !strings.Contains(event.Message, "unexpected fault address") || strings.Contains(event.Message, "0x") {
		t.Fatalf("event = %s", marshal(t, event))
	}
	// The trace travels as an attachment, scrubbed of the runner's home.
	if len(event.Attachments) != 1 || event.Attachments[0].Filename != CrashLogName {
		t.Fatalf("attachments = %+v", event.Attachments)
	}
	payload := string(event.Attachments[0].Payload)
	if !strings.Contains(payload, "goroutine 1") {
		t.Fatal("the trace is not in the attachment")
	}
	if strings.Contains(payload, home) || strings.Contains(marshal(t, event), home) {
		t.Fatal("the runner's home directory is in the event")
	}
}

func TestInstallCrashOutputReportsNothingWhenOff(t *testing.T) {
	root := t.TempDir()
	writeCrashLog(t, root, "fatal error: fault\n\ngoroutine 1 [running]:\n")
	previous, err := InstallCrashOutput(root)
	if err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	if previous == "" {
		t.Fatal("a non-empty crash.log was not rotated")
	}
	// With reporting off (no Init) the report is skipped, and the rotated file keeps the trace on disk.
	if err := ReportCrashFile(context.Background(), previous); err != nil {
		t.Fatalf("ReportCrashFile: %v", err)
	}
	logs := rotatedCrashLogs(t, root)
	if len(logs) != 1 {
		t.Fatalf("rotated crash files = %v, want the one kept", logs)
	}
	content, err := os.ReadFile(filepath.Join(root, "server", logs[0]))
	if err != nil || !strings.Contains(string(content), "fatal error: fault") {
		t.Fatalf("the rotated file lost the trace: %q %v", content, err)
	}
}

func TestInstallCrashOutputEmptyFileIsNotRotated(t *testing.T) {
	root := t.TempDir()
	writeCrashLog(t, root, "")
	previous, err := InstallCrashOutput(root)
	if err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	if previous != "" {
		t.Fatalf("previous = %q, want empty", previous)
	}
	if len(rotatedCrashLogs(t, root)) != 0 {
		t.Fatal("an empty crash.log was rotated")
	}
}

func TestInstallCrashOutputMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-there")
	if _, err := InstallCrashOutput(root); err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	if info, err := os.Stat(CrashLogPath(root)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("crash.log = (%v, %v), want a 0600 file", info, err)
	}
}

func TestCrashLogRotationKeepsFive(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"crash-20260101-000001.log", "crash-20260102-000001.log", "crash-20260103-000001.log", "crash-20260104-000001.log", "crash-20260105-000001.log", "crash-20260106-000001.log", "other.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCrashLog(t, root, "fatal error: fault\n")
	if _, err := InstallCrashOutput(root); err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	logs := rotatedCrashLogs(t, root)
	if len(logs) != 5 {
		t.Fatalf("rotated crash files = %v, want 5", logs)
	}
	for _, gone := range []string{"crash-20260101-000001.log", "crash-20260102-000001.log"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the rotation", gone)
		}
	}
}

func TestReportCrashFileMissingAndEmpty(t *testing.T) {
	_, transport := startTest(t)
	if err := ReportCrashFile(context.Background(), filepath.Join(t.TempDir(), "gone.log")); err != nil {
		t.Fatalf("ReportCrashFile: %v", err)
	}
	root := t.TempDir()
	writeCrashLog(t, root, "")
	if err := ReportCrashFile(context.Background(), CrashLogPath(root)); err != nil {
		t.Fatalf("ReportCrashFile: %v", err)
	}
	if events := transport.sent(); len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}

func TestReportCrashFileTitleIsScrubbedAndGrouped(t *testing.T) {
	_, transport := startTest(t)
	root := t.TempDir()
	// A hex address and a home path in the first line: the title normalises both, so one crash kind is one issue.
	writeCrashLog(t, root, "unexpected fault address 0x1068eea38 at /Users/someone/go/pkg/mod/x.go\nfatal error: fault\n")
	previous, err := InstallCrashOutput(root)
	if err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	if err := ReportCrashFile(context.Background(), previous); err != nil {
		t.Fatalf("ReportCrashFile: %v", err)
	}
	events := transport.sent()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if strings.Contains(event.Message, "0x1068eea38") || strings.Contains(event.Message, "/Users/someone") {
		t.Fatalf("title = %q, want it scrubbed", event.Message)
	}
	if len(event.Fingerprint) == 0 || event.Fingerprint[0] == "" {
		t.Fatalf("fingerprint = %+v", event.Fingerprint)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "0x1068eea38") {
		t.Fatal("the fault address is in the event")
	}
}
