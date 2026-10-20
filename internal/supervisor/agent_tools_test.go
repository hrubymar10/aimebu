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

func TestDockerSpawnRejectsUnreachableCheckoutBeforeWritingKey(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "claude-docker")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho 'Directory is not mounted in the harness container' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(t.Context(), filepath.Join(root, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	result := c.applySpawn(protocol.Frame{AgentID: "agt_docker", AgentKey: "aak_" + strings.Repeat("x", 43), CheckoutPath: checkout, Agent: &adapter.AgentSpec{Tool: "claude", Variant: "docker"}})
	if result.Result != "error" || !strings.Contains(result.Message, "not mounted") {
		t.Fatalf("spawn result=%+v", result)
	}
	if _, err := os.Stat(c.agentToolsPath("agt_docker")); !os.IsNotExist(err) {
		t.Fatalf("credential file exists or stat failed: %v", err)
	}
}

func TestSpawnWriteFailureKeepsMachineConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	responses := make(chan []protocol.Frame, 1)
	checkout := t.TempDir()
	errorsSeen := make(chan error, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			errorsSeen <- err
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(ctx, conn); err != nil {
			errorsSeen <- err
			return
		}
		write := func(frame protocol.Frame) error {
			data, err := json.Marshal(frame)
			if err != nil {
				return err
			}
			return conn.Write(ctx, websocket.MessageText, data)
		}
		for _, frame := range []protocol.Frame{
			{V: protocol.Version, Type: protocol.HelloAck},
			{V: protocol.Version, Type: protocol.Spawn, ReqID: "bad/0", AgentID: "bad", AgentKey: "invalid", CheckoutPath: checkout},
			{V: protocol.Version, Type: protocol.Spawn, ReqID: "good/0", AgentID: "good", AgentKey: "aak_" + strings.Repeat("g", 43), CheckoutPath: checkout},
		} {
			if err := write(frame); err != nil {
				errorsSeen <- err
				return
			}
		}
		var seen []protocol.Frame
		for range 2 {
			frame, err := readFrame(ctx, conn)
			if err != nil {
				errorsSeen <- err
				return
			}
			seen = append(seen, frame)
		}
		responses <- seen
	}))
	defer httpServer.Close()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", httpServer.URL, j, adapter.NewFake())
	c.OnError = func(error) {}
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	select {
	case seen := <-responses:
		if len(seen) != 2 || seen[0].Type != protocol.SpawnResult || seen[0].Result != "error" ||
			seen[1].Type != protocol.SpawnResult || seen[1].Result != "ok" {
			t.Fatalf("spawn results=%+v", seen)
		}
	case err := <-errorsSeen:
		t.Fatalf("machine connection failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not leave closed machine connection")
	}
}

func TestSpawnConfigFailureIsAgentScopedAndStaleSpawnCannotReplaceKey(t *testing.T) {
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	c.OnError = func(error) {}
	oldKey := "aak_" + strings.Repeat("o", 43)
	if err := c.writeAgentToolsConfig("agt_test", oldKey); err != nil {
		t.Fatal(err)
	}
	c.desired["agt_test"] = protocol.DesiredAgent{AgentID: "agt_test", Generation: 2, DesiredRev: 4, Desired: "active"}
	spawn := protocol.Frame{ReqID: "agt_test/1", AgentID: "agt_test", Generation: 1, DesiredRev: 0,
		AgentKey: "aak_" + strings.Repeat("n", 43), CheckoutPath: t.TempDir()}
	if result := c.applySpawn(spawn); result.Result != "error" {
		t.Fatalf("stale spawn result=%+v", result)
	}
	data, err := os.ReadFile(c.agentToolsPath("agt_test"))
	if err != nil || !strings.Contains(string(data), oldKey) {
		t.Fatalf("stale spawn replaced credential: %q, %v", data, err)
	}
	spawn.Generation, spawn.ReqID, spawn.AgentKey = 3, "agt_test/3", "invalid-key"
	if result := c.applySpawn(spawn); result.Result != "error" || result.Message == "" {
		t.Fatalf("failed write result=%+v", result)
	}
	if c.desired["agt_test"].Generation != 2 {
		t.Fatal("failed config write advanced desired generation")
	}
	spawn.AgentKey = "aak_" + strings.Repeat("y", 43)
	if result := c.applySpawn(spawn); result.Result != "ok" {
		t.Fatalf("subsequent valid spawn result=%+v", result)
	}
	data, err = os.ReadFile(c.agentToolsPath("agt_test"))
	if err != nil || !strings.Contains(string(data), spawn.AgentKey) || c.desired["agt_test"].Generation != 3 {
		t.Fatalf("subsequent spawn failed: config=%q desired=%+v err=%v", data, c.desired["agt_test"], err)
	}
}

func TestSpawnRejectsMissingCheckoutBeforeWritingAgentKey(t *testing.T) {
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	frame := protocol.Frame{AgentID: "agt_missing", Generation: 0, AgentKey: "aak_" + strings.Repeat("m", 43), CheckoutPath: filepath.Join(t.TempDir(), "absent")}
	result := c.applySpawn(frame)
	if result.Result != "error" || !strings.Contains(result.Message, "does not exist on this machine") {
		t.Fatalf("result: %+v", result)
	}
	if _, err := os.Stat(c.agentToolsPath(frame.AgentID)); !os.IsNotExist(err) {
		t.Fatalf("agent key written despite invalid checkout: %v", err)
	}
}

func TestSpawnRejectsUnavailableLauncherForRestartAndReplay(t *testing.T) {
	j, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	c.Launchers = []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "launch", Tool: "codex", Enabled: true}, UnavailableReason: "command not found"}}
	for _, name := range []string{"restart", "pending replay"} {
		frame := protocol.Frame{AgentID: "agt_" + strings.ReplaceAll(name, " ", "_"),
			AgentKey: "aak_" + strings.Repeat("x", 43), CheckoutPath: t.TempDir(),
			Agent: &adapter.AgentSpec{Tool: "codex", LauncherID: "launch"}}
		result := c.applySpawn(frame)
		if result.Result != "error" || !strings.Contains(result.Message, "launcher unavailable") {
			t.Fatalf("%s: %+v", name, result)
		}
		if _, err := os.Stat(c.agentToolsPath(frame.AgentID)); !os.IsNotExist(err) {
			t.Fatalf("%s wrote credentials: %v", name, err)
		}
	}
}

func TestSlowPathProbeDoesNotBlockMachineFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	root := t.TempDir()
	command := filepath.Join(root, "slow-launcher")
	if err := os.WriteFile(filepath.Join(root, ".slow-probe-only"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nif [ -f .slow-probe-only ]; then sleep 2; fi\necho version\n"), 0700); err != nil {
		t.Fatal(err)
	}
	definition := protocol.LauncherDefinition{ID: "launch", Name: "slow", Tool: "codex", Command: command, Enabled: true}
	frames := make(chan []protocol.Frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(ctx, conn); err != nil {
			return
		}
		for _, frame := range []protocol.Frame{
			{V: protocol.Version, Type: protocol.HelloAck, LauncherDefinitions: []protocol.LauncherDefinition{definition}},
			{V: protocol.Version, Type: protocol.PathProbe, ReqID: "slow", CheckoutPath: root, LauncherID: "launch"},
			{V: protocol.Version, Type: protocol.Ping},
		} {
			data, _ := json.Marshal(frame)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
		var seen []protocol.Frame
		for range 3 {
			frame, err := readFrame(ctx, conn)
			if err != nil {
				return
			}
			seen = append(seen, frame)
		}
		frames <- seen
	}))
	defer server.Close()
	j, err := OpenJournal(ctx, filepath.Join(root, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", server.URL, j, adapter.NewFake())
	c.OnError = func(error) {}
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	select {
	case seen := <-frames:
		if seen[0].Type != protocol.LauncherStatus || seen[1].Type != protocol.Pong || seen[2].Type != protocol.PathResult || seen[2].ReqID != "slow" || seen[2].Code != "" {
			t.Fatalf("machine frames blocked or probe failed: %+v", seen)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-done
}

func TestPathProbeDoesNotReportAfterConnectionEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	root := t.TempDir()
	command := filepath.Join(root, "probe-launcher")
	if err := os.WriteFile(filepath.Join(root, ".slow-probe-only"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nif [ -f .slow-probe-only ]; then touch .probe-started; sleep 2; fi\necho version\n"), 0700); err != nil {
		t.Fatal(err)
	}
	definition := protocol.LauncherDefinition{ID: "launch", Name: "probe", Tool: "codex", Command: command, Enabled: true}
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(ctx, conn); err != nil {
			return
		}
		write := func(frame protocol.Frame) error {
			data, _ := json.Marshal(frame)
			return conn.Write(ctx, websocket.MessageText, data)
		}
		if write(protocol.Frame{V: protocol.Version, Type: protocol.HelloAck, LauncherDefinitions: []protocol.LauncherDefinition{definition}}) != nil {
			return
		}
		if _, err := readFrame(ctx, conn); err != nil { // launcher status
			return
		}
		if write(protocol.Frame{V: protocol.Version, Type: protocol.PathProbe, ReqID: "abandoned", CheckoutPath: root, LauncherID: "launch"}) != nil {
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, ".probe-started")); err == nil {
				close(closed)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer server.Close()
	j, err := OpenJournal(ctx, filepath.Join(root, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", server.URL, j, adapter.NewFake())
	errorsSeen := make(chan error, 1)
	c.OnError = func(err error) { errorsSeen <- err }
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-errorsSeen:
		t.Fatalf("completed abandoned probe after disconnect: %v", err)
	case <-time.After(2500 * time.Millisecond):
	}
}

func TestSpawnRejectsCheckoutAliasBeforeWritingAgentKey(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	frame := protocol.Frame{AgentID: "agt_alias", Generation: 0, AgentKey: "aak_" + strings.Repeat("a", 43), CheckoutPath: alias}
	result := c.applySpawn(frame)
	if result.Result != "error" || !strings.Contains(result.Message, "must be canonical") {
		t.Fatalf("result: %+v", result)
	}
	if _, err := os.Stat(c.agentToolsPath(frame.AgentID)); !os.IsNotExist(err) {
		t.Fatalf("agent key written despite checkout alias: %v", err)
	}
}

func TestAgentToolsConfigIsPrivateAndOnlyPathInJournal(t *testing.T) {
	t.Setenv("AIMEBU2_INSECURE_SKIP_VERIFY", "1")
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	j, err := OpenJournal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "machine-key", "http://127.0.0.1:9897", j, adapter.NewFake())
	key := "aak_" + strings.Repeat("x", 43)
	if err := c.writeAgentToolsConfig("agt_test", key); err != nil {
		t.Fatal(err)
	}
	configPath := c.agentToolsPath("agt_test")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode=%o", info.Mode().Perm())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	server := config.Servers["aimebu"]
	if !filepath.IsAbs(server.Command) || len(server.Args) != 1 || server.Args[0] != "agent-tools" || server.Env["AIMEBU2_AGENT_KEY"] != key || server.Env["AIMEBU2_AGENT_ID"] != "agt_test" || server.Env["AIMEBU2_INSECURE_SKIP_VERIFY"] != "1" {
		t.Fatalf("bad config: %+v", server)
	}
	journalData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journalData), key) {
		t.Fatal("agent key leaked to journal")
	}
	newKey := "aak_" + strings.Repeat("y", 43)
	if err := c.writeAgentToolsConfig("agt_test", newKey); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), key) || !strings.Contains(string(updated), newKey) {
		t.Fatal("credential refresh did not atomically replace the config")
	}
}
