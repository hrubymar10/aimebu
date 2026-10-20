package supervisor

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func TestAnswerFrameLimitKeepsDigestAndSendsReceiptMarker(t *testing.T) {
	base := protocol.Frame{V: protocol.Version, Type: protocol.Answer, Digest: strings.Repeat("a", 64)}
	for _, test := range []struct {
		name, candidate string
		marked          bool
	}{
		{"at answer limit", strings.Repeat("x", answer.MaxAnswerBytes), false},
		{"over answer limit", strings.Repeat("x", answer.MaxAnswerBytes+1), true},
		{"escaped frame over limit", strings.Repeat("\\", answer.MaxAnswerBytes), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := base
			frame.Candidate = test.candidate
			encoded, err := encodeMachineFrame(frame)
			if err != nil || len(encoded) > protocol.MaxMachineFrameBytes {
				t.Fatalf("encoded=%d err=%v", len(encoded), err)
			}
			var got protocol.Frame
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got.AnswerTooLarge != test.marked || got.Digest != base.Digest {
				t.Fatalf("frame=%+v", got)
			}
			if test.marked && got.Candidate != "" {
				t.Fatal("oversize candidate crossed machine frame")
			}
			if !test.marked && got.Candidate != test.candidate {
				t.Fatal("candidate changed at limit")
			}
		})
	}
}
