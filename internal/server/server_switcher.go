package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// switcherRoutes mounts the /api/usages/switcher route group. The switcher
// manager is the pure filesystem library; the usages manager provides
// per-profile usage snapshot caching. HTTP handlers are thin wrappers with no
// logic beyond request parsing, error mapping, and response serialisation.
type switcherRoutes struct {
	sm *switcher.Manager
	um *usages.Manager
}

func (r switcherRoutes) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/usages/switcher/settings", r.handleSettings)
	mux.HandleFunc("POST /api/usages/switcher/switch", r.handleSwitch)
	mux.HandleFunc("POST /api/usages/switcher/profiles", r.handleProfileCreate)
	mux.HandleFunc("DELETE /api/usages/switcher/profiles", r.handleProfileDelete)
}

func (r switcherRoutes) handleSettings(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeSwitcherErr(w, http.StatusBadRequest, errors.New("invalid JSON: "+err.Error()))
		return
	}
	if err := r.sm.SetEnabled(body.Enabled); err != nil {
		writeSwitcherErr(w, http.StatusInternalServerError, err)
		return
	}
	enabled, err := r.sm.Enabled()
	if err != nil {
		writeSwitcherErr(w, http.StatusInternalServerError, err)
		return
	}
	writeSwitcherJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

func (r switcherRoutes) handleSwitch(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Tool    string `json:"tool"`
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeSwitcherErr(w, http.StatusBadRequest, errors.New("invalid JSON: "+err.Error()))
		return
	}

	// Record the outgoing active profile so we can invalidate both sides of the
	// switch in the per-profile cache.
	outgoing, _ := r.sm.Active(body.Tool)

	active, switched, err := r.sm.Switch(body.Tool, body.Profile)
	if err != nil {
		writeSwitcherErr(w, switcherHTTPStatus(err), err)
		return
	}

	// Invalidate cache for both sides so the next GET fetches fresh data. A
	// no-op switch (already on that profile) changes nothing, so skip it.
	if switched && r.um != nil {
		if outgoing != "" {
			r.um.InvalidateProfileSnapshot(body.Tool, outgoing)
		}
		if body.Profile != outgoing {
			r.um.InvalidateProfileSnapshot(body.Tool, body.Profile)
		}
	}
	writeSwitcherJSON(w, http.StatusOK, map[string]any{"active": active, "switched": switched})
}

func (r switcherRoutes) handleProfileCreate(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Tool    string `json:"tool"`
		Profile string `json:"profile"`
		Mode    string `json:"mode"` // "add" or "import"
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeSwitcherErr(w, http.StatusBadRequest, errors.New("invalid JSON: "+err.Error()))
		return
	}
	var opErr error
	switch body.Mode {
	case "add":
		opErr = r.sm.Add(body.Tool, body.Profile)
	case "import":
		opErr = r.sm.Import(body.Tool, body.Profile)
	default:
		writeSwitcherErr(w, http.StatusBadRequest, errors.New(`mode must be "add" or "import"`))
		return
	}
	if opErr != nil {
		writeSwitcherErr(w, switcherHTTPStatus(opErr), opErr)
		return
	}
	writeSwitcherJSON(w, http.StatusOK, map[string]string{"profile": body.Profile, "mode": body.Mode})
}

func (r switcherRoutes) handleProfileDelete(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Tool    string `json:"tool"`
		Profile string `json:"profile"`
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeSwitcherErr(w, http.StatusBadRequest, errors.New("invalid JSON: "+err.Error()))
		return
	}
	if err := r.sm.Remove(body.Tool, body.Profile, body.Force); err != nil {
		writeSwitcherErr(w, switcherHTTPStatus(err), err)
		return
	}
	if r.um != nil {
		r.um.InvalidateProfileSnapshot(body.Tool, body.Profile)
	}
	writeSwitcherJSON(w, http.StatusOK, map[string]string{"deleted": body.Profile})
}

// profileCredPath returns the credential file path for profile p. The active
// profile reads from the live harness directory; non-active profiles read from
// their stored copy under the switcher root.
func (r switcherRoutes) profileCredPath(p switcher.Profile) string {
	if p.Active {
		// Live credential path — harness may have refreshed tokens since capture.
		h, err := r.sm.ResolveHome()
		if err != nil {
			return r.sm.StoredCredPath(p.Tool, p.Name)
		}
		switch p.Tool {
		case switcher.ToolClaude:
			return usages.ClaudeLiveCredPath(h)
		case switcher.ToolCodex:
			if env := codexHome(); env != "" {
				return env + "/auth.json"
			}
			return filepath.Join(h, ".codex", "auth.json")
		}
	}
	return r.sm.StoredCredPath(p.Tool, p.Name)
}

// switcherHTTPStatus maps switcher sentinel errors to HTTP status codes.
func switcherHTTPStatus(err error) int {
	switch {
	case errors.Is(err, switcher.ErrProfileNotFound):
		return http.StatusNotFound
	case errors.Is(err, switcher.ErrProfileExists):
		return http.StatusConflict
	case errors.Is(err, switcher.ErrActiveProfile):
		// 409 so the UI can ask for confirmation before retrying with force=true.
		return http.StatusConflict
	case errors.Is(err, switcher.ErrDisabled),
		errors.Is(err, switcher.ErrNotEligible),
		errors.Is(err, switcher.ErrNoActiveProfile),
		errors.Is(err, switcher.ErrInvalidCredentials),
		errors.Is(err, switcher.ErrInvalidName):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeSwitcherErr(w http.ResponseWriter, status int, err error) {
	// Never reflect credential material. The switcher library contract ensures
	// error text names what failed, never what was in the file. We return the
	// full error chain, but it's safe because no error in the library carries
	// credential bytes.
	msg := err.Error()
	writeSwitcherJSON(w, status, map[string]string{"error": msg})
}

func writeSwitcherJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// codexHome returns the CODEX_HOME env var (trimmed), or "" if unset.
func codexHome() string {
	return strings.TrimSpace(os.Getenv("CODEX_HOME"))
}
