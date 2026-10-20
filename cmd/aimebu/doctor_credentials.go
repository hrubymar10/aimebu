package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// lookPath is exec.LookPath; tests replace it so the host's PATH never matters.
var lookPath = exec.LookPath

// dockerReachable is whether `docker` is on PATH and its daemon answers; tests replace it. The refresher calls docker
// directly (image inspect, run), so this is checked next to harness-docker-ctrl.
var dockerReachable = func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return usages.DockerReachable(ctx)
}

// harnessDockerCtrl is the binary the refresher needs on the server's PATH: it
// gates the whole refresher.
const harnessDockerCtrl = "harness-docker-ctrl"

// migratedTools reads the token-copies marker through the one shared reader. A
// missing, unreadable or unknown-version file means no tool is migrated.
func migratedTools(switcherRoot string) map[string]bool {
	marker, ok := usages.ReadTokenCopiesMarker(switcherRoot)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, tool := range switcher.Tools() {
		if _, migrated := marker.Since(tool); migrated {
			out[tool] = true
		}
	}
	return out
}

// checkCredentialTargets reports the health of the token-only credentials from
// files alone, so it works while the server is down: with no refresh token in the
// live files a CLI cannot renew itself, so expiry is the thing to watch. It says
// nothing on an install that has not migrated.
func checkCredentialTargets(root string) []doctorCheck {
	checks := credentialTargetChecks(root, time.Now())
	checks = append(checks, checkV1CredentialTargetsCarryOver(root)...)
	checks = append(checks, checkTokenRootConflicts(root)...)
	if home, err := os.UserHomeDir(); err == nil {
		checks = append(checks, claudeAccountFileChecks(root, home)...)
	}
	return checks
}

func checkTokenRootConflicts(root string) []doctorCheck {
	m, err := switcher.NewReadOnly(filepath.Join(root, "switcher"))
	if err != nil {
		return []doctorCheck{{"credential refresher ownership", statusFail, err.Error()}}
	}
	conflicts, err := m.TokenCopyConflicts()
	if err != nil {
		return []doctorCheck{{"credential refresher ownership", statusFail, "cannot check other token-copy roots: " + err.Error()}}
	}
	var checks []doctorCheck
	for _, tool := range switcher.Tools() {
		if other := conflicts[tool]; other != "" {
			checks = append(checks, doctorCheck{"credential refresher " + tool, statusFail, "another token-copy root uses the same live credentials: " + other})
		}
	}
	return checks
}

func checkV1CredentialTargetsCarryOver(root string) []doctorCheck {
	source := filepath.Join(root, v1BackupName, "server", credentialTargetsFile)
	if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	target := filepath.Join(root, "server", credentialTargetsFile)
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return []doctorCheck{{"v1 credential targets", statusWarn, "the v1 backup has external credential targets, but the v2 server registry is missing"}}
	}
	return nil
}

// claudeAccountFileChecks warns when claude-code's account files name different
// accounts: a login's identity (which profile it is filed under) is read from
// them, so a disagreement could file a login under the wrong profile.
func claudeAccountFileChecks(rootDir, home string) []doctorCheck {
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
	return []doctorCheck{{"claude account files", statusWarn, msg}}
}

func credentialTargetChecks(rootDir string, now time.Time) []doctorCheck {
	switcherRoot := filepath.Join(rootDir, "switcher")
	migrated := migratedTools(switcherRoot)
	if len(migrated) == 0 {
		return nil // not a token-only install: nothing to say
	}
	var checks []doctorCheck
	if _, err := lookPath(harnessDockerCtrl); err != nil {
		checks = append(checks, doctorCheck{"credential refresher", statusWarn, harnessDockerCtrl + " is not on PATH (as seen by this command; the server needs it on its own PATH): the refresher can't refresh, so token-only credentials will expire"})
	}
	if !dockerReachable() {
		checks = append(checks, doctorCheck{"credential refresher", statusWarn, "docker is not on PATH or its daemon does not answer (`docker info` fails as seen by this command; the server needs it too): the refresher runs its CLI in a container, so token-only credentials will expire"})
	}
	m, err := switcher.NewReadOnly(switcherRoot)
	if err != nil {
		return append(checks, doctorCheck{"credential targets", statusWarn, err.Error()})
	}
	var rows []usages.TargetStatus
	for _, row := range usages.CredentialReport(switcherRoot, now, m.LiveCredPath) {
		if migrated[row.Tool] {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return append(checks, doctorCheck{"credential targets", statusWarn, "token-only install, but no credentials were found"})
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
			checks = append(checks, doctorCheck{name, statusFail, "the provider rejected the refresh token; log in again"})
		case row.State == usages.AccountNeedsReloginSoon:
			checks = append(checks, doctorCheck{name, statusFail, "a refresh may have been spent without a result and is not retried; log in again before it expires (" + left + ")"})
		case row.Level == usages.TargetLevelExpired:
			checks = append(checks, doctorCheck{name, statusFail, "access token expired (" + left + "); requests fail until the server rewrites it"})
		case row.State == usages.AccountRefreshFailed:
			checks = append(checks, doctorCheck{name, statusWarn, "refreshing keeps failing: " + row.Detail})
		case row.Level == usages.TargetLevelExpiring:
			checks = append(checks, doctorCheck{name, statusWarn, fmt.Sprintf("expires in under %d minutes (%s)", int(usages.TargetAlertWindow/time.Minute), left)})
		default:
			healthy++
		}
	}
	if healthy > 0 {
		msg := fmt.Sprintf("%d credential(s) healthy", healthy)
		if soonest != nil {
			msg += "; next live expiry " + soonest.Local().Format("2006-01-02 15:04")
		}
		checks = append(checks, doctorCheck{"credential targets", statusOK, msg})
	}
	return checks
}
