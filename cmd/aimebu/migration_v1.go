package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	_ "modernc.org/sqlite"
)

const v1BackupName = ".old-v1-bkp"

// Development builds can point at a v1 root while still listening on their
// separate v2 port. Check the v1 listener too before moving its database.
// AIMEBU_DEV_SKIP_V1_PROBE=1 skips only this listener probe, so a dev instance
// can rehearse the cutover on a copy of v1 data while the real v1 keeps
// serving; checkV1Daemon and the backup's SQLite write lock still apply.
func checkV1CutoverPort(root string, boundPort, legacyPort int, stderr io.Writer) error {
	if boundPort == legacyPort {
		return nil
	}
	v1, err := isV1Root(root)
	if err != nil {
		return err
	}
	_, completeErr := os.Lstat(filepath.Join(root, v1BackupName))
	_, partialErr := os.Lstat(filepath.Join(root, v1BackupName+".partial"))
	if !v1 && os.IsNotExist(completeErr) && os.IsNotExist(partialErr) {
		return nil
	}
	if completeErr != nil && !os.IsNotExist(completeErr) {
		return completeErr
	}
	if partialErr != nil && !os.IsNotExist(partialErr) {
		return partialErr
	}
	if os.Getenv("AIMEBU_DEV_SKIP_V1_PROBE") == "1" {
		fmt.Fprintf(stderr, "warning: v1 listener probe on port %d skipped by the AIMEBU_DEV_SKIP_V1_PROBE=1 dev escape hatch; never use it in production\n", legacyPort)
		return nil
	}
	hosts := map[string]bool{"127.0.0.1": true, "::1": true}
	if host := strings.TrimSpace(os.Getenv("AIMEBU_BIND")); host != "" {
		hosts[host] = true
	}
	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok && !network.IP.IsUnspecified() {
				hosts[network.IP.String()] = true
			}
		}
	}
	for host := range hosts {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(legacyPort)), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return errors.New("stop the v1 aimebu server, then start v2 again: legacy listener is active")
		}
	}
	return nil
}

// checkV1Daemon refuses to move a v1 root whose daemon is still alive, whatever port it listens on. v1 daemons record
// their pid in server/aimebu.pid (aimebu.pid in the oldest layout) and keep their port only in their environment, so the pid
// is the one reliable handle. A pid file that cannot be read is ambiguous and refuses too. A pid that is alive but is not an
// aimebu process is a recycled number from a crashed daemon and is ignored. A v1 started in a terminal writes no pid file;
// the SQLite write lock in backupV1Layout is the only guard there, and it does not catch an idle one (see the migration guide).
func checkV1Daemon(root string) error {
	moving, err := v1MovePending(root)
	if err != nil || !moving {
		return err
	}
	// A resumed move may already have carried the pid file into the partial backup.
	partial := v1BackupName + ".partial"
	for _, rel := range []string{filepath.Join("server", "aimebu.pid"), "aimebu.pid", filepath.Join(partial, "server", "aimebu.pid"), filepath.Join(partial, "aimebu.pid")} {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot read v1's pid file %s (%v): stop the v1 server, make sure it is gone, then start v2 again", path, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 0 {
			return fmt.Errorf("v1's pid file %s does not hold a pid: stop the v1 server, make sure it is gone, delete the file, then start v2 again", path)
		}
		if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
			continue
		}
		// This process is v2 itself: a pid file that names it is a recycled number or a stale file, never a v1 daemon.
		if pid == os.Getpid() {
			continue
		}
		if exe, command, err := processInfo(pid); err == nil && !looksLikeV1Server(exe, command) {
			continue
		}
		return fmt.Errorf("stop the v1 aimebu server (pid %d from %s), then start v2 again: it is still running, possibly on a port other than 9997", pid, path)
	}
	return nil
}

// processInfo is the executable (when it can be resolved) and the full command line of a live process; tests replace it.
// The executable comes from /proc/<pid>/exe where that exists, otherwise from `ps -o comm=`, which prints the full path on
// macOS; either may be empty or only a name, and then looksLikeV1Server falls back to reading the command line.
var processInfo = func(pid int) (exe, command string, err error) {
	if link, linkErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); linkErr == nil {
		exe = strings.TrimSuffix(link, " (deleted)")
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		// No ps (or it failed): Linux has the command line in /proc, its arguments separated by NULs.
		raw, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if readErr != nil || len(raw) == 0 {
			return "", "", err
		}
		return exe, strings.TrimSpace(strings.ReplaceAll(strings.TrimRight(string(raw), "\x00"), "\x00", " ")), nil
	}
	command = strings.TrimSpace(string(out))
	if exe == "" {
		if comm, commErr := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output(); commErr == nil {
			exe = strings.TrimSpace(string(comm))
		}
	}
	return exe, command, nil
}

// looksLikeV1Server reports whether a live process is an aimebu server: an executable named aimebu (or the aimebu-<hash>
// binary bin/aimebu builds) run with the server subcommand. A recycled pid is far more likely to be an unrelated
// process, or an aimebu agent or CLI, than another aimebu server, and a bare "contains aimebu" would refuse to start on
// a stranger that merely has aimebu in its path or arguments.
//
// When the executable resolves to a real file its name decides ("vim /home/me/aimebu server" and "cat /tmp/aimebu-log
// server" are not aimebu); only when it does not resolve (a name without a path, a process we cannot read the link of)
// is the command line trusted alone. The executable path in a command line can contain spaces, so every
// whitespace-joined prefix is tried as the executable and the next field must be the server subcommand.
func looksLikeV1Server(exe, command string) bool {
	if exe != "" {
		if info, err := os.Stat(exe); err == nil && info.Mode().IsRegular() && !aimebuName(filepath.Base(exe)) {
			return false
		}
	}
	fields := strings.Fields(command)
	for i := 1; i < len(fields); i++ {
		if aimebuName(filepath.Base(strings.Join(fields[:i], " "))) && fields[i] == "server" {
			return true
		}
	}
	return false
}

func aimebuName(name string) bool { return name == "aimebu" || strings.HasPrefix(name, "aimebu-") }

// v1MovePending reports whether this start is about to move a v1 root into .old-v1-bkp, or to finish a move that a
// crash interrupted: the root holds a v1 database and no backup exists yet, or a partial backup (whose manifest says
// what is left to move) exists without a complete one. A finished backup, or a root that is already v2, moves nothing.
// The interrupted case counts because it still moves v1's files, so it needs the same preflight and, if the start fails
// afterwards, the same notice about where the files went.
func v1MovePending(root string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(root, v1BackupName)); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if _, err := os.Lstat(filepath.Join(root, v1BackupName+".partial")); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return isV1Root(root)
}

// preflightV1Cutover refuses, before anything is moved, a first start that is certain to fail afterwards. The failures
// it knows: half an owner login/password pair, a turn-limit setting the embedded supervisor rejects, and a root this process cannot write to. A start that fails after the move
// leaves v1's data in .old-v1-bkp while the root looks empty, and a v1 started on it would build its schema inside
// the v2 database.
func preflightV1Cutover(root string, getenv func(string) string) error {
	// A resumed move has already carried part of v1's files into the partial backup; say so instead of "nothing".
	untouched := "nothing has been moved"
	if _, err := os.Lstat(filepath.Join(root, v1BackupName+".partial")); err == nil {
		untouched = "the interrupted move was not continued; v1's files are in " + v1BackupName + ".partial"
	}
	// Neither owner variable means first-run setup in the web UI; exactly one is a mistake that would fail after the move.
	if (strings.TrimSpace(getenv("AIMEBU_OWNER_LOGIN")) == "") != (getenv("AIMEBU_OWNER_PASSWORD") == "") {
		return fmt.Errorf("set both AIMEBU_OWNER_LOGIN and AIMEBU_OWNER_PASSWORD, or neither to create the owner in the browser (%s)", untouched)
	}
	if getenv("AIMEBU_EMBEDDED_SUPERVISOR") != "0" {
		if _, err := supervisor.TurnLimitsFromEnv(getenv); err != nil {
			return fmt.Errorf("%w (%s)", err, untouched)
		}
	}
	// The registry of external credential targets is carried over after the move and must be a regular file.
	for _, dir := range []string{"server", ""} {
		if info, err := os.Lstat(filepath.Join(root, dir, credentialTargetsFile)); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("v1's %s is not a regular file, so it cannot be carried over: fix or remove it and start again (%s)", filepath.Join(dir, credentialTargetsFile), untouched)
		}
	}
	// A token-only install keeps its logins alive only while the refresher can run its CLI in a container; without
	// harness-docker-ctrl and a reachable docker daemon those logins expire, so refuse before anything moves.
	if len(migratedTools(filepath.Join(root, "switcher"))) > 0 {
		if _, err := lookPath(harnessDockerCtrl); err != nil {
			return fmt.Errorf("%s is not on PATH, and the token-only logins in %s can only be kept alive through it: put it on the PATH of the process that starts v2 and start again (%s)", harnessDockerCtrl, filepath.Join(root, "switcher"), untouched)
		}
		if !dockerReachable() {
			return fmt.Errorf("docker is not on PATH or its daemon does not answer (`docker info` fails), and the token-only logins can only be kept alive through it: start docker and start again (%s)", untouched)
		}
	}
	probe, err := os.CreateTemp(root, ".v2-preflight-*")
	if err != nil {
		return fmt.Errorf("the data directory %s is not writable: %w (%s)", root, err, untouched)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// v1FilesInBackup reports whether any of v1's files are in the backup folder, complete or partial.
func v1FilesInBackup(root string) bool {
	for _, name := range []string{v1BackupName, v1BackupName + ".partial"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
}

// v1MovedNotice is printed when a start fails after v1's files moved. It names the backup and warns off the one
// thing that makes it worse, starting v1 on the original folder. A move that did not finish names the partial folder
// and says the next start finishes it.
func v1MovedNotice(root string) string {
	backup := filepath.Join(root, v1BackupName)
	state := "v2 moved your v1 files into " + backup + " before this failure.\n"
	if _, err := os.Lstat(backup); err != nil {
		backup += ".partial"
		state = "v2 started moving your v1 files into " + backup + " before this failure; the move is not finished.\n"
	}
	return fmt.Sprintf("\n%s"+
		"Fix the problem above and start v2 again: it continues from that backup.\n"+
		"Do not start v1 on %s now: it would see no history and write its tables into the new v2 database.\n"+
		"To go back to v1, follow \"Roll back\" in docs/migrating-from-v1.md.\n", state, root)
}

type v1BackupManifest struct {
	Entries   []string `json:"entries"`
	SourceKey string   `json:"source_key,omitempty"`
}

func v1SourceKey(root string) (string, error) {
	path := filepath.Join(root, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(root, "aimebu.sqlite")
	}
	hash := sha256.New()
	for _, name := range []string{path, path + "-wal"} {
		file, err := os.Open(name)
		if os.IsNotExist(err) && name != path {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	return "v1-" + hex.EncodeToString(hash.Sum(nil))[:24], nil
}

// backupV1Layout holds one root lock while moving all v1-owned entries on the
// same filesystem. A manifest is durable before the first rename so a crash
// can finish the exact original batch without guessing what belonged to v1.
func backupV1Layout(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".v2-migration.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	complete := filepath.Join(root, v1BackupName)
	partial := complete + ".partial"
	if info, err := os.Lstat(complete); err == nil {
		if !info.IsDir() {
			return errors.New("v1 backup path is not a directory")
		}
		if _, e := os.Lstat(partial); e == nil {
			return errors.New("both complete and partial v1 backups exist; inspect them before starting")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, ".v2-migrated")); err == nil {
		return errors.New("v1 migration marker exists without the complete backup")
	} else if !os.IsNotExist(err) {
		return err
	}
	manifestPath := filepath.Join(partial, "manifest.json")
	var manifest v1BackupManifest
	newManifest := false
	if raw, err := os.ReadFile(manifestPath); err == nil {
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return fmt.Errorf("read partial v1 backup manifest: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if _, err := os.Lstat(partial); err == nil {
			return errors.New("partial v1 backup lacks its manifest; inspect before starting")
		} else if !os.IsNotExist(err) {
			return err
		}
		newManifest = true
	}
	if newManifest {
		v1, err := isV1Root(root)
		if err != nil || !v1 {
			return err
		}
	}
	// SQLite's write lock excludes a concurrent writer while the source files
	// are renamed. A listener on a different port may otherwise go unnoticed.
	var databasePath string
	for _, candidate := range []string{
		filepath.Join(root, "server", "aimebu.sqlite"),
		filepath.Join(root, "aimebu.sqlite"),
		filepath.Join(partial, "server", "aimebu.sqlite"),
		filepath.Join(partial, "aimebu.sqlite"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			databasePath = candidate
			break
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if databasePath == "" {
		return errors.New("v1 database missing from migration source")
	}
	// Deliberately no busy timeout (and busy_timeout is set to 0 below): a live v1 server must make this fail at once with
	// "stop the v1 aimebu server", not wait for it.
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	connection, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(context.Background(), "PRAGMA busy_timeout = 0"); err != nil {
		return err
	}
	if _, err := connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("stop the v1 aimebu server, then start v2 again: database write lock: %w", err)
	}
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	if newManifest {
		manifest.SourceKey, err = v1SourceKey(root)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "switcher" || name == "usages" || name == ".v2-migration.lock" || name == v1BackupName || name == v1BackupName+".partial" || name == ".v2-migrated" {
				continue
			}
			manifest.Entries = append(manifest.Entries, name)
		}
		sort.Strings(manifest.Entries)
		if err := os.Mkdir(partial, 0700); err != nil {
			return err
		}
		raw, _ := json.Marshal(manifest)
		file, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := syncDirectory(partial); err != nil {
			return err
		}
		if err := syncDirectory(root); err != nil {
			return err
		}
	}
	for _, name := range manifest.Entries {
		if name == "" || filepath.Base(name) != name || name == "." || name == ".." || name == "switcher" || name == "usages" || name == ".v2-migration.lock" || name == ".v2-migrated" || name == v1BackupName || name == v1BackupName+".partial" {
			return fmt.Errorf("invalid backup manifest entry %q", name)
		}
		source, target := filepath.Join(root, name), filepath.Join(partial, name)
		_, sourceErr := os.Lstat(source)
		_, targetErr := os.Lstat(target)
		if sourceErr == nil && targetErr == nil {
			return fmt.Errorf("backup entry %q exists in both places", name)
		}
		if sourceErr == nil {
			if !os.IsNotExist(targetErr) {
				return targetErr
			}
			if err := os.Rename(source, target); err != nil {
				return err
			}
		} else if !os.IsNotExist(sourceErr) {
			return sourceErr
		} else if targetErr != nil {
			return fmt.Errorf("backup entry %q missing in both places: %w", name, targetErr)
		}
	}
	if _, err := connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		return err
	}
	if manifest.SourceKey != "" {
		key, err := v1SourceKey(partial)
		if err != nil {
			return err
		}
		if key != manifest.SourceKey {
			return errors.New("v1 source changed while making the backup")
		}
	}
	if err := syncDirectory(partial); err != nil {
		return err
	}
	if err := syncDirectory(root); err != nil {
		return err
	}
	if err := os.Rename(partial, complete); err != nil {
		return err
	}
	return syncDirectory(root)
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func isV1Root(root string) (bool, error) {
	path := filepath.Join(root, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(root, "aimebu.sqlite")
	} else if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return false, err
	}
	defer db.Close()
	var version int
	query := func(statement string, dest *int) error {
		return sqliteutil.Retry(func() error { return db.QueryRow(statement).Scan(dest) })
	}
	if err := query("SELECT version FROM schema_meta LIMIT 1", &version); err != nil {
		// A v2 database has schema_version rather than schema_meta.
		var v2 int
		if e := query("SELECT COALESCE(MAX(version),0) FROM schema_version", &v2); e == nil && v2 > 0 {
			return false, nil
		}
		return false, fmt.Errorf("cannot identify existing database at %s: %w", path, err)
	}
	if version != 1 {
		return false, fmt.Errorf("unsupported v1 schema %d", version)
	}
	return true, nil
}
