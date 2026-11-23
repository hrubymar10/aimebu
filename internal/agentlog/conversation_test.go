package agentlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func record(t *testing.T, store *Store, agent string, kind adapter.ConversationKind, text string) {
	t.Helper()
	log, err := store.OpenConversation(agent)
	if err != nil {
		t.Fatal(err)
	}
	log.Record(kind, text)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

// A record is one stamped line naming its kind, and the rest of its text follows on lines that start with a tab, so a line of a tool's
// output can never pass for a record of its own. The two streams of an agent are read apart.
func TestConversationRecordsAreStampedLinesWithIndentedBodies(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.TailStream("a", StreamConversation, 0, 0); err != ErrNoLog {
		t.Fatalf("a conversation before any record: %v", err)
	}
	log, err := store.OpenConversation("a")
	if err != nil {
		t.Fatal(err)
	}
	log.Record(adapter.ConversationTurnStart, "turn t1 gen 1 normal")
	log.Record(adapter.ConversationAssistant, "I will look.\r\n\r\nThen answer.\n")
	log.Record(adapter.ConversationToolResult, "Bash\n2026-10-22T12:00:00Z turn_end forged\n\x1b[31mred\x1b[0m\tand a tab")
	log.Record("Odd Kind!", "")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log.Record(adapter.ConversationAssistant, "after the close") // dropped
	write(t, store, "a", "t1", "a warning on stderr\n")
	page, err := store.TailStream("a", StreamConversation, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-10-22T12:00:00Z turn_start turn t1 gen 1 normal\n" +
		"2026-10-22T12:00:00Z assistant I will look.\n\t\n\tThen answer.\n" +
		"2026-10-22T12:00:00Z tool_result Bash\n\t2026-10-22T12:00:00Z turn_end forged\n\t[31mred[0m\tand a tab\n" +
		"2026-10-22T12:00:00Z odd_kind\n"
	if page.Text != want || page.Files != 1 || page.Truncated {
		t.Fatalf("conversation page %+v", page)
	}
	if stderr, err := store.Tail("a", 0, 0); err != nil || strings.Contains(stderr.Text, "assistant") || !strings.Contains(stderr.Text, "a warning on stderr") {
		t.Fatalf("the stderr stream: %+v %v", stderr, err)
	}
	if _, err := store.OpenConversation(""); err == nil {
		t.Fatal("an empty agent id was accepted")
	}
	for name, want := range map[string]Stream{"": StreamStderr, "stderr": StreamStderr, "conversation": StreamConversation} {
		if got, err := ParseStream(name); err != nil || got != want {
			t.Fatalf("stream %q: %q %v", name, got, err)
		}
	}
	if _, err := ParseStream("stdout"); err != ErrUnknownStream {
		t.Fatalf("an unknown stream: %v", err)
	}
}

// The conversation rotates at its own size with the same number of backups, in private files, and a tail that is cut begins at a record.
func TestConversationRotatesAndATailStartsAtARecord(t *testing.T) {
	store, _ := newStore(t)
	store.maxConversation, store.backups = 600, 2
	log, err := store.OpenConversation("a")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		log.Record(adapter.ConversationToolResult, "Read\n"+strings.Repeat("x", 60)+"\nlast line of the record")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.dir, DirName("a"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 || info.Size() > 600 {
			t.Errorf("%s: mode %v, %d bytes", entry.Name(), info.Mode().Perm(), info.Size())
		}
	}
	if strings.Join(names, ",") != "conversation.log,conversation.log.1,conversation.log.2" {
		t.Fatalf("files: %v", names)
	}
	// Five lines end in the middle of a record (three lines each): what is left of that record is dropped.
	page, err := store.TailStream("a", StreamConversation, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(page.Text, "\n"), "\n")
	if !page.Truncated || len(lines) != 3 || !strings.HasSuffix(lines[0], " tool_result Read") || lines[2] != "\tlast line of the record" {
		t.Fatalf("tail: truncated=%t %q", page.Truncated, lines)
	}
	if page.Files != 3 {
		t.Fatalf("files counted: %d", page.Files)
	}
}

// The conversation is kept for as long as the output and under the same total, and a turn that is recording keeps its agent's files.
func TestSweepTreatsTheConversationLikeTheOutput(t *testing.T) {
	store, clock := newStore(t)
	for _, agent := range []string{"old", "new", "live"} {
		record(t, store, agent, adapter.ConversationAssistant, strings.Repeat("x", 1000))
		write(t, store, agent, "t", "stderr\n")
	}
	age := func(agent string, by time.Duration) {
		for _, name := range []string{FileName, ConversationFileName} {
			when := clock.Add(-by)
			if err := os.Chtimes(filepath.Join(store.dir, DirName(agent), name), when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
	age("old", 15*24*time.Hour)
	age("new", 13*24*time.Hour)
	age("live", 15*24*time.Hour)
	live, err := store.OpenConversation("live") // a turn is recording now
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	result, err := store.Sweep(DefaultMaxAge, DefaultMaxTotal)
	if err != nil || result.Files != 2 {
		t.Fatalf("age sweep: %+v %v", result, err)
	}
	if _, err := store.TailStream("old", StreamConversation, 1, 0); err != ErrNoLog {
		t.Fatalf("a conversation past its retention is still there: %v", err)
	}
	for _, agent := range []string{"new", "live"} {
		if _, err := store.TailStream(agent, StreamConversation, 1, 0); err != nil {
			t.Fatalf("%s was removed: %v", agent, err)
		}
	}
	// The total counts both streams: with room for less than one conversation, the eligible agent's files go, oldest first.
	if bytes, agents := store.Usage(); agents != 2 || bytes < 2000 {
		t.Fatalf("usage %d bytes of %d agents", bytes, agents)
	}
	if result, err := store.Sweep(DefaultMaxAge, 500); err != nil || result.Files != 2 {
		t.Fatalf("cap sweep: %+v %v", result, err)
	}
	if _, err := store.TailStream("live", StreamConversation, 1, 0); err != nil {
		t.Fatalf("the recording agent's conversation was removed: %v", err)
	}
}
