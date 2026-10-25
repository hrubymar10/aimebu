package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
)

const (
	// orphanBlobGrace keeps a freshly uploaded blob that its row has not caught up with.
	orphanBlobGrace = time.Hour
	// scheduleWindow is how long after its hour the daily cleanup may still start.
	scheduleWindow = 6 * time.Hour
	// compactMinFree is the share of free database pages, and compactMinBytes the amount, that make a rebuild worth its lock.
	compactMinFree  = 0.20
	compactMinBytes = 32 << 20
	// lowDiskBytes and lowDiskRatio are the free space under which Health warns.
	lowDiskBytes     = 10 << 30
	lowDiskRatio     = 0.10
	cleanupLogKeep   = 90 * 24 * time.Hour
	diskReportMaxAge = 2 * time.Minute
)

// worktreeFacts is what a cleanup knows about one worktree: the row and what the machine reported about it.
type worktreeFacts struct {
	store.RoomWorktreeInstance
	Exists       bool
	SizeBytes    int64
	DirtyFiles   int
	Merged       bool
	OwnCommits   bool
	LastActivity time.Time
	Error        string
}

// worktreeState is the state Health shows: active (an agent works in it), dirty (uncommitted or generated files), merged (clean and merged),
// orphan (its room is closed but it is still there), idle (untouched for the orphan window), or ready.
func worktreeState(f worktreeFacts, orphanAfter time.Duration, now time.Time) string {
	switch {
	case f.InUse:
		return "active"
	case f.DirtyFiles > 0:
		return "dirty"
	case f.Merged && (f.OwnCommits || f.Lifecycle == "closed"):
		return "merged"
	case f.Lifecycle == "closed":
		return "orphan"
	case !f.LastActivity.IsZero() && now.Sub(f.LastActivity) > orphanAfter:
		return "idle"
	}
	return "ready"
}

func (s *Server) inspectWorktrees(ctx context.Context) ([]worktreeFacts, error) {
	items, err := s.Store.ReadyWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]worktreeFacts, 0, len(items))
	for _, item := range items {
		facts := worktreeFacts{RoomWorktreeInstance: item}
		machine, err := s.Store.MachineByID(ctx, item.MachineID)
		if err != nil || machine.RevokedAt.Valid {
			facts.Error = "machine is unavailable"
			out = append(out, facts)
			continue
		}
		root, err := s.selectedMachinePath(ctx, item.ProjectID, machine)
		if err != nil {
			facts.Error = "project folder is unavailable"
			out = append(out, facts)
			continue
		}
		result, err := s.machineWorktree(ctx, machine, root, item.RoomID, item.BaseRef, "inspect", false)
		if err != nil {
			facts.Error = err.Error()
			out = append(out, facts)
			continue
		}
		facts.Exists = result.State == "ready"
		facts.SizeBytes, facts.DirtyFiles, facts.Merged = result.SizeBytes, result.DirtyFiles, result.Merged
		facts.OwnCommits = result.HeadCommit != "" && result.HeadCommit != item.BaseCommit
		if when, err := time.Parse(time.RFC3339, result.LastActivity); err == nil {
			facts.LastActivity = when
		}
		out = append(out, facts)
	}
	return out, nil
}

func dirSize(ctx context.Context, root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// cleanupRun serializes cleanups: a scheduled run and a Clean now click must not remove the same thing twice.
var cleanupRun sync.Mutex

// runCleanup decides what to remove and, unless dryRun, removes it. The same code builds the dry-run plan and does the work, so the
// preview is what happens. Removals are written to the cleanup log; a dry run writes nothing.
func (s *Server) runCleanup(ctx context.Context, dryRun bool, actor string) ([]store.CleanupEntry, error) {
	cleanupRun.Lock()
	defer cleanupRun.Unlock()
	settings, err := s.Store.CleanupSettings(ctx)
	if err != nil {
		return nil, err
	}
	var plan []store.CleanupEntry
	record := func(entry store.CleanupEntry) {
		if dryRun && entry.Action == "removed" {
			entry.Action = "would_remove"
		}
		entry.Actor = actor
		plan = append(plan, entry)
	}
	now := time.Now()

	if settings.AttachmentDays > 0 {
		old, err := s.Store.AttachmentsOlderThan(ctx, now.AddDate(0, 0, -settings.AttachmentDays))
		if err != nil {
			return plan, err
		}
		for _, item := range old {
			entry := store.CleanupEntry{Category: "attachment", Target: item.Name + " · " + item.ID, ProjectID: item.ProjectID, Bytes: item.Size, Action: "removed",
				Reason: fmt.Sprintf("older than %d days; the message stays and says the attachment was removed", settings.AttachmentDays)}
			if !dryRun {
				// The record is marked first, then the file goes: a file that fails to go is an orphan the next sweep collects.
				deleteFile, mark := s.Store.MarkAttachmentRemoved(ctx, item.ID, settings.AttachmentDays)
				if mark != nil {
					entry.Action, entry.Reason = "kept", "could not mark it removed: "+mark.Error()
				} else if deleteFile {
					if rerr := os.Remove(item.Path); rerr != nil && !os.IsNotExist(rerr) {
						entry.Reason += "; the file could not be deleted yet: " + rerr.Error()
					}
				}
			}
			record(entry)
		}
	}

	orphans, skipped, err := s.orphanBlobs(ctx, now)
	if err != nil {
		return plan, err
	}
	if skipped != "" {
		record(store.CleanupEntry{Category: "attachment", Target: s.Store.AttachmentDir(), Action: "kept", Reason: skipped})
	}
	for _, blob := range orphans {
		entry := store.CleanupEntry{Category: "attachment", Target: blob.path, Bytes: blob.size, Action: "removed", Reason: "uploaded file that no message or upload record points at"}
		if !dryRun {
			if rerr := os.Remove(blob.path); rerr != nil && !os.IsNotExist(rerr) {
				entry.Action, entry.Reason = "kept", "removal failed: "+rerr.Error()
			}
		}
		record(entry)
	}

	footprint, err := s.Store.DatabaseFootprint(ctx)
	if err != nil {
		return plan, err
	}
	if footprint.FreeRatio >= compactMinFree && footprint.FreeBytes >= compactMinBytes {
		entry := store.CleanupEntry{Category: "database", Target: "aimebu.sqlite", Bytes: footprint.FreeBytes, Action: "removed",
			Reason: fmt.Sprintf("%.0f%% of the database is free pages; rebuild it", footprint.FreeRatio*100)}
		if !dryRun {
			if cerr := s.Store.Compact(ctx); cerr != nil {
				entry.Action, entry.Reason = "kept", "rebuild failed: "+cerr.Error()
			}
		}
		record(entry)
	}

	if !dryRun {
		var logged []store.CleanupEntry
		for _, entry := range plan {
			if entry.Action == "removed" {
				logged = append(logged, entry)
			}
		}
		if err := s.Store.AppendCleanupLog(ctx, logged); err != nil {
			return plan, err
		}
		if _, err := s.Store.TrimCleanupLog(ctx, now.Add(-cleanupLogKeep)); err != nil {
			return plan, err
		}
		s.diskMu.Lock()
		s.diskCache = nil
		s.diskMu.Unlock()
	}
	sort.SliceStable(plan, func(i, j int) bool { return plan[i].Category < plan[j].Category })
	return plan, nil
}

type blobFile struct {
	path string
	size int64
}

// orphanBlobs lists files in the attachment directory that the registry does not know, older than a grace period (an upload's file
// can exist for a moment before its row does). Files are matched by name, since every blob is `<id>.bin` inside the one directory;
// the registry's absolute paths may name an earlier location of it. If the registry has rows but none of their files is here, the
// directory is not the one the registry describes, and nothing in it is called an orphan: skipped says so.
func (s *Server) orphanBlobs(ctx context.Context, now time.Time) (orphans []blobFile, skipped string, err error) {
	known, err := s.Store.RegisteredAttachmentPaths(ctx)
	if err != nil {
		return nil, "", err
	}
	dir := s.Store.AttachmentDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	registered := make(map[string]bool, len(known))
	for path := range known {
		registered[filepath.Base(path)] = true
	}
	present, matched := 0, 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		present++
		if registered[entry.Name()] {
			matched++
		}
	}
	if len(registered) > 0 && present > 0 && matched == 0 {
		return nil, "the attachment registry does not match the files in the attachments folder, so no upload was treated as an orphan", nil
	}
	for _, entry := range entries {
		if entry.IsDir() || registered[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < orphanBlobGrace {
			continue
		}
		orphans = append(orphans, blobFile{path: filepath.Join(dir, entry.Name()), size: info.Size()})
	}
	return orphans, "", nil
}

// cleanupAttempts limits retries of the daily run: a day is recorded as done only after a run that succeeded, and a failing run is
// tried again at most maxScheduleAttempts times, retryBackoff apart, until the server restarts.
type cleanupAttempts struct {
	mu   sync.Mutex
	day  string
	n    int
	last time.Time
}

const (
	maxScheduleAttempts = 3
	retryBackoff        = 30 * time.Minute
)

func (a *cleanupAttempts) allow(day string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.day != day {
		a.day, a.n, a.last = day, 0, time.Time{}
	}
	if a.n >= maxScheduleAttempts || (!a.last.IsZero() && now.Sub(a.last) < retryBackoff) {
		return false
	}
	a.n++
	a.last = now
	return true
}

// scheduledCleanup runs the daily cleanup once, in the hours after the configured hour (so a server that was down at that hour still
// does it), unless it is switched off. The day counts as done only when the run succeeded.
func (s *Server) scheduledCleanup(ctx context.Context) error {
	settings, err := s.Store.CleanupSettings(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	now := time.Now()
	if s.CleanupClock != nil {
		now = s.CleanupClock()
	}
	// Today's and yesterday's scheduled moments are both candidates, so a run that was due late in the evening is still made after
	// midnight. A moment is open for six hours; the day it belongs to is what is recorded.
	for _, back := range []int{0, 1} {
		due := time.Date(now.Year(), now.Month(), now.Day()-back, settings.Hour, 0, 0, 0, now.Location())
		if now.Before(due) || !now.Before(due.Add(scheduleWindow)) {
			continue
		}
		day := due.Format("2006-01-02")
		if settings.LastRun >= day || !s.cleanupAttempt.allow(day, now) {
			return nil
		}
		if _, err := s.runCleanup(ctx, false, "schedule"); err != nil {
			return err
		}
		return s.Store.MarkCleanupRun(ctx, day)
	}
	return nil
}

type diskCategory struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
}

type diskProject struct {
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	Attachments int64  `json:"attachments"`
	Worktrees   int64  `json:"worktrees"`
}

type diskWorktree struct {
	RoomID       string `json:"room_id"`
	Room         string `json:"room"`
	ProjectID    string `json:"project_id"`
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	State        string `json:"state"`
	DirtyFiles   int    `json:"dirty_files,omitempty"`
	LastActivity string `json:"last_activity,omitempty"`
	Error        string `json:"error,omitempty"`
}

// diskMachine is one remote machine's agent output logs. Its harnesses' stderr stays on that machine, so only it can say how much there is.
type diskMachine struct {
	MachineID string `json:"machine_id"`
	Name      string `json:"name"`
	LogBytes  int64  `json:"agent_log_bytes"`
	LogAgents int    `json:"agent_log_agents,omitempty"`
	Error     string `json:"error,omitempty"`
}

type diskReport struct {
	GeneratedAt string                `json:"generated_at"`
	TotalBytes  int64                 `json:"total_bytes"`
	Categories  []diskCategory        `json:"categories"`
	Projects    []diskProject         `json:"projects"`
	Worktrees   []diskWorktree        `json:"worktrees"`
	Machines    []diskMachine         `json:"machines,omitempty"`
	FreeBytes   int64                 `json:"free_bytes"`
	VolumeBytes int64                 `json:"volume_bytes"`
	LowDisk     bool                  `json:"low_disk"`
	Settings    store.CleanupSettings `json:"settings"`
	LastCleanup *store.CleanupEntry   `json:"last_cleanup,omitempty"`
	Warnings    []string              `json:"warnings,omitempty"`
	builtAt     time.Time
}

func (s *Server) dataRoot() string {
	if s.DataRoot != "" {
		return s.DataRoot
	}
	if s.ImportV1Root != "" {
		return s.ImportV1Root
	}
	return filepath.Dir(filepath.Dir(s.Store.DBPath()))
}

// diskUsage builds the Health disk report. It walks directories and asks machines about worktrees, so the result is cached for a
// couple of minutes and rebuilt after a cleanup.
func (s *Server) diskUsage(ctx context.Context) (*diskReport, error) {
	s.diskMu.Lock()
	cached := s.diskCache
	s.diskMu.Unlock()
	if cached != nil && time.Since(cached.builtAt) < diskReportMaxAge {
		return cached, nil
	}
	// The walk and the machine calls take a while, so they run without the lock; two builds at once are harmless and the last one wins.
	report, err := s.buildDiskReport(ctx)
	if err != nil {
		return nil, err
	}
	s.diskMu.Lock()
	s.diskCache = report
	s.diskMu.Unlock()
	return report, nil
}

// remoteAgentLogs asks every approved remote machine, in parallel, how much agent output it keeps. A machine that is offline, too old
// to answer or slow only gets an error on its own row; it never fails the report.
func (s *Server) remoteAgentLogs(ctx context.Context) ([]diskMachine, error) {
	machines, err := s.Store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	var out []diskMachine
	for _, m := range machines {
		if !m.IsHost && m.Approved && !m.RevokedAt.Valid {
			out = append(out, diskMachine{MachineID: m.ID, Name: m.Name})
		}
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(row *diskMachine) {
			defer wg.Done()
			size, err := s.machineAgentLogSize(ctx, row.MachineID)
			switch {
			case err == nil:
				row.LogBytes, row.LogAgents = size.Bytes, size.Agents
			case errors.Is(err, ErrMachineUnavailable):
				row.Error = "machine is offline"
			case errors.Is(err, errAgentLogsUnsupported):
				row.Error = "supervisor needs an update to report agent output"
			default:
				row.Error = err.Error()
			}
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}

func (s *Server) buildDiskReport(ctx context.Context) (*diskReport, error) {
	now := time.Now()
	settings, err := s.Store.CleanupSettings(ctx)
	if err != nil {
		return nil, err
	}
	report := &diskReport{GeneratedAt: now.UTC().Format(time.RFC3339), Settings: settings, builtAt: now}
	root := s.dataRoot()
	footprint, err := s.Store.DatabaseFootprint(ctx)
	if err != nil {
		return nil, err
	}
	attachmentBytes := dirSize(ctx, s.Store.AttachmentDir())
	soundBytes := dirSize(ctx, filepath.Join(root, "server", "sounds"))
	supervisorBytes := dirSize(ctx, filepath.Join(root, "supervisor"))
	backupBytes := dirSize(ctx, filepath.Join(root, ".old-v1-bkp"))
	migrationBackupBytes := dirSize(ctx, filepath.Join(root, "server", "backups"))
	switcherBytes := dirSize(ctx, filepath.Join(root, "switcher"))
	usagesBytes := dirSize(ctx, filepath.Join(root, "usages"))
	// The machines are asked at the same time as the worktrees, so a slow one costs the longer of the two waits, not their sum.
	type machinesAnswer struct {
		rows []diskMachine
		err  error
	}
	answered := make(chan machinesAnswer, 1)
	go func() {
		rows, err := s.remoteAgentLogs(ctx)
		answered <- machinesAnswer{rows, err}
	}()
	facts, err := s.inspectWorktrees(ctx)
	machines := <-answered
	if err != nil {
		return nil, err
	}
	if machines.err != nil {
		return nil, machines.err
	}
	report.Machines = machines.rows
	var remoteLogBytes int64
	for _, m := range report.Machines {
		remoteLogBytes += m.LogBytes
	}
	if _, skipped, err := s.orphanBlobs(ctx, now); err == nil && skipped != "" {
		report.Warnings = append(report.Warnings, skipped)
	}
	var worktreeBytes int64
	byProject := map[string]*diskProject{}
	projectOf := func(id, name string) *diskProject {
		if byProject[id] == nil {
			byProject[id] = &diskProject{ProjectID: id, Name: name}
		}
		return byProject[id]
	}
	orphanAfter := time.Duration(settings.OrphanDays) * 24 * time.Hour
	for _, f := range facts {
		item := diskWorktree{RoomID: f.RoomID, Room: f.RoomTitle, ProjectID: f.ProjectID, Path: f.Path, Bytes: f.SizeBytes, Error: f.Error}
		if f.Error == "" {
			item.State, item.DirtyFiles = worktreeState(f, orphanAfter, now), f.DirtyFiles
			if !f.LastActivity.IsZero() {
				item.LastActivity = f.LastActivity.UTC().Format(time.RFC3339)
			}
		} else {
			item.State = "unknown"
		}
		report.Worktrees = append(report.Worktrees, item)
		worktreeBytes += f.SizeBytes
		projectOf(f.ProjectID, f.ProjectName).Worktrees += f.SizeBytes
	}
	perProject, err := s.Store.AttachmentBytesByProject(ctx)
	if err != nil {
		return nil, err
	}
	names, err := s.Store.ProjectNames(ctx)
	if err != nil {
		return nil, err
	}
	for id, bytes := range perProject {
		if id == "" {
			continue
		}
		projectOf(id, names[id]).Attachments += bytes
	}
	for id, p := range byProject {
		if p.Name == "" {
			p.Name = names[id]
		}
		p.Bytes = p.Attachments + p.Worktrees
		report.Projects = append(report.Projects, *p)
	}
	sort.Slice(report.Projects, func(i, j int) bool { return report.Projects[i].Bytes > report.Projects[j].Bytes })
	for _, c := range []diskCategory{
		{"database", "Database", footprint.Bytes}, {"attachments", "Attachments", attachmentBytes}, {"worktrees", "Worktrees", worktreeBytes},
		{"supervisor", "Agent runtime", supervisorBytes}, {"remote_agent_logs", "Remote agent logs", remoteLogBytes}, {"sounds", "Sounds", soundBytes}, {"v1_backup", "v1 backup", backupBytes},
		{"migration_backups", "Migration backups", migrationBackupBytes},
		{"switcher", "Account switcher", switcherBytes}, {"usages", "Usage cache", usagesBytes},
	} {
		report.Categories = append(report.Categories, c)
		report.TotalBytes += c.Bytes
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err == nil {
		report.FreeBytes = int64(stat.Bavail) * int64(stat.Bsize)
		report.VolumeBytes = int64(stat.Blocks) * int64(stat.Bsize)
		report.LowDisk = report.FreeBytes < lowDiskBytes || (report.VolumeBytes > 0 && float64(report.FreeBytes)/float64(report.VolumeBytes) < lowDiskRatio)
	}
	if log, err := s.Store.CleanupLog(ctx, 1); err == nil && len(log) > 0 {
		report.LastCleanup = &log[0]
	}
	return report, nil
}

// GET /api/disk: disk usage for Health.
func (s *Server) diskReport(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	report, err := s.diskUsage(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, report)
}

// POST /api/cleanup {dry_run}: plan or run the cleanup. Clean now in the UI always plans first.
func (s *Server) cleanupNow(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	var req struct {
		DryRun *bool `json:"dry_run"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.DryRun == nil {
		apiError(w, 400, "invalid_request", "dry_run is required")
		return
	}
	// The maintenance middleware already refuses this write during an import and holds the read lock for the whole request; taking it
	// again here could deadlock against an import waiting for the write lock.
	plan, err := s.runCleanup(r.Context(), *req.DryRun, userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var total int64
	for _, entry := range plan {
		if entry.Action == "removed" || entry.Action == "would_remove" {
			total += entry.Bytes
		}
	}
	if plan == nil {
		plan = []store.CleanupEntry{}
	}
	writeJSON(w, 200, map[string]any{"dry_run": *req.DryRun, "items": plan, "bytes": total})
}

// GET/PUT /api/cleanup/settings: the schedule.
func (s *Server) cleanupSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		settings, err := s.Store.CleanupSettings(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	var req struct {
		Enabled        *bool `json:"enabled"`
		Hour           *int  `json:"hour"`
		OrphanDays     *int  `json:"orphan_days"`
		AttachmentDays *int  `json:"attachment_days"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.Hour == nil && req.OrphanDays == nil && req.AttachmentDays == nil {
		apiError(w, 400, "invalid_request", "enabled, hour, orphan_days or attachment_days is required")
		return
	}
	if (req.Hour != nil && (*req.Hour < 0 || *req.Hour > 23)) || (req.OrphanDays != nil && (*req.OrphanDays < 1 || *req.OrphanDays > 365)) ||
		(req.AttachmentDays != nil && (*req.AttachmentDays < 0 || *req.AttachmentDays > store.MaxAttachmentDays)) {
		apiError(w, 400, "invalid_request", "hour must be 0-23, orphan_days 1-365 and attachment_days 0-3650 (0 keeps attachments for ever)")
		return
	}
	settings, err := s.Store.SetCleanupSettings(r.Context(), req.Enabled, req.Hour, req.OrphanDays, req.AttachmentDays)
	if err != nil {
		fail(w, err)
		return
	}
	s.diskMu.Lock()
	s.diskCache = nil
	s.diskMu.Unlock()
	writeJSON(w, 200, settings)
}

// GET /api/cleanup/log: what earlier cleanups removed, newest first.
func (s *Server) cleanupLog(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	entries, err := s.Store.CleanupLog(r.Context(), 100)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": entries})
}
