# Ollama Cloud usage

Settings → Usages → Ollama Cloud accepts a browser `Cookie` request header
from `ollama.com/settings`, an API key, or both. **Auto** tries the cookie for
quota data and falls back to the API key; **Cookie** only uses the browser
header; **API key** only verifies Cloud API access. The API-key endpoint does
not expose quota windows, so that mode shows access status without usage bars.

Cookie mode parses session, weekly, and monthly Cloud Usage windows from the
settings page. Pasted raw pairs, `Cookie:` headers, and common `curl` cookie
forms are accepted. Multiple recognized session cookies are retried when a
stale one appears alongside a current one. A signed-out redirect reports
`auth_missing`. Credentials stay in `usages/config.json` (0600); the API only
returns configured flags. **Clear credentials** removes both values and
disables the provider.
