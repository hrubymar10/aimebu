// Package doctor runs local and remote health checks for an aimebu
// installation and prints a human-readable report.
package doctor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// expectedSchemaVersion mirrors the server's sqliteSchemaVersion constant.
const expectedSchemaVersion = 1

// Result holds the outcome of a single health check.
type Result struct {
	Name    string
	Status  Status
	Message string
}

// Status is the outcome of a health check.
type Status int

const (
	StatusOK   Status = iota
	StatusWarn        // non-fatal, but worth noting
	StatusFail        // definite problem
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK  "
	case StatusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Run executes all checks and returns the results. baseURL is the aimebu
// server URL (e.g. "http://localhost:9997"). rootDir is the config root
// (from config.Root()).
func Run(baseURL, rootDir string) []Result {
	var results []Result

	results = append(results, checkServerReachable(baseURL))
	results = append(results, checkConfigDir(rootDir))
	results = append(results, checkSQLite(rootDir))
	results = append(results, checkSchemaVersion(rootDir))
	results = append(results, checkDataDirs(rootDir))
	results = append(results, checkTLSEnv())
	results = append(results, checkAllowlist())
	results = append(results, checkAgentLiveness(baseURL))
	results = append(results, checkSwitcher(rootDir)...)

	return results
}

// Print writes a formatted report to w and returns the worst Status seen.
func Print(w io.Writer, results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		if r.Status > worst {
			worst = r.Status
		}
		fmt.Fprintf(w, "[%s] %-30s %s\n", r.Status, r.Name, r.Message)
	}

	fmt.Fprintln(w)
	switch worst {
	case StatusOK:
		fmt.Fprintln(w, "All checks passed.")
	case StatusWarn:
		fmt.Fprintln(w, "Some warnings — see above.")
	default:
		fmt.Fprintln(w, "Some checks failed — see above.")
	}
	return worst
}

func checkServerReachable(baseURL string) Result {
	name := "server reachable"
	url := strings.TrimRight(baseURL, "/") + "/health"
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(url) //nolint:noctx
	if err != nil {
		return Result{name, StatusFail, fmt.Sprintf("GET %s: %v", url, err)}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{name, StatusFail, fmt.Sprintf("GET %s: HTTP %d", url, resp.StatusCode)}
	}
	return Result{name, StatusOK, fmt.Sprintf("HTTP 200 at %s", url)}
}

func checkConfigDir(rootDir string) Result {
	name := "config dir"
	info, err := os.Stat(rootDir)
	if err != nil {
		return Result{name, StatusFail, fmt.Sprintf("%s: %v", rootDir, err)}
	}
	if !info.IsDir() {
		return Result{name, StatusFail, fmt.Sprintf("%s: not a directory", rootDir)}
	}
	probe := filepath.Join(rootDir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return Result{name, StatusFail, fmt.Sprintf("%s: not writable: %v", rootDir, err)}
	}
	os.Remove(probe)
	return Result{name, StatusOK, rootDir}
}

func checkSQLite(rootDir string) Result {
	name := "sqlite db"
	dbPath := filepath.Join(rootDir, "server", "aimebu.sqlite")
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Not existing is fine if the server has never been started yet.
			return Result{name, StatusWarn, fmt.Sprintf("%s: not found (server not yet started?)", dbPath)}
		}
		return Result{name, StatusFail, fmt.Sprintf("%s: %v", dbPath, err)}
	}
	if info.Size() == 0 {
		return Result{name, StatusWarn, fmt.Sprintf("%s: exists but is empty", dbPath)}
	}
	return Result{name, StatusOK, fmt.Sprintf("%s (%.1f KB)", dbPath, float64(info.Size())/1024)}
}

func checkDataDirs(rootDir string) Result {
	name := "data dirs"
	dirs := []string{
		filepath.Join(rootDir, "server"),
		filepath.Join(rootDir, "agents"),
	}
	var missing []string
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return Result{name, StatusWarn, fmt.Sprintf("not yet created: %s (server not yet started?)", strings.Join(missing, ", "))}
	}
	return Result{name, StatusOK, fmt.Sprintf("%s/{server,agents}", rootDir)}
}

func checkSchemaVersion(rootDir string) Result {
	name := "schema version"
	dbPath := filepath.Join(rootDir, "server", "aimebu.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return Result{name, StatusWarn, "database not found (server not yet started?)"}
		}
		return Result{name, StatusFail, fmt.Sprintf("stat %s: %v", dbPath, err)}
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return Result{name, StatusFail, fmt.Sprintf("open %s: %v", dbPath, err)}
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_meta ORDER BY rowid DESC LIMIT 1`).Scan(&version); err != nil {
		return Result{name, StatusFail, fmt.Sprintf("query schema_meta: %v", err)}
	}
	if version != expectedSchemaVersion {
		return Result{name, StatusWarn, fmt.Sprintf("schema version %d; binary expects %d", version, expectedSchemaVersion)}
	}
	return Result{name, StatusOK, fmt.Sprintf("version %d", version)}
}

func checkAllowlist() Result {
	name := "allowlist (AIMEBU_ALLOW)"
	raw := os.Getenv("AIMEBU_ALLOW")
	if raw == "" {
		raw = "127.0.0.0/8,::1/128"
	}
	parts := strings.Split(raw, ",")
	var cidrs []string
	for _, p := range parts {
		entry := strings.TrimSpace(p)
		if entry == "" {
			return Result{name, StatusFail, fmt.Sprintf("empty entry — stray comma in %q", raw)}
		}
		var pref netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return Result{name, StatusFail, fmt.Sprintf("%q: %v", entry, err)}
			}
			pref = parsed.Masked()
		} else {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return Result{name, StatusFail, fmt.Sprintf("%q: not an IP or CIDR: %v", entry, err)}
			}
			bits := 32
			if addr.Unmap().Is6() {
				bits = 128
			}
			pref = netip.PrefixFrom(addr.Unmap(), bits)
		}
		cidrs = append(cidrs, pref.String())
	}
	return Result{name, StatusOK, strings.Join(cidrs, ", ")}
}

type agentEntry struct {
	Status string `json:"status"`
}

type agentListPayload struct {
	Agents []agentEntry `json:"agents"`
}

func checkAgentLiveness(baseURL string) Result {
	name := "agent liveness"
	url := strings.TrimRight(baseURL, "/") + "/agents"
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(url) //nolint:noctx
	if err != nil {
		return Result{name, StatusWarn, fmt.Sprintf("GET %s: %v (server down?)", url, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{name, StatusWarn, fmt.Sprintf("GET %s: HTTP %d", url, resp.StatusCode)}
	}
	var payload agentListPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Result{name, StatusWarn, fmt.Sprintf("decode: %v", err)}
	}
	var stale, offline int
	for _, a := range payload.Agents {
		switch a.Status {
		case "stale":
			stale++
		case "offline":
			offline++
		}
	}
	total := len(payload.Agents)
	if offline > 0 {
		return Result{name, StatusWarn, fmt.Sprintf("%d total, %d stale, %d offline", total, stale, offline)}
	}
	return Result{name, StatusOK, fmt.Sprintf("%d registered (%d stale)", total, stale)}
}

// switcherState mirrors the relevant fields from switcher/state.json without
// importing the switcher package.
type switcherState struct {
	Version int               `json:"version"`
	Enabled bool              `json:"enabled"`
	Active  map[string]string `json:"active"`
}

// switcherCredFilename returns the known credential filename for a tool.
// These must match the switcher package's profileCredFilename constants.
func switcherCredFilename(tool string) string {
	switch tool {
	case "claude":
		return ".credentials.json"
	case "codex":
		return "auth.json"
	default:
		return ""
	}
}

// checkSwitcher validates the switcher state and profile directories.
// §14.7: reports stale active pointers, profiles with no credentials while
// the live file is also absent, and available backup files.
func checkSwitcher(rootDir string) []Result {
	swRoot := filepath.Join(rootDir, "switcher")
	stateFile := filepath.Join(swRoot, "state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // switcher not configured, nothing to check
		}
		return []Result{{Name: "switcher state", Status: StatusWarn, Message: fmt.Sprintf("read state.json: %v", err)}}
	}
	var state switcherState
	if err := json.Unmarshal(data, &state); err != nil {
		return []Result{{Name: "switcher state", Status: StatusFail, Message: fmt.Sprintf("parse state.json: %v", err)}}
	}
	if !state.Enabled {
		return []Result{{Name: "switcher", Status: StatusOK, Message: "disabled"}}
	}

	var results []Result
	home, _ := os.UserHomeDir()
	liveCredPath := map[string]string{
		"claude": filepath.Join(home, ".claude", ".credentials.json"),
		"codex":  filepath.Join(home, ".codex", "auth.json"),
	}
	for _, tool := range []string{"claude", "codex"} {
		name := fmt.Sprintf("switcher/%s", tool)
		active := state.Active[tool]
		if active == "" {
			results = append(results, Result{Name: name, Status: StatusOK, Message: "no active profile"})
			continue
		}
		profileDir := filepath.Join(swRoot, "profiles", tool, active)
		if _, err := os.Stat(profileDir); os.IsNotExist(err) {
			results = append(results, Result{Name: name, Status: StatusWarn,
				Message: fmt.Sprintf("active profile %q directory missing (stale pointer)", active)})
			continue
		}
		credFile := switcherCredFilename(tool)
		storedCred := filepath.Join(profileDir, credFile)
		_, storedErr := os.Stat(storedCred)
		livePath := liveCredPath[tool]
		_, liveErr := os.Stat(livePath)
		if os.IsNotExist(storedErr) && os.IsNotExist(liveErr) {
			// Check for a backup that could help recover.
			backupGlob := filepath.Join(swRoot, "backups", tool, active+"-*.json")
			backups, _ := filepath.Glob(backupGlob)
			backupHint := ""
			if len(backups) > 0 {
				backupHint = fmt.Sprintf("; backup available: %s", backups[len(backups)-1])
			}
			results = append(results, Result{Name: name, Status: StatusWarn,
				Message: fmt.Sprintf("active profile %q has no stored credentials and no live file%s — login required", active, backupHint)})
			continue
		}
		results = append(results, Result{Name: name, Status: StatusOK, Message: fmt.Sprintf("active: %q", active)})
	}
	return results
}

func checkTLSEnv() Result {
	name := "TLS config"
	cert := os.Getenv("AIMEBU_TLS_CERT")
	key := os.Getenv("AIMEBU_TLS_KEY")
	if cert == "" && key == "" {
		return Result{name, StatusOK, "not configured (plain HTTP)"}
	}
	if cert == "" || key == "" {
		return Result{name, StatusFail, "AIMEBU_TLS_CERT and AIMEBU_TLS_KEY must both be set or both unset"}
	}
	if _, err := os.Stat(cert); err != nil {
		return Result{name, StatusFail, fmt.Sprintf("AIMEBU_TLS_CERT %q: %v", cert, err)}
	}
	if _, err := os.Stat(key); err != nil {
		return Result{name, StatusFail, fmt.Sprintf("AIMEBU_TLS_KEY %q: %v", key, err)}
	}
	return Result{name, StatusOK, fmt.Sprintf("cert=%s key=%s", cert, key)}
}
