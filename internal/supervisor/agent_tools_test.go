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
	"github.com/hrubymar10/aimebu/internal/ccsessions"
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
	ctx, cancel := context.WithCancel(t.Context())
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
	<-done
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

// A session scan answers from the machine's own Claude state without blocking machine frames, and the hello advertises it.
func TestSessionScanListsAndFindsSessionsOfTheMachineClaudeHome(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	claude := t.TempDir() // a fixture Claude configuration folder; the real one is never read
	project := filepath.Join(t.TempDir(), "proj")
	const id, other = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	write := func(cwd, sid string) {
		dir := filepath.Join(claude, "projects", ccsessions.FolderName(cwd))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "sessionId": sid, "gitBranch": "dev", "message": map[string]any{"content": "Plan the release"}})
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(project, id)
	write("/somewhere/else", other)
	type seen struct{ hello, folder, byID, missing, both protocol.Frame }
	results := make(chan seen, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var out seen
		if out.hello, err = readFrame(ctx, conn); err != nil {
			return
		}
		for _, frame := range []protocol.Frame{
			{V: protocol.Version, Type: protocol.HelloAck},
			{V: protocol.Version, Type: protocol.SessionScan, ReqID: "folder", ScanFolders: []string{project}},
			{V: protocol.Version, Type: protocol.SessionScan, ReqID: "byid", ScanSessionID: other},
			{V: protocol.Version, Type: protocol.SessionScan, ReqID: "missing", ScanSessionID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"},
			{V: protocol.Version, Type: protocol.SessionScan, ReqID: "both", ScanSessionID: id, ScanFolders: []string{project}},
		} {
			data, _ := json.Marshal(frame)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
		byID := map[string]protocol.Frame{}
		for len(byID) < 4 {
			frame, err := readFrame(ctx, conn)
			if err != nil {
				return
			}
			if frame.Type == protocol.SessionScanResult {
				byID[frame.ReqID] = frame
			}
		}
		out.folder, out.byID, out.missing, out.both = byID["folder"], byID["byid"], byID["missing"], byID["both"]
		results <- out
	}))
	defer server.Close()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", server.URL, j, adapter.NewFake())
	c.ClaudeConfigDir = claude
	c.OnError = func(error) {}
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	select {
	case got := <-results:
		if !got.hello.SupportsSessionScan {
			t.Fatal("the hello does not advertise session scans")
		}
		if got.folder.Code != "" || len(got.folder.Sessions) != 1 || got.folder.Sessions[0].ID != id || got.folder.Sessions[0].CWD != project ||
			got.folder.Sessions[0].Title != "Plan the release" || got.folder.Sessions[0].GitBranch != "dev" || got.folder.Sessions[0].ModifiedAt == "" {
			t.Fatalf("folder scan: %+v", got.folder)
		}
		if got.byID.Code != "" || len(got.byID.Sessions) != 1 || got.byID.Sessions[0].ID != other || got.byID.Sessions[0].CWD != "/somewhere/else" {
			t.Fatalf("scan by id: %+v", got.byID)
		}
		if got.missing.Code != "session_not_found" || len(got.missing.Sessions) != 0 {
			t.Fatalf("a missing id: %+v", got.missing)
		}
		if got.both.Code != "invalid_request" {
			t.Fatalf("a folder and an id together: %+v", got.both)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-done
}

// A scan reads the Claude home of the launcher named, which may point its own CLAUDE_CONFIG_DIR elsewhere, not the supervisor's.
func TestSessionScanReadsTheClaudeHomeOfTheNamedLauncher(t *testing.T) {
	supervisorHome, launcherHome := t.TempDir(), t.TempDir() // fixture folders; the real ~/.claude is never read
	t.Setenv("CLAUDE_CONFIG_DIR", supervisorHome)
	t.Setenv("WORK_CLAUDE_HOME", launcherHome)
	project := filepath.Join(t.TempDir(), "proj")
	const own, work = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	write := func(home, sid string) {
		dir := filepath.Join(home, "projects", ccsessions.FolderName(project))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(map[string]any{"type": "user", "cwd": project, "sessionId": sid, "message": map[string]any{"content": "hi"}})
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(supervisorHome, own)
	write(launcherHome, work)
	c := &Client{Launchers: []protocol.LauncherRow{
		{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_work", EnvRefs: map[string]string{"CLAUDE_CONFIG_DIR": "WORK_CLAUDE_HOME"}}},
		{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_plain"}},
	}}
	ids := func(frame protocol.Frame) []string {
		var out []string
		for _, session := range frame.Sessions {
			out = append(out, session.ID)
		}
		return out
	}
	scan := func(launcher string) protocol.Frame {
		return c.sessionScanResult(protocol.Frame{ReqID: "r", LauncherID: launcher, ScanFolders: []string{project}})
	}
	if got := ids(scan("lch_work")); len(got) != 1 || got[0] != work {
		t.Fatalf("the launcher's own home: %v", got)
	}
	if got := ids(scan("lch_plain")); len(got) != 1 || got[0] != own {
		t.Fatalf("a launcher with no references reads the supervisor's home: %v", got)
	}
	if got := ids(scan("")); len(got) != 1 || got[0] != own {
		t.Fatalf("no launcher: %v", got)
	}
	if frame := scan("lch_missing"); frame.Code != "launcher_unavailable" {
		t.Fatalf("an unknown launcher: %+v", frame)
	}
	byID := c.sessionScanResult(protocol.Frame{ReqID: "r", LauncherID: "lch_work", ScanSessionID: own})
	if byID.Code != "session_not_found" {
		t.Fatalf("a session of the other home is not found through this launcher: %+v", byID)
	}
}

// An attribution scan returns only ids (the registered agent and the message numbers it sent), for the sessions of the asked folders and
// agent, and the hello advertises it.
func TestSessionScanAttributionReturnsOnlyIds(t *testing.T) {
	claude := t.TempDir() // a fixture Claude configuration folder; the real one is never read
	project := t.TempDir()
	dir := filepath.Join(claude, "projects", ccsessions.FolderName(project))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	line := func(v map[string]any) string { data, _ := json.Marshal(v); return string(data) }
	transcript := strings.Join([]string{
		line(map[string]any{"type": "user", "cwd": project, "sessionId": id, "message": map[string]any{"content": "PRIVATE prompt"}}),
		line(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-opus-4-5", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "mcp__aimebu__bus_register", "input": map[string]any{}}}}}),
		line(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_1", "content": `{"id":"sage@old"}`}}}}),
		line(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-opus-4-5", "content": []map[string]any{{"type": "tool_use", "id": "toolu_2", "name": "mcp__aimebu__bus_say", "input": map[string]any{"body": "PRIVATE body"}}}}}),
		line(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_2", "content": `{"id":37965,"room":"r"}`}}}}),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{ClaudeConfigDir: claude}
	reply := c.sessionScanResult(protocol.Frame{ReqID: "r", ScanFolders: []string{project}, ScanAttribution: true, ScanAgent: "sage@old", ScanSince: "2020-01-01T00:00:00Z"})
	if reply.Code != "" || len(reply.Attributions) != 1 || len(reply.Sessions) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
	got := reply.Attributions[0]
	if got.ID != id || got.Sender != "sage@old" || got.CWD != project || len(got.MessageNos) != 1 || got.MessageNos[0] != 37965 || got.Model != "claude-opus-4-5" {
		t.Fatalf("attribution = %+v", got)
	}
	if data, _ := json.Marshal(reply); strings.Contains(string(data), "PRIVATE") {
		t.Fatalf("transcript text left the supervisor: %s", data)
	}
	if other := c.sessionScanResult(protocol.Frame{ReqID: "r", ScanFolders: []string{project}, ScanAttribution: true, ScanAgent: "someone@else"}); len(other.Attributions) != 0 {
		t.Fatalf("another agent's attribution: %+v", other.Attributions)
	}
	if bad := c.sessionScanResult(protocol.Frame{ReqID: "r", ScanFolders: []string{project}, ScanAttribution: true, ScanSince: "yesterday"}); bad.Code != "invalid_request" {
		t.Fatalf("a bad since: %+v", bad)
	}
}
