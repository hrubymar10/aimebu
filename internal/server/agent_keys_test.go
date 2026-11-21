package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// hello sends a hello that says whether the supervisor takes agent keys and which it holds, and returns the ack.
func (f *fixture) helloKeys(t *testing.T, takes bool, held []string) protocol.Frame {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-keys", SupportsStopReplay: true, SupportsAgentKeys: takes, HeldKeys: held})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := json.Unmarshal(raw, &ack); err != nil || ack.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v %v", ack, err)
	}
	return ack
}

// A supervisor that restarted holds no key: its hello_ack gives it a new one for each agent it should be running, and the key the old process
// held stops authenticating. One that holds the key, one that does not take keys, and a stopped agent get nothing.
func TestHelloAckGrantsKeysToARestartedSupervisorAndRevokesTheOldOnes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, _, _, _ := f.execution(t)
	ctx := context.Background()
	oldKey, err := f.srv.Auth.IssueAgentKey(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Auth.VerifyAgentKey(ctx, agent.ID, oldKey); err != nil {
		t.Fatalf("the issued key does not verify: %v", err)
	}
	if ack := f.helloKeys(t, true, []string{agent.ID}); len(ack.AgentKeys) != 0 {
		t.Fatalf("a supervisor that holds the key was granted another: %+v", ack.AgentKeys)
	}
	if err := f.srv.Auth.VerifyAgentKey(ctx, agent.ID, oldKey); err != nil {
		t.Fatalf("the key was revoked although the supervisor holds it: %v", err)
	}
	if ack := f.helloKeys(t, false, nil); len(ack.AgentKeys) != 0 {
		t.Fatalf("a supervisor that does not take keys was sent one: %+v", ack.AgentKeys)
	}
	ack := f.helloKeys(t, true, nil)
	if len(ack.AgentKeys) != 1 || ack.AgentKeys[0].AgentID != agent.ID || !strings.HasPrefix(ack.AgentKeys[0].Key, "aak_") || ack.AgentKeys[0].Key == oldKey {
		t.Fatalf("grants=%+v", ack.AgentKeys)
	}
	if err := f.srv.Auth.VerifyAgentKey(ctx, agent.ID, oldKey); err == nil {
		t.Fatal("the key the restarted supervisor lost still authenticates")
	}
	if err := f.srv.Auth.VerifyAgentKey(ctx, agent.ID, ack.AgentKeys[0].Key); err != nil {
		t.Fatalf("the granted key does not authenticate: %v", err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ack := f.helloKeys(t, true, nil); len(ack.AgentKeys) != 0 {
		t.Fatalf("a stopped agent was granted a key: %+v", ack.AgentKeys)
	}
}

// An agent whose spawn key is waiting for the supervisor's answer is not given another by a reconnect: the second key would replace the first, and the agent
// would keep the one it was sent until it restarted.
func TestHelloAckLeavesAnAgentWithASpawnKeyInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, _, _, _ := f.execution(t)
	ctx := context.Background()
	spawnKey, err := f.srv.Auth.IssueAgentKey(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	cacheID := fmt.Sprintf("%s/%d", agent.ID, 0) // the fixture agent is generation 0
	f.srv.spawnMu.Lock()
	f.srv.spawnKeys[cacheID] = spawnKey
	f.srv.spawnMu.Unlock()
	if ack := f.helloKeys(t, true, nil); len(ack.AgentKeys) != 0 {
		t.Fatalf("a reconnect replaced the key of a spawn in flight: %+v", ack.AgentKeys)
	}
	if err := f.srv.Auth.VerifyAgentKey(ctx, agent.ID, spawnKey); err != nil {
		t.Fatalf("the spawn's key was revoked: %v", err)
	}
	f.srv.spawnMu.Lock()
	delete(f.srv.spawnKeys, cacheID)
	f.srv.spawnMu.Unlock()
	if ack := f.helloKeys(t, true, nil); len(ack.AgentKeys) != 1 {
		t.Fatalf("once the spawn is answered the reconnect grants a key: %+v", ack.AgentKeys)
	}
}
