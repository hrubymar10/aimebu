package live

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

// rpcAt posts to the stand-in with a query string, as an adapter's endpoint carries it (adapter.AnswerToolEndpoint).
func rpcAt(t *testing.T, port int, key, query, body string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/agent/mcp%s", port, query), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return out.String()
}

func listTools(t *testing.T, port int, key, query string) []agenttools.Definition {
	t.Helper()
	var out struct {
		Result struct {
			Tools []agenttools.Definition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(rpcAt(t, port, key, query, `{"id":1,"method":"tools/list"}`)), &out); err != nil {
		t.Fatal(err)
	}
	return out.Result.Tools
}

// What a model reads in the stand-in is production's catalog, name by name: the description and the input schema are the ones agenttools serves, and
// every tool a scenario offers exists there. The catalog does not depend on the role (agenttools.Definitions): the same list is compared for each
// scenario's own toolbox.
func TestStandInCatalogIsProductions(t *testing.T) {
	m, err := StartMCP()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	production := map[string]agenttools.Definition{}
	for _, def := range agenttools.Definitions(true) {
		production[def.Name] = def
	}
	all, err := eval.LoadDir("../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range all {
		key := "aak_" + sc.ID
		m.Register(key, sc.Input.AllowedTools)
		got := listTools(t, m.Port(), key, "")
		if len(got) != len(sc.Input.AllowedTools) {
			t.Errorf("%s offers %d tools, its toolbox names %d: %v", sc.ID, len(got), len(sc.Input.AllowedTools), got)
		}
		for _, name := range sc.Input.AllowedTools {
			want, ok := production[name]
			if !ok {
				t.Errorf("%s: tool %q is not in production's catalog", sc.ID, name)
				continue
			}
			for _, def := range got {
				if def.Name != name {
					continue
				}
				a, _ := json.Marshal(def)
				b, _ := json.Marshal(want)
				if !bytes.Equal(a, b) {
					t.Errorf("%s: %s differs from production:\nstand-in   %.200s\nproduction %.200s", sc.ID, name, a, b)
				}
			}
		}
	}
	// Every production tool, one at a time, comes back whole (the schemas of the tools no scenario offers too).
	for name, want := range production {
		if name == "submit_answer" {
			continue
		}
		key := "aak_one_" + name
		m.Register(key, []string{name})
		got := listTools(t, m.Port(), key, "")
		a, _ := json.Marshal(got)
		b, _ := json.Marshal([]agenttools.Definition{want})
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs from production:\nstand-in   %.200s\nproduction %.200s", name, a, b)
		}
	}
}

func TestSubmitAnswerIsOfferedOnlyWhereProductionOffersIt(t *testing.T) {
	m, err := StartMCP()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Register("aak_a", []string{"react"})
	has := func(tools []agenttools.Definition) bool {
		for _, def := range tools {
			if def.Name == "submit_answer" {
				return true
			}
		}
		return false
	}
	if has(listTools(t, m.Port(), "aak_a", "")) {
		t.Fatal("a connection that did not ask for it is not offered submit_answer")
	}
	with := listTools(t, m.Port(), "aak_a", "?"+agenttools.AnswerToolQuery+"=1")
	if !has(with) || len(with) != 2 {
		t.Fatalf("the connection that asked is offered the tool and its own tools: %v", with)
	}
	call := func(query, args string) string {
		return rpcAt(t, m.Port(), "aak_a", query, `{"id":2,"method":"tools/call","params":{"name":"submit_answer","arguments":`+args+`}}`)
	}
	good := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"PONG"}]}`
	if out := call("", good); !strings.Contains(out, "unknown tool") || len(m.Submitted("aak_a")) != 0 {
		t.Fatalf("called without being offered: %s", out)
	}
	on := "?" + agenttools.AnswerToolQuery + "=1"
	if out := call(on, `{"messages":[{"room":"`+eval.TaskRoom+`","body":123}]}`); !strings.Contains(out, `"isError":true`) || len(m.Submitted("aak_a")) != 0 {
		t.Fatalf("a wrong type is refused by production's validator and not kept: %s", out)
	}
	if out := call(on, ` `+good); !strings.Contains(out, "Answer accepted") || string(m.Submitted("aak_a")) != good {
		t.Fatalf("a valid answer is accepted and kept compacted: %s / %q", out, m.Submitted("aak_a"))
	}
	if calls := m.Calls("aak_a"); len(calls) != 3 || calls[0] != "submit_answer" {
		t.Fatalf("the calls are recorded: %v", calls)
	}
}

// The answer an adapter reports is attributed to the tool when it is what submit_answer accepted, and to the text otherwise.
func TestAnswerViaToolOrText(t *testing.T) {
	good := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"It scores answers.","reply_to":"7001"}]}`
	for name, tc := range map[string]struct {
		viaTool bool
		want    string
	}{"tool": {true, eval.ViaTool}, "text": {false, eval.ViaText}} {
		t.Run(name, func(t *testing.T) {
			st := &stub{}
			st.reply = func(spec adapter.TurnSpec, n int) []adapter.Event {
				if tc.viaTool {
					var port int
					_, _ = fmtSscanPort(spec.AgentTools.ServerURL, &port)
					out := rpcAt(t, port, spec.AgentTools.Key, "?"+agenttools.AnswerToolQuery+"=1", `{"id":2,"method":"tools/call","params":{"name":"submit_answer","arguments":`+good+`}}`)
					if !strings.Contains(out, "Answer accepted") {
						t.Errorf("submit_answer: %s", out)
					}
				}
				return []adapter.Event{answerEvent(good)}
			}
			o := newOptions(t, st, "s01-plain-reply")
			cases, err := o.Run(context.Background())
			if err != nil || len(cases) != 1 {
				t.Fatalf("%v %v", cases, err)
			}
			a := cases[0].Attempts[0]
			if a.Via != tc.want || !a.Correct {
				t.Fatalf("via %q correct %t, want %q correct", a.Via, a.Correct, tc.want)
			}
		})
	}
}

// Whether the turn is offered the tool is the adapter's decision (adapter.OffersAnswerTool) from the launcher row's harness version, which the runner
// carries into the spec (supervisor.ApplyLauncher): a version that supports it gets the answer_tool query on its toolbox endpoint, one that does
// not gets none. The real OpenCode Docker adapter writes its config to the stand-in launcher's environment, which the script records.
func TestHarnessVersionDecidesWhetherTheToolIsOffered(t *testing.T) {
	for version, want := range map[string]bool{adapter.MinOpenCodeAnswerTool: true, opencode.SupportedVersion: false} {
		t.Run(version, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "config")
			o := opencodeOptions(t, "printf '%s' \"$OPENCODE_CONFIG_CONTENT\" > '"+record+"'\nexit 1\n")
			o.Launchers[0].Version = version
			if _, err := o.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				MCP map[string]struct {
					URL string `json:"url"`
				} `json:"mcp"`
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			url := config.MCP["aimebu"].URL
			if got := strings.Contains(url, agenttools.AnswerToolQuery+"=1"); got != want {
				t.Fatalf("version %s: toolbox endpoint %q, offered=%t want %t", version, url, got, want)
			}
		})
	}
}
