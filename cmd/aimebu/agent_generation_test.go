package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

type fixedGenerationDetector struct {
	observation generationObservation
}

func (d fixedGenerationDetector) Observe([]byte, time.Time) generationObservation {
	return d.observation
}

type generationFixtureLine struct {
	At    time.Time       `json:"at"`
	Event json.RawMessage `json:"event"`
}

func generationFixture(t *testing.T, harness, name string) []int64 {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	det := newGenerationDetector(harness, "")
	var got []int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line generationFixtureLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		observation := det.Observe(line.Event, line.At)
		if observation.GenerationMS > 0 {
			got = append(got, observation.GenerationMS)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPiGenerationDetectorCapturedFixture(t *testing.T) {
	got := generationFixture(t, "pi", "generation-pi-sanitized.jsonl")
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	wantSeconds := []int64{6, 18, 50, 66, 72, 91, 109, 217, 248, 266, 308, 309, 327}
	want := make([]int64, len(wantSeconds))
	for i, seconds := range wantSeconds {
		want[i] = seconds * 1000
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generation intervals = %v, want %v", got, want)
	}
}

func TestCodexGenerationDetectorCapturedFixture(t *testing.T) {
	got := generationFixture(t, "codex", "generation-codex-sanitized.jsonl")
	if want := []int64{39_000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generation intervals = %v, want %v", got, want)
	}
}

func TestClaudeGenerationDetectorCapturedFixture(t *testing.T) {
	got := generationFixture(t, "claude-code", "generation-claude-sanitized.jsonl")
	if want := []int64{2_000, 2_000, 4_000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generation intervals = %v, want %v", got, want)
	}
}

func TestClaudeGenerationStartsAtToolResult(t *testing.T) {
	det := newGenerationDetector("claude-code", "session-a")
	base := time.Unix(100, 0)
	det.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`), base)
	det.Observe([]byte(`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`), base.Add(2*time.Minute))
	got := det.Observe([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`), base.Add(2*time.Minute+4*time.Second))
	if got.GenerationMS != 4_000 {
		t.Fatalf("generation_ms = %d, want 4000", got.GenerationMS)
	}
}

func TestUnsupportedHarnessHasNoGenerationDetector(t *testing.T) {
	if got := newGenerationDetector("vibe", "session-a"); got != nil {
		t.Fatalf("detector = %#v, want nil", got)
	}
}

func TestAgentGenerationPusherBuffersUntilRegistration(t *testing.T) {
	received := make(chan generationSample, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/worker@aimebu/generation" {
			t.Errorf("path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var sample generationSample
		if err := json.NewDecoder(r.Body).Decode(&sample); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- sample
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	oldRetry := agentGenerationRetryInterval
	agentGenerationRetryInterval = 10 * time.Millisecond
	defer func() { agentGenerationRetryInterval = oldRetry }()

	agentID := newAgentIDProvider("")
	writer := startAgentGenerationPusher(context.Background(), srv.URL, agentID, fixedGenerationDetector{
		observation: generationObservation{GenerationMS: 4_000, SessionID: "thread-a", Complete: true},
	})
	defer writer.Close()
	if _, err := writer.Write([]byte("event\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case sample := <-received:
		t.Fatalf("sample pushed before registration: %#v", sample)
	case <-time.After(30 * time.Millisecond):
	}

	agentID.Set("worker@aimebu")
	select {
	case sample := <-received:
		if sample.GenerationMS != 4_000 || sample.SessionID != "thread-a" {
			t.Fatalf("sample = %#v", sample)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered generation sample")
	}
}
