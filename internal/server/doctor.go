package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/buildversion"
	"github.com/hrubymar10/aimebu/v2/internal/doctorcheck"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

var doctorIntegrityTimeout = 30 * time.Second
var doctorIntegrityQuery = func(ctx context.Context, conn *sql.Conn) (string, error) {
	var result string
	err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result)
	return result, err
}

// DoctorKeyFile is the file under server/ that holds the key `aimebu doctor --live` authenticates with. The server writes it at start with
// mode 0600 and removes it at shutdown; reading it needs the same file permissions that guard the database, so owning the config
// directory is the credential. GET /api/doctor accepts the key only from a same-host connection that carries no forwarding header.
const DoctorKeyFile = "doctor.key"

// EnableDoctorKey creates a fresh random key in <root>/server/doctor.key and arms GET /api/doctor with it. The returned function removes
// the file; call it at shutdown. A new key at every start means a key left behind by a crash opens nothing.
func (s *Server) EnableDoctorKey(root string) (func(), error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(raw[:])
	dir := filepath.Join(root, "server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, DoctorKeyFile)
	tmp, err := os.CreateTemp(dir, ".doctor-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	s.doctorKey = key
	return func() { _ = os.Remove(path) }, nil
}

// doctorCapabilities lists the hello's capability flags with what a supervisor without each one loses.
var doctorCapabilities = []struct {
	flag     string
	degrades string
	get      func(*machineConn) bool
}{
	{"supports_path_probe", "folder checks for a project on this machine are refused: a remote folder cannot be verified", func(c *machineConn) bool { return c.supportsPathProbe }},
	{"supports_worktree", "room worktrees cannot be created or removed on this machine", func(c *machineConn) bool { return c.supportsWorktree }},
	{"supports_launchers", "harness launchers are unavailable: the Harnesses page reports that the supervisor needs updating", func(c *machineConn) bool { return c.supportsLaunchers }},
	{"supports_detect", "Settings → Harnesses & models cannot auto-detect harnesses or models on this machine", func(c *machineConn) bool { return c.supportsDetect }},
	{"supports_detect_dir", "auto-detect cannot run in a chosen folder", func(c *machineConn) bool { return c.supportsDetectDir }},
	{"supports_session_scan", "Claude Code sessions on this machine cannot be listed for import", func(c *machineConn) bool { return c.supportsSessionScan }},
	{"supports_session_fork", "a Claude Code session cannot be continued as a copy (fork); only continued in place", func(c *machineConn) bool { return c.supportsSessionFork }},
	{"supports_session_copy", "a Claude Code session cannot be worked on as a copy in a worktree of its own", func(c *machineConn) bool { return c.supportsSessionCopy }},
	{"supports_legacy_session_copy", "an imported Codex, Pi, OpenCode or Vibe session cannot be continued on this machine", func(c *machineConn) bool { return c.supportsLegacySessionCopy }},
	{"supports_launch_path", "a Claude Code session made under a link to its folder cannot be continued on this machine: its turns are held", func(c *machineConn) bool { return c.supportsLaunchPath }},
	{"supports_session_attribution", "an imported v1 sender whose messages carry no session cannot be matched to the Claude Code session that sent them; the session v1 kept under its name is offered, checked against its messages", func(c *machineConn) bool { return c.supportsSessionAttribution }},
	{"supports_providers", "an OpenCode agent on a custom provider cannot run on this machine: its turns are held", func(c *machineConn) bool { return c.supportsProviders }},
	{"supports_warm", "agents cannot be kept warm on this machine, and its switch of account cannot end them: every turn starts its process", func(c *machineConn) bool { return c.supportsWarm }},
	{"supports_agent_logs", "an agent's captured stderr cannot be read from the web UI", func(c *machineConn) bool { return c.supportsAgentLogs }},
	{"supports_agent_log_size", "the size of the agent logs on this machine is not shown", func(c *machineConn) bool { return c.supportsAgentLogSize }},
	{"supports_agent_conversation_log", "an agent's conversation log cannot be read from the web UI", func(c *machineConn) bool { return c.supportsAgentConversation }},
	{"supports_stop_replay", "a stop sent while the machine was offline is applied by a fallback that can kill a live process early", func(c *machineConn) bool { return c.supportsStopReplay }},
	{"supports_turn_interrupt", "a turn cannot be cut without stopping its agent: interrupting an agent a team flow change waits for stops it and starts it again", func(c *machineConn) bool { return c.supportsTurnInterrupt }},
	{"supports_attachment_save", "an agent that reaches its toolbox over HTTP (Docker, Pi, OpenCode, Vibe) cannot get an attachment saved as a file in its workspace", func(c *machineConn) bool { return c.supportsAttachmentSave }},
	{"supports_handoff_outcome", "its agents' prompts and response schemas have no handoff outcome: their handoffs are committed without one (shown as unknown)", func(c *machineConn) bool { return c.supportsHandoffOutcome }},
}

// sameHostPeer says whether a connection comes from the machine the server runs on: from a loopback address, or from the very address it
// was accepted on. The second case is what `aimebu doctor --live` looks like when AIMEBU_BIND is a VPN or LAN address, because the CLI
// dials that address and the kernel then uses it as the source too. A remote host cannot fake it: a TCP connection needs the handshake's
// reply, which goes to the source address, so a peer address equal to the server's own local address is always this host.
func sameHostPeer(peer, local netip.Addr) bool {
	peer = peer.WithZone("").Unmap()
	return peer.IsLoopback() || (local.IsValid() && peer == local.WithZone("").Unmap())
}

func (s *Server) doctorAuthorized(r *http.Request) bool {
	if s.doctorKey == "" || viaProxy(r) {
		return false
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	var local netip.Addr
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if parsed, err := netip.ParseAddrPort(addr.String()); err == nil {
			local = parsed.Addr()
		}
	}
	if !sameHostPeer(peer.Addr(), local) {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(s.doctorKey)) == 1
}

// doctorLive answers GET /api/doctor: per machine, which capability flags its connected supervisor announced and what its launchers last
// reported. It is read-only and says nothing a secret could hide in (no keys, no environment, no launcher arguments).
func (s *Server) doctorLive(w http.ResponseWriter, r *http.Request) {
	if !s.doctorAuthorized(r) {
		apiError(w, 403, "forbidden", "a same-host request with the doctor key is required")
		return
	}
	if !s.doctorIntegrityRunning.CompareAndSwap(false, true) {
		apiError(w, http.StatusTooManyRequests, "busy", "a database integrity check is already running")
		return
	}
	defer s.doctorIntegrityRunning.Store(false)
	database := s.doctorDatabase(r.Context())
	machines, err := s.Store.Machines(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	answer := protocol.DoctorLive{Machines: []protocol.DoctorMachine{}, Database: &database}
	for _, machine := range machines {
		if machine.RevokedAt.Valid {
			continue
		}
		item := protocol.DoctorMachine{ID: machine.ID, Name: machine.Name, Approved: machine.Approved,
			SupervisorVersion: machine.SupervisorVersion.String, ServerVersion: s.Version,
			OlderBuild: buildversion.Older(machine.SupervisorVersion.String, s.Version), Launchers: []protocol.DoctorLauncher{}}
		if machine.ID == s.localSupervisorID() {
			item.RestartPending = s.localSupervisorPending.Load()
			item.UpgradeRequired = s.localSupervisorUpgradeRequired.Load()
		}
		observed := map[string]protocol.LauncherRow{}
		if conn, err := s.machineForID(machine.ID); err == nil {
			select {
			case <-conn.done:
			default:
				item.Connected = true
				for _, capability := range doctorCapabilities {
					item.Supports = append(item.Supports, protocol.DoctorSupport{Flag: capability.flag, Supported: capability.get(conn), Degrades: capability.degrades})
				}
				conn.harnessMu.Lock()
				for _, row := range conn.launchers {
					observed[row.ID] = row
				}
				conn.harnessMu.Unlock()
			}
		}
		definitions, err := s.Store.MachineLaunchers(r.Context(), machine.ID)
		if err != nil {
			fail(w, fmt.Errorf("launchers of %s: %w", machine.ID, err))
			return
		}
		for _, definition := range definitions {
			launcher := protocol.DoctorLauncher{ID: definition.ID, Tool: definition.Tool, Name: definition.Name, Label: definition.Label, Enabled: definition.Enabled, Containerized: definition.Containerized}
			if row, ok := observed[definition.ID]; ok {
				launcher.Reported = true
				launcher.Available = row.Available
				launcher.UnavailableReason = row.UnavailableReason
				launcher.Version = row.Version
				launcher.Features = row.Features
				launcher.FeatureProblem = row.FeatureProblem
			}
			item.Launchers = append(item.Launchers, launcher)
		}
		answer.Machines = append(answer.Machines, item)
	}
	writeJSON(w, 200, answer)
}

func (s *Server) doctorDatabase(ctx context.Context) protocol.DoctorDatabase {
	readDB := s.Store.DoctorReadDB()
	answer := protocol.DoctorDatabase{Checks: []protocol.DoctorCheck{}}
	checkCtx, cancel := context.WithTimeout(ctx, doctorIntegrityTimeout)
	defer cancel()
	conn, err := readDB.Conn(checkCtx)
	if err == nil {
		answer.IntegrityCheck, err = doctorIntegrityQuery(checkCtx, conn)
		_ = conn.Close()
	}
	if checkCtx.Err() == context.DeadlineExceeded {
		answer.IntegrityCheck = "timeout"
	} else if err != nil {
		answer.IntegrityCheck = "error: " + err.Error()
	}
	if err := readDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&answer.SchemaVersion); err != nil && err != sql.ErrNoRows {
		answer.Checks = append(answer.Checks, protocol.DoctorCheck{Name: "schema version", Status: "warn", Description: err.Error()})
	}
	var mixed int
	if err := readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&mixed); err == nil {
		answer.HasV1SchemaMeta = mixed > 0
	}
	if answer.IntegrityCheck == "ok" {
		answer.Checks = append(answer.Checks, doctorcheck.Checks(readDB, time.Now(), true)...)
	}
	return answer
}

func (s *Server) doctorSessions(w http.ResponseWriter, r *http.Request) {
	if !s.doctorAuthorized(r) {
		apiError(w, 403, "forbidden", "a same-host request with the doctor key is required")
		return
	}
	sessions, err := store.ReadDoctorSessions(r.Context(), s.Store.DoctorReadDB())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Sessions []protocol.DoctorSession `json:"sessions"`
	}{Sessions: sessions})
}
