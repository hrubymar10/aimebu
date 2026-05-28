package usages

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoWithRetryReturnsFirstSuccessWithoutRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doWithRetry(context.Background(), server.Client(), req, testRetryPolicy(nil))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestDoWithRetryRetriesTransientStatusThenSucceeds(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var delays []time.Duration
	resp, err := doWithRetry(context.Background(), server.Client(), req, testRetryPolicy(&delays))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(delays) != 1 || delays[0] != time.Second {
		t.Fatalf("delays = %v, want [1s]", delays)
	}
}

func TestDoWithRetryReturnsLastResponseAfterRetryExhausted(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "still unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doWithRetry(context.Background(), server.Client(), req, testRetryPolicy(nil))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "still unavailable") {
		t.Fatalf("body = %q", body)
	}
}

func TestDoWithRetryHonorsRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var delays []time.Duration
	resp, err := doWithRetry(context.Background(), server.Client(), req, testRetryPolicy(&delays))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if len(delays) != 1 || delays[0] != 2*time.Second {
		t.Fatalf("delays = %v, want [2s]", delays)
	}
}

func TestDoWithRetrySkipsNonIdempotentMethod(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "do not retry", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doWithRetry(context.Background(), server.Client(), req, testRetryPolicy(nil))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestDoWithRetryRetriesExplicitReplayablePost(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		if string(body) != "refresh=token" {
			t.Fatalf("body = %q", body)
		}
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "try again", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("refresh=token"))
	if err != nil {
		t.Fatal(err)
	}
	var delays []time.Duration
	policy := testRetryPolicy(&delays)
	policy.RetryableMethods = []string{http.MethodPost}
	resp, err := doWithRetry(context.Background(), server.Client(), req, policy)
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(delays) != 1 || delays[0] != 0 {
		t.Fatalf("delays = %v, want [0s]", delays)
	}
}

func TestDoWithRetryReturnsContextErrorDuringBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "retry", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := testRetryPolicy(nil)
	policy.sleep = func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}
	resp, err := doWithRetry(ctx, server.Client(), req, policy)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %v, want nil", resp.Status)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDoWithRetryRetriesTransientTransportError(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.DNSError{Err: "temporary lookup failure", Name: req.URL.Host}
		}
		return httpJSON(http.StatusOK, `ok`), nil
	})}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doWithRetry(context.Background(), client, req, testRetryPolicy(nil))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDoWithRetryRetriesRequestTimeout(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, context.DeadlineExceeded
		}
		return httpJSON(http.StatusOK, `ok`), nil
	})}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doWithRetry(context.Background(), client, req, testRetryPolicy(nil))
	if err != nil {
		t.Fatalf("doWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func testRetryPolicy(delays *[]time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxRetries: 1,
		sleep: func(ctx context.Context, delay time.Duration) error {
			if delays != nil {
				*delays = append(*delays, delay)
			}
			return ctx.Err()
		},
	}
}
