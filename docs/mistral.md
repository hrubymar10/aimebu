# Mistral Usage

Mistral usage snapshots use a manually pasted browser `Cookie` header from
<https://console.mistral.ai>. Automatic browser or keychain cookie import is
intentionally not supported.

The primary snapshot is the Mistral Vibe monthly quota. aimebu calls the
console billing endpoint and maps `usage_percentage` to a `monthly` usage
window with the returned `reset_at` time. It also calls
`admin.mistral.ai/api/users/me` to read `organization.active_api_plan` and
display your real plan tier (e.g. "Free", "Pro") as the card badge; if that
call fails, the badge falls back to "Vibe" so a plan-fetch failure never
degrades a working quota card. When Mistral also reports pay-as-you-go API
spend, aimebu fetches the admin billing endpoint and shows a secondary
`Credits` row for monthly spend.

## Web Setup

In Settings -> Usages -> Mistral:

1. Open <https://console.mistral.ai> in the same browser session that is
   signed in.
2. Open developer tools, then the Network panel.
3. Reload the page or select the request to
   `api-ui/trpc/billing.vibeUsage`.
4. Copy the request `Cookie` header. It must include both `csrftoken` and at
   least one `ory_session_*` cookie.
5. Paste the header into the Mistral row and save.

Secrets are stored in `~/.aimebu/usages/config.json` with file mode `0600`.
After saving, aimebu only shows whether a cookie is configured; it never
returns the secret through the HTTP API, CLI, cache, websocket payloads, or
the Settings UI.

For the console Vibe quota request, aimebu forwards only `csrftoken` and
`ory_session_*` cookies to `console.mistral.ai` and drops other cookies from
the pasted header. The API-spend fallback uses the normalized cookie header
against `admin.mistral.ai`.

CLI:

```bash
aimebu usages mistral
aimebu usages mistral --json
```

Common failure states:

- `auth_missing`: no cookie is configured, `csrftoken` is missing or invalid,
  no `ory_session_*` cookie was pasted, or Mistral rejected the cookie.
- `fetch_error`: the console or admin billing endpoint could not be fetched,
  or the response shape changed.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.
