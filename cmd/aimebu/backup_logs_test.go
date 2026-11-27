package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
)

func TestBackupLeavesRotatedLogsOut(t *testing.T) {
	root, archive := backupFixture(t)
	logs := []string{
		"server/aimebu.log", "server/aimebu.log.1", "server/aimebu.log.2",
		"supervisor/agent-logs/abc/stderr.log", "supervisor/agent-logs/abc/stderr.log.1",
		"supervisor/agent-logs/abc/conversation.log.3",
	}
	kept := []string{"config/audit.log.txt", "config/notes.log.old", "config/x.logo", "config/dir.log.1x"}
	for _, p := range append(append([]string{}, logs...), kept...) {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errOut := runBackup(t, "--out", archive); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	paths := archivePaths(t, archive)
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, p := range logs {
		if have[p] {
			t.Fatalf("%s archived", p)
		}
	}
	for _, p := range kept {
		if !have[p] {
			t.Fatalf("%s should still be archived", p)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "supervisor")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := restoreArchive(context.Background(), root, archive, true, &out); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "server", "aimebu.sqlite")); err != nil {
		t.Fatal(err)
	}
}

// The conversation log is data (the agent output view reads it), so the current file of each agent is archived, and restored where the
// reader looks for it. Nothing else named like a log is.
func TestBackupKeepsAgentConversationLog(t *testing.T) {
	root, archive := backupFixture(t)
	dir := filepath.Join("supervisor", "agent-logs", agentlog.DirName("agt_one"))
	conversation := []byte("record one\nrecord two\n")
	files := map[string][]byte{
		filepath.Join(dir, agentlog.ConversationFileName):      conversation,
		filepath.Join(dir, "stderr.log"):                       []byte("err"),
		filepath.Join(dir, "stderr.log.1"):                     []byte("err1"),
		filepath.Join(dir, agentlog.ConversationFileName+".1"): []byte("old"),
		// A conversation.log anywhere else stays excluded by the .log rule.
		filepath.Join("config", agentlog.ConversationFileName):                   []byte("x"),
		filepath.Join("supervisor", agentlog.ConversationFileName):               []byte("x"),
		filepath.Join("supervisor", "agent-logs", agentlog.ConversationFileName): []byte("x"),
		filepath.Join(dir, "nested", agentlog.ConversationFileName):              []byte("x"),
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errOut := runBackup(t, "--out", archive); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	have := map[string]bool{}
	for _, p := range archivePaths(t, archive) {
		have[p] = true
	}
	kept := filepath.ToSlash(filepath.Join(dir, agentlog.ConversationFileName))
	for p := range files {
		if want := filepath.ToSlash(p) == kept; have[filepath.ToSlash(p)] != want {
			t.Fatalf("%s archived=%v, want %v", p, have[filepath.ToSlash(p)], want)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "supervisor")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	if err := restoreArchive(context.Background(), root, archive, true, io.Discard); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, dir, agentlog.ConversationFileName))
	if err != nil || !bytes.Equal(got, conversation) {
		t.Fatalf("restored conversation = %q, %v", got, err)
	}
	for _, p := range []string{"stderr.log", "stderr.log.1", agentlog.ConversationFileName + ".1"} {
		if _, err := os.Stat(filepath.Join(root, dir, p)); err == nil {
			t.Fatalf("%s restored", p)
		}
	}
	// What the agent output view runs on the restored file.
	page, err := agentlog.New(filepath.Join(root, "supervisor", "agent-logs")).TailStream("agt_one", agentlog.StreamConversation, 0, 0)
	if err != nil || !strings.Contains(page.Text, "record two") {
		t.Fatalf("restored log not readable by the agent output reader: %+v %v", page, err)
	}
}
