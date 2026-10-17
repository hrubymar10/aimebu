package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// tokenCopiesMarker is the file `aimebu switcher migrate-copies` writes last.
// The credential check only applies to the tools it lists as migrated.
const tokenCopiesMarker = "token-copies.json"

// lookPath is exec.LookPath; tests replace it so the host's PATH never matters.
var lookPath = exec.LookPath

// harnessDockerCtrl is the binary the refresher needs on the server's PATH: it
// gates the whole refresher (usages.harnessDockerCtrlAvailable).
const harnessDockerCtrl = "harness-docker-ctrl"

// migratedTools reads the marker through the one shared reader
// (usages.ReadTokenCopiesMarker): a version 2 file lists each migrated tool in
// `migrated`; a version 1 file means both. A missing, unreadable or
// unknown-version file means no tool is migrated.
func migratedTools(swRoot string) map[string]bool {
	marker, ok := usages.ReadTokenCopiesMarker(swRoot)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, tool := range []string{"claude", "codex"} {
		if _, migrated := marker.Since(tool); migrated {
			out[tool] = true
		}
	}
	return out
}

// checkCredentialTargets reports the health of the token-only credentials from
// files alone, so it works while the server is down: with no refresh token in
// the live files a CLI cannot renew itself, so expiry is the thing to watch.
func checkCredentialTargets(rootDir string) []Result {
	results := credentialTargetResults(rootDir, time.Now())
	if home, err := os.UserHomeDir(); err == nil {
		results = append(results, claudeAccountFileResults(rootDir, home)...)
	}
	return results
}

// claudeAccountFileResults warns when claude-code's account files name different
// accounts: a login's identity (which profile it is filed under) is read from
// them, so a disagreement could file a login under the wrong profile.
func claudeAccountFileResults(rootDir, home string) []Result {
	if !migratedTools(filepath.Join(rootDir, "switcher"))["claude"] {
		return nil
	}
	sources, disagree := credtarget.ClaudeAccountFilesDisagree(usages.ClaudeLiveCredPath(home), home)
	if !disagree {
		return nil
	}
	msg := "claude account files name different accounts:"
	for _, src := range sources {
		msg += fmt.Sprintf(" %s (%s)", src.Path, src.Email)
	}
	return []Result{{Name: "claude account files", Status: StatusWarn, Message: msg}}
}

func credentialTargetResults(rootDir string, now time.Time) []Result {
	swRoot := filepath.Join(rootDir, "switcher")
	migrated := migratedTools(swRoot)
	if len(migrated) == 0 {
		return nil // not a token-only install: nothing to say
	}
	var results []Result
	if _, err := lookPath(harnessDockerCtrl); err != nil {
		results = append(results, Result{Name: "credential refresher", Status: StatusWarn, Message: harnessDockerCtrl + " is not on PATH (as seen by this command; the server needs it on its own PATH): the refresher can't refresh, so token-only credentials will expire"})
	}
	var rows []usages.TargetStatus
	for _, row := range usages.CredentialReport(swRoot, now) {
		if migrated[row.Tool] {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return append(results, Result{Name: "credential targets", Status: StatusWarn, Message: "token-only install, but no credentials were found"})
	}
	healthy := 0
	var soonest *time.Time
	for _, row := range rows {
		name := "credential " + row.Tool + "/" + row.Role
		if row.Profile != "" {
			name = "credential " + row.Tool + "/" + row.Profile
		}
		left := "expiry unknown"
		if row.ExpiresAt != nil {
			left = "expires " + row.ExpiresAt.Local().Format("2006-01-02 15:04")
			if row.Level == usages.TargetLevelOK && (soonest == nil || row.ExpiresAt.Before(*soonest)) && row.Role == "live" {
				soonest = row.ExpiresAt
			}
		}
		switch {
		case row.State == usages.AccountReauthRequired:
			results = append(results, Result{Name: name, Status: StatusFail, Message: "the provider rejected the refresh token; log in again"})
		case row.State == usages.AccountNeedsReloginSoon:
			results = append(results, Result{Name: name, Status: StatusFail, Message: "a refresh may have been spent without a result and is not retried; log in again before it expires (" + left + ")"})
		case row.Level == usages.TargetLevelExpired:
			results = append(results, Result{Name: name, Status: StatusFail, Message: "access token expired (" + left + "); requests fail until the server rewrites it"})
		case row.State == usages.AccountRefreshFailed:
			results = append(results, Result{Name: name, Status: StatusWarn, Message: "refreshing keeps failing: " + row.Detail})
		case row.Level == usages.TargetLevelExpiring:
			results = append(results, Result{Name: name, Status: StatusWarn, Message: fmt.Sprintf("expires in under %d minutes (%s)", int(usages.TargetAlertWindow/time.Minute), left)})
		default:
			healthy++
		}
	}
	if healthy > 0 {
		msg := fmt.Sprintf("%d credential(s) healthy", healthy)
		if soonest != nil {
			msg += "; next live expiry " + soonest.Local().Format("2006-01-02 15:04")
		}
		results = append(results, Result{Name: "credential targets", Status: StatusOK, Message: msg})
	}
	return results
}
