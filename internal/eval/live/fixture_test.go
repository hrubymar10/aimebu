package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

// seen is what a model could learn about its situation through the stand-in's tools and the checkout, read while the turn runs.
type seen struct {
	history  string // room_history of the task room
	messages map[int64]string
	log      string // git log of the checkout, subjects, newest first
	files    map[string]bool
}

// observe runs one scenario through the runner with a stand-in adapter that, instead of answering from a script, reads what the model would read:
// the task room's history, each message by number, the checkout's log and files.
func observe(t *testing.T, id string, numbers ...int64) seen {
	t.Helper()
	var got seen
	st := &stub{}
	st.reply = func(spec adapter.TurnSpec, n int) []adapter.Event {
		port := 0
		if spec.AgentTools != nil {
			_, _ = fmtSscanPort(spec.AgentTools.ServerURL, &port)
		}
		key := spec.AgentTools.Key
		call := func(name, args string) string {
			_, body := rpc(t, port, key, `{"id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
			var out struct {
				Result struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Result.Content) != 1 {
				t.Fatalf("%s: %s (%v)", name, body, err)
			}
			return out.Result.Content[0].Text
		}
		got.history = call("room_history", `{"room_id":"`+eval.TaskRoom+`"}`)
		got.messages = map[int64]string{}
		for _, no := range numbers {
			got.messages[no] = call("message_get", `{"no":`+itoa(no)+`}`)
		}
		out, err := exec.Command("git", "-C", spec.CheckoutPath, "log", "--format=%s").Output()
		if err != nil {
			t.Fatal(err)
		}
		got.log = string(out)
		got.files = map[string]bool{}
		entries, _ := os.ReadDir(spec.CheckoutPath)
		for _, e := range entries {
			got.files[e.Name()] = true
		}
		return []adapter.Event{answerEvent(`{"messages":[]}`)}
	}
	o := newOptions(t, st, id)
	if _, err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.messages == nil {
		t.Fatalf("%s: the stand-in adapter never ran", id)
	}
	return got
}

func TestScenarioFixturesMatchTheirSituation(t *testing.T) {
	const task = eval.TaskRoom
	for _, tc := range []struct {
		id      string
		numbers []int64
		history []string // texts room_history must hold
		message map[int64]string
		commit  string // the checkout's newest commit subject
		files   []string
	}{
		// s02: a reply in a conversation the agent started; the two earlier messages the note quotes exist.
		{id: "s02-thread-reply", numbers: []int64{7100, 7101}, history: []string{"Should we cache the lookups?", "Option A reads the cache"},
			message: map[int64]string{7101: "Option A reads the cache; option B recomputes it."}},
		// s03, s04, s05 and s06: the brief is a message of the room, and the checkout holds the work the brief says was done.
		{id: "s03-handoff-done", numbers: []int64{7203}, history: []string{"confirm the checkout builds"}, message: map[int64]string{7203: "confirm the checkout builds"},
			commit: "make the checkout build and vet clean", files: []string{"go.mod", "main.go", "util.go"}},
		{id: "s04-handoff-partial", numbers: []int64{7304}, history: []string{"do part A (rename the flag)"}, message: map[int64]string{7304: "part B (migrate the stored config)"},
			commit: "rename the flag (part A) and update its test", files: []string{"flag.go", "flag_test.go"}},
		{id: "s05-handoff-failed", numbers: []int64{7405}, history: []string{"run the schema migration"}, message: map[int64]string{7405: "permission denied"},
			commit: "add the schema migration script", files: []string{"migrate.sh"}},
		{id: "s06-handoff-unhinted", numbers: []int64{7506}, history: []string{"Report back when you are done"}, message: map[int64]string{7506: "its tests pass"},
			commit: "make the change", files: []string{"util.go", "util_test.go"}},
		// s14 and s15: the brief a command was started for is in the history; the checkout is the project the command runs in.
		{id: "s14-command-finished", numbers: []int64{8300, 8314}, history: []string{"Run the eval tests and hand off the result."}, message: map[int64]string{8300: "hand off the result"},
			commit: "initial project", files: []string{"go.mod", "main.go", "main_test.go"}},
		{id: "s15-waiting-on-command", numbers: []int64{8400, 8415}, history: []string{"Run the full test suite with run_command"}, message: map[int64]string{8400: "hand off the result"},
			commit: "initial project", files: []string{"go.mod", "main.go", "main_test.go"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got := observe(t, tc.id, tc.numbers...)
			for _, text := range tc.history {
				if !strings.Contains(got.history, text) {
					t.Errorf("room_history of %s lacks %q: %s", task, text, got.history)
				}
			}
			for no, text := range tc.message {
				if !strings.Contains(got.messages[no], text) {
					t.Errorf("message_get #%d lacks %q: %q", no, text, got.messages[no])
				}
			}
			for no, text := range got.messages {
				if strings.Contains(text, "not available") {
					t.Errorf("message_get #%d: %q (every number the scenario names exists)", no, text)
				}
			}
			if tc.commit != "" && !strings.HasPrefix(got.log, tc.commit+"\n") {
				t.Errorf("the checkout's newest commit is not %q:\n%s", tc.commit, got.log)
			}
			for _, name := range tc.files {
				if !got.files[name] {
					t.Errorf("the checkout lacks %s: %v", name, got.files)
				}
			}
		})
	}
}

// A case starts from the base project whatever an earlier case left, and keeps the folder a harness keeps its own state in.
func TestPrepareCheckoutResetsBetweenCases(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aimebu", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := &eval.Checkout{Commit: "the work", Files: map[string]string{"sub/work.go": "package sub\n"}}
	if err := prepareCheckout(dir, work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "work.go")); err != nil {
		t.Fatalf("the work is committed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("left by the agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareCheckout(dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"sub", "stray.txt"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s survived into the next case", gone)
		}
	}
	for _, kept := range []string{"main.go", "go.mod", filepath.Join(".aimebu", "state")} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s should be there: %v", kept, err)
		}
	}
	out, _ := exec.Command("git", "-C", dir, "log", "--format=%s").Output()
	if strings.TrimSpace(string(out)) != "initial project" {
		t.Errorf("a case with no work of its own sits on the base commit:\n%s", out)
	}
}

func TestPrepareCheckoutIgnoresGlobalGitExclude(t *testing.T) {
	dir := t.TempDir()
	ignore := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(ignore, []byte(".aimebu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\texcludesFile = "+ignore+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aimebu"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimebu", "state"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareCheckout(dir, &eval.Checkout{Commit: "scenario", Files: map[string]string{"work.go": "package main\n"}}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "ls-files", ".aimebu").Output(); err != nil || len(out) != 0 {
		t.Fatalf("runtime state was staged: %q, %v", out, err)
	}
}

func TestScratchGitCallsUseHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "git.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `exec.Command("git"`) || strings.Contains(string(data), `exec.CommandContext("git"`) {
			t.Errorf("%s runs git outside gitCommand", name)
		}
	}
}

func TestCheckoutAndHistoryAreValidated(t *testing.T) {
	for _, bad := range []string{"../escape", "/abs", ".git/hooks/x", ".aimebu/x", "a//b"} {
		in := &eval.Input{Checkout: &eval.Checkout{Commit: "c", Files: map[string]string{bad: "x"}}}
		s := eval.Scenario{ID: "x", Layer: eval.LayerLive, Input: in, Expect: eval.Expect{Parse: eval.ParseValid, MaxTurnsToValid: 1}}
		in.Notes, in.AllowedTools = json.RawMessage(`[]`), []string{}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "input.checkout") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	in := &eval.Input{Notes: json.RawMessage(`[{"no":5,"room_id":"{task_room}","from":"{person}","body":"hi"}]`), AllowedTools: []string{},
		History: []eval.HistoryMessage{{No: 5, Room: "{task_room}", From: "{person}", Body: "again"}}}
	if _, err := in.Messages(); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Errorf("a history message that repeats a note's number: %v", err)
	}
}

func itoa(n int64) string { return fmt.Sprint(n) }

// fmtSscanPort reads the port of a stand-in URL such as http://127.0.0.1:41234.
func fmtSscanPort(url string, port *int) (int, error) {
	i := strings.LastIndex(url, ":")
	p, err := strconv.Atoi(url[i+1:])
	*port = p
	return p, err
}
