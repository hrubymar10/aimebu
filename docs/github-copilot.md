# GitHub Copilot Usage

GitHub Copilot usage snapshots use a GitHub device flow from Settings →
Usages. Click **Sign in with GitHub**, open the verification page, enter the
shown user code, and wait for aimebu to finish polling.

The access token is stored in `~/.aimebu/usages/config.json` with file mode
`0600`. It is never returned through the HTTP API, CLI, cache, or websocket
payloads. **Sign out** deletes the local token and disables the provider.

Enterprise setups can enter an HTTPS GitHub Enterprise host in the Copilot
row before signing in. Empty means `https://api.github.com`; an enterprise
host such as `https://github.example.com` is normalized to
`https://api.github.example.com` for the quota request.

CLI:

```bash
aimebu usages github-copilot
aimebu usages github-copilot --json
```

Common failure states:

- `auth_missing`: no local Copilot token is configured, the device-flow code
  expired, sign-in was denied, or the token was rejected. Start the GitHub
  sign-in flow again from Settings -> Usages.
- `scope_missing`: GitHub accepted the token but denied the usage endpoint.
  Check the account plan or enterprise policy.
- `fetch_error`: the Copilot usage response changed shape or returned an
  unexpected status.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.
