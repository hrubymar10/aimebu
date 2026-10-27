package ccsessions

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// Every test reads and writes only a fixture configuration folder under t.TempDir(); the real ~/.claude is never touched.

func jsonLine(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// transcript writes a session transcript the way Claude lays it out: the first lines are bookkeeping, the working folder is on the
// conversation lines, and titles are appended later. extra lines go at the end.
func transcript(t *testing.T, configDir, cwd, id string, extra ...string) string {
	t.Helper()
	return transcriptIn(t, configDir, FolderName(cwd), cwd, id, extra...)
}

// transcriptIn is transcript in a project directory of the given name (Claude's own for a long path is not FolderName's).
func transcriptIn(t *testing.T, configDir, dirName, cwd, id string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		jsonLine(t, map[string]any{"type": "permission-mode", "permissionMode": "default", "sessionId": id}),
		jsonLine(t, map[string]any{"type": "file-history-snapshot", "messageId": "m1", "snapshot": map[string]any{}}),
		jsonLine(t, map[string]any{"type": "user", "cwd": cwd, "sessionId": id, "gitBranch": "main", "version": "2.1.309", "entrypoint": "cli",
			"message": map[string]any{"role": "user", "content": "  Fix the\n\tlogin   bug  "}}),
		jsonLine(t, map[string]any{"type": "assistant", "cwd": cwd, "sessionId": id, "message": map[string]any{"role": "assistant", "content": "ok"}}),
	}
	lines = append(lines, extra...)
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func scanSessions(cfg string, folders ...string) ([]Session, error) {
	result, err := Scan(cfg, folders...)
	return result.Sessions, err
}

func age(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

const (
	idA = "11111111-1111-4111-8111-111111111111"
	idB = "22222222-2222-4222-8222-222222222222"
	idC = "33333333-3333-4333-8333-333333333333"
	idD = "44444444-4444-4444-8444-444444444444"
)

func TestScanListsTheSessionsOfAFolderAndBelowNewestFirst(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	age(t, transcript(t, cfg, "/work/proj", idA), 3*time.Hour)
	age(t, transcript(t, cfg, "/work/proj/sub", idB), time.Hour)
	// A sibling folder that shares the name's prefix is not part of the project, even though Claude's directory name starts the same.
	transcript(t, cfg, "/work/proj-other", idC)
	sessions, err := scanSessions(cfg, "/work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != idB || sessions[1].ID != idA {
		t.Fatalf("sessions=%+v", sessions)
	}
	first := sessions[1]
	if first.Dir != FolderName("/work/proj") {
		t.Fatalf("dir=%q", first.Dir)
	}
	if first.CWD != "/work/proj" || first.GitBranch != "main" || first.Version != "2.1.309" || first.Entrypoint != "cli" || first.Size == 0 {
		t.Fatalf("session facts: %+v", first)
	}
	// With no title Claude made, the first prompt is the title, on one line.
	if first.Title != "Fix the login bug" {
		t.Fatalf("title=%q", first.Title)
	}
}

func TestScanTitlePrefersTheTitleClaudeMadeThenTheLastPrompt(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	transcript(t, cfg, "/p", idA,
		jsonLine(t, map[string]any{"type": "last-prompt", "lastPrompt": "the last thing asked", "sessionId": idA}),
		jsonLine(t, map[string]any{"type": "ai-title", "aiTitle": "Refactor the API", "sessionId": idA}))
	transcript(t, cfg, "/p", idB, jsonLine(t, map[string]any{"type": "last-prompt", "lastPrompt": "only a last prompt", "sessionId": idB}))
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	titles := map[string]string{}
	for _, s := range sessions {
		titles[s.ID] = s.Title
	}
	if titles[idA] != "Refactor the API" || titles[idB] != "only a last prompt" {
		t.Fatalf("titles=%v", titles)
	}
}

func TestScanSkipsWhatCannotBeContinued(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "projects", FolderName("/p"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conversation := func(id string, sidechain bool) string {
		return jsonLine(t, map[string]any{"type": "user", "cwd": "/p", "sessionId": id, "isSidechain": sidechain, "message": map[string]any{"content": "hi"}})
	}
	write("agent-abcdef12.jsonl", conversation("abcdef12-0000", false))                                                                         // a subagent transcript
	write(idA+".jsonl", conversation(idA, true))                                                                                                // a sidechain
	write(idB+".jsonl", jsonLine(t, map[string]any{"type": "file-history-snapshot"}), jsonLine(t, map[string]any{"type": "mode", "mode": "x"})) // nothing said, no folder
	write(idC+".jsonl", jsonLine(t, map[string]any{"type": "file-history-snapshot"}))
	write("notes.txt", "not a transcript")
	write("short.jsonl", conversation("short", false)) // not an id
	write(idD+".jsonl", conversation(idD, false))      // the one good one
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != 1 || sessions[0].ID != idD {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
}

// A long transcript is read only at its two ends: the folder from the start, the title Claude appended from the end, however much lies between.
func TestScanReadsOnlyTheEndsOfALongTranscript(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	filler := jsonLine(t, map[string]any{"type": "assistant", "cwd": "/p", "message": map[string]any{"content": strings.Repeat("x", 4000)}})
	var middle []string
	for i := 0; i < 2000; i++ { // about 8 MB
		middle = append(middle, filler)
	}
	// One line in the middle is far longer than either window; it must not stop the scan.
	middle = append(middle, jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"content": strings.Repeat("y", 300<<10)}}))
	middle = append(middle, jsonLine(t, map[string]any{"type": "ai-title", "aiTitle": "Long session", "sessionId": idA}))
	path := transcript(t, cfg, "/p", idA, middle...)
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if sessions[0].Title != "Long session" || sessions[0].CWD != "/p" {
		t.Fatalf("session=%+v", sessions[0])
	}
	if info, _ := os.Stat(path); sessions[0].Size != info.Size() || info.Size() < 4<<20 {
		t.Fatalf("size=%d file=%d", sessions[0].Size, info.Size())
	}
}

func TestScanCutsAndCleansTitles(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	transcript(t, cfg, "/p", idA, jsonLine(t, map[string]any{"type": "ai-title", "aiTitle": "a\x00b\x07c " + strings.Repeat("é", 200), "sessionId": idA}))
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	title := sessions[0].Title
	if !strings.HasPrefix(title, "abc é") || len([]rune(title)) != TitleRunes+1 || !strings.HasSuffix(title, "…") {
		t.Fatalf("title=%q", title)
	}
}

func TestFindLooksAnIdUpInEveryProjectDirectory(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	transcript(t, cfg, "/elsewhere/deep", idA)
	found, err := Find(cfg, idA)
	if err != nil || found.CWD != "/elsewhere/deep" || found.ID != idA {
		t.Fatalf("found=%+v err=%v", found, err)
	}
	for _, id := range []string{"", idB, "../" + idA, "*", idA[:5], idA + "/x", strings.Repeat("a", 90)} {
		if _, err := Find(cfg, id); err != ErrNotFound {
			t.Errorf("Find(%q) = %v", id, err)
		}
	}
}

func TestSessionsAFolderRegistersAsActiveOnlyWhileTheirProcessLives(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	transcript(t, cfg, "/p", idA) // registered by this very process: alive
	transcript(t, cfg, "/p", idB) // registered by a process that is gone
	transcript(t, cfg, "/p", idC) // not registered
	registry := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(registry, 0o755); err != nil {
		t.Fatal(err)
	}
	register := func(name string, entry map[string]any) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(registry, name), []byte(jsonLine(t, entry)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	register("live.json", map[string]any{"pid": os.Getpid(), "sessionId": idA, "kind": "interactive", "status": "busy"})
	register("gone.json", map[string]any{"pid": 0x7ffffff0, "sessionId": idB, "kind": "interactive"})
	register("broken.json", map[string]any{"pid": "x"})
	if err := os.WriteFile(filepath.Join(registry, "garbage.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != 3 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	active := map[string]bool{}
	for _, s := range sessions {
		active[s.ID] = s.Active
	}
	if !active[idA] || active[idB] || active[idC] {
		t.Fatalf("active=%v", active)
	}
	if found, err := Find(cfg, idA); err != nil || !found.Active {
		t.Fatalf("find: %+v %v", found, err)
	}
}

func TestScanOfAFolderWithNoSessionsOrNoConfigIsEmpty(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	sessions, err := scanSessions(cfg, "/nothing/here")
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if _, err := scanSessions(cfg, "relative/path"); err == nil {
		t.Fatal("a relative folder was accepted")
	}
	if _, err := scanSessions("", "/p"); err == nil {
		t.Fatal("no configuration folder was accepted")
	}
}

func TestScanStopsAtTheResultLimit(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	for i := 0; i < MaxResults+20; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		age(t, transcript(t, cfg, "/p", id), time.Duration(i)*time.Minute)
	}
	sessions, err := scanSessions(cfg, "/p")
	if err != nil || len(sessions) != MaxResults {
		t.Fatalf("got %d sessions, err=%v", len(sessions), err)
	}
	if sessions[0].ID != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("newest first: %s", sessions[0].ID)
	}
}

func TestConfigDirFollowsTheEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	if got := ConfigDir(); got != "/custom/claude" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/someone")
	if got := ConfigDir(); got != "/home/someone/.claude" {
		t.Fatalf("got %q", got)
	}
}

// A folder reached through a link is recorded as the spelling Claude was started in; naming both spellings finds the session either way.
func TestScanTakesSeveralSpellingsOfOneFolder(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	transcript(t, cfg, "/tmp/link/proj", idA)
	transcript(t, cfg, "/private/tmp/real/proj", idB)
	sessions, err := scanSessions(cfg, "/private/tmp/real/proj", "/tmp/link/proj")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if only, err := scanSessions(cfg, "/tmp/link/proj"); err != nil || len(only) != 1 || only[0].ID != idA {
		t.Fatalf("one spelling: %+v %v", only, err)
	}
}

// Claude shortens a long directory name and ends it with a hash, so the name no longer starts like the folder's: the session is found by
// the working folder its transcript records.
func TestScanFindsSessionsInAHashShortenedDirectory(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	cwd := "/work/" + strings.Repeat("deeply-nested-", 20) + "proj"
	shortName := "-work-" + strings.Repeat("deeply-nested-", 12) + "-kx83jd2"
	transcriptIn(t, cfg, shortName, cwd, idA)
	// A long directory of another folder is opened and left out.
	transcriptIn(t, cfg, "-work-"+strings.Repeat("another-nested-", 12)+"-zz11aa", "/work/"+strings.Repeat("another-nested-", 20)+"x", idB)
	result, err := Scan(cfg, cwd)
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].ID != idA || result.Sessions[0].Dir != shortName {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// A session below the long folder is found in the same directory.
	if found, err := Find(cfg, idA); err != nil || found.CWD != cwd {
		t.Fatalf("find: %+v %v", found, err)
	}
}

// Sessions of other folders must not use up the limits meant for the project's: the folder decides first, the limits count what is left.
func TestScanFiltersByFolderBeforeCapping(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	age(t, transcript(t, cfg, "/p", idA), 48*time.Hour)
	for i := 0; i < MaxResults+50; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		transcript(t, cfg, "/p-sibling", id) // the directory name starts like the project's; the folder does not
	}
	result, err := Scan(cfg, "/p")
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].ID != idA || result.Truncated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// A first line longer than the head window is read with a larger one; one longer still is counted, not dropped silently.
func TestScanCountsTranscriptsWhoseFolderCannotBeRead(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "projects", FolderName("/p"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(id string, pad int) string {
		return jsonLine(t, map[string]any{"type": "file-history-snapshot", "snapshot": strings.Repeat("s", pad)}) + "\n" +
			jsonLine(t, map[string]any{"type": "user", "cwd": "/p", "sessionId": id, "message": map[string]any{"content": "hi"}}) + "\n"
	}
	write := func(id string, pad int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line(id, pad)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(idA, 200<<10) // past the head window, within the retry
	write(idB, 2<<20)   // past the retry too
	result, err := Scan(cfg, "/p")
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].ID != idA || result.Unreadable != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := Find(cfg, idB); err != ErrUnreadable {
		t.Fatalf("find of an unreadable transcript: %v", err)
	}
}

func TestConfigDirWithFollowsTheLauncherReferences(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/supervisors/own")
	t.Setenv("WORK_CLAUDE", "/work/claude")
	t.Setenv("WORK_HOME", "/work/home")
	if got := ConfigDirWith(map[string]string{"CLAUDE_CONFIG_DIR": "WORK_CLAUDE"}); got != "/work/claude" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := ConfigDirWith(map[string]string{"HOME": "WORK_HOME"}); got != "/work/home/.claude" {
		t.Fatalf("got %q", got)
	}
	// A reference to an unset variable is the launcher's choice of nothing: its Claude does not fall back to the supervisor's value.
	if got := ConfigDirWith(map[string]string{"CLAUDE_CONFIG_DIR": "NOT_SET_ANYWHERE_XYZ", "HOME": "WORK_HOME"}); got != "/work/home/.claude" {
		t.Fatalf("got %q", got)
	}
}

// A directory that may be the folder's but was not opened because the probes ran out is said, not hidden.
func TestScanSaysWhenLongDirectoriesWereNotAllLookedInto(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	for i := 0; i < MaxProbes+5; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		transcriptIn(t, cfg, fmt.Sprintf("-other-%s-%03d", strings.Repeat("long-", 30), i), "/other/"+strings.Repeat("long-", 30)+fmt.Sprint(i), id)
	}
	result, err := Scan(cfg, "/p")
	if err != nil || len(result.Sessions) != 0 || !result.Truncated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func toolUse(t *testing.T, id, name string) string {
	t.Helper()
	return jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": "claude-opus-4-5-20251101",
		"content": []map[string]any{{"type": "text", "text": "SECRET prose the scan must never return"}, {"type": "tool_use", "id": id, "name": name, "input": map[string]any{"body": "SECRET body"}}}}})
}

func toolResult(t *testing.T, id, text string, asParts bool) string {
	t.Helper()
	var content any = text
	if asParts {
		content = []map[string]any{{"type": "text", "text": text}}
	}
	return jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": content}}}})
}

// The attribution of a session reads the ids its aimebu tool results returned: who it registered as, which messages it sent. Whatever the
// transcript says besides is never part of the answer.
func TestAttributeReadsSendersAndMessageNumbersFromToolResults(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	cwd := t.TempDir()
	// yann registered, then sent two messages and a DM, with the results written both as a string and as typed parts.
	yann := transcript(t, cfg, cwd, "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		toolUse(t, "toolu_1", "mcp__aimebu__bus_register"), toolResult(t, "toolu_1", `{"id":"yann@aimebu","name":"yann","kind":"ai"}`, true),
		toolUse(t, "toolu_2", "mcp__aimebu__bus_say"), toolResult(t, "toolu_2", `{"id":37965,"room":"aimebu-leader"}`, false),
		toolUse(t, "toolu_3", "mcp__aimebu-dev__bus_say"), toolResult(t, "toolu_3", `{"addressed_to":["wren"],"id":37970,"room":"aimebu"}`, true),
		toolUse(t, "toolu_4", "mcp__aimebu__bus_dm"), toolResult(t, "toolu_4", `{"id":37972,"room":"dm:wren:yann"}`, true),
		toolUse(t, "toolu_5", "Bash"), toolResult(t, "toolu_5", `{"id":12345}`, true), // another tool's result is not a message number
	)
	// hal registered in a different session of the same folder.
	transcript(t, cfg, cwd, "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		toolUse(t, "toolu_9", "mcp__aimebu__bus_register"), toolResult(t, "toolu_9", `{"id":"hal@aimebu"}`, false),
		toolUse(t, "toolu_10", "mcp__aimebu__bus_say"), toolResult(t, "toolu_10", `{"id":37999,"room":"aimebu"}`, false))
	// Nothing registered: no sender.
	transcript(t, cfg, cwd, "33333333-cccc-4ccc-8ccc-cccccccccccc")
	age(t, yann, 0)

	all, err := Attribute(cfg, []string{cwd}, time.Time{}, "", time.Time{})
	if err != nil || len(all.Sessions) != 2 {
		t.Fatalf("all: %+v %v", all, err)
	}
	only, err := Attribute(cfg, []string{cwd}, time.Time{}, "yann@aimebu", time.Time{})
	if err != nil || len(only.Sessions) != 1 {
		t.Fatalf("yann: %+v %v", only, err)
	}
	got := only.Sessions[0]
	if got.SessionID != "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || got.Sender != "yann@aimebu" || got.CWD != cwd ||
		!reflect.DeepEqual(got.MessageNos, []int64{37965, 37970, 37972}) || got.Model != "claude-opus-4-5-20251101" {
		t.Fatalf("attribution = %+v", got)
	}
	if text := fmt.Sprintf("%+v", all); strings.Contains(text, "SECRET") {
		t.Fatalf("transcript text leaked into the attribution: %s", text)
	}
	// A session last written before since is not read at all.
	old := filepath.Join(cfg, "projects", FolderName(cwd), "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb.jsonl")
	age(t, old, 48*time.Hour)
	recent, err := Attribute(cfg, []string{cwd}, time.Now().Add(-time.Hour), "", time.Time{})
	if err != nil || len(recent.Sessions) != 1 || recent.Sessions[0].Sender != "yann@aimebu" {
		t.Fatalf("since: %+v %v", recent, err)
	}
}

// A transcript line far longer than any message is skipped, not parsed or kept in memory, and the lines around it still count.
func TestAttributeSkipsAnOversizedLine(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	cwd := t.TempDir()
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", MaxAttributeLine+10) + `"}}`
	transcript(t, cfg, cwd, "44444444-dddd-4ddd-8ddd-dddddddddddd",
		toolUse(t, "toolu_1", "mcp__aimebu__bus_register"), toolResult(t, "toolu_1", `{"id":"ivy@aimebu"}`, true), huge,
		toolUse(t, "toolu_2", "mcp__aimebu__bus_say"), toolResult(t, "toolu_2", `{"id":5,"room":"r"}`, true))
	got, err := Attribute(cfg, []string{cwd}, time.Time{}, "", time.Time{})
	if err != nil || len(got.Sessions) != 1 || got.Sessions[0].Sender != "ivy@aimebu" || !reflect.DeepEqual(got.Sessions[0].MessageNos, []int64{5}) {
		t.Fatalf("attribution around a huge line: %+v %v", got, err)
	}
}
