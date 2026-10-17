package usages

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// refreshRunResult is what a refresh executor reports beyond its error.
type refreshRunResult struct {
	// Started is false when the CLI process never ran (image missing, docker
	// daemon error, command not found). A refresh that never reached the CLI
	// cannot have spent the refresh token.
	Started bool
	// Output is the bounded CLI output, used only to recognise error classes.
	Output string
}

// refreshRunner is the optional richer form of refreshExecutor. The docker
// executors implement it; a plain executor (tests) is treated as "started".
// RunStarted calls onStart once the CLI process exists, before it is waited for,
// so the caller can journal that a spend may now be in progress.
type refreshRunner interface {
	RunStarted(ctx context.Context, dir, modelFlag string, onStart func()) (refreshRunResult, error)
}

func runRefreshExecutor(ctx context.Context, e refreshExecutor, dir, modelFlag string, onStart func()) (refreshRunResult, error) {
	if onStart == nil {
		onStart = func() {}
	}
	if r, ok := e.(refreshRunner); ok {
		return r.RunStarted(ctx, dir, modelFlag, onStart)
	}
	onStart()
	err := e.Refresh(ctx, dir, modelFlag)
	result := refreshRunResult{Started: true}
	if err != nil {
		result.Output = boundedRefreshOutput([]byte(err.Error()))
	}
	return result, err
}

const refreshOutputLimit = 4096

func boundedRefreshOutput(out []byte) string {
	if len(out) > refreshOutputLimit {
		out = out[len(out)-refreshOutputLimit:]
	}
	return string(out)
}

// runDockerCapture runs `docker args...`, calls onStart as soon as the docker
// client process exists (before waiting for it) and returns the bounded combined
// output. Started is false when the client could not be started at all or docker
// itself exited without running the command (125-127).
func runDockerCapture(ctx context.Context, args []string, onStart func()) (refreshRunResult, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return refreshRunResult{Started: false}, err
	}
	if onStart != nil {
		onStart()
	}
	err := cmd.Wait()
	return refreshRunResult{Started: dockerRunStarted(err), Output: boundedRefreshOutput(out.Bytes())}, err
}

// dockerRunStarted tells whether `docker run` got as far as starting the
// container's process. docker itself exits 125 (daemon error), 126 (command not
// invokable) or 127 (command not found) without running the CLI; any other
// non-zero status is the CLI's own.
func dockerRunStarted(err error) bool {
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 125, 126, 127:
			return false
		}
		return true
	}
	// The docker binary could not be executed at all.
	return false
}

// refreshAttempt is the classified result of one refresh run.
type refreshAttempt struct {
	Outcome RefreshOutcome
	// Rotated holds the new credential document when Outcome is OutcomeRefreshed.
	Rotated []byte
	// Noop marks a NotSent outcome where the CLI ran and exited cleanly without
	// rotating anything (it judged the token still valid): nothing was sent, but
	// the attempt did not move the expiry either.
	Noop   bool
	Detail string
}

// noSendRE matches CLI output proving the request never left the machine.
var noSendRE = regexp.MustCompile(`(?i)ENOTFOUND|EAI_AGAIN|ECONNREFUSED|getaddrinfo|no such host|could not resolve host|network is unreachable|dial tcp [^ ]+: connect: (connection refused|network is unreachable|no route to host)`)

// codexRejectedRE matches the codex messages for a refresh token the provider
// rejected for good (expired, already used, revoked, signed out elsewhere).
var codexRejectedRE = regexp.MustCompile(`(?i)refresh token (has expired|was already used|was revoked|was rejected)|since logged out or signed in to another account`)

// claudeRefreshTokenField reports a claude credential document's refresh token
// and whether the key is present at all. Claude itself overwrites the refresh
// token with "" when the provider answers invalid_grant, so present-and-empty
// after a run is the signal that the account needs a new login.
func claudeRefreshTokenField(data []byte) (token string, present bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", false
	}
	for _, section := range []string{"claudeAiOauth", "claude_ai_oauth"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		for _, key := range []string{"refreshToken", "refresh_token"} {
			if v, ok := fields[key]; ok {
				var s string
				_ = json.Unmarshal(v, &s)
				return s, true
			}
		}
	}
	return "", false
}

// classifyRefreshRun turns what is on disk after a run into a RefreshOutcome.
// rotated holds the temp credential document after the run (nil when it could
// not be read); advanced says whether it carries a newer expiry than the start;
// startToken is the refresh token the run spent.
func classifyRefreshRun(tool string, run refreshRunResult, runErr error, rotated []byte, advanced bool, startToken string) refreshAttempt {
	switch {
	case advanced && len(rotated) > 0:
		// The rotated file is the proof, whatever the exit status said.
		return refreshAttempt{Outcome: OutcomeRefreshed, Rotated: rotated}
	case !run.Started:
		return refreshAttempt{Outcome: OutcomeNotSent, Detail: "the CLI never started: " + errString(runErr)}
	}
	if tool == "claude" {
		if token, present := claudeRefreshTokenField(rotated); present && token == "" && startToken != "" {
			return refreshAttempt{Outcome: OutcomeInvalidGrant, Detail: "the provider rejected the refresh token (invalid_grant)"}
		}
		if strings.Contains(run.Output, "invalid_grant") {
			return refreshAttempt{Outcome: OutcomeInvalidGrant, Detail: "the provider rejected the refresh token (invalid_grant)"}
		}
	} else if codexRejectedRE.MatchString(run.Output) {
		return refreshAttempt{Outcome: OutcomeInvalidGrant, Detail: "the provider rejected the refresh token"}
	}
	if runErr == nil {
		// Clean exit and nothing rotated: the CLI judged the token still usable.
		return refreshAttempt{Outcome: OutcomeNotSent, Noop: true, Detail: "the CLI exited cleanly without rotating the credentials"}
	}
	if noSendRE.MatchString(run.Output) || noSendRE.MatchString(errString(runErr)) {
		return refreshAttempt{Outcome: OutcomeNotSent, Detail: "the request never left the machine: " + errString(runErr)}
	}
	// The CLI ran, failed, and rotated nothing, with no proof the request was
	// not sent: it may have been processed with its answer lost.
	return refreshAttempt{Outcome: OutcomeUnknown, Detail: "the CLI failed after starting, so the refresh token may have been spent: " + errString(runErr)}
}

// errString is the one place an error becomes alert, journal or API text: it is
// cut to a short length and passed through the detail redactor, because a CLI's
// own output can end up inside an error.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return cleanDetail(err.Error())
}

// cleanDetail bounds and redacts text before it is stored, logged or shown.
func cleanDetail(msg string) string {
	msg = redactDetail(msg)
	if len(msg) > 300 {
		cut := 300
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut-- // never split a multi-byte rune
		}
		msg = msg[:cut] + "…"
	}
	return msg
}

// accessTokenOf returns a credential document's access token ("" when it has
// none). It tells whether a profile already holds a given rotated login even when
// the provider kept the refresh token unchanged.
func accessTokenOf(tool string, data []byte) string {
	if tool == "codex" {
		var raw codexAuthFile
		if json.Unmarshal(data, &raw) != nil {
			return ""
		}
		return firstNonEmpty(raw.Tokens.AccessToken, raw.Tokens.AccessTokenC)
	}
	var raw claudeAuthFile
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	tokens := raw.ClaudeAiOAuth
	if firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS) == "" {
		tokens = raw.ClaudeAiOAuthS
	}
	return firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS)
}

// credentialAdvanced says whether candidate is a usable login newer than start:
// Claude by a later expiry with a non-empty refresh token, Codex by a different
// access token with a later last_refresh. It looks at the tokens that actually
// moved, never at the refresh token alone, because a provider may rotate the
// access token without rotating the refresh token.
func credentialAdvanced(tool string, start, candidate []byte) bool {
	if tool == "codex" {
		old, _, err := parseCodexAuthDataFull(start)
		if err != nil {
			return false
		}
		next, _, err := parseCodexAuthDataFull(candidate)
		if err != nil {
			return false
		}
		return next.AccessToken != old.AccessToken && next.LastRefresh != nil && (old.LastRefresh == nil || next.LastRefresh.After(*old.LastRefresh))
	}
	if _, err := refreshToken(candidate, "claude"); err != nil {
		return false // claude wrote a dead refresh token (or dropped it): not a usable login
	}
	oldExpiry, newExpiry := tolerantExpiry("claude", start), tolerantExpiry("claude", candidate)
	return oldExpiry != nil && newExpiry != nil && newExpiry.After(*oldExpiry)
}

// targetBackoff is the per-account delay after the n-th consecutive failed
// attempt: 5 min, doubling, capped at 30 min.
func targetBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := 5 * time.Minute
	for i := 1; i < failures && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}
