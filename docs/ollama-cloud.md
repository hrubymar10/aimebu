# Ollama Cloud usage

Settings → Server → Accounts & usage → Ollama Cloud accepts a browser `Cookie` request header
from `ollama.com/settings`, an API key, or both. **Auto** tries the cookie for
quota data and falls back to the API key; **Cookie** only uses the browser
header; **API key** only reads the balance endpoint. A rejected key (HTTP 401
or 403) reports `auth_missing`; an unrecognized balance response is a fetch
error, never zeroed numbers.

API-key mode sends one authenticated GET to `https://ollama.com/api/balance`.
The included allowance becomes the **Monthly** window (used = allowance −
included balance, reset at `period.until`), and purchased credits appear in the
same **Credit balance** row the cookie path uses. Numeric and decimal-string
amounts are both accepted; a zero allowance carries no percentage, and a
purchased-only account shows its balance without quota bars. The legacy
balance shape — per-window `remaining_percent` for `session` and `weekly` —
maps to the same **Session** and **Weekly** windows the cookie path reports.

Cookie mode parses session, weekly, and monthly Cloud Usage windows from the
settings page. Free plans label the included-credit meter **Free usage**
instead of **Monthly usage**; both labels drive the same Monthly window.
Usage labels are matched on their own text node, so explanatory prose that
mentions a label ("Free usage credits can be used with the following cloud
models:") neither starts a window nor truncates a neighbouring reset
timestamp. A credit-wallet page — a **Usage credits** heading with a balance
and monthly credits used instead of quota meters — shows the reported balance
as **Credit balance** (never a percentage) beside any meters the page also
reports. Pasted raw pairs, `Cookie:` headers, and common `curl` cookie
forms are accepted. Multiple recognized session cookies are retried when a
stale one appears alongside a current one. A signed-out redirect reports
`auth_missing`. Credentials stay in `usages/config.json` (0600); the API only
returns configured flags. **Clear credentials** removes both values and
disables the provider.
