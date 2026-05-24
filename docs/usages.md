# Usage Snapshots

The Usages feature shows provider quota snapshots in the right sidebar,
Settings -> Usages, and the `aimebu usages` CLI command. The top-bar Usages
button opens the right-sidebar Usages view, where all known providers are
rendered as a vertical list.

Supported providers:

| Provider | Auth source | Setup surface |
|---|---|---|
| `codex` | Codex OAuth file at `$CODEX_HOME/auth.json`, or `~/.codex/auth.json` | Enable in Settings -> Usages |
| `claude-code` | Claude Code OAuth file at `~/.claude/.credentials.json` | Enable in Settings -> Usages |
| `github-copilot` | GitHub device flow token stored locally by aimebu | Settings -> Usages -> Sign in with GitHub |
| `ollama-cloud` | Browser `Cookie` header from `https://ollama.com/settings`, or Ollama API key | Settings -> Usages -> Ollama Cloud credentials |

Provider secrets are stored in `~/.aimebu/usages/config.json` with file mode
`0600`. `~/.aimebu/usages/cache.json` stores only redacted last successful
snapshots and has file mode `0644`.

## CLI

```bash
aimebu usages
aimebu usages codex
aimebu usages claude-code --json
aimebu usages github-copilot --plain
aimebu usages ollama-cloud --json
```

Plain text is the default. `--json` returns the normalized response used by
the web UI, including provider metadata, settings, and snapshots keyed by
provider.

## Refresh Behavior

The stored refresh interval defaults to `120` seconds and has a minimum of
`15` seconds. `AIMEBU_USAGES_REFRESH` overrides the stored value for both the
server and CLI.

The right-sidebar force-refresh button calls `POST /api/usages/refresh`. It
bypasses the normal interval but has a separate server-side 15 second
cooldown. During cooldown the endpoint returns HTTP `429` with:

```json
{"retry_after_sec": 15}
```

## Provider Ordering

Settings -> Usages includes up/down controls for the provider rows. The saved
order controls the vertical order in the web Usages sidebar and the provider
metadata order in `aimebu usages --json`. Empty or older configs use the
canonical order (`codex`, `claude-code`, `github-copilot`, `ollama-cloud`);
unknown entries are ignored and missing known providers are appended.

## Stale Cache

When a provider fetch fails from a transient transport or server-side problem
after a previous successful snapshot exists, aimebu returns the previous plan,
windows, and credits with:

- `status: "stale_cache"`
- `stale: true`
- sanitized `error` / `error_detail` for the current failure

The CLI marks these rows with `(stale)`. The web sidebar shows the stale state
and error instead of presenting cached values as fresh.

Stale-cache preservation covers timeouts, canceled refreshes, DNS failures,
connection failures or resets, and HTTP 5xx responses. Credential and
permission failures such as `auth_missing` and `scope_missing` replace the
cached snapshot so expired or unauthorized auth does not look like live quota
data.

Credit snapshots can include both current spend and a spend limit. The CLI
prints those as `used/limit`; the web sidebar shows the same pair in the
provider's credits row.

## Troubleshooting

- `auth_missing`: authenticate in the provider's own CLI or complete the
  provider setup in Settings -> Usages.
- `scope_missing`: the token was accepted but lacks access to the usage
  endpoint. Re-authenticate or check the account's plan/enterprise policy.
- `timeout`: a provider request exceeded aimebu's per-request timeout. The
  provider may be slow or unreachable; retry later.
- `fetch_error`: upstream returned an unexpected status or shape. The
  `error_detail.fields` map records field names and types only, never values.
- `stale_cache`: the latest fetch failed but cached values are still shown.

Provider-specific setup and failure notes live in:

- [Codex](codex.md#usage-snapshots)
- [Claude Code](claude-code.md#usage-snapshots)
- [GitHub Copilot](github-copilot.md)
- [Ollama Cloud](ollama-cloud.md)
