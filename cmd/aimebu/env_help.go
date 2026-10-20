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
	{[]string{"AIMEBU2_CONFIG_DIR"}, []string{"Config root directory (default: ~/.aimebu2)"}},
	{[]string{"AIMEBU2_BIND"}, []string{"Server bind address (default: 127.0.0.1)"}},
	{[]string{"AIMEBU2_PORT"}, []string{"Server listen port"}},
	{[]string{"AIMEBU2_ALLOW"}, []string{"Comma-separated IPs/CIDRs allowed to connect"}},
	{[]string{"AIMEBU2_TLS_CERT"}, []string{"TLS certificate PEM path; set with AIMEBU2_TLS_KEY for HTTPS"}},
	{[]string{"AIMEBU2_TLS_KEY"}, []string{"TLS private key PEM path; set with AIMEBU2_TLS_CERT for HTTPS"}},
	{[]string{"AIMEBU2_TLS_PORT"}, []string{"HTTPS listen port when TLS is configured"}},
	{[]string{"AIMEBU2_TRUSTED_PROXIES"}, []string{
		"Comma-separated proxy IPs/CIDRs whose X-Forwarded-For separates clients",
		"for login throttling only (default: none)"}},
	{[]string{"AIMEBU2_PUBLIC_URL"}, []string{"HTTP(S) origin the server advertises when it sits behind a proxy"}},
	{[]string{"AIMEBU2_URL"}, []string{"Server URL for the supervisor and agent-tools"}},
	{[]string{"AIMEBU2_AGENT_ID", "AIMEBU2_AGENT_KEY"}, []string{
		"A managed agent's identity and key for agent-tools; the supervisor sets",
		"them in the agent's MCP config, you normally never do"}},
	{[]string{"AIMEBU2_OWNER_LOGIN", "AIMEBU2_OWNER_PASSWORD"}, []string{"Bootstrap the owner account on first start (set both; without them the server prints a one-time setup token)"}},
	{[]string{"AIMEBU2_EMBEDDED_SUPERVISOR"}, []string{"Set to 0 to run the server without its local supervisor (remote-only)"}},
	{[]string{"AIMEBU2_ALLOW_NATIVE"}, []string{
		"Set to 1 when migrating a database to enable host-native launchers; has no",
		"effect afterwards"}},
	{[]string{"AIMEBU2_USAGES"}, []string{"Set to 0 to disable credential reads and provider usage requests"}},
	{[]string{"AIMEBU_TOKEN_LEAD_CLAUDE", "AIMEBU_TOKEN_LEAD_CODEX"}, []string{
		"How long before expiry the token refresher renews a profile (Go durations,",
		"10m to 14d; defaults 60m Claude, 24h Codex); only with the token-copy model"}},
	{[]string{"AIMEBU2_DEV_IMPORT_V1"}, []string{
		"Set to 1 to enable the offline `import-v1` command for development tests;",
		"normal imports happen only in the first-run wizard"}},
	{[]string{"AIMEBU2_DEV_SKIP_V1_PROBE"}, []string{
		"Set to 1 to skip the v1 port 9997 listener probe so a dev instance can",
		"rehearse the cutover on a copy of v1 data; never in production, the v1",
		"daemon pid-file check still runs"}},
	{[]string{"AIMEBU2_STUCK_MINUTES"}, []string{"Minutes of silence (1-1440, default 10) before a running turn counts as stuck"}},
	{[]string{"AIMEBU2_TURN_IDLE", "AIMEBU2_TURN_MAX"}, []string{
		"Supervisor stall deadlines per turn: silence with no tool running, and",
		"wall-clock cap (Go durations, 0 disables; defaults depend on the harness)"}},
	{[]string{"AIMEBU2_INSECURE_SKIP_VERIFY"}, []string{"Disable client TLS verification for development self-signed certificates"}},
	{[]string{"AIMEBU_BIND"}, []string{
		"v1's bind address; probed for a running v1 server before the first start",
		"over a v1 data directory"}},
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
