package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSendEchoesAddressedTo verifies the send response carries an explicit
// addressed_to array reflecting the server's single addressing decision (read
// back from the stored message, not recomputed). A normal mention echoes the
// resolved slug; a backticked mention — the documented escape — echoes an
// explicit empty array, never a missing key or null, so the sender can see that
// nobody was addressed rather than silently believing the mention landed.
func TestSendEchoesAddressedTo(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.registerAI("gpt5", "codex", "test", nil, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", bob.ID); err != nil {
		t.Fatal(err)
	}

	send := func(body string) []string {
		data, _ := json.Marshal(map[string]any{"from": alice.ID, "body": body})
		resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("send %q returned %d, want 200", body, resp.StatusCode)
		}
		var out struct {
			AddressedTo []string `json:"addressed_to"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.AddressedTo
	}

	// Normal mention resolves to the in-room slug.
	if got := send("@bob please review"); len(got) != 1 || got[0] != "bob" {
		t.Errorf("normal mention addressed_to = %v, want [bob]", got)
	}
	// Backticked mention is the documented escape — addressed_to must be an
	// explicit empty array, not nil (a missing key reads the same to a skimmer).
	if got := send("`@bob` please review"); got == nil || len(got) != 0 {
		t.Errorf("backticked mention addressed_to = %v, want non-nil empty []", got)
	}
	// No mention at all is also an explicit empty array.
	if got := send("just a status update"); got == nil || len(got) != 0 {
		t.Errorf("no-mention addressed_to = %v, want non-nil empty []", got)
	}
}

// TestDMEchoesAddressedTo verifies the DM path carries the same addressed_to
// echo. It routes through roomSendWithVisualPlan and shares the failure mode
// where a backticked mention silently addresses nobody.
func TestDMEchoesAddressedTo(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.registerAI("gpt5", "codex", "test", nil, "bob")
	if err != nil {
		t.Fatal(err)
	}

	dm := func(body string) []string {
		data, _ := json.Marshal(map[string]any{"from": alice.ID, "to": bob.ID, "body": body})
		resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("dm %q returned %d, want 200", body, resp.StatusCode)
		}
		var out struct {
			AddressedTo []string `json:"addressed_to"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.AddressedTo
	}

	// Normal mention in a DM still resolves and echoes.
	if got := dm("@bob please review"); len(got) != 1 || got[0] != "bob" {
		t.Errorf("DM normal mention addressed_to = %v, want [bob]", got)
	}
	// Backticked mention in a DM echoes an explicit empty array.
	if got := dm("`@bob` please review"); got == nil || len(got) != 0 {
		t.Errorf("DM backticked mention addressed_to = %v, want non-nil empty []", got)
	}
}
