package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
)

const doctorUsage = "usage: aimebu doctor [--json] [--strict] [--live | --machine]"

// doctorOpenReadOnly opens the stopped server's database for the checks. It must never migrate or write (store.Open does both).
var doctorOpenReadOnly = store.OpenReadOnly

// doctorCommand implements `aimebu doctor [--json] [--strict] [--machine]`:
// health checks for a server installation, or with --machine for a
// host that runs only the supervisor. Each check reports ok, warn, or
// fail with a one-line explanation and, when something is wrong, a hint on
// fixing it. The exit code is 1 when any check fails; warnings alone exit 0,
// unless --strict turns them into failures for scripts.
//
// The CLI never opens the database while the server owns its startup lock.
func doctorCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	jsonOutput, strict, machineMode, liveMode := false, false, false, false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "--strict":
			strict = true
		case "--machine":
			machineMode = true
		case "--live":
			liveMode = true
		default:
			fmt.Fprintln(stderr, doctorUsage)
			return 2
		}
	}
	if machineMode && liveMode {
		fmt.Fprintln(stderr, doctorUsage)
		return 2
	}

	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	now := time.Now()
	configCheck := checkConfigDir(config.root)
	var oldRootChecks []doctorCheck
	if notice := oldDevelopmentRootNotice(); notice != "" {
		oldRootChecks = append(oldRootChecks, doctorCheck{"older v2 data", statusWarn, notice, "Set AIMEBU_CONFIG_DIR to that root, or move its v2 data into the default root.", ""})
	}
	if machineMode {
		result := doctorResult{Version: resolveVersion(), GeneratedAt: now.UTC().Format(time.RFC3339), Checks: scopeAll([]doctorCheck{configCheck}, scopeMachine)}
		result.Checks = append(result.Checks, scopeAll(oldRootChecks, scopeMachine)...)
		if configCheck.Status != statusFail {
			result.Checks = append(result.Checks, checkMachineMode(ctx, config)...)
		}
		return finishDoctor(result, strict, jsonOutput, stdout, stderr)
	}
	serverCheck := checkServerHealth(ctx, config.bind, config.port)
	path := filepath.Join(config.root, "server", "aimebu.sqlite")
	var dbCheck doctorCheck
	var db *sql.DB
	var closeOffline func()
	var live protocol.DoctorLive
	var liveFailure *doctorCheck
	var liveFetched bool
	var keyCheck doctorCheck
	if serverCheck.Status == statusOK {
		key, finding, ok := readDoctorKey(config.root)
		keyCheck = finding
		if ok {
			live, liveFailure = fetchDoctorLive(ctx, config, key)
			liveFetched = liveFailure == nil
		}
		if liveFailure == nil && live.Database != nil {
			dbCheck = checkServerDatabase(*live.Database)
		} else {
			dbCheck = doctorCheck{Name: "sqlite database", Status: statusWarn, Description: "server running; database check unavailable from server"}
		}
	} else {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			dbCheck = checkSQLiteDB(path, nil)
		} else if v1Err := refuseV1RootWrite(config.root); v1Err != nil && !errors.Is(v1Err, errMixedV1RootWrite) {
			// v1's database lives at this same path. Opening it here would not be a read, and the lock file is not ours to create.
			dbCheck = doctorV1RootCheck(config.root, v1Err)
		} else {
			lock, err := lockServerDirectory(filepath.Dir(path))
			if err != nil {
				dbCheck = doctorCheck{Name: "sqlite database", Status: statusWarn, Description: "server running or lock held; database check skipped"}
			} else {
				defer unlockServerDirectory(lock)
				if v1Err := refuseV1RootWriteHeld(config.root); v1Err != nil && !errors.Is(v1Err, errMixedV1RootWrite) {
					dbCheck = doctorV1RootCheck(config.root, v1Err)
				} else if _, err := os.Stat(path); err == nil {
					opened, openErr := doctorOpenReadOnly(path)
					if openErr == nil {
						db = opened
						closeOffline = func() { _ = opened.Close() }
					} else {
						dbCheck = doctorCheck{Name: "sqlite database", Status: statusFail, Description: "open " + path + ": " + openErr.Error()}
					}
				}
				if dbCheck.Name == "" {
					dbCheck = checkSQLiteDB(path, db)
				}
			}
		}
	}

	if closeOffline != nil {
		defer closeOffline()
	}
	result := doctorResult{Version: resolveVersion(), GeneratedAt: now.UTC().Format(time.RFC3339), Checks: scopeAll([]doctorCheck{
		configCheck,
		dbCheck,
		serverCheck,
		checkTLS(config.port),
		checkBindAllow(config),
	}, scopeServer)}
	result.Checks = append(result.Checks, scopeAll(oldRootChecks, scopeServer)...)
	if check, ok := checkProxyThrottle(config); ok {
		result.Checks = append(result.Checks, check)
	}
	result.Checks = append(result.Checks, checkSwitcher()...)
	result.Checks = append(result.Checks, scopeAll(checkHarnessAnswerTools(doctorDetectHarnesses(ctx)), scopeServer)...)
	result.Checks = append(result.Checks, checkCredentialTargets(config.root)...)
	// The offline checks need a readable config root and a database that passed
	// its integrity check; a server answering at all is what lets them judge
	// machine liveness.
	if configCheck.Status != statusFail {
		queryDB := db
		if !strings.Contains(dbCheck.Description, "integrity ok") {
			queryDB = nil
		}
		result.Checks = append(result.Checks, doctorServerChecks(config.root, now, serverCheck.Status != statusFail, queryDB)...)
		if live.Database != nil {
			for _, check := range live.Database.Checks {
				result.Checks = append(result.Checks, doctorCheck(check))
			}
		}
	}
	result.Checks = scopeAll(result.Checks, scopeServer)
	// The key goes only to a server that answered the way an aimebu server does (the 401 that makes the server check ok), never to
	// whatever else happens to listen on the port.
	if liveMode && serverCheck.Status == statusOK {
		result.Checks = append(result.Checks, scopeAll([]doctorCheck{keyCheck}, scopeLive)...)
		if liveFailure != nil {
			result.Checks = append(result.Checks, *liveFailure)
		} else if liveFetched {
			result.Checks = append(result.Checks, scopeAll(liveMachineChecks(live), scopeLive)...)
		}
	}
	return finishDoctor(result, strict, jsonOutput, stdout, stderr)
}

// finishDoctor sets the exit code (any failure, or any warning under --strict)
// and prints the report.
func finishDoctor(result doctorResult, strict, jsonOutput bool, stdout, stderr io.Writer) int {
	for _, check := range result.Checks {
		if check.Status == statusFail || (strict && check.Status == statusWarn) {
			result.ExitCode = 1
			break
		}
	}
	if jsonOutput {
		printDoctorJSON(stdout, stderr, result)
	} else {
		printDoctorTable(stdout, result)
	}
	return result.ExitCode
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// doctorCheck is one finding. Hint says how to fix a warn or fail; Scope says
// where the fact was read: "server" (this host's config root and database),
// "live" (the running server) or "machine" (a supervisor host).
type doctorCheck protocol.DoctorCheck

type doctorResult struct {
	Version     string        `json:"version,omitempty"`
	GeneratedAt string        `json:"generated_at,omitempty"`
	Checks      []doctorCheck `json:"checks"`
	ExitCode    int           `json:"exit_code"`
}

// checkConfigDir verifies the config root exists, is a directory, and is not
// world-writable.
func checkConfigDir(root string) doctorCheck {
	const name = "config dir"
	info, err := os.Stat(root)
	if err != nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("%s: %v", root, err), "", ""}
	}
	if !info.IsDir() {
		return doctorCheck{name, statusFail, fmt.Sprintf("%s: not a directory", root), "", ""}
	}
	if info.Mode().Perm()&0o002 != 0 {
		return doctorCheck{name, statusWarn, fmt.Sprintf("%s: world-writable (mode %o)", root, info.Mode().Perm()), "", ""}
	}
	return doctorCheck{name, statusOK, fmt.Sprintf("%s (mode %o)", root, info.Mode().Perm()), "", ""}
}

// checkSQLiteDB reads an already-open store connection, runs PRAGMA integrity_check,
// and compares the stored schema version against the newest migration this
// binary applies. A missing database warns instead of failing: on a machine
// where the server has never started, doctor should still report the rest of
// the installation.
// doctorV1RootCheck is the database finding for a stopped root that still holds v1's data (or an interrupted v1 move): doctor leaves it
// alone, and says the v2 server has not taken the root over yet.
func doctorV1RootCheck(root string, err error) doctorCheck {
	const name = "sqlite database"
	if errors.Is(err, errV1RootWrite) {
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s holds aimebu v1 data (or an interrupted v1 move); not opened, start the v2 server to finish cutover first", root)}
	}
	return doctorCheck{Name: name, Status: statusFail, Description: "cannot inspect " + root + ": " + err.Error()}
}

func checkSQLiteDB(path string, db *sql.DB) doctorCheck {
	const name = "sqlite database"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return doctorCheck{name, statusWarn, fmt.Sprintf("%s: not found (server not started yet?)", path), "", ""}
	} else if err != nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("%s: %v", path, err), "", ""}
	}
	if db == nil {
		return doctorCheck{name, statusWarn, "server running or lock held; database check skipped", "", ""}
	}

	var integrity string
	if err := sqliteutil.Retry(func() error { return db.QueryRow("PRAGMA integrity_check").Scan(&integrity) }); err != nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("integrity_check: %v", err), "", ""}
	}
	if integrity != "ok" {
		return doctorCheck{name, statusFail, fmt.Sprintf("integrity_check: %s", integrity), "", ""}
	}

	var current int
	if err := sqliteutil.Retry(func() error {
		return db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	}); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			current = 0 // database file exists but migrations never ran
		} else {
			return doctorCheck{name, statusFail, fmt.Sprintf("read schema_version: %v", err), "", ""}
		}
	}
	// v2 never creates schema_meta. Its presence in a v2 database means a v1 server was started on this folder after v2
	// moved v1's files away: it saw no history and built its own tables here (a mixed database).
	if current > 0 {
		var mixed int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&mixed); err == nil && mixed > 0 {
			return doctorCheck{name, statusFail, mixedV1DatabaseDescription(), "", ""}
		}
	}
	latest := store.LatestSchemaVersion()
	switch {
	case current == 0:
		return doctorCheck{name, statusWarn, "integrity ok; database is empty (server not started yet?)", "", ""}
	case current == latest:
		return doctorCheck{name, statusOK, fmt.Sprintf("integrity ok; schema v%d matches this binary", current), "", ""}
	case current < latest:
		return doctorCheck{name, statusWarn, fmt.Sprintf("integrity ok; schema v%d, this binary supports v%d (forward-migrates on next start)", current, latest), "", ""}
	default:
		return doctorCheck{name, statusFail, fmt.Sprintf("integrity ok; schema v%d is newer than this binary supports (v%d)", current, latest), "", ""}
	}
}

func checkServerDatabase(data protocol.DoctorDatabase) doctorCheck {
	const name = "sqlite database"
	if data.IntegrityCheck == "timeout" {
		return doctorCheck{Name: name, Status: statusWarn, Description: "integrity_check: timeout"}
	}
	if data.IntegrityCheck != "ok" {
		return doctorCheck{Name: name, Status: statusFail, Description: "integrity_check: " + data.IntegrityCheck}
	}
	if data.HasV1SchemaMeta {
		return doctorCheck{Name: name, Status: statusFail, Description: mixedV1DatabaseDescription()}
	}
	latest := store.LatestSchemaVersion()
	switch {
	case data.SchemaVersion == 0:
		return doctorCheck{Name: name, Status: statusWarn, Description: "integrity ok; database is empty (server not started yet?)"}
	case data.SchemaVersion == latest:
		return doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("integrity ok; schema v%d matches this binary", data.SchemaVersion)}
	case data.SchemaVersion < latest:
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("integrity ok; schema v%d, this binary supports v%d (forward-migrates on next start)", data.SchemaVersion, latest)}
	default:
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("integrity ok; schema v%d is newer than this binary supports (v%d)", data.SchemaVersion, latest)}
	}
}

func mixedV1DatabaseDescription() string {
	return "integrity ok, but this v2 database also holds v1's tables (schema_meta): a v1 server was started on this folder after v2 moved v1's files into " + v1BackupName + ". Stop it, then restore the backup into a new folder (docs/migrating-from-v1.md, Roll back)"
}

// doctorHTTPClient is the client doctor uses to talk to the server it was pointed at (the health probe and --live). It never goes through
// a proxy named in the environment (HTTP_PROXY would otherwise receive the request, and with --live its bearer key, for a server address that
// is not loopback, such as a VPN bind), and it does not follow redirects, which could carry the key to another host.
func doctorHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// checkServerHealth probes the configured HTTP address. The server keeps its
// plain-HTTP listener even when TLS is enabled, so an HTTP GET is the right
// probe either way. An unauthenticated GET /api/me answers 401; any other
// HTTP answer means something is listening but not as expected, which warns
// rather than fails.
func checkServerHealth(ctx context.Context, bind string, port int) doctorCheck {
	const name = "server"
	address := net.JoinHostPort(bind, strconv.Itoa(port))
	requestURL := "http://" + address + "/api/me"
	client := doctorHTTPClient(3 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("GET %s: %v", requestURL, err), "", ""}
	}
	resp, err := client.Do(req)
	if err != nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("GET %s: %v (refused?)", requestURL, err), "", ""}
	}
	defer resp.Body.Close()
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return doctorCheck{name, statusOK, fmt.Sprintf("HTTP 401 at %s (unauthenticated, as expected)", requestURL), "", ""}
	}
	return doctorCheck{name, statusWarn, fmt.Sprintf("HTTP %d at %s (expected 401)", resp.StatusCode, requestURL), "", ""}
}

// checkTLS reports the TLS situation using the same resolution the server
// uses, so doctor agrees with `aimebu server`: both AIMEBU_TLS_CERT and
// AIMEBU_TLS_KEY must be set, must name readable regular files, must form a
// parseable pair, and AIMEBU_TLS_PORT must differ from AIMEBU_PORT. A
// certificate expiring in less than 14 days warns.
func checkTLS(httpPort int) doctorCheck {
	const name = "tls"
	config, err := resolveServerTLSConfig(httpPort)
	if err != nil {
		return doctorCheck{name, statusFail, err.Error(), "", ""}
	}
	if config == nil {
		return doctorCheck{name, statusOK, "not configured (HTTP only)", "", ""}
	}
	cert := config.certificate.Leaf
	if cert == nil {
		return doctorCheck{name, statusFail, "certificate did not parse", "", ""}
	}
	if time.Now().After(cert.NotAfter) {
		return doctorCheck{name, statusFail, fmt.Sprintf("certificate expired on %s", cert.NotAfter.Format("2006-01-02")), "", ""}
	}
	daysLeft := int(time.Until(cert.NotAfter).Hours() / 24)
	if daysLeft < 14 {
		return doctorCheck{name, statusWarn, fmt.Sprintf("certificate expires on %s (%d days left)", cert.NotAfter.Format("2006-01-02"), daysLeft), "", ""}
	}
	return doctorCheck{name, statusOK, fmt.Sprintf("certificate expires on %s (HTTPS on port %d)", cert.NotAfter.Format("2006-01-02"), config.port), "", ""}
}

// checkBindAllow reports the effective listener address and allowlist. Both
// values were already validated by resolveServerConfig, so this check only
// fails defensively and warns when a non-loopback bind has no TLS.
func checkBindAllow(config serverConfig) doctorCheck {
	const name = "bind/allow"
	bindIP := net.ParseIP(config.bind)
	if bindIP == nil {
		return doctorCheck{name, statusFail, fmt.Sprintf("invalid bind address %q", config.bind), "", ""}
	}
	entries := make([]string, len(config.allow))
	for i, prefix := range config.allow {
		entries[i] = prefix.String()
	}
	allowSummary := strings.Join(entries, ", ")
	if config.allowDefault {
		allowSummary += " (default)"
	}
	if !bindIP.IsLoopback() && !(os.Getenv("AIMEBU_TLS_CERT") != "" && os.Getenv("AIMEBU_TLS_KEY") != "") {
		return doctorCheck{name, statusWarn, fmt.Sprintf("bind %s is non-loopback without TLS; allow %s", config.bind, allowSummary), "", ""}
	}
	return doctorCheck{name, statusOK, fmt.Sprintf("bind %s; allow %s", config.bind, allowSummary), "", ""}
}

// checkProxyThrottle is shown only when AIMEBU_PUBLIC_URL is set. It fails
// on a value the server would refuse and warns when no proxy is trusted, since
// every proxied client then shares one throttle identity.
func checkProxyThrottle(config serverConfig) (doctorCheck, bool) {
	const name = "proxy"
	if os.Getenv("AIMEBU_PUBLIC_URL") == "" {
		return doctorCheck{}, false
	}
	publicURL, err := resolvePublicURL()
	if err != nil {
		return doctorCheck{name, statusFail, err.Error(), "", ""}, true
	}
	if warning := proxyThrottleWarning(publicURL, config.trusted); warning != "" {
		return doctorCheck{name, statusWarn, warning, "", ""}, true
	}
	entries := make([]string, len(config.trusted))
	for i, prefix := range config.trusted {
		entries[i] = prefix.String()
	}
	return doctorCheck{name, statusOK, fmt.Sprintf("public URL %s; trusted proxies %s", publicURL, strings.Join(entries, ", ")), "", ""}, true
}

// checkSwitcher validates the account switcher state read-only: the state
// file parses, and each active profile still has a directory and either stored
// or live credentials. It reports nothing when the switcher was never set up,
// so installations that do not use it see no extra rows.
func checkSwitcher() []doctorCheck {
	const name = "switcher"
	root, err := switcher.DefaultRoot()
	if err != nil {
		return []doctorCheck{{name, statusWarn, err.Error(), "", ""}}
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return []doctorCheck{{name, statusWarn, fmt.Sprintf("read state.json: %v", err), "", ""}}
	}
	m, err := switcher.New(root)
	if err != nil {
		return []doctorCheck{{name, statusWarn, err.Error(), "", ""}}
	}
	enabled, err := m.Enabled()
	if err != nil {
		return []doctorCheck{{name, statusFail, err.Error(), "", ""}}
	}
	if !enabled {
		return []doctorCheck{{name, statusOK, "disabled", "", ""}}
	}
	var checks []doctorCheck
	for _, tool := range switcher.Tools() {
		checkName := name + "/" + tool
		active, err := m.Active(tool)
		if err != nil {
			checks = append(checks, doctorCheck{checkName, statusFail, err.Error(), "", ""})
			continue
		}
		if active == "" {
			checks = append(checks, doctorCheck{checkName, statusOK, "no active profile", "", ""})
			continue
		}
		stored := m.StoredCredPath(tool, active)
		if _, err := os.Stat(filepath.Dir(stored)); os.IsNotExist(err) {
			checks = append(checks, doctorCheck{checkName, statusWarn, fmt.Sprintf("active profile %q directory missing (stale pointer)", active), "", ""})
			continue
		}
		live, err := m.LiveCredentialPath(tool)
		if err != nil {
			checks = append(checks, doctorCheck{checkName, statusWarn, err.Error(), "", ""})
			continue
		}
		_, storedErr := os.Stat(stored)
		_, liveErr := os.Stat(live)
		hint := ""
		if newest := m.NewestBackup(tool, active); newest != "" {
			hint = fmt.Sprintf("; backup available: %s", newest)
		}
		if os.IsNotExist(storedErr) && os.IsNotExist(liveErr) {
			checks = append(checks, doctorCheck{checkName, statusWarn, fmt.Sprintf("active profile %q has no stored credentials and no live file%s — login required", active, hint), "", ""})
			continue
		}
		if os.IsNotExist(liveErr) {
			checks = append(checks, doctorCheck{checkName, statusWarn, fmt.Sprintf("active profile %q has a stored copy but no live credential file%s — login required", active, hint), "", ""})
			continue
		}
		checks = append(checks, doctorCheck{checkName, statusOK, fmt.Sprintf("active: %q", active), "", ""})
	}
	return checks
}

func printDoctorTable(stdout io.Writer, result doctorResult) {
	symbols := map[string]string{statusOK: "✓", statusWarn: "!", statusFail: "✗"}
	for _, check := range result.Checks {
		fmt.Fprintf(stdout, "%s %-18s %s\n", symbols[check.Status], check.Name+":", check.Description)
		if check.Hint != "" && check.Status != statusOK {
			fmt.Fprintf(stdout, "  %-18s → %s\n", "", check.Hint)
		}
	}
	count := map[string]int{}
	for _, check := range result.Checks {
		count[check.Status]++
	}
	fmt.Fprintf(stdout, "\n%d checks: %d ok, %d warn, %d fail\n", len(result.Checks), count[statusOK], count[statusWarn], count[statusFail])
	switch {
	case count[statusFail] > 0:
		fmt.Fprintln(stdout, "Some checks failed — fix them before relying on this installation.")
	case count[statusWarn] > 0:
		fmt.Fprintln(stdout, "All critical checks passed; warnings above may still need attention.")
	default:
		fmt.Fprintln(stdout, "All checks passed.")
	}
}

func printDoctorJSON(stdout, stderr io.Writer, result doctorResult) {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(stderr, "doctor: failed to encode JSON: %v\n", err)
	}
}
