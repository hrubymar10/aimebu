package main

import (
	"fmt"
	"io"
	"strings"
)

// envVar documents environment variables for `aimebu help`. Every variable the
// non-test sources read (or set for a child process) belongs here, and
// TestEnvironmentHelpMatchesSourceAndDocs fails when one is missing or
// undocumented, so the help text cannot drift from the code.
type envVar struct {
	names []string // shown together, joined with " / "
	help  []string // one line per entry
}

var environment = []envVar{
	{[]string{"AIMEBU_CONFIG_DIR"}, []string{"Absolute config root directory (default: ~/.aimebu; leading ~ expands to home)"}},
	{[]string{"AIMEBU_BIND"}, []string{"Server bind address (default: 127.0.0.1); also probed for a running v1 server before import"}},
	{[]string{"AIMEBU_PORT"}, []string{"Server listen port (default: 9997)"}},
	{[]string{"AIMEBU_ALLOW"}, []string{"Comma-separated IPs/CIDRs allowed to connect"}},
	{[]string{"AIMEBU_TLS_CERT"}, []string{"TLS certificate PEM path; set with AIMEBU_TLS_KEY for HTTPS"}},
	{[]string{"AIMEBU_TLS_KEY"}, []string{"TLS private key PEM path; set with AIMEBU_TLS_CERT for HTTPS"}},
	{[]string{"AIMEBU_TLS_PORT"}, []string{"HTTPS listen port when TLS is configured (default: 9996)"}},
	{[]string{"AIMEBU_TRUSTED_PROXIES"}, []string{
		"Comma-separated proxy IPs/CIDRs whose X-Forwarded-For separates clients",
		"for login throttling only (default: none)"}},
	{[]string{"AIMEBU_ALLOWED_HOSTS"}, []string{
		"Comma-separated host names (besides localhost, IP addresses and the public",
		"URL's host) that sign-in, setup, invite and pairing answer to (default: none)"}},
	{[]string{"AIMEBU_PUBLIC_URL"}, []string{"HTTP(S) origin the server advertises when it sits behind a proxy"}},
	{[]string{"AIMEBU_URL"}, []string{"Server URL for the supervisor and agent-tools"}},
	{[]string{"AIMEBU_AGENT_ID", "AIMEBU_AGENT_KEY"}, []string{
		"A managed agent's identity and key for agent-tools; the supervisor sets",
		"them in the agent's MCP config, you normally never do"}},
	{[]string{"AIMEBU_AGENT_WORKDIR"}, []string{
		"The agent's workspace, where agent-tools saves an attachment it cannot",
		"return inline; the supervisor sets it for native Claude Code and Codex"}},
	{[]string{"AIMEBU_OWNER_LOGIN", "AIMEBU_OWNER_PASSWORD"}, []string{"Bootstrap the owner account on first start (set both; without them the server prints a one-time setup token)"}},
	{[]string{"AIMEBU_EMBEDDED_SUPERVISOR"}, []string{"Override the host machine setting: 0 runs without the local supervisor, 1 forces it on"}},
	{[]string{"AIMEBU_SUPERVISOR"}, []string{"Ignored: the local supervisor always runs as its own process (a server that has it set says so in its log)"}},
	{[]string{"AIMEBU_ALLOW_NATIVE"}, []string{
		"Set to 1 when migrating a database to enable host-native launchers; has no",
		"effect afterwards"}},
	{[]string{"AIMEBU_SENTRY_DSN"}, []string{
		"Sentry DSN for error and performance reports (server and web UI); \"off\" or",
		"empty disables, and either overrides Settings. See docs/telemetry.md"}},
	{[]string{"AIMEBU_SENTRY_ENV"}, []string{"Sentry environment name (default: dev)"}},
	{[]string{"AIMEBU_SENTRY_TRACES_SAMPLE_RATE"}, []string{"Share of requests and page loads traced, 0 to 1 (default: 1)"}},
	{[]string{"AIMEBU_USAGES"}, []string{"Set to 0 to disable credential reads and provider usage requests"}},
	{[]string{"AIMEBU_WEB_BUNDLE"}, []string{
		"Set to off to serve the web UI from its ~240 separate modules instead of the one",
		"bundle the server builds when it starts (for debugging in a browser)"}},
	{[]string{"AIMEBU_TOKEN_LEAD_CLAUDE", "AIMEBU_TOKEN_LEAD_CODEX"}, []string{
		"How long before expiry the token refresher renews a profile (Go durations,",
		"10m to 14d; defaults 60m Claude, 24h Codex); only with the token-copy model"}},
	{[]string{"AIMEBU_DEV_IMPORT_V1"}, []string{
		"Set to 1 to enable the offline `import-v1` command for development tests;",
		"normal imports happen only in the first-run wizard"}},
	{[]string{"AIMEBU_DEV_SKIP_V1_PROBE"}, []string{
		"Set to 1 to skip the v1 port 9997 listener probe so a dev instance can",
		"rehearse the cutover on a copy of v1 data; never in production, the v1",
		"daemon pid-file check still runs"}},
	{[]string{"AIMEBU_UPDATE_CHANNEL"}, []string{
		"Release channel the wrapper and the server's update check follow: stable,",
		"prerelease, edge or edge:<branch> (default: computed — prerelease until the",
		"remote has a stable tag of the running major, then stable; a one-word",
		"update-channel file in the config root works too, the variable wins)"}},
	{[]string{"AIMEBU_UPDATE_CHECK"}, []string{
		"Wrapper update check: 0 turns it and the server's own check off, apply",
		"updates without asking; the server never updates or prompts either way"}},
	{[]string{"AIMEBU_UPDATE_REMOTE"}, []string{"Git remote the update checks follow (default: origin)"}},
	{[]string{"AIMEBU_DEV_CONFIG_DIR", "AIMEBU_DEV_PORT", "AIMEBU_DEV_TLS_PORT", "AIMEBU_DEV_SENTRY_ENV"}, []string{
		"Overrides for make dev only: config root, HTTP port, HTTPS port, and",
		"telemetry environment. Any AIMEBU_DEV_<name> becomes AIMEBU_<name>",
		"for the dev server (for example BIND, ALLOW, TLS_CERT, or TLS_KEY).",
		"Inherited production AIMEBU_* settings are cleared first."}},
	{[]string{"AIMEBU_DEV_SENTRY_DSN"}, []string{
		"make dev only: the Sentry DSN the dev server reports to; without it",
		"make dev runs with AIMEBU_SENTRY_DSN=off"}},
	{[]string{"AIMEBU_STUCK_MINUTES"}, []string{"Minutes of silence (1-1440, default 10) before a running turn counts as stuck"}},
	{[]string{"AIMEBU_DEBUG"}, []string{"Set to log lookups the server could not answer (a worktree's base drift, for now)"}},
	{[]string{"AIMEBU_DEBUG_PPROF"}, []string{"Serve owner and admin profiling endpoints under /api/debug/pprof/ when set to 1"}},
	{[]string{"AIMEBU_EVAL"}, []string{
		"Set to 1 to let `make eval` (the live answer-robustness eval) run; without it the",
		"runner refuses, because a live run spends subscription quota. See docs/eval.md"}},
	{[]string{"AIMEBU_BASE_DRIFT_MIN"}, []string{
		"Commits a room worktree must be behind its base ref before the turn context says so",
		"(default 5, 0 turns it off)"}},
	{[]string{"AIMEBU_TURN_IDLE", "AIMEBU_TURN_MAX"}, []string{
		"Supervisor stall deadlines per turn: silence with no tool running (default",
		"30m) and the wall-clock cap (default 90m) (Go durations, 0 disables)"}},
	{[]string{"AIMEBU_WARM_IDLE", "AIMEBU_WARM_MAX_LIFETIME", "AIMEBU_WARM_MIN_FREE_MB", "AIMEBU_WARM_MIN_FREE_PERCENT"}, []string{
		"Warm agents (on by default, 5 minutes): AIMEBU_WARM_IDLE (a Go duration such as 10m) keeps an",
		"agent's harness process up that long after a turn so the next message skips the start; 0 is",
		"off, unset is 5m, and the owner's setting in Settings -> General wins over it when one is saved.",
		"AIMEBU_WARM_MAX_LIFETIME (default 1h) ends a process that old at the end of its turn;",
		"AIMEBU_WARM_MIN_FREE_MB (default 1536) and AIMEBU_WARM_MIN_FREE_PERCENT (default 10) end",
		"the longest-idle process while free memory is below either"}},
	{[]string{"AIMEBU_TEST_RESTRICT_PATH"}, []string{
		"Set to 1 by the test helpers: harness discovery then reads PATH only, not the",
		"machine-wide install directories, so a test never finds the developer's CLIs;",
		"never set it in production"}},
	{[]string{"AIMEBU_INSECURE_SKIP_VERIFY"}, []string{"Disable client TLS verification for development self-signed certificates"}},
}

const envNameColumn = 22

// printEnvironment writes the Environment section of `aimebu help`. A single
// short name shares a line with its text; anything else puts the text on the
// lines below, indented under the names.
func printEnvironment(w io.Writer) {
	fmt.Fprintln(w, "Environment:")
	pad := strings.Repeat(" ", envNameColumn+2)
	for _, entry := range environment {
		names := strings.Join(entry.names, " / ")
		if len(entry.names) == 1 && len(names) <= envNameColumn-2 {
			fmt.Fprintf(w, "  %-*s%s\n", envNameColumn, names, entry.help[0])
			for _, line := range entry.help[1:] {
				fmt.Fprintf(w, "%s%s\n", pad, line)
			}
			continue
		}
		fmt.Fprintf(w, "  %s\n", names)
		for _, line := range entry.help {
			fmt.Fprintf(w, "%s%s\n", pad, line)
		}
	}
}
