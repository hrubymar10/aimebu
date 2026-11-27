package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// A warm process in a container is gone once its container is. The launcher says which container it starts the process in (an ID, never a
// name: see adapter.ContainerIdentity), and when it then does not confirm the end of the process, Docker is asked about that ID. A
// container that is no longer there, or is there and not running, runs nothing; one that was made again has another ID, so the old one is
// simply not there. Anything Docker cannot say leaves the question open, and the quarantine answers it as before.
var (
	// warmContainerPollEvery is how often the watcher of a stuck process asks Docker about its container, between its own retries.
	warmContainerPollEvery = 5 * time.Second
	// warmContainerSightEvery and warmContainerSightWithin bound the wait for a new process's launcher to say its container: it does so
	// before its `docker exec`, well within a second.
	warmContainerSightEvery  = 250 * time.Millisecond
	warmContainerSightWithin = time.Minute
	// warmContainerSightTries is how often Docker is asked for a first look at a container, a couple of seconds apart.
	warmContainerSightTries = 3
	warmContainerSightRetry = 2 * time.Second
	// warmContainerQueryLimit bounds one question to Docker.
	warmContainerQueryLimit = 5 * time.Second
)

// containerState asks what state a container is in: Docker's own word ("running", "exited", "removing", ...), or "" when Docker knows no
// container with that ID. An error says Docker could not be asked, which says nothing about the container.
func (c *Client) containerState(id string) (string, error) {
	if !adapter.ValidDockerContainerID(id) {
		return "", errors.New("not a container ID")
	}
	if c.ContainerState != nil {
		return c.ContainerState(id)
	}
	return dockerContainerState(id)
}

// dockerDaemonID identifies the daemon queried by this supervisor's Docker CLI.
func (c *Client) dockerDaemonID() (string, error) {
	if c.DockerDaemonID != nil {
		return c.DockerDaemonID()
	}
	out, err := dockerOutputWithin(warmContainerQueryLimit, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", errors.New("Docker did not report a daemon ID")
	}
	return id, nil
}

// dockerContainerState lists the container by its ID, stopped ones too. `docker ps` answers with nothing and no error for an ID it does not
// know, and with an error when the daemon cannot be reached, which is the difference that matters here.
func dockerContainerState(id string) (string, error) {
	out, err := dockerOutputWithin(warmContainerQueryLimit, "ps", "--all", "--no-trunc", "--filter", "id="+id, "--format", "{{.ID}} {{.State}}")
	if err != nil {
		return "", err
	}
	return parseContainerState(out, id)
}

// parseContainerState reads `docker ps --format '{{.ID}} {{.State}}'`. The filter matches the ID anywhere in a container's own, so only a line
// whose ID begins with the one asked for counts.
func parseContainerState(out, id string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], id) {
			continue
		}
		if len(fields) != 2 {
			return "", fmt.Errorf("docker did not report the state of container %s", shortContainer(id))
		}
		return strings.ToLower(fields[1]), nil
	}
	return "", nil
}

func shortContainer(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sightContainer takes a first look at the container of a new warm process, once its launcher has said which it is. Only a container Docker
// has shown here once is later believed gone when Docker no longer shows it: a launcher that talks to another Docker than the supervisor's
// own `docker` would otherwise make every container look gone.
func (c *Client) sightContainer(pool *warmPool, entry *warmEntry) {
	session, ok := entry.session.(adapter.ContainerSession)
	if !entry.containerized || !ok {
		return
	}
	pool.watchers.Add(1)
	telemetry.Go(context.Background(), func() {
		defer pool.watchers.Done()
		within := time.NewTimer(warmContainerSightWithin)
		defer within.Stop()
		exited := entry.session.Exited()
		tries := 0
		for {
			wait := warmContainerSightEvery
			if id, neverStarted := session.Container(); id != "" {
				if proof, ok := entry.session.(interface{ ContainerToken() string }); ok && c.Journal != nil {
					if token := proof.ContainerToken(); token != "" {
						if err := c.Journal.RecordWarmExecToken(context.Background(), entry.session.Pid(), token, entry.journalStarted); err != nil {
							c.report("recording a warm exec token failed", err)
						}
					}
				}
				state, err := c.containerState(id)
				if err == nil && state != "" {
					entry.containerSeen.Store(true)
					if c.Journal != nil {
						if daemonID, daemonErr := c.dockerDaemonID(); daemonErr == nil {
							if writeErr := c.Journal.RecordWarmContainerSight(context.Background(), entry.session.Pid(), entry.journalStarted, daemonID); writeErr != nil {
								c.report("recording a warm container sight failed", writeErr)
							}
						} else {
							c.report("identifying the warm container daemon failed", daemonErr)
						}
					}
					return
				}
				if tries++; tries >= warmContainerSightTries {
					log.Printf("agent %s: Docker does not show container %s its warm process was started in (%v); its end will be confirmed by its launcher only", entry.agentID, shortContainer(id), err)
					return
				}
				wait = warmContainerSightRetry
			} else if neverStarted || exited == nil {
				return // the launcher is gone and named no container
			}
			timer := time.NewTimer(wait)
			select {
			case <-exited:
				exited = nil // one more look: the launcher may have said its container just before it ended
			case <-pool.ctx.Done():
				timer.Stop()
				return
			case <-within.C:
				timer.Stop()
				return
			case <-timer.C:
			}
			timer.Stop()
		}
	})
}

// warmContainerGone says the process of a warm entry whose launcher did not confirm its end is gone all the same, and how that is known. It
// asks Docker, so it is not for a caller that must not wait (see warmContainerQueryLimit).
func (c *Client) warmContainerGone(entry *warmEntry) (why string, gone bool) {
	if !entry.containerized || entry.session.Alive() {
		return "", false // while the host wrapper lives, the slot is its own
	}
	session, ok := entry.session.(adapter.ContainerSession)
	if !ok {
		return "", false
	}
	id, neverStarted := session.Container()
	if neverStarted {
		return "its launcher ended before it started anything in a container", true
	}
	if id == "" {
		return "", false
	}
	if entry.endProbeCommand != "" {
		if proof, ok := entry.session.(interface {
			ContainerToken() string
			EndReportEnabled() bool
		}); ok && proof.EndReportEnabled() {
			token := proof.ContainerToken()
			if token != "" {
				ctx, cancel := context.WithTimeout(context.Background(), warmContainerQueryLimit)
				answer, err := adapter.ProbeDockerActivity(ctx, entry.endProbeCommand, entry.endProbeArgs, entry.endProbeDir, entry.endProbeEnv, token, time.Time{})
				cancel()
				if err == nil && answer == "ended" {
					return "its launcher confirmed the exec session ended", true
				}
			}
		}
	}
	state, err := c.containerState(id)
	if err != nil {
		if entry.containerCheckFailed.CompareAndSwap(false, true) {
			c.reportLocal(fmt.Errorf("agent %s: Docker could not be asked about container %s of its warm process, so its quarantine stands: %w", entry.agentID, shortContainer(id), err))
		}
		return "", false
	}
	switch state {
	case "":
		if !entry.containerSeen.Load() {
			return "", false // never shown here: its absence proves nothing
		}
		return "its container " + shortContainer(id) + " no longer exists", true
	case "removing", "exited", "dead":
		return "its container " + shortContainer(id) + " is " + state, true
	}
	return "", false
}
