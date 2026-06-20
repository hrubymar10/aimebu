package mcp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
)

// TestProcessLineNullByte is a regression test for the go-json v0.10.6 bug
// where strings with embedded null bytes cause an index-out-of-range panic
// in skipWhiteSpace. processLine must recover and return nil without crashing.
func TestProcessLineNullByte(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer stub.Close()
	c := &client.Client{BaseURL: stub.URL}
	var heartbeatID atomic.Pointer[string]

	// This exact string triggered the go-json panic found by FuzzHandleJSONRPC.
	badLine := "{\"\\x00:\\\"\"}"
	got := processLine(c, badLine, &heartbeatID)
	if got != nil {
		t.Fatalf("expected nil response for invalid input, got %+v", got)
	}
}

// FuzzHandleJSONRPC verifies that handle never panics for any JSON-RPC
// message shape. A minimal stub bus server answers every request with an
// empty-but-valid JSON object so the tool implementations don't fail on
// missing fields before we can observe the panic-safety invariant.
func FuzzHandleJSONRPC(f *testing.F) {
	// Seed: well-formed requests that exercise different code paths.
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bus_register","arguments":{"model":"gpt5","harness":"codex"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"bus_join","arguments":{"room":"general"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bus_say","arguments":{"room":"general","body":"hi"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"bus_wait","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"method":"unknown/method","params":{}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"bus_register","arguments":null}}`,
		`{}`,
		`{"jsonrpc":"2.0"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	// Stub server: echo a minimal valid JSON body for every request.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	f.Cleanup(stub.Close)

	c := &client.Client{BaseURL: stub.URL}
	var heartbeatID atomic.Pointer[string]

	f.Fuzz(func(t *testing.T, line string) {
		// go-json panics on some inputs (e.g. embedded null bytes in strings)
		// instead of returning an error — that is a library bug, not ours.
		// Isolate the unmarshal so we still catch panics from handle() itself.
		var req request
		var parseErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					parseErr = fmt.Errorf("go-json panic: %v", r)
				}
			}()
			parseErr = json.Unmarshal([]byte(line), &req)
		}()
		if parseErr != nil {
			return
		}

		// handle must never panic.
		resp := handle(c, req, &heartbeatID)

		// If a response is returned it must carry the JSON-RPC version string.
		if resp != nil && resp.JSONRPC != "2.0" {
			t.Fatalf("handle(%q) returned response with JSONRPC=%q, want 2.0", line, resp.JSONRPC)
		}
	})
}
