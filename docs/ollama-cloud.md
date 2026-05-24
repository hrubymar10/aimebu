# Ollama Cloud Usage

Ollama Cloud usage snapshots can use either an Ollama API key or a manually
pasted browser `Cookie` header from <https://ollama.com/settings>.

The API-key path verifies Cloud API access by calling
`https://ollama.com/api/tags`. Ollama does not expose Cloud quota windows
through that API, so API-key snapshots show access as verified but do not show
session or weekly usage bars.

Cookie auth fetches the settings page and parses the Cloud Usage quota
windows. Use this path when you want plan, session, and weekly usage data.
Automatic browser or keychain cookie import is intentionally not supported.

## Web Setup

In Settings -> Usages -> Ollama Cloud, choose one of:

- `Auto`: try the Cookie header first for quota windows, then fall back to the
  API key if the cookie path fails and an API key is configured.
- `Cookie`: only use the browser Cookie header.
- `API key`: only verify API-key access.

To configure an API key, create one at <https://ollama.com/settings/keys>,
paste it into the Ollama Cloud row, and save.

To configure a Cookie header:

1. Open Ollama settings in the same browser session that is signed in.
2. Open developer tools, then the Network panel.
3. Reload or select the `settings` request.
4. Copy the request `Cookie` header.
5. Paste it into the Ollama Cloud row and save.

Secrets are stored in `~/.aimebu/usages/config.json` with file mode `0600`.
After saving, aimebu only shows whether a cookie or API key is configured; it
never returns either secret through the HTTP API, CLI, cache, websocket
payloads, or the Settings UI.

When a pasted header contains multiple recognized Ollama session cookies,
aimebu tries the full header first, then retries distinct session-cookie
candidates before treating a signed-out settings page as an expired login.
This helps with browser headers that contain both stale and current session
cookies.

If credentials expire or are rejected, Ollama Cloud snapshots show
`auth_missing`. Paste a fresh Cookie header or API key to resume updates.

CLI:

```bash
aimebu usages ollama-cloud
aimebu usages ollama-cloud --json
```

Common failure states:

- `auth_missing`: no matching credential is configured, the API key was
  rejected, the cookie is missing a recognized session cookie, or the settings
  page requires sign-in.
- `fetch_error`: the settings page or API endpoint could not be fetched, or
  the response shape changed.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.
