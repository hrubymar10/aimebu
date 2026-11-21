package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
)

// agentToolMCP exposes the same narrow toolbox over streamable HTTP. The
// authenticated request is dispatched to the existing agent API in-process.
func (s *Server) agentToolMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request", "MCP request too large")
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	toolbox := agenttools.Server{URL: "http://aimebu.local", Key: key,
		Client: &http.Client{Transport: localAgentTransport{handler: s.localHandler(), remoteAddr: r.RemoteAddr}}}
	response, notification := toolbox.Process(r.Context(), raw)
	if notification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// localHandler is the handler the agent MCP endpoint dispatches its tool calls to. It is built once per Server: building it prepares the whole web UI
// (reads and hashes the embedded files, gzips the page), which is far too much work to repeat for every MCP request.
func (s *Server) localHandler() http.Handler {
	s.localOnce.Do(func() { s.localBuilds.Add(1); s.localMux = s.Handler() })
	return s.localMux
}

type localAgentTransport struct {
	handler    http.Handler
	remoteAddr string
}

func (t localAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := &localAgentRecorder{header: make(http.Header)}
	req.RemoteAddr = t.remoteAddr
	t.handler.ServeHTTP(recorder, req)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return &http.Response{StatusCode: recorder.status, Header: recorder.header,
		Body: io.NopCloser(bytes.NewReader(recorder.body.Bytes())), Request: req}, nil
}

type localAgentRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *localAgentRecorder) Header() http.Header    { return r.header }
func (r *localAgentRecorder) WriteHeader(status int) { r.status = status }
func (r *localAgentRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}
