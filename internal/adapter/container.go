package adapter

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ContainerSession is implemented by a warm session that runs through a Docker launcher and can say which container its process was started
// in. The supervisor asks it when the launcher did not confirm the end of the process: a process is gone once its container is.
type ContainerSession interface {
	// Container is what the launcher said at the start. id is the ID of the container the process was started in, "" when it is not known
	// (the launcher has not said yet, cannot say, or printed something that is not an ID). neverStarted says the launcher was asked, can say,
	// and ended without saying: it ended before its `docker exec`, so nothing of it ever ran in a container.
	Container() (id string, neverStarted bool)
}

// dockerContainerID is the shape of a container ID as Docker prints it: 12 hexadecimal digits when shortened, 64 in full. A name is refused:
// a container that is made again keeps its name, so a name says nothing about whether the old one is gone.
var dockerContainerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// ValidDockerContainerID says whether text has the shape of a container ID (not of a name).
func ValidDockerContainerID(text string) bool { return dockerContainerID.MatchString(text) }

// ContainerIdentity records which container a launch runs in, from the exec ID line a launcher with turn_activity prints on stderr before
// its `docker exec` starts (DockerReportExecIDEnv asks for it). The launcher resolves the container once, to an ID, and that ID is what its
// process runs in for as long as it lives.
type ContainerIdentity struct {
	mu           sync.Mutex
	id           string
	token        string
	reportEnd    bool
	confirmed    bool
	ended        chan struct{}
	reportDone   chan struct{}
	reportToken  string
	neverStarted bool
}

// EnableEndReport opts a warm launch into session-scoped end evidence.
func (c *ContainerIdentity) EnableEndReport() {
	c.mu.Lock()
	c.reportEnd = true
	c.ended = make(chan struct{})
	c.reportDone = make(chan struct{})
	c.mu.Unlock()
}

// AttachEndReport gives the launcher a private fd 3, separate from docker
// exec stderr. The caller closes write immediately after cmd.Start and runs
// ReadEndReport on read. No other child may inherit fd 3.
func (c *ContainerIdentity) AttachEndReport(cmd *exec.Cmd) (read, write *os.File, err error) {
	if len(cmd.ExtraFiles) != 0 {
		return nil, nil, fmt.Errorf("session end fd 3 is occupied")
	}
	read, write, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	c.EnableEndReport()
	cmd.ExtraFiles = []*os.File{write}
	cmd.Env = append(cmd.Env, DockerSessionEndFDEnv+"=3")
	return read, write, nil
}

// ReadEndReport accepts at most one complete line, and only after EOF. This
// ensures a second record cannot invalidate a release that already happened.
func (c *ContainerIdentity) ReadEndReport(read *os.File) {
	defer read.Close()
	defer close(c.reportDone)
	scanner := bufio.NewScanner(read)
	scanner.Buffer(make([]byte, 128), 4096)
	count := 0
	var token, how string
	for scanner.Scan() {
		count++
		if count > 1 {
			return
		}
		kind, value, reason := parseDockerReportLine(scanner.Bytes())
		if kind == "end" {
			token, how = value, reason
		}
	}
	if scanner.Err() == nil && count == 1 {
		c.trustEndReport(token, how)
	}
}

// WaitEndReport lets a reaped launcher finish its private-fd read before the
// caller classifies a missing report. A leaked writer cannot hold close forever.
func (c *ContainerIdentity) WaitEndReport(limit time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	done := c.reportDone
	c.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(limit):
	}
}

func (c *ContainerIdentity) Token() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *ContainerIdentity) EndConfirmed() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confirmed
}

func (c *ContainerIdentity) Ended() <-chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended
}

func (c *ContainerIdentity) ReportEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportEnd
}

// trustEndReport is the one place that accepts a private-fd record. Stderr
// records never call it; they may come from the untrusted container process.
func (c *ContainerIdentity) trustEndReport(token, how string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	validHow := how == "exited" || how == "stopped" || how == "killed" || how == "cancelled"
	if validHow {
		c.reportToken = token
	}
	c.confirmEndLocked()
}

func (c *ContainerIdentity) confirmEndLocked() {
	if c.reportEnd && c.token != "" && c.reportToken == c.token && !c.confirmed {
		c.confirmed = true
		close(c.ended)
	}
}

// Container implements ContainerSession for the session that owns the identity. A nil identity knows nothing.
func (c *ContainerIdentity) Container() (id string, neverStarted bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id, c.neverStarted
}

// Filter wraps dst so the exec ID line is taken out of the launcher's stderr and its container ID kept. Close it when the stderr has been
// read to its end: only then can the identity say the launcher never started anything. A nil identity returns dst unchanged.
func (c *ContainerIdentity) Filter(dst io.Writer) io.WriteCloser {
	if c == nil {
		return nopWriteCloser{dst}
	}
	activity := &DockerActivity{}
	return &containerIdentityFilter{ExecIDFilter: ExecIDFilter{dst: dst, activity: activity, identity: c}, identity: c, activity: activity}
}

type containerIdentityFilter struct {
	ExecIDFilter
	identity *ContainerIdentity
	activity *DockerActivity
	noted    bool
}

func (f *containerIdentityFilter) Write(p []byte) (int, error) {
	n, err := f.ExecIDFilter.Write(p)
	f.note()
	return n, err
}

func (f *containerIdentityFilter) note() {
	if f.noted {
		return
	}
	token := f.activity.Token()
	if token == "" {
		return
	}
	f.noted = true
	container, _, _ := strings.Cut(token, ":")
	f.identity.mu.Lock()
	f.identity.token = token
	f.identity.confirmEndLocked()
	if ValidDockerContainerID(container) {
		f.identity.id = container
	}
	f.identity.mu.Unlock()
}

// Close ends the stream. A stream that ended with every line read and no exec ID line among them is a launcher that never reached its
// `docker exec`; one the filter gave up on (too much output, an overlong line) or that ends in a cut-off exec ID line says nothing.
func (f *containerIdentityFilter) Close() error {
	gaveUp := f.done
	cutOff := bytes.HasPrefix(f.buf, []byte(DockerExecIDPrefix))
	err := f.ExecIDFilter.Close()
	f.note()
	if !f.noted && !gaveUp && !cutOff {
		f.identity.mu.Lock()
		f.identity.neverStarted = true
		f.identity.mu.Unlock()
	}
	return err
}
