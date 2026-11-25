package supervisor

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// The journals of one machine's supervisor instances live beside each other under the supervisor directory: the legacy journal.sqlite (what a
// build from before instances holds) and journals/<name>.sqlite, one per instance started next to it. A manager that keeps instances running
// (cmd/aimebu) finds them, and what they owe, with these.

// LegacyJournalName is the journal of the instance that lives at the old path.
const LegacyJournalName = "journal.sqlite"

// InstanceJournalDir is the directory of the journals of instances started next to another.
func InstanceJournalDir(supervisorDir string) string { return filepath.Join(supervisorDir, "journals") }

// NewInstanceJournalPath names a journal for a new instance: time-ordered, so listings read oldest first, and random so two managers never
// pick the same one.
func NewInstanceJournalPath(supervisorDir string, now time.Time) (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%x.sqlite", now.UTC().Format("20060102T150405"), random)
	return filepath.Join(InstanceJournalDir(supervisorDir), name), nil
}

// InstanceJournals lists the journal files that exist: the legacy one first, then the others by name.
func InstanceJournals(supervisorDir string) []string {
	var out []string
	legacy := filepath.Join(supervisorDir, LegacyJournalName)
	if _, err := os.Stat(legacy); err == nil {
		out = append(out, legacy)
	}
	found, err := filepath.Glob(filepath.Join(InstanceJournalDir(supervisorDir), "*.sqlite"))
	if err == nil {
		sort.Strings(found)
		out = append(out, found...)
	}
	return out
}

// IsLegacyJournal says path is the journal of the instance at the old path.
func IsLegacyJournal(path string) bool {
	return filepath.Base(filepath.Dir(path)) != "journals" && filepath.Base(path) == LegacyJournalName
}

// JournalState is what a journal whose lock is free holds.
type JournalState struct {
	ID string
	// Owes is true when something in it is still open: a turn, a command, an end or answer the server has not acknowledged, or a warm process
	// the dead holder left. A process that opens it finishes that and its ends.
	Owes bool
}

// InspectJournal opens a journal whose lock is free, reads what it owes and lets it go again. It returns ErrJournalInUse when someone holds it.
func InspectJournal(ctx context.Context, path string) (JournalState, error) {
	j, err := OpenJournal(ctx, path)
	if err != nil {
		return JournalState{}, err
	}
	defer j.Close()
	return JournalState{ID: j.ID, Owes: j.leftOwed(ctx)}, nil
}

// RemoveJournal deletes the files of a journal that owes nothing and whose lock is free. It refuses (false, nil) when the journal owes
// something or is held, so a manager can call it on whatever it finds.
func RemoveJournal(ctx context.Context, path string) (bool, error) {
	j, err := OpenJournal(ctx, path)
	if err != nil {
		if errors.Is(err, ErrJournalInUse) {
			return false, nil
		}
		return false, err
	}
	if j.leftOwed(ctx) {
		return false, j.Close()
	}
	return true, j.Remove()
}

// leftOwed is Owes for a journal whose holder is dead: its recorded warm processes count too, as the sweep at the start of the process that opens
// the journal is what ends them.
func (j *Journal) leftOwed(ctx context.Context) bool {
	if j.Owes(ctx) {
		return true
	}
	warm, err := j.WarmProcesses(ctx)
	return err != nil || len(warm) > 0
}

// Owes says the journal still has something open (see JournalState.Owes; the warm processes are the process's own business while it lives). An error reading it counts as owing: nothing is deleted on a guess.
func (j *Journal) Owes(ctx context.Context) bool {
	if agents, err := j.OpenTurns(ctx); err != nil || len(agents) > 0 {
		return true
	}
	if open, err := j.OpenCommands(ctx); err != nil || len(open) > 0 {
		return true
	}
	if exits, err := j.PendingExits(ctx); err != nil || len(exits) > 0 {
		return true
	}
	if answers, err := j.PendingAnswers(ctx); err != nil || len(answers) > 0 {
		return true
	}
	if exits, err := j.PendingCommandExits(ctx); err != nil || len(exits) > 0 {
		return true
	}
	return false
}

// SupervisorDirOf is the supervisor directory a journal path lies in, whether it is the legacy journal or one in journals/.
func SupervisorDirOf(journalPath string) string {
	dir := filepath.Dir(journalPath)
	if filepath.Base(dir) == "journals" {
		dir = filepath.Dir(dir)
	}
	return dir
}

// EndDeadTurns records an exit for every turn the journal still lists as running: the process that ran it is gone with its holder. Each exit is then
// reported and acknowledged like any other, which is how a drain instance empties the journal of a dead one. It returns how many turns it ended.
func (j *Journal) EndDeadTurns(ctx context.Context, exit adapter.ExitInfo) (int, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT turn_id FROM turn_start WHERE exec='running' ORDER BY started_at,turn_id`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := j.RecordExitInfo(ctx, id, exit); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
