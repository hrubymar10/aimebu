# GitHub Copilot usage

In Settings → Usages, select **Sign in with GitHub**, open the displayed
verification page, and enter the code. The verification address GitHub (or an Enterprise host) returns
must be `https`: the server refuses a device-code answer with any other, and the page links only an `https` address. The server owns the expiring device
flow and enforces GitHub's polling interval. Starting a new sign-in keeps the
previous token and host together; successful polling replaces both in one
config update. The token is saved in `usages/config.json` (0600) and enables
Copilot. The token is never returned through the browser API or cache.
**Sign out** deletes it and
disables the provider.

An optional GitHub Enterprise host uses its own HTTPS device-code and token
endpoints (`/login/device/code` and `/login/oauth/access_token`) and its
`api.` host for usage and identity requests. A configured port is retained
for both. Invalid hosts are rejected before the flow begins. A saved token
is only sent to the API host associated with its completed sign-in.
Copilot monthly quotas appear as usage windows.
Account email is fetched separately at most once per hour per token and host,
so switching enterprise hosts cannot reuse another account's identity.
Token-billed seats may instead report raw premium requests used; the UI shows
that count without inventing a percentage. Missing or rejected credentials
report `auth_missing`; missing quota permission reports `scope_missing`.
