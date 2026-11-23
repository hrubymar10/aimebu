package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

const (
	containerOne = "1111111111111111111111111111111111111111111111111111111111111111"
	containerTwo = "2222222222222222222222222222222222222222222222222222222222222222"
)

// fakeDocker answers the supervisor's question about a container from a table, the way `docker ps --all --filter id=` does: the state of a
// container it has, nothing for one it does not, and an error while it is down. No test here asks a real Docker.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]string
	down       error
	asked      int
}

func (d *fakeDocker) state(id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked++
	if d.down != nil {
		return "", d.down
	}
	return d.containers[id], nil
}

func (d *fakeDocker) set(change func(*fakeDocker)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	change(d)
}

func (d *fakeDocker) questions() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

// containerHarness is a warm harness whose agent runs in containerOne, with the watcher's own retry far away and its look at the container
// frequent, so that what settles a stuck slot in these tests is the container and nothing else.
func containerHarness(t *testing.T, quarantine time.Duration) (*warmHarness, *fakeDocker) {
	t.Helper()
	oldRetry, oldQuarantine, oldPoll, oldSight, oldSightRetry := warmRetryEvery, warmStuckQuarantine, warmContainerPollEvery, warmContainerSightEvery, warmContainerSightRetry
	warmRetryEvery, warmStuckQuarantine, warmContainerPollEvery, warmContainerSightEvery, warmContainerSightRetry = time.Hour, quarantine, 10*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	h := newWarmHarness(t)
	t.Cleanup(func() {
		// The watchers read the intervals: they are ended, and waited for, before the intervals go back.
		pool := h.c.pool()
		h.c.shutdownWarm()
		pool.watchers.Wait()
		warmRetryEvery, warmStuckQuarantine, warmContainerPollEvery, warmContainerSightEvery, warmContainerSightRetry = oldRetry, oldQuarantine, oldPoll, oldSight, oldSightRetry
	})
	docker := &fakeDocker{containers: map[string]string{containerOne: "running"}}
	h.c.ContainerState = docker.state
	h.f.container = containerOne
	h.spawn("agt_a")
	return h, docker
}

func containerTurn(s *adapter.TurnSpec) { s.LauncherContainerized = true }

// seen waits for the supervisor's first look at the container of the agent's idle process.
func (h *warmHarness) seen(agent string) {
	h.t.Helper()
	slot := h.c.pool().slot(agent)
	eventually(h.t, "the first look at the container", func() bool {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		return slot.idle != nil && slot.idle.containerSeen.Load()
	})
}

func (h *warmHarness) stuck(agent string) bool {
	slot := h.c.pool().slot(agent)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.stuck != nil
}

// A warm process whose launcher does not confirm its end is confirmed gone at once when its container is: no quarantine, no report of an
// uncertain end, the journal's note dropped, and the next turn starts a process without waiting.
func TestWarmContainerGoneConfirmsTheEndAtOnce(t *testing.T) {
	for name, change := range map[string]func(*fakeDocker){
		"removed":  func(d *fakeDocker) { delete(d.containers, containerOne) },
		"removing": func(d *fakeDocker) { d.containers[containerOne] = "removing" },
		"exited":   func(d *fakeDocker) { d.containers[containerOne] = "exited" },
		"made again with another ID": func(d *fakeDocker) {
			delete(d.containers, containerOne)
			d.containers[containerTwo] = "running"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, docker := containerHarness(t, time.Hour)
			reports := countWarmReports(h.c)
			h.run("agt_a", containerTurn)
			h.seen("agt_a")
			h.f.sessions[0].lingers.Store(true)
			docker.set(change)
			if !h.c.retireWarm("agt_a", "test") {
				t.Fatal("a process whose container is gone was not confirmed gone")
			}
			if h.stuck("agt_a") || h.c.warmLive.Load() != 0 || reports.Load() != 0 {
				t.Fatalf("stuck=%t live=%d reports=%d", h.stuck("agt_a"), h.c.warmLive.Load(), reports.Load())
			}
			if noted, err := h.c.Journal.WarmProcesses(context.Background()); err != nil || len(noted) != 0 {
				t.Fatalf("the journal still notes the process: %+v %v", noted, err)
			}
			h.f.container = containerTwo
			h.run("agt_a", containerTurn)
			if h.f.started != 2 {
				t.Fatalf("the next turn did not start a new process: %d", h.f.started)
			}
		})
	}
}

// While the container is still there and running, nothing is known about the process in it: the quarantine holds as before, however often
// Docker is asked, and ends at its own deadline.
func TestWarmContainerStillThereKeepsTheQuarantine(t *testing.T) {
	h, docker := containerHarness(t, 300*time.Millisecond)
	reports := countWarmReports(h.c)
	h.run("agt_a", containerTurn)
	h.seen("agt_a")
	h.f.sessions[0].lingers.Store(true)
	started := time.Now()
	if h.c.retireWarm("agt_a", "test") {
		t.Fatal("an unconfirmed end in a running container was treated as confirmed")
	}
	asked := docker.questions()
	eventually(t, "the watcher asking Docker again", func() bool { return docker.questions() >= asked+3 })
	if !h.stuck("agt_a") && time.Since(started) < 300*time.Millisecond {
		t.Fatal("the slot was released while its container was running and its quarantine was not over")
	}
	eventually(t, "the release at the end of the quarantine", func() bool { return !h.stuck("agt_a") })
	if time.Since(started) < 300*time.Millisecond {
		t.Fatalf("released after %v, before the quarantine was over", time.Since(started))
	}
	if reports.Load() != 1 {
		t.Fatalf("the uncertain end was reported %d times, want once", reports.Load())
	}
}

// A slot that went stuck with its container running is released as soon as that container goes: the watcher looks at it between its own
// retries (an hour apart here), so the release does not wait for the backoff or the quarantine.
func TestWarmStuckSlotIsReleasedWhenItsContainerGoes(t *testing.T) {
	h, docker := containerHarness(t, time.Hour)
	h.run("agt_a", containerTurn)
	h.seen("agt_a")
	h.f.sessions[0].lingers.Store(true)
	if h.c.retireWarm("agt_a", "test") {
		t.Fatal("an unconfirmed end in a running container was treated as confirmed")
	}
	time.Sleep(50 * time.Millisecond)
	if !h.stuck("agt_a") {
		t.Fatal("the slot was released while its container was running")
	}
	// The container is made again: the old ID is gone, a new one runs.
	docker.set(func(d *fakeDocker) {
		delete(d.containers, containerOne)
		d.containers[containerTwo] = "running"
	})
	eventually(t, "the release once the container is gone", func() bool { return !h.stuck("agt_a") && h.c.warmLive.Load() == 0 })
	if noted, err := h.c.Journal.WarmProcesses(context.Background()); err != nil || len(noted) != 0 {
		t.Fatalf("the journal still notes the process: %+v %v", noted, err)
	}
}

// A Docker that cannot be asked says nothing about the container: the quarantine is the fallback, the failure is noted once, and the slot is
// released at the quarantine's end as it always was.
func TestWarmContainerCheckThatFailsKeepsTheQuarantine(t *testing.T) {
	h, docker := containerHarness(t, 300*time.Millisecond)
	var failures atomic.Int32
	h.c.OnError = func(err error) {
		if strings.Contains(err.Error(), "Docker could not be asked") {
			failures.Add(1)
		}
	}
	h.run("agt_a", containerTurn)
	h.seen("agt_a")
	h.f.sessions[0].lingers.Store(true)
	docker.set(func(d *fakeDocker) { d.down = errors.New("Cannot connect to the Docker daemon") })
	started := time.Now()
	if h.c.retireWarm("agt_a", "test") {
		t.Fatal("a container that could not be checked was treated as gone")
	}
	asked := docker.questions()
	eventually(t, "the watcher asking Docker again", func() bool { return docker.questions() >= asked+3 })
	eventually(t, "the release at the end of the quarantine", func() bool { return !h.stuck("agt_a") })
	if time.Since(started) < 300*time.Millisecond {
		t.Fatalf("released after %v, before the quarantine was over", time.Since(started))
	}
	if failures.Load() != 1 {
		t.Fatalf("the failed check was noted %d times, want once", failures.Load())
	}
}

// What is not real container identity confirms nothing, and Docker's silence about a container it never showed proves nothing either.
func TestWarmContainerIdentityIsNeverGuessed(t *testing.T) {
	t.Run("the launcher named no container", func(t *testing.T) {
		h, docker := containerHarness(t, time.Hour)
		h.f.container = ""
		h.run("agt_a", containerTurn)
		h.f.sessions[0].lingers.Store(true)
		docker.set(func(d *fakeDocker) { delete(d.containers, containerOne) })
		if h.c.retireWarm("agt_a", "test") || !h.stuck("agt_a") {
			t.Fatal("a process with no known container was confirmed gone")
		}
		if docker.questions() != 0 {
			t.Fatalf("Docker was asked %d times about nothing", docker.questions())
		}
	})
	t.Run("the launcher named a container by name", func(t *testing.T) {
		h, docker := containerHarness(t, time.Hour)
		h.f.container = "harness-docker-g7-harness-1"
		h.run("agt_a", containerTurn)
		h.f.sessions[0].lingers.Store(true)
		if h.c.retireWarm("agt_a", "test") || !h.stuck("agt_a") {
			t.Fatal("a container known only by name was taken as gone")
		}
		if docker.questions() != 0 {
			t.Fatalf("Docker was asked %d times about a name", docker.questions())
		}
	})
	t.Run("Docker never showed the container", func(t *testing.T) {
		// The launcher talks to another Docker than the supervisor's: this one knows no such container, at the start or later.
		h, docker := containerHarness(t, time.Hour)
		docker.set(func(d *fakeDocker) { delete(d.containers, containerOne) })
		h.run("agt_a", containerTurn)
		eventually(t, "the first looks at the container", func() bool { return docker.questions() >= warmContainerSightTries })
		h.f.sessions[0].lingers.Store(true)
		if h.c.retireWarm("agt_a", "test") || !h.stuck("agt_a") {
			t.Fatal("a container Docker never showed was taken as gone")
		}
	})
	t.Run("the host wrapper is still alive", func(t *testing.T) {
		h, docker := containerHarness(t, time.Hour)
		h.run("agt_a", containerTurn)
		h.seen("agt_a")
		h.f.sessions[0].killFails.Store(true)
		docker.set(func(d *fakeDocker) { delete(d.containers, containerOne) })
		if h.c.retireWarm("agt_a", "test") || !h.stuck("agt_a") {
			t.Fatal("a process whose host wrapper lives was released")
		}
		h.f.sessions[0].killFails.Store(false)
		h.f.sessions[0].lingers.Store(true)
	})
}

// A launcher that was asked for its container and ended without naming one started nothing in any container: its end needs no quarantine
// and no question to Docker.
func TestWarmLauncherThatStartedNothingIsConfirmedGone(t *testing.T) {
	h, docker := containerHarness(t, time.Hour)
	reports := countWarmReports(h.c)
	h.f.container, h.f.neverStartedNext = "", true
	h.f.next = "fail"
	h.run("agt_a", containerTurn)
	if h.stuck("agt_a") || h.c.warmLive.Load() != 0 || reports.Load() != 0 || docker.questions() != 0 {
		t.Fatalf("stuck=%t live=%d reports=%d questions=%d", h.stuck("agt_a"), h.c.warmLive.Load(), reports.Load(), docker.questions())
	}
}

// What `docker ps --all --no-trunc --filter id=<id> --format '{{.ID}} {{.State}}'` prints is read strictly: only a line whose ID begins
// with the one asked for counts (the filter matches anywhere in an ID), and no such line means Docker has no such container.
func TestParseContainerState(t *testing.T) {
	short := containerOne[:12]
	cases := []struct {
		name, out, id, want string
		fails               bool
	}{
		{"running", containerOne + " running\n", containerOne, "running", false},
		{"a short ID asked for", containerOne + " removing\n", short, "removing", false},
		{"none", "", containerOne, "", false},
		{"only another container whose ID contains it", "ab" + containerOne[:62] + " running\n", short, "", false},
		{"among others", containerTwo + " running\n" + containerOne + " Exited\n", containerOne, "exited", false},
		{"no state", containerOne + "\n", containerOne, "", true},
	}
	for _, tc := range cases {
		got, err := parseContainerState(tc.out, tc.id)
		if got != tc.want || (err != nil) != tc.fails {
			t.Errorf("%s: %q %v, want %q (error %t)", tc.name, got, err, tc.want, tc.fails)
		}
	}
	c := &Client{}
	if _, err := c.containerState("harness-1"); err == nil {
		t.Error("a name was asked about as if it were an ID")
	}
}

// An account switch's retire names a stuck process in a container that still runs, and confirms it gone once the container is (the retire asks
// Docker itself; the watcher may get there first, which is the same result).
func TestRetireForASwitchNamesAStuckContainerAndSettlesAGoneOne(t *testing.T) {
	h, docker := containerHarness(t, time.Hour)
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true; s.Agent.Tool = "claude" })
	h.seen("agt_a")
	h.f.sessions[0].lingers.Store(true)
	if blocked := h.c.retireWarmToolBlocking("claude", "an account switch"); len(blocked) != 1 || blocked[0] != "agt_a" {
		t.Fatalf("a process in a running container is not confirmed gone and is named: %v", blocked)
	}
	if !h.stuck("agt_a") {
		t.Fatal("the unconfirmed process must stay quarantined")
	}
	docker.set(func(d *fakeDocker) { d.containers[containerOne] = "exited" })
	if blocked := h.c.retireWarmToolBlocking("claude", "an account switch"); len(blocked) != 0 {
		t.Fatalf("a process whose container exited is confirmed gone by the retire itself: %v", blocked)
	}
	if h.stuck("agt_a") || h.c.warmLive.Load() != 0 {
		t.Fatalf("stuck=%t live=%d", h.stuck("agt_a"), h.c.warmLive.Load())
	}
}
