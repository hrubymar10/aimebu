package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/internal/cmdprobe"
)

// The harness-docker modes beyond a turn (docs/design/harness-docker-run-command.md): a command run in the agent's container outside any
// turn (run_command), and the exec ID plus the probe that say whether a tool runs under a turn's harness (turn_activity).
const (
	// FeatureRunCommand is the launcher feature that runs HARNESS_DOCKER_RUN_COMMAND in the container instead of a harness.
	FeatureRunCommand = "run_command"
	// FeatureTurnActivity is the launcher feature that prints a launch's exec ID (on request) and answers HARNESS_DOCKER_PROBE for it.
	FeatureTurnActivity = "turn_activity"
	// DockerReportExecIDEnv asks a launcher with turn_activity to print its exec ID line on stderr; without it nothing is printed.
	DockerReportExecIDEnv = "HARNESS_DOCKER_REPORT_EXEC_ID"
	// DockerExecIDPrefix starts the line that carries the exec ID.
	DockerExecIDPrefix = "HARNESS_DOCKER_EXEC_ID="
)

// dockerControlEnv are the launcher's mode switches: aimebu sets them itself where it means to, and never takes one from an owner's
// environment reference (one would turn a turn into a command, or a command into a probe).
var dockerControlEnv = map[string]bool{"HARNESS_DOCKER_RUN_COMMAND": true, "HARNESS_DOCKER_CWD": true, "HARNESS_DOCKER_EXIT_MARKER": true,
	"HARNESS_DOCKER_TIMEOUT_SECONDS": true, "HARNESS_DOCKER_PROBE": true, "HARNESS_DOCKER_SINCE": true, DockerReportExecIDEnv: true,
	"HARNESS_DOCKER_FORWARD_ENV": true, "HARNESS_DOCKER_PRINT_FEATURES": true, AgentKeyEnv: true}

// DockerControlEnvName says whether a variable is one of the launcher's mode switches (or the agent key), which an owner's environment
// reference may not set.
func DockerControlEnvName(name string) bool { return dockerControlEnv[name] }

// DockerControlEnv is the launcher's environment for anything that is not a turn (a feature probe, a command run, an activity probe): what
// the launcher itself needs on the host and the owner's environment references, minus the mode switches and the agent key. Nothing is
// forwarded into the container.
func DockerControlEnv(refs map[string]string) []string {
	kept := make(map[string]string, len(refs))
	for target, source := range refs {
		if !dockerControlEnv[target] {
			kept[target] = source
		}
	}
	return LauncherEnv(DockerBaseEnv(), kept)
}

// CheckDockerFeature refuses a launcher whose feature answer lacks a feature the tool depends on, with the way out in the message.
func CheckDockerFeature(features map[string]bool, tool, command, feature string) error {
	if !features[feature] {
		return fmt.Errorf("the Docker launcher %s is too old for %s: it does not %s (update harness-docker, then retry)", command, tool, featureDescription(feature))
	}
	return nil
}

// dockerFeatureTTL bounds how long a cached feature answer is trusted. The launcher's file identity catches a replaced launcher; the TTL
// catches one whose entry script is unchanged while a library it sources was updated underneath it.
const dockerFeatureTTL = 5 * time.Minute

type dockerFeatureEntry struct {
	identity string
	features map[string]bool
	at       time.Time
}

var dockerFeatureCache = struct {
	sync.Mutex
	entries map[string]dockerFeatureEntry
}{entries: map[string]dockerFeatureEntry{}}

// CachedDockerFeatures is DockerFeatures asked once per launcher and binary: the answer is kept per command and arguments, for as long as
// the launcher file is the same one (device, inode, size and modification time) and at most dockerFeatureTTL. A failed probe is not kept.
func CachedDockerFeatures(ctx context.Context, command string, args []string, dir string, env []string) (map[string]bool, error) {
	path := command
	if !filepath.IsAbs(path) {
		if found, err := exec.LookPath(command); err == nil {
			path = found
		}
	}
	identity := launcherIdentity(path)
	key := path + "\x00" + strings.Join(args, "\x00")
	if identity != "" {
		dockerFeatureCache.Lock()
		entry, found := dockerFeatureCache.entries[key]
		dockerFeatureCache.Unlock()
		if found && entry.identity == identity && time.Since(entry.at) < dockerFeatureTTL {
			return maps.Clone(entry.features), nil
		}
	}
	features, err := DockerFeatures(ctx, command, args, dir, env)
	if err != nil {
		return nil, err
	}
	if identity != "" {
		dockerFeatureCache.Lock()
		dockerFeatureCache.entries[key] = dockerFeatureEntry{identity: identity, features: maps.Clone(features), at: time.Now()}
		dockerFeatureCache.Unlock()
	}
	return features, nil
}

// launcherIdentity names one version of a launcher file; empty when it cannot be read, which disables the cache for it.
func launcherIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	identity := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity += fmt.Sprintf(":%d:%d", stat.Dev, stat.Ino)
	}
	return identity
}

// dockerExecToken is the shape of an exec ID harness-docker prints: <container id>:<session id>. The probe refuses any other.
var dockerExecToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*:[A-Za-z0-9_-]+$`)

// ValidDockerExecID says whether a token has the exec ID's shape (and a sane length).
func ValidDockerExecID(token string) bool {
	return len(token) <= 256 && dockerExecToken.MatchString(token)
}

// DockerActivity asks a Docker launcher whether a tool runs under the harness of one turn (turn_activity). The launcher prints the turn's
// exec ID on stderr at the start, before any harness output; ExecIDFilter takes it from there and Busy hands it back on the probe.
type DockerActivity struct {
	Command string
	Args    []string
	Dir     string
	// Env is the launcher's environment for the probe (DockerControlEnv): no forwarded variable and no agent key.
	Env []string
	// Timeout bounds one probe; zero means dockerActivityTimeout.
	Timeout time.Duration

	mu    sync.Mutex
	token string
	// ctx is the turn's (Bind): a cancelled turn interrupts a probe in flight instead of holding its watch loop for the probe's timeout.
	ctx  context.Context
	stop context.CancelFunc
}

// Bind ties the probes to a turn's context; Stop (a cancel, the turn's end) interrupts one in flight. Call it before the turn starts.
func (d *DockerActivity) Bind(ctx context.Context) {
	if d == nil {
		return
	}
	d.ctx, d.stop = context.WithCancel(ctx)
}

// Stop interrupts a probe in flight and answers every later one as not busy. Safe on a nil or unbound activity.
func (d *DockerActivity) Stop() {
	if d != nil && d.stop != nil {
		d.stop()
	}
}

// dockerActivityTimeout bounds one probe: the launcher answers in well under a second, and the idle check waits for it.
const dockerActivityTimeout = 5 * time.Second

// Token is the exec ID the launcher printed, empty until it has.
func (d *DockerActivity) Token() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.token
}

func (d *DockerActivity) setToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token == "" {
		d.token = token
	}
}

// Busy asks the launcher whether a process under the turn's harness began at or after since (less its two seconds). busy and starting (the
// session's `docker exec` is still pending) are activity; idle, and any failure (no token yet, an unknown or ended token, a timeout), is
// not: the idle deadline then keeps its old meaning, it never guesses.
func (d *DockerActivity) Busy(since time.Time) bool {
	token := d.Token()
	if token == "" {
		return false
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = dockerActivityTimeout
	}
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	answer, err := ProbeDockerActivity(ctx, d.Command, d.Args, d.Dir, d.Env, token, since)
	return err == nil && (answer == "busy" || answer == "starting")
}

// ProbeDockerActivity runs `HARNESS_DOCKER_PROBE=<token> HARNESS_DOCKER_SINCE=<unix seconds> <launcher>` and returns its answer: busy,
// idle or starting. Anything else is an error.
func ProbeDockerActivity(ctx context.Context, command string, args []string, dir string, env []string, token string, since time.Time) (string, error) {
	if !ValidDockerExecID(token) {
		return "", errors.New("malformed exec ID")
	}
	seconds := max(since.Unix(), 0)
	probe := exec.CommandContext(ctx, command, args...)
	probe.Dir = dir
	probe.Env = append(append([]string(nil), env...), "HARNESS_DOCKER_PROBE="+token, "HARNESS_DOCKER_SINCE="+strconv.FormatInt(seconds, 10))
	out, err := cmdprobe.Output(probe)
	if err != nil {
		return "", fmt.Errorf("Docker activity probe: %w", err)
	}
	answer := strings.TrimSpace(string(out))
	switch answer {
	case "busy", "idle", "starting":
		return answer, nil
	}
	return "", fmt.Errorf("Docker activity probe: unexpected answer %q", SanitizeStderr(answer))
}

// execIDScanLimit is how much of the stderr is searched for the exec ID line: the launcher prints it before `docker exec` starts, so it
// is among the first lines or not there at all.
const execIDScanLimit = 64 << 10

// ExecIDFilter passes a launcher's stderr on to dst, except the exec ID line, which it hands to the activity. Close flushes a last line
// without a newline.
type ExecIDFilter struct {
	dst      io.Writer
	activity *DockerActivity
	buf      []byte
	scanned  int
	done     bool
}

// Filter wraps dst so the exec ID line is taken out of the launcher's stderr. A nil activity returns dst unchanged (as an io.WriteCloser
// whose Close does nothing).
func (d *DockerActivity) Filter(dst io.Writer) io.WriteCloser {
	if d == nil {
		return nopWriteCloser{dst}
	}
	return &ExecIDFilter{dst: dst, activity: d}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (f *ExecIDFilter) Write(p []byte) (int, error) {
	if f.done {
		_, _ = f.dst.Write(p)
		return len(p), nil
	}
	f.buf = append(f.buf, p...)
	for !f.done {
		end := bytes.IndexByte(f.buf, '\n')
		if end < 0 {
			if len(f.buf) > 4096 { // a line this long is no exec ID line
				f.done = true
			}
			break
		}
		line := f.buf[:end+1]
		if value, found := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte(DockerExecIDPrefix)); found && ValidDockerExecID(string(value)) {
			f.activity.setToken(string(value))
			f.done = true
		} else {
			_, _ = f.dst.Write(line)
			f.scanned += len(line)
			f.done = f.scanned >= execIDScanLimit
		}
		f.buf = append(f.buf[:0], f.buf[end+1:]...)
	}
	if f.done && len(f.buf) > 0 {
		_, _ = f.dst.Write(f.buf)
		f.buf = nil
	}
	return len(p), nil
}

// Close writes out what is still held (a last line without a newline).
func (f *ExecIDFilter) Close() error {
	if len(f.buf) > 0 {
		_, _ = f.dst.Write(f.buf)
		f.buf = nil
	}
	f.done = true
	return nil
}
