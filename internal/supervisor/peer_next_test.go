package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A test whose RunOnce has returned must not wait out peerFrameLimit for a frame: next gives up as soon as RunOnce is done, but still
// returns a frame the supervisor sent before it returned.
func TestMachinePeerNextStopsWhenRunOnceReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := newMachinePeer(t, activeAck("agent"))
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", peer.server.URL, j, adapter.NewFake())
	done := make(chan error, 1)
	go func() { done <- peer.ran(c.RunOnce(ctx)) }()
	waitHello(t, peer)
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ping}
	if _, err := peer.wait(time.Minute, protocol.Pong); err != nil {
		t.Fatalf("a frame sent before RunOnce returned must still be found: %v", err)
	}
	cancel()
	<-done

	start := time.Now()
	_, err = peer.wait(time.Minute, "never_sent")
	if err == nil || !strings.Contains(err.Error(), "RunOnce returned") {
		t.Fatalf("wait after RunOnce returned = %v, want the RunOnce-returned error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait took %v after RunOnce returned", elapsed)
	}
}
