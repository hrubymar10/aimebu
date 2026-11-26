package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-json"
)

// WarmOrphanStatus is a snapshot written by the journal owner for local status
// readers. They never need to open a database held by a running supervisor.
type WarmOrphanStatus struct {
	JournalID   string                `json:"journal_id"`
	PID         int                   `json:"pid"`
	WarmOrphans []WarmOrphanStatusRow `json:"warm_orphans"`
}

type WarmOrphanStatusRow struct {
	AgentID    string `json:"agent"`
	Journal    string `json:"journal"`
	SinceUnix  int64  `json:"since_unix"`
	AgeSeconds int64  `json:"age_seconds"`
}

func warmOrphanStatusPath(journalPath string) string {
	return journalPath + ".warm-orphans.json"
}

// PublishWarmOrphanStatus atomically replaces the local status snapshot. The
// owner calls it after every orphan sweep, including one that clears a row.
func (j *Journal) PublishWarmOrphanStatus(ctx context.Context, now time.Time) error {
	j.statusMu.Lock()
	defer j.statusMu.Unlock()
	alerts, err := j.WarmOrphansAwaitingProof(ctx, now)
	if err != nil {
		return err
	}
	status := WarmOrphanStatus{JournalID: j.ID, PID: os.Getpid(), WarmOrphans: make([]WarmOrphanStatusRow, 0, len(alerts))}
	for _, alert := range alerts {
		status.WarmOrphans = append(status.WarmOrphans, WarmOrphanStatusRow{
			AgentID: alert.AgentID, Journal: filepath.Base(j.path),
			SinceUnix: now.Add(-alert.Age).Unix(), AgeSeconds: int64(alert.Age / time.Second),
		})
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(j.path), ".warm-orphans-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), warmOrphanStatusPath(j.path))
}

// ReadWarmOrphanStatus reads only the owner's status file, never its journal.
func ReadWarmOrphanStatus(journalPath string) (WarmOrphanStatus, error) {
	data, err := os.ReadFile(warmOrphanStatusPath(journalPath))
	if err != nil {
		return WarmOrphanStatus{}, err
	}
	var status WarmOrphanStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return WarmOrphanStatus{}, err
	}
	return status, nil
}
