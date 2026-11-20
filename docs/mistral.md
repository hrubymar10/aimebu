# Mistral usage

Paste the browser `Cookie` request header from a signed-in
`console.mistral.ai` request into Settings → Server → Accounts & usage → Mistral. It must include
`csrftoken` and an `ory_session_*` cookie. The saved header lives only in
`usages/config.json` (0600); Settings shows a configured flag, never its value.
Automatic browser or keychain cookie import is not supported.

The Vibe monthly quota comes from the console billing endpoint. The provider
reads the active API plan from the admin endpoint for the badge and, when
available, adds pay-as-you-go spend in a Credits row. A plan or spend lookup
failure does not erase a working Vibe window. The Vibe request forwards only
`csrftoken` and `ory_session_*` cookies. Clear the cookie in Settings to
remove it and disable the provider.

API spend is priced from the billing usage response's price table. Mistral
lists one price per event type, metric, group, API zone, and service tier, and
the same metric and group can carry a far higher per-second audio price or a
priority-tier price, so every reported dimension is part of the key; a legacy
table that omits both zone and tier is matched through that explicitly
unqualified row. A unit whose zone and tier have no matching price prices at
zero rather than at another tier's rate.

A missing or expired session answers with a redirect to
`auth.mistral.ai/self-service/login`; the provider reports that as
`auth_missing` (the cookie must be pasted fresh) instead of a fetch error.
Unrelated redirects stay fetch errors.
