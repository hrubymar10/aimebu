# Ollama Cloud Usage

Ollama Cloud usage snapshots use a manually pasted browser `Cookie` header
from <https://ollama.com/settings>. Automatic browser or keychain cookie import
is intentionally not supported.

To configure it from the web UI:

1. Open Ollama settings in the same browser session that is signed in.
2. Open developer tools, then the Network panel.
3. Reload or select the `settings` request.
4. Copy the request `Cookie` header.
5. In aimebu Settings -> Usages, paste it into the Ollama Cloud row and save.

The cookie is stored in `~/.aimebu/usages/config.json` with file mode `0600`.
After saving, aimebu only shows whether a cookie is configured; it never
returns the cookie through the HTTP API, CLI, cache, websocket payloads, or the
Settings UI.

If the cookie expires, Ollama Cloud snapshots show `auth_missing`. Paste a
fresh `Cookie` header from a signed-in browser session to resume updates.

CLI:

```bash
aimebu usages ollama-cloud
aimebu usages ollama-cloud --json
```

Common failure states:

- `auth_missing`: no cookie is configured, the cookie is missing a recognized
  session cookie, or the settings page requires sign-in. Paste a fresh Cookie
  header from a signed-in browser session.
- `fetch_error`: the settings page could not be fetched or its usage markup
  changed shape.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.
