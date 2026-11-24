package server

import (
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// keyBody are the base64 lines of a made-up PEM private key: no line matches any of adapter.RedactSecrets's patterns on its own.
var keyBody = []string{
	"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj",
	"MzEfYyjiWA4R4M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu",
	"NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ",
}

// pemKey is a key as a tool printed it into a worker's conversation log: each line a continuation line of the record (a tab first).
func pemKey(kind string) string {
	return "\t-----BEGIN " + kind + "PRIVATE KEY-----\n\t" + strings.Join(keyBody, "\n\t") + "\n\t-----END " + kind + "PRIVATE KEY-----\n"
}

func assertNoKeyBody(t *testing.T, what, text string) {
	t.Helper()
	for _, line := range keyBody {
		if strings.Contains(text, line[:24]) {
			t.Fatalf("%s: a line of the key's body is in the text:\n%s", what, text)
		}
	}
	if strings.Contains(text, "PRIVATE KEY-----") {
		t.Fatalf("%s: a BEGIN or END line is in the text:\n%s", what, text)
	}
}

// The server's second pass follows a private key across lines, as the machine's log writer does: the key's body never comes through,
// the text around it does, an unterminated key masks to the end, two keys are both masked, and a text that begins inside a key (its BEGIN
// line before the page) loses the rest of that key.
func TestRedactOutputDropsAPrivateKeyAcrossLines(t *testing.T) {
	for name, item := range map[string]struct {
		text      string
		kept, out []string
	}{
		"one key": {
			text: "2026-10-22T12:00:03Z tool_result Bash\n\tcat id_rsa\n" + pemKey("RSA ") + "2026-10-22T12:00:04Z assistant the key is above\n",
			kept: []string{"\tcat id_rsa\n", "\t[redacted private key]\n", "2026-10-22T12:00:04Z assistant the key is above\n"},
		},
		"unterminated": {
			text: "before the key\n\t-----BEGIN PRIVATE KEY-----\n\t" + strings.Join(keyBody, "\n\t") + "\n2026-10-22T12:00:04Z assistant never ended\n",
			kept: []string{"before the key\n", "\t[redacted private key]\n"},
			out:  []string{"never ended"},
		},
		"two keys": {
			text: "first\n" + pemKey("") + "between\n" + pemKey("OPENSSH ") + "after\n",
			kept: []string{"first\n", "between\n", "after\n"},
		},
		"begins inside a key": {
			text: "\t" + strings.Join(keyBody[1:], "\n\t") + "\n\t-----END RSA PRIVATE KEY-----\n2026-10-22T12:00:04Z assistant after the key\n",
			kept: []string{"\t[redacted private key]\n", "2026-10-22T12:00:04Z assistant after the key\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := redactOutput(item.text)
			assertNoKeyBody(t, name, got)
			for _, want := range item.kept {
				if !strings.Contains(got, want) {
					t.Fatalf("%q was lost:\n%s", want, got)
				}
			}
			for _, gone := range item.out {
				if strings.Contains(got, gone) {
					t.Fatalf("%q follows an unterminated key and came through:\n%s", gone, got)
				}
			}
		})
	}
	if got := redactOutput("\tdGhpcyBpcyBqdXN0IGJhc2U2NA==\nplain\n"); got != "\tdGhpcyBpcyBqdXN0IGJhc2U2NA==\nplain\n" {
		t.Fatalf("base64 that no END follows was dropped: %q", got)
	}
}

// keyLog answers the output fixture's log reads with one turn whose tool printed a private key, the text around it, and filler after it
// long enough that the notice's 2 KiB tail of the raw text would begin inside the key's body.
func keyLog(t *testing.T, o *outputFixture, turnID string) {
	t.Helper()
	o.conversationTurn(t, turnID, o.room.ID)
	text := "2026-10-22T12:00:01Z turn_start turn " + turnID + " gen 0 normal claude/native test\n" +
		"2026-10-22T12:00:01Z tool_result Bash\n\tcat id_rsa\n\t-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("\t"+strings.Join(keyBody, "\n\t")+"\n", 12) + "\t-----END RSA PRIVATE KEY-----\n" +
		"2026-10-22T12:00:02Z assistant the key is above\n"
	// Cut before masking, the notice's tail would begin inside the key: key lines and its END, but not its BEGIN.
	if raw := text[len(text)-noticeOutputBytes:]; strings.Contains(raw, "BEGIN") || !strings.Contains(raw, keyBody[1][:24]) {
		t.Fatalf("the fixture's 2 KiB tail does not start inside the key: %q", raw[:80])
	}
	o.mu.Lock()
	o.answer = func(request protocol.Frame) protocol.Frame {
		page, _ := json.Marshal(agentlog.Page{Text: text, Bytes: int64(len(text)), Files: 1, UpdatedAt: time.Date(2026, 10, 22, 12, 0, 5, 0, time.UTC)})
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: request.ReqID, AgentID: request.AgentID, Details: page}
	}
	o.mu.Unlock()
}

// The end of a worker's turn a notice carries never holds a private key's body, also when the 2 KiB cut falls inside the key: the text is
// masked before it is cut.
func TestNoticeOutputDropsAPrivateKeyAcrossLines(t *testing.T) {
	o := newOutputFixture(t)
	lead, _ := o.createToolAgentNamed(t, "role_leader_default", "outputlead")
	keyLog(t, o, "trn_key")
	delivery := protocol.Delivery{AgentID: lead.ID, TurnID: "trn_lead_key", Notes: []protocol.DeliveryNote{{NoteID: "note_key", OutputSource: &protocol.OutputSource{AgentID: o.agent.ID, TurnID: "trn_key"}}}}
	o.srv.addWorkerOutput(t.Context(), &delivery)
	out := delivery.Notes[0].WorkerOutput
	if out == nil || out.Withheld != "" {
		t.Fatalf("notice: %+v", out)
	}
	assertNoKeyBody(t, "notice", out.Text)
	for _, want := range []string{"\tcat id_rsa\n", "\t[redacted private key]\n", "assistant the key is above\n"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("notice lost %q:\n%s", want, out.Text)
		}
	}
}

// agent_output, the lead's own read of the same log, masks the key the same way.
func TestAgentOutputDropsAPrivateKeyAcrossLines(t *testing.T) {
	o := newOutputFixture(t)
	_, leadKey := o.createToolAgentNamed(t, "role_leader_default", "outputlead")
	keyLog(t, o, "trn_key")
	status, out := readAgentOutput(t, o, leadKey, "/api/agent/agents/"+o.agent.ID+"/output")
	if status != 200 {
		t.Fatalf("agent_output: %d %+v", status, out)
	}
	assertNoKeyBody(t, "agent_output", out.Text)
	for _, want := range []string{"\tcat id_rsa\n", "\t[redacted private key]\n", "assistant the key is above\n"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("agent_output lost %q:\n%s", want, out.Text)
		}
	}
}
