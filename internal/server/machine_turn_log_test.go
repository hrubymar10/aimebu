package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// lockedBuffer is a log destination the test can read while server goroutines write to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Every answered turn leaves one log line with the agent, the turn, how many messages it posted and why it was held, so a turn that ended
// without a message can be found in the server log. Not parallel: it swaps the process-wide log output.
func TestMachineAnswerLogsOneLinePerTurn(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	candidate := `{"messages":[]}`
	sum := sha256.Sum256([]byte(candidate))
	digest := hex.EncodeToString(sum[:])
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0, Digest: digest, Candidate: candidate})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, reply, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var receipt protocol.Frame
	if err := json.Unmarshal(reply, &receipt); err != nil || receipt.Type != protocol.Receipt || receipt.Result != "committed" {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	want := "turn answered: agent=" + agent.ID + " turn=" + turn.ID + " mode=normal result=committed messages=0 held_reason=none"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Fatalf("want one %q line, got %d in:\n%s", want, got, logs.String())
	}
}
