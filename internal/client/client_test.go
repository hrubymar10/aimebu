package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeleteAgentUsesDeleteEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL}
	if err := c.DeleteAgent("worker@aimebu", time.Second); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("DeleteAgent used method %q, want %q", gotMethod, http.MethodDelete)
	}
	if gotPath != "/agents/worker@aimebu" {
		t.Fatalf("DeleteAgent used path %q, want %q", gotPath, "/agents/worker@aimebu")
	}
}
