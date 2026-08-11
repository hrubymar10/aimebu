package usages

import (
	"bytes"
	"context"
	"path/filepath"
	"time"
)

var (
	fetchClaudeSnapshotFromPathFunc = fetchClaudeSnapshotFromPath
	fetchCodexSnapshotFromPathFunc  = fetchCodexSnapshotFromPath
)

// claudeCredentialsPathInDir returns the credential file path for claude inside dir.
func claudeCredentialsPathInDir(dir string) string {
	return filepath.Join(dir, ".credentials.json")
}

// codexCredentialsPathInDir returns the credential file path for codex inside dir.
func codexCredentialsPathInDir(dir string) string {
	return filepath.Join(dir, "auth.json")
}

// fetchClaudeSnapshotFromPath fetches claude usage from the credential file at
// path. Used for per-profile reads where path may be a stored profile copy
// rather than the live credential location.
func fetchClaudeSnapshotFromPath(ctx context.Context, path string) (Snapshot, error) {
	creds, detail, err := loadClaudeAuth(path)
	if err != nil {
		return claudeStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	raw, detail, status, unauthorized, err := fetchClaudeUsage(ctx, creds)
	if unauthorized {
		return claudeStatus(status, "Claude usage endpoint rejected the OAuth token.", detail), nil
	}
	if err != nil {
		snap := claudeStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeClaudeUsage(raw, creds)
	if err != nil {
		snap = claudeStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}

// fetchCodexSnapshotFromPath fetches codex usage from the auth.json at path.
// When persist is false, token refreshes are NOT written back — required for
// non-active profiles to avoid rotating a stored token (§14.2 complement).
// When persist is true and a refresh is needed, withLock wraps the save so
// a concurrent switch cannot overwrite the just-refreshed live credentials.
func fetchCodexSnapshotFromPath(ctx context.Context, path string, persist bool, withLock func(func() error) error) (Snapshot, error) {
	// Use loadCodexAuthSnapshot so we capture the file's fingerprint for the
	// compare-and-swap in the persist path below.
	auth, detail, err := loadCodexAuthSnapshot(path)
	if err != nil {
		return codexStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	creds := auth.Credentials
	if persist && creds.needsRefresh(time.Now()) {
		refreshed, rDetail, rErr := refreshCodexAuth(ctx, creds)
		if rErr != nil {
			return codexStatus(StatusAuthMissing, rErr.Error(), rDetail), nil
		}
		creds = refreshed
		// CAS: inside the lock, re-read the fingerprint. If the file changed
		// (a switch landed while we were on the network), drop this write.
		expect := auth.Fingerprint
		save := func() error {
			if cur := codexAuthFingerprint(path); !bytes.Equal(cur, expect) {
				return nil // switch landed mid-refresh; do not overwrite new profile
			}
			return saveCodexAuth(path, creds)
		}
		var saveErr error
		if withLock != nil {
			saveErr = withLock(save)
		} else {
			saveErr = save()
		}
		if saveErr != nil {
			return codexStatus(StatusAuthMissing, "Codex auth refresh could not be saved.", nil), nil
		}
	}
	raw, detail, status, err := fetchCodexUsage(ctx, creds)
	if err != nil {
		snap := codexStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeCodexUsage(raw, creds)
	if err != nil {
		snap = codexStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}
