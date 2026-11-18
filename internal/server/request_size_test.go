package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readJSONResult calls a readJSON* wrapper as a handler would and returns what it answered.
func readJSONResult(t *testing.T, call func(w http.ResponseWriter, r *http.Request, out any) bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	var out map[string]any
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	call(w, r, &out)
	return w
}

func TestReadJSONTooLargeAnswers413(t *testing.T) {
	t.Parallel()
	body := `{"x":"` + strings.Repeat("a", 64<<10) + `"}`
	for _, tc := range []struct {
		name    string
		call    func(w http.ResponseWriter, r *http.Request, out any) bool
		message string
	}{
		{"generic", func(w http.ResponseWriter, r *http.Request, out any) bool { return readJSON(w, r, out) }, "request body exceeds 64 KB"},
		{"limit", func(w http.ResponseWriter, r *http.Request, out any) bool { return readJSONLimit(w, r, out, 2<<10) }, "request body exceeds 2 KB"},
		{"worded", func(w http.ResponseWriter, r *http.Request, out any) bool {
			return readJSONTooLarge(w, r, out, 2<<10, "Message is too long (max 2 KB)")
		}, "Message is too long (max 2 KB)"},
	} {
		response := readJSONResult(t, tc.call, body)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: status = %d, want 413", tc.name, response.Code)
		}
		var answer struct{ Error, Message string }
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
			t.Fatal(err)
		}
		if answer.Error != "request_too_large" || answer.Message != tc.message {
			t.Fatalf("%s: answer = %q %q, want request_too_large %q", tc.name, answer.Error, answer.Message, tc.message)
		}
	}
}

func TestReadJSONStillAnswers400ForBadJSON(t *testing.T) {
	t.Parallel()
	response := readJSONResult(t, func(w http.ResponseWriter, r *http.Request, out any) bool { return readJSON(w, r, out) }, `{"x":`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	var answer struct{ Error, Message string }
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if answer.Error != "invalid_request" || answer.Message != "invalid JSON" {
		t.Fatalf("answer = %q %q", answer.Error, answer.Message)
	}
}

func TestMessagePostAnswersClearTooLongErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	// A message at the body limit still posts.
	if resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": strings.Repeat("a", messageBodyLimit)}, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a message at the body limit: status = %d", resp.StatusCode)
	}
	// Over the body limit but inside the JSON limit: the body check refuses it by name, not as invalid JSON.
	over := strings.Repeat("a", messageBodyLimit+1)
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": over}, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a message over the body limit: status = %d, want 400", resp.StatusCode)
	}
	var answer struct{ Error, Message string }
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if answer.Error != "invalid_answer" || answer.Message != messageTooLarge() {
		t.Fatalf("answer = %q %q, want invalid_answer %q", answer.Error, answer.Message, messageTooLarge())
	}
	// Far over the JSON limit: 413 request_too_large, with the same message for the composer.
	huge := strings.Repeat("a", 128<<10)
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": huge}, true)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a message over the JSON limit: status = %d, want 413", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if answer.Error != "request_too_large" || answer.Message != messageTooLarge() {
		t.Fatalf("answer = %q %q, want request_too_large %q", answer.Error, answer.Message, messageTooLarge())
	}
}
