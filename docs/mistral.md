# Mistral usage

Paste the browser `Cookie` request header from a signed-in
`console.mistral.ai` request into Settings → Usages → Mistral. It must include
`csrftoken` and an `ory_session_*` cookie. The saved header lives only in
`usages/config.json` (0600); Settings shows a configured flag, never its value.
Automatic browser or keychain cookie import is not supported.

The Vibe monthly quota comes from the console billing endpoint. The provider
reads the active API plan from the admin endpoint for the badge and, when
available, adds pay-as-you-go spend in a Credits row. A plan or spend lookup
failure does not erase a working Vibe window. The Vibe request forwards only
`csrftoken` and `ory_session_*` cookies. Clear the cookie in Settings to
remove it and disable the provider.
