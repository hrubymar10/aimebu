package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func commandFixture(t *testing.T) (*Store, Project, Room, Agent) {
	t.Helper()
	s, project, room, worker, _ := wakeFixture(t)
	return s, project, room, worker
}

// A command that ends wakes its agent with a note in the room it was started from: the exit code, the tail of the output and where the whole
// log is.
func TestCommandFinishWakesTheAgentWithTheTail(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	job, reused, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, Generation: 0, MachineID: "mch_wake",
		ProjectID: project.ID, RoomID: room.ID, Label: "test gate", Cmd: "go test ./...", Cwd: ".",
		CheckoutPath: "/tmp/wake", LogPath: "/tmp/wake/.aimebu/commands/x.log"},
		RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "k1", Digest: "d1"})
	if err != nil || reused || job.ID == "" {
		t.Fatalf("create: %+v reused=%v err=%v", job, reused, err)
	}
	if job.State != "queued" {
		t.Fatalf("state %q", job.State)
	}
	code := 0
	finished, err := s.FinishCommand(ctx, job.ID, CommandResult{State: "done", ExitCode: &code,
		EndedAt: "2026-11-18T22:00:00.000000000Z", Tail: "line one\nline two", LogBytes: 42})
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "done" {
		t.Fatalf("state %q", finished.State)
	}
	note, err := s.NoteForAgentMessage(ctx, worker.ID, lastMessageID(t, s, room.ID))
	if err != nil {
		t.Fatalf("nothing woke the agent: %v", err)
	}
	if note.WakeReason != "command_finished" {
		t.Fatalf("wake reason %q", note.WakeReason)
	}
	body := messageBody(t, s, note.MessageID)
	for _, want := range []string{"exit 0", "go test ./...", "line one", "/tmp/wake/.aimebu/commands/x.log"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the note is missing %q:\n%s", want, body)
		}
	}
	if why := noteWhy("command_finished", nil); why != "your command finished" {
		t.Fatalf("why %q", why)
	}
	// A replayed report of the same end changes nothing and wakes nobody twice.
	if _, err := s.FinishCommand(ctx, job.ID, CommandResult{State: "done"}); err != nil {
		t.Fatal(err)
	}
	if count := noteCount(t, s, room.ID); count != 1 {
		t.Fatalf("notes %d", count)
	}
}

// A job whose room closed or whose agent left gets no note: its result stays in the listing and the event says why nothing woke.
func TestCommandFinishWithoutWakeWhenTheRoomIsClosed(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, Generation: 0, MachineID: "mch_wake",
		ProjectID: project.ID, RoomID: room.ID, Cmd: "make gate", CheckoutPath: "/tmp/wake"},
		RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "k2", Digest: "d2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishCommand(ctx, job.ID, CommandResult{State: "done"}); err != nil {
		t.Fatal(err)
	}
	if count := noteCount(t, s, room.ID); count != 0 {
		t.Fatalf("a closed room woke the agent anyway: %d notes", count)
	}
	var reason string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.not_woken') FROM event WHERE kind='command_finished' AND agent_id=?`,
		worker.ID).Scan(&reason); err != nil || reason != "room_closed" {
		t.Fatalf("not_woken=%q err=%v", reason, err)
	}
}

// The per-agent cap bounds concurrency, and a request key makes a retry return the job it already made.
func TestCommandCapAndRequestKey(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	makeJob := func(key string) (CommandJob, bool, error) {
		return s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, Generation: 0, MachineID: "mch_wake",
			ProjectID: project.ID, RoomID: room.ID, Cmd: "sleep 1", CheckoutPath: "/tmp/wake"},
			RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: key, Digest: "digest-" + key})
	}
	first, _, err := makeJob("same")
	if err != nil {
		t.Fatal(err)
	}
	again, reused, err := makeJob("same")
	if err != nil || !reused || again.ID != first.ID {
		t.Fatalf("the retry did not return the original job: %+v reused=%v err=%v", again, reused, err)
	}
	if _, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, Generation: 0, MachineID: "mch_wake",
		ProjectID: project.ID, RoomID: room.ID, Cmd: "other", CheckoutPath: "/tmp/wake"},
		RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "same", Digest: "different"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the same key with another digest must conflict: %v", err)
	}
	if _, _, err := makeJob("second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := makeJob("third"); !errors.Is(err, ErrCommandLimit) {
		t.Fatalf("the cap was not enforced: %v", err)
	}
	if _, err := s.CancelCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := makeJob("third"); err != nil {
		t.Fatalf("a cancelled job must free its slot: %v", err)
	}
}

// A job that never ran is recorded as failed with the reason; the tool call itself says it, so nothing wakes.
func TestCommandStartFailureWakesNobody(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, Generation: 0, MachineID: "mch_wake",
		ProjectID: project.ID, RoomID: room.ID, Cmd: "make gate", CheckoutPath: "/tmp/wake"},
		RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "k4", Digest: "d4"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.FailCommandStart(ctx, job.ID, "update harness-docker, then retry")
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != "failed" || !strings.Contains(failed.Error, "harness-docker") {
		t.Fatalf("%+v", failed)
	}
	if count := noteCount(t, s, room.ID); count != 0 {
		t.Fatalf("a refused start woke the agent: %d notes", count)
	}
}

func lastMessageID(t *testing.T, s *Store, roomID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM message WHERE room_id=? ORDER BY seq DESC LIMIT 1`, roomID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func messageBody(t *testing.T, s *Store, messageID string) string {
	t.Helper()
	var body string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE id=?`, messageID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func noteCount(t *testing.T, s *Store, roomID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE room_id=?`, roomID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// A cwd may not leave the agent's checkout, however it is spelled; one that only wanders inside it is kept, cleaned.
func TestCommandCwdStaysInsideTheCheckout(t *testing.T) {
	t.Parallel()
	for _, cwd := range []string{"..", "./..", "../x", "a/../..", "a/../../b", "/etc", "C:/x", "a\x00b"} {
		if CommandCwdInside(cwd) {
			t.Errorf("%q must be refused", cwd)
		}
	}
	for _, cwd := range []string{"", ".", "a", "a/..", "./a/b", "a/../b", "..a"} {
		if !CommandCwdInside(cwd) {
			t.Errorf("%q must be allowed", cwd)
		}
	}
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	for i, cwd := range []string{"..", "./.."} {
		_, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, MachineID: "mch_wake", ProjectID: project.ID, RoomID: room.ID,
			Cmd: "ls", Cwd: cwd, CheckoutPath: "/tmp/wake"},
			RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "cwd" + string(rune('a'+i)), Digest: cwd})
		if !errors.Is(err, ErrCommandInvalid) {
			t.Fatalf("cwd %q: %v", cwd, err)
		}
	}
	job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, MachineID: "mch_wake", ProjectID: project.ID, RoomID: room.ID,
		Cmd: "ls", Cwd: "./sub/../pkg/", CheckoutPath: "/tmp/wake"},
		RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "cwd-ok", Digest: "ok"})
	if err != nil || job.Cwd != "pkg" {
		t.Fatalf("job %+v err=%v", job, err)
	}
}

// A fast command's end can be recorded before its start: recording the start then keeps the end, and returns the row.
func TestCommandStartAfterItsEndKeepsTheEnd(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, MachineID: "mch_wake", ProjectID: project.ID, RoomID: room.ID,
		Cmd: "true", CheckoutPath: "/tmp/wake"}, RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: "fast", Digest: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := s.FinishCommand(ctx, job.ID, CommandResult{State: "done", ExitCode: &code, StartedAt: "2026-11-19T10:00:00.000000000Z"}); err != nil {
		t.Fatal(err)
	}
	started, err := s.RecordCommandStarted(ctx, job.ID, "2026-11-19T10:00:05.000000000Z", "/visible/log", "/visible")
	if err != nil {
		t.Fatal(err)
	}
	if started.State != "done" || started.StartedAt != "2026-11-19T10:00:00.000000000Z" || started.VisibleLogPath != "/visible/log" {
		t.Fatalf("job %+v", started)
	}
	if _, err := s.FailCommandStart(ctx, job.ID, "late refusal"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a finished job cannot fail its start: %v", err)
	}
	if _, err := s.RecordCommandStarted(ctx, "cmd_missing", "x", "", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no such job: %v", err)
	}
}

// An end the server only inferred (cancelled before the start was confirmed, or failed as never started) gives way to the end the machine
// reports, and to a late start; a real end, or another inference, never replaces it.
func TestInferredCommandEndGivesWayToTheMachinesEvidence(t *testing.T) {
	t.Parallel()
	s, project, room, worker := commandFixture(t)
	ctx := context.Background()
	newJob := func(key string) CommandJob {
		job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: worker.ID, MachineID: "mch_wake", ProjectID: project.ID, RoomID: room.ID,
			Cmd: "true", CheckoutPath: "/tmp/wake"}, RequestKey{ActorKind: "agent", ActorID: worker.ID, Kind: "command_run", Key: key, Digest: key})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	code := 2
	cancelled := newJob("cancelled")
	if _, err := s.CancelCommand(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if job, _ := s.FinishCommand(ctx, cancelled.ID, CommandResult{State: "lost"}); job.State != "cancelled" {
		t.Fatalf("an inference must not replace another: %+v", job)
	}
	if job, _ := s.FinishCommand(ctx, cancelled.ID, CommandResult{State: "done", ExitCode: &code, Reported: true}); job.State != "done" || job.Error != "" {
		t.Fatalf("the reported end must replace the cancel: %+v", job)
	}
	other := 9
	if job, _ := s.FinishCommand(ctx, cancelled.ID, CommandResult{State: "done", ExitCode: &other, Reported: true}); *job.ExitCode != 2 {
		t.Fatalf("a real end is never replaced: %+v", job)
	}
	never := newJob("never")
	if _, err := s.FailUnconfirmedCommand(ctx, never.ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.RecordCommandStarted(ctx, never.ID, timestamp(), "/v/log", "/v")
	if err != nil || job.State != "running" || job.Error != "" || job.EndedAt != "" {
		t.Fatalf("a late start revives a job inferred never started: %+v err=%v", job, err)
	}
	refused := newJob("refused")
	if _, err := s.FailCommandStart(ctx, refused.ID, "refused by the machine"); err != nil {
		t.Fatal(err)
	}
	if job, _ := s.RecordCommandStarted(ctx, refused.ID, timestamp(), "", ""); job.State != "failed" {
		t.Fatalf("a refusal is the machine's own answer, not an inference: %+v", job)
	}
}
