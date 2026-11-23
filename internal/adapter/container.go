package adapter

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
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
	neverStarted bool
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
	return &containerIdentityFilter{ExecIDFilter: ExecIDFilter{dst: dst, activity: activity}, identity: c, activity: activity}
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
	if ValidDockerContainerID(container) {
		f.identity.mu.Lock()
		f.identity.id = container
		f.identity.mu.Unlock()
	}
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
