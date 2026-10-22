package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

type importV1JobState struct {
	ID     string                `json:"id"`
	Phase  string                `json:"phase"`
	Error  string                `json:"error,omitempty"`
	Report *store.V1ImportReport `json:"report,omitempty"`
	// Invites are the one-time links for the accounts the import made (valid for an hour); InviteError says why they are missing.
	Invites     []importV1Invite `json:"invites,omitempty"`
	InviteError string           `json:"invite_error,omitempty"`
}

type importV1Invite struct {
	Login     string `json:"login"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

var errInconsistentV1Migration = errors.New("migration marker and import ledger disagree")

func (s *Server) importSource() string { return filepath.Join(s.ImportV1Root, ".old-v1-bkp") }

func (s *Server) carriedV1Files() []string {
	if s.ImportV1Root == "" {
		return nil
	}
	for _, path := range []string{
		filepath.Join(s.importSource(), "server", "credential-targets.json"),
		filepath.Join(s.ImportV1Root, "server", "credential-targets.json"),
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
	}
	return []string{"server/credential-targets.json"}
}

func (s *Server) importV1Status(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	choice, err := s.Store.V1ImportChoice(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if choice != "" {
		writeJSON(w, 200, map[string]any{"state": choice})
		return
	}
	if s.ImportV1Root == "" {
		writeJSON(w, 200, map[string]any{"state": "none"})
		return
	}
	if _, err := os.Stat(s.importSource()); err != nil {
		if !os.IsNotExist(err) {
			fail(w, err)
			return
		}
		if _, e := os.Stat(s.importSource() + ".partial"); e == nil {
			writeJSON(w, 200, map[string]any{"state": "backup_incomplete"})
			return
		}
		// Reading the status never closes the offer on its own: a backup may still be on its way.
		writeJSON(w, 200, map[string]any{"state": "none"})
		return
	}
	marker, markerErr := os.ReadFile(filepath.Join(s.ImportV1Root, ".v2-migrated"))
	if markerErr != nil && !os.IsNotExist(markerErr) {
		fail(w, markerErr)
		return
	}
	if markerErr == nil {
		raw, err := os.ReadFile(filepath.Join(s.importSource(), "manifest.json"))
		if err != nil && !os.IsNotExist(err) {
			fail(w, err)
			return
		}
		if err == nil {
			var manifest struct {
				SourceKey string `json:"source_key"`
			}
			if json.Unmarshal(raw, &manifest) != nil {
				apiError(w, 409, "inconsistent_migration", "migration marker and backup manifest disagree")
				return
			}
			if manifest.SourceKey != "" {
				if strings.TrimSpace(string(marker)) != manifest.SourceKey {
					apiError(w, 409, "inconsistent_migration", "migration marker and backup manifest disagree")
					return
				}
				count, err := s.Store.V1ImportLedgerCount(r.Context(), manifest.SourceKey)
				if err != nil {
					fail(w, err)
					return
				}
				if count == 0 {
					apiError(w, 409, "inconsistent_migration", "migration marker and import ledger disagree")
					return
				}
				if err := s.Store.SetV1ImportChoice(r.Context(), "done"); err != nil && !errors.Is(err, store.ErrConflict) {
					fail(w, err)
					return
				}
				s.importMu.Lock()
				job := s.importJob
				s.importMu.Unlock()
				writeJSON(w, 200, map[string]any{"state": "done", "source_key": manifest.SourceKey, "ledger_rows": count, "job": job})
				return
			}
		}
	}
	owner, err := s.Store.V1ImportOwnerLogin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	preview, err := store.PreviewV1Import(r.Context(), s.importSource(), owner)
	if err != nil {
		if errors.Is(err, store.ErrV1NoRooms) {
			// An empty backup has nothing to import; there is nothing to offer, and nothing to close.
			writeJSON(w, 200, map[string]any{"state": "none"})
			return
		}
		if errors.Is(err, store.ErrV1BackupChanged) {
			backupChanged(w)
			return
		}
		fail(w, err)
		return
	}
	count, err := s.Store.V1ImportLedgerCount(r.Context(), preview.SourceKey)
	if err != nil {
		fail(w, err)
		return
	}
	state := "ready"
	if count > 0 {
		state = "resume"
	}
	if count == 0 && markerErr != nil {
		closed, err := s.closeImportIfUsed(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if closed {
			writeJSON(w, 200, map[string]any{"state": "skipped"})
			return
		}
	}
	missed, err := s.Store.V1ReservationMissed(r.Context(), preview.HighestMessageID)
	if err != nil {
		fail(w, err)
		return
	}
	if markerErr == nil {
		if count == 0 || strings.TrimSpace(string(marker)) != preview.SourceKey {
			apiError(w, 409, "inconsistent_migration", "migration marker and import ledger disagree")
			return
		}
		state = "done"
	}
	s.importMu.Lock()
	job := s.importJob
	s.importMu.Unlock()
	writeJSON(w, 200, map[string]any{"state": state, "source_key": preview.SourceKey, "ledger_rows": count, "job": job, "reservation_missed": missed})
}

// closeImportIfUsed ends the one-time offer on an install that already has messages and never
// started an import (a database from before this rule, or one used before the first sign-in).
// Every import route goes through it, so none works on a used install.
func (s *Server) closeImportIfUsed(ctx context.Context) (bool, error) {
	used, err := s.Store.V1ImportUsed(ctx)
	if err != nil || !used {
		return false, err
	}
	if err := s.Store.SetV1ImportChoice(ctx, "skipped"); err != nil && !errors.Is(err, store.ErrConflict) {
		return false, err
	}
	return true, nil
}

func (s *Server) importV1Pending(w http.ResponseWriter, r *http.Request) bool {
	choice, err := s.Store.V1ImportChoice(r.Context())
	if err != nil {
		fail(w, err)
		return false
	}
	if choice == "" {
		closed, err := s.closeImportIfUsed(r.Context())
		if err != nil {
			fail(w, err)
			return false
		}
		if !closed {
			return true
		}
	}
	apiError(w, 410, "import_closed", "the first-run import choice is closed")
	return false
}

// backupChanged answers every import route the same way when the v1 backup no longer matches the key recorded when it was
// made, so the wizard and any client see one code (status, preview and dry run alike) and one explanation.
func backupChanged(w http.ResponseWriter) {
	apiError(w, 409, "backup_changed", "the v1 backup was modified after it was made, so it can no longer be verified and will not be imported. If you started v1 on the original folder or on a copy of the backup, stop it; then restore a pristine copy of .old-v1-bkp or skip the import (see docs/migrating-from-v1.md, Roll back)")
}

func (s *Server) importV1Skip(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importJob.Phase != "" && s.importJob.Phase != "done" && s.importJob.Phase != "error" {
		apiError(w, 409, "import_running", "import is already running")
		return
	}
	if prior, err := s.Store.V1ImportChoice(r.Context()); err != nil {
		fail(w, err)
		return
	} else if prior == "done" {
		// A completed import is closed for good, like every other import route; only a repeated skip stays a no-op.
		apiError(w, 410, "import_closed", "the first-run import choice is closed")
		return
	}
	choice := "skipped"
	if started, err := s.Store.V1ImportStarted(r.Context()); err != nil {
		fail(w, err)
		return
	} else if started {
		choice = "done" // skipping a partly finished import ends it, it does not decline one
	}
	if err := s.Store.SetV1ImportChoice(r.Context(), choice); err != nil {
		if errors.Is(err, store.ErrConflict) {
			apiError(w, 410, "import_closed", "the first-run import choice is closed")
		} else {
			fail(w, err)
		}
		return
	}
	s.importReceipt = "" // a dry run's receipt must not outlive the choice
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) importV1Preview(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	if !s.importV1Pending(w, r) {
		return
	}
	owner, err := s.Store.V1ImportOwnerLogin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	preview, err := store.PreviewV1Import(r.Context(), s.importSource(), owner)
	if errors.Is(err, store.ErrV1BackupChanged) {
		backupChanged(w)
		return
	}
	if err != nil {
		apiError(w, 409, "source_unavailable", err.Error())
		return
	}
	if preview.ReservationMissed, err = s.Store.V1ReservationMissed(r.Context(), preview.HighestMessageID); err != nil {
		fail(w, err)
		return
	}
	preview.CarriedOverFiles = s.carriedV1Files()
	preview.DestinationUsers, err = s.Store.V1ImportUserLogins(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	preview.ImportedMacros, err = s.Store.V1ImportedMacroKeys(r.Context(), preview.SourceKey)
	if err != nil {
		fail(w, err)
		return
	}
	preview.ImportedRoles, err = s.Store.V1ImportedRoleKeys(r.Context(), preview.SourceKey)
	if err != nil {
		fail(w, err)
		return
	}
	roles, err := s.Store.ListRoles(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	preview.ResolveRoles(roles)
	preview.ResolvePeople()
	if saved, ok, err := s.Store.V1ImportSavedMap(r.Context(), preview.SourceKey); err != nil {
		fail(w, err)
		return
	} else if ok {
		preview.Suggested = saved
	}
	writeJSON(w, 200, preview)
}

func readImportRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		apiError(w, 400, "invalid_request", "trailing JSON")
		return false
	}
	return true
}

func (s *Server) checkedImportMap(ctx context.Context, ownerID string, mapping *store.V1ImportMap) error {
	owner, err := s.Store.V1ImportOwnerLogin(ctx, ownerID)
	if err != nil {
		return err
	}
	preview, err := store.PreviewV1Import(ctx, s.importSource(), owner)
	if err != nil {
		return err
	}
	if mapping.SourceKey != preview.SourceKey || mapping.OwnerLogin != owner {
		return errors.New("import source or owner changed; review the preview again")
	}
	if err := s.checkImportMarker(ctx, preview.SourceKey); err != nil {
		return err
	}
	backup, err := filepath.EvalSymlinks(s.importSource())
	if err != nil {
		return err
	}
	for alias, project := range mapping.Projects {
		if project.ExistingID != "" {
			continue
		}
		folder := filepath.Clean(project.Folder)
		if !filepath.IsAbs(folder) {
			return fmt.Errorf("project %q needs an absolute workspace folder", alias)
		}
		if folder == backup || strings.HasPrefix(folder, backup+string(os.PathSeparator)) {
			return fmt.Errorf("project %q workspace cannot be inside the v1 backup", alias)
		}
	}
	mapping.Wizard = true
	return nil
}

func (s *Server) checkImportMarker(ctx context.Context, sourceKey string) error {
	marker, err := os.ReadFile(filepath.Join(s.ImportV1Root, ".v2-migrated"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	count, err := s.Store.V1ImportLedgerCount(ctx, sourceKey)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(marker)) != sourceKey || count == 0 {
		return errInconsistentV1Migration
	}
	return nil
}

func (s *Server) importV1DryRun(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	if !s.importV1Pending(w, r) {
		return
	}
	var mapping store.V1ImportMap
	if !readImportRequest(w, r, &mapping) {
		return
	}
	if err := s.checkedImportMap(r.Context(), userID(r), &mapping); err != nil {
		if errors.Is(err, errInconsistentV1Migration) {
			apiError(w, 409, "inconsistent_migration", err.Error())
			return
		}
		if errors.Is(err, store.ErrV1BackupChanged) {
			backupChanged(w)
			return
		}
		apiError(w, 400, "invalid_mapping", err.Error())
		return
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importJob.Phase != "" && s.importJob.Phase != "done" && s.importJob.Phase != "error" {
		apiError(w, 409, "import_running", "import is already running")
		return
	}
	report, err := s.Store.ImportV1(r.Context(), s.importSource(), mapping, true)
	if errors.Is(err, store.ErrV1BackupChanged) {
		backupChanged(w)
		return
	}
	if err != nil {
		apiError(w, 400, "invalid_mapping", err.Error())
		return
	}
	report.CarriedOverFiles = s.carriedV1Files()
	digest := store.V1ImportDigest(mapping)
	s.importReceipt = digest
	writeJSON(w, 200, map[string]any{"digest": digest, "report": report})
}

func (s *Server) importV1Commit(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	if !s.importV1Pending(w, r) {
		return
	}
	var req struct {
		Mapping store.V1ImportMap `json:"mapping"`
		Digest  string            `json:"digest"`
	}
	if !readImportRequest(w, r, &req) {
		return
	}
	if err := s.checkedImportMap(r.Context(), userID(r), &req.Mapping); err != nil {
		if errors.Is(err, errInconsistentV1Migration) {
			apiError(w, 409, "inconsistent_migration", err.Error())
			return
		}
		if errors.Is(err, store.ErrV1BackupChanged) {
			backupChanged(w)
			return
		}
		apiError(w, 400, "invalid_mapping", err.Error())
		return
	}
	digest := store.V1ImportDigest(req.Mapping)
	s.importMu.Lock()
	// The choice may have been closed (a skip) after the check above and before the lock; decide again under it.
	if choice, err := s.Store.V1ImportChoice(r.Context()); err != nil {
		s.importMu.Unlock()
		fail(w, err)
		return
	} else if choice != "" {
		s.importMu.Unlock()
		apiError(w, 410, "import_closed", "the first-run import choice is closed")
		return
	}
	if req.Digest == "" || req.Digest != digest || req.Digest != s.importReceipt {
		s.importMu.Unlock()
		apiError(w, 409, "dry_run_required", "run the dry run for these exact choices first")
		return
	}
	if s.importJob.Phase != "" && s.importJob.Phase != "done" && s.importJob.Phase != "error" {
		s.importMu.Unlock()
		apiError(w, 409, "import_running", "import is already running")
		return
	}
	var idBytes [12]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		s.importMu.Unlock()
		fail(w, err)
		return
	}
	id := hex.EncodeToString(idBytes[:])
	s.importReceipt = ""
	s.importJob = importV1JobState{ID: id, Phase: "scan"}
	s.maintenance.Store(true)
	s.importMu.Unlock()
	go s.runImportV1Job(id, req.Mapping, userID(r), s.publicBase(r))
	writeJSON(w, 202, map[string]string{"job_id": id, "phase": "scan"})
}

func (s *Server) setImportPhase(id, phase string, err error, report *store.V1ImportReport) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importJob.ID != id {
		return
	}
	s.importJob.Phase = phase
	s.importJob.Report = report
	if err != nil {
		s.importJob.Error = err.Error()
	}
}

func (s *Server) runImportV1Job(id string, mapping store.V1ImportMap, ownerID, base string) {
	s.maintenanceMu.Lock()
	defer func() { s.maintenance.Store(false); s.maintenanceMu.Unlock() }()
	ctx := context.Background()
	s.setImportPhase(id, "validate", nil, nil)
	if _, err := s.Store.ImportV1(ctx, s.importSource(), mapping, true); err != nil {
		s.setImportPhase(id, "error", err, nil)
		return
	}
	s.setImportPhase(id, "copy", nil, nil)
	report, err := s.Store.ImportV1(ctx, s.importSource(), mapping, false)
	if err != nil {
		s.setImportPhase(id, "error", err, nil)
		return
	}
	report.CarriedOverFiles = s.carriedV1Files()
	s.setImportPhase(id, "commit", nil, &report)
	if err := writeV1MigrationMarker(s.ImportV1Root, mapping.SourceKey); err != nil {
		s.setImportPhase(id, "error", err, &report)
		return
	}
	if err := s.Store.SetV1ImportChoice(ctx, "done"); err != nil {
		s.setImportPhase(id, "error", err, &report)
		return
	}
	s.createImportInvites(ctx, id, ownerID, base, report.NewAccounts)
	s.setImportPhase(id, "done", nil, &report)
}

// createImportInvites makes a one-time password-reset link for each account the import made, so its person can set a password. The
// import has committed by now, so a failure here is reported in the job, never undone: the owner can make the links in Settings -> Users.
func (s *Server) createImportInvites(ctx context.Context, id, ownerID, base string, accounts []store.V1NewAccount) {
	var invites []importV1Invite
	var failure string
	for _, account := range accounts {
		invite, token, err := s.Auth.CreateUserInvite(ctx, ownerID, "reset", "", "", account.UserID)
		if err != nil {
			failure = fmt.Sprintf("could not create the invite link for %s: %v", account.Login, err)
			break
		}
		invites = append(invites, importV1Invite{Login: account.Login, URL: base + "/#invite=" + url.QueryEscape(token), ExpiresAt: invite.ExpiresAt})
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importJob.ID == id {
		s.importJob.Invites, s.importJob.InviteError = invites, failure
	}
}

func writeV1MigrationMarker(root, source string) error {
	path := filepath.Join(root, ".v2-migrated")
	if raw, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(raw)) == source {
			return nil
		}
		return errors.New("a different v1 backup was already marked migrated")
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(root, ".v2-migrated.*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if _, err = file.WriteString(source + "\n"); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) importV1Job(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	s.importMu.Lock()
	job := s.importJob
	s.importMu.Unlock()
	if job.ID == "" || job.ID != r.PathValue("id") {
		apiError(w, 404, "not_found", "import job not found")
		return
	}
	writeJSON(w, 200, job)
}
