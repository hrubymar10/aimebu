package server

import (
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
)

// The pprof endpoints are a debugging aid for the person running the server:
// the standard net/http/pprof handlers (stdlib only) under /api/debug/pprof/.
// They are off by default and turned on with AIMEBU_DEBUG_PPROF=1; when they
// are off the routes are not registered at all, so a request falls through to
// the API's not-found answer and nothing about them leaks. An owner or admin
// session is required even when they are on: a profile shows the server's own
// activity, which is the owner's reach.

// debugPprofEnv turns the pprof endpoints on: set to 1.
const debugPprofEnv = "AIMEBU_DEBUG_PPROF"

// debugPprofEnabled says whether the pprof endpoints are served.
func debugPprofEnabled() bool {
	return os.Getenv(debugPprofEnv) == "1"
}

// registerDebugPprof adds the pprof routes to the mux. Called only while the
// flag is on, so the handlers do not exist when it is off.
func (s *Server) registerDebugPprof(mux *http.ServeMux) {
	gate := func(next http.Handler) http.Handler {
		return s.Auth.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.adminMachine(w, r) {
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
	mux.Handle("GET /api/debug/pprof/", gate(http.HandlerFunc(s.debugPprofIndex)))
	mux.Handle("GET /api/debug/pprof/cmdline", gate(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle("GET /api/debug/pprof/profile", gate(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /api/debug/pprof/symbol", gate(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("POST /api/debug/pprof/symbol", gate(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("GET /api/debug/pprof/trace", gate(http.HandlerFunc(pprof.Trace)))
}

// debugPprofIndex runs pprof.Index at this mount. The package's handlers are
// written for /debug/pprof/, so the path is rewritten to the shape they expect
// before it is answered; the index page links profiles by name, so it works
// unchanged under /api/debug/pprof/.
func (s *Server) debugPprofIndex(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "/debug/pprof/" + strings.TrimPrefix(r.URL.Path, "/api/debug/pprof/")
	pprof.Index(w, r)
}
