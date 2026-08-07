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
	mux.HandleFunc("GET /api/usages/switcher", r.handleGet)
	mux.HandleFunc("POST /api/usages/switcher/settings", r.handleSettings)
	mux.HandleFunc("POST /api/usages/switcher/switch", r.handleSwitch)
	mux.HandleFunc("POST /api/usages/switcher/profiles", r.handleProfileCreate)
	mux.HandleFunc("DELETE /api/usages/switcher/profiles", r.handleProfileDelete)
}

// switcherGetResponse is the response shape for GET /api/usages/switcher.
type switcherGetResponse struct {
	Enabled     bool                   `json:"enabled"`
	Eligibility []switcher.Eligibility `json:"eligibility"`
	Profiles    []switcher.Profile     `json:"profiles"`
	// Snapshots holds per-profile usage data, keyed by "tool/name".
	// Never placed in Response.Snapshots so existing provider iteration paths
	// stay unchanged.
	Snapshots map[string]usages.Snapshot `json:"snapshots,omitempty"`
}

func (r switcherRoutes) handleGet(w http.ResponseWriter, req *http.Request) {
	enabled, err := r.sm.Enabled()
	if err != nil {
		writeSwitcherErr(w, http.StatusInternalServerError, err)
		return
	}
	eligibility, err := r.sm.Eligibility()
	if err != nil {
		writeSwitcherErr(w, http.StatusInternalServerError, err)
		return
	}
	if eligibility == nil {
		eligibility = []switcher.Eligibility{}
	}
	profiles, err := r.sm.List()
	if err != nil {
		writeSwitcherErr(w, http.StatusInternalServerError, err)
		return
	}
	if profiles == nil {
		profiles = []switcher.Profile{}
	}

	snapshots := make(map[string]usages.Snapshot, len(profiles))
	if r.um != nil {
		for _, p := range profiles {
			credPath := r.profileCredPath(p)
			snap, snapErr := r.um.FetchProfileSnapshot(req.Context(), p.Tool, p.Name, credPath)
			if snapErr == nil {
				snapshots[usages.ProfileSnapshotKey(p.Tool, p.Name)] = snap
			}
			// Snapshot errors are silently omitted — usage data is best-effort.
		}
	}

	resp := switcherGetResponse{
		Enabled:     enabled,
		Eligibility: eligibility,
		Profiles:    profiles,
		Snapshots:   snapshots,
	}
	writeSwitcherJSON(w, http.StatusOK, resp)
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

	active, err := r.sm.Switch(body.Tool, body.Profile)
	if err != nil {
		writeSwitcherErr(w, switcherHTTPStatus(err), err)
		return
	}

	// Invalidate cache for both sides so the next GET fetches fresh data.
	if r.um != nil {
		if outgoing != "" {
			r.um.InvalidateProfileSnapshot(body.Tool, outgoing)
		}
		if body.Profile != outgoing {
			r.um.InvalidateProfileSnapshot(body.Tool, body.Profile)
		}
	}
	writeSwitcherJSON(w, http.StatusOK, map[string]string{"active": active})
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
