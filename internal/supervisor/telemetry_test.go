package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// A supervisor that takes the server's telemetry settings says so in its hello, gets them in hello_ack and again in a telemetry_config frame; one that
// does not take them (the server's embedded one) does not announce the capability.
func TestSupervisorTakesTelemetrySettingsFromHelloAckAndFrames(t *testing.T) {
	for _, takes := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		announced := make(chan bool, 1)
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			hello, err := readFrame(ctx, conn)
			if err != nil {
				return
			}
			announced <- hello.SupportsTelemetryConfig
			for _, frame := range []protocol.Frame{
				{V: protocol.Version, Type: protocol.HelloAck, Telemetry: &protocol.TelemetrySettings{Enabled: true, DSN: "https://key@first.example/1", Environment: "dev", TracesSampleRate: 0.5}},
				{V: protocol.Version, Type: protocol.TelemetryConfig, Telemetry: &protocol.TelemetrySettings{}},
			} {
				data, _ := json.Marshal(frame)
				if conn.Write(ctx, websocket.MessageText, data) != nil {
					return
				}
			}
			<-ctx.Done()
		}))
		j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		c := NewClient("machine", "key", httpServer.URL, j, adapter.NewFake())
		c.OnError = func(error) {}
		got := make(chan protocol.TelemetrySettings, 4)
		if takes {
			c.OnTelemetry = func(settings protocol.TelemetrySettings) { got <- settings }
		}
		done := make(chan error, 1)
		go func() { done <- c.RunOnce(ctx) }()
		select {
		case flag := <-announced:
			if flag != takes {
				t.Fatalf("takes=%v but the hello says %v", takes, flag)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no hello")
		}
		if takes {
			first, second := <-got, <-got
			if !first.Enabled || first.DSN != "https://key@first.example/1" || first.TracesSampleRate != 0.5 || first.Environment != "dev" {
				t.Fatalf("hello_ack settings: %+v", first)
			}
			if second.Enabled {
				t.Fatalf("a later telemetry_config frame (off) was not taken: %+v", second)
			}
		} else {
			select {
			case settings := <-got:
				t.Fatalf("a supervisor that does not take them got %+v", settings)
			case <-time.After(300 * time.Millisecond):
			}
		}
		cancel()
		<-done
		httpServer.Close()
		j.Close()
	}
}

// The telemetry variables reach the agent-tools bridge through the environment of its MCP config entry, never its arguments, in a config written
// now and in the ones that exist (the agent's key stays), and an empty set takes them out again.
func TestTelemetryEnvReachesTheBridgeThroughItsConfigEnvironment(t *testing.T) {
	j, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	c.OnError = func(error) {}
	key := "aak_" + strings.Repeat("k", 43)
	if err := c.writeAgentToolsConfig("agt_old", key); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AIMEBU2_SENTRY_DSN": "https://key@o1.ingest.example/1", "AIMEBU2_SENTRY_ENV": "dev", "AIMEBU2_SENTRY_TRACES_SAMPLE_RATE": "0.5"}
	c.SetTelemetryEnv(env)
	read := func(agent string) (map[string]any, []any) {
		t.Helper()
		data, err := os.ReadFile(c.agentToolsPath(agent))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Servers map[string]struct {
				Args []any          `json:"args"`
				Env  map[string]any `json:"env"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		return config.Servers["aimebu"].Env, config.Servers["aimebu"].Args
	}
	envNow, args := read("agt_old")
	if envNow["AIMEBU2_SENTRY_DSN"] != env["AIMEBU2_SENTRY_DSN"] || envNow["AIMEBU2_AGENT_KEY"] != key || envNow["AIMEBU2_SENTRY_TRACES_SAMPLE_RATE"] != "0.5" {
		t.Fatalf("an existing config: %v", envNow)
	}
	for _, arg := range args {
		if strings.Contains(arg.(string), "sentry") || strings.Contains(arg.(string), "ingest") {
			t.Fatalf("telemetry in the bridge's arguments: %v", args)
		}
	}
	if err := c.writeAgentToolsConfig("agt_new", "aak_"+strings.Repeat("n", 43)); err != nil {
		t.Fatal(err)
	}
	if envNew, _ := read("agt_new"); envNew["AIMEBU2_SENTRY_DSN"] != env["AIMEBU2_SENTRY_DSN"] {
		t.Fatalf("a config written afterwards: %v", envNew)
	}
	c.SetTelemetryEnv(nil)
	if envNow, _ = read("agt_old"); envNow["AIMEBU2_SENTRY_DSN"] != nil || envNow["AIMEBU2_AGENT_KEY"] != key {
		t.Fatalf("after an empty set: %v", envNow)
	}
}
