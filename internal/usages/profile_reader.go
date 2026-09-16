package usages

import (
	"context"
	"path/filepath"
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
// Usage reads are read-only. Token rotation is owned by the isolated
// harness-docker maintenance path.
func fetchCodexSnapshotFromPath(ctx context.Context, path string) (Snapshot, error) {
	auth, detail, err := loadCodexAuthSnapshot(path)
	if err != nil {
		return codexStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	creds := auth.Credentials
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
