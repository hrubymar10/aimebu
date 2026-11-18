# Authentication foundation

## First-run owner setup

A first `aimebu server` run with no owner can create one two ways.

**In the browser (default).** With neither `AIMEBU_OWNER_LOGIN` nor
`AIMEBU_OWNER_PASSWORD` set, the server starts and prints a setup token to
stderr (and the log):

```
No owner exists yet. Open the setup page and enter this setup token (single use, valid until the server restarts):
  setup_…
The token is also in /home/you/.aimebu/.setup_token
Shortcut that fills the token in for you:
  http://127.0.0.1:9997/setup?token=setup_…
```

The same token is written to `$AIMEBU_CONFIG_DIR/.setup_token` (0600) and the
file is deleted once the owner exists. The shortcut link uses
`AIMEBU_PUBLIC_URL` when set, otherwise the listen address (HTTPS when TLS is
on). Opening the server's plain address (or `/setup`) shows the welcome, then
**Enter the setup token to continue**: an input and Continue, with the hint
"find it in the output of the server, or in the file `<full path>`" (the path
comes from `GET /api/setup/status`, which answers `{open:false}` (and no path) once an owner exists;
a client on the same machine (a loopback connection with no
forwarding header, so not a reverse proxy relaying someone) gets the full path, any other only
`<config dir>/.setup_token`, so the page does not reveal the server's user name
or directory layout; the token itself is never returned by any API). Continue checks the token with
`POST /api/setup/token {token}` (204 when right, 403 `invalid_setup_token`
when wrong, 429 rate limited, 404 when setup is closed), without spending it.
The link with `?token=` is only a shortcut: the token is checked at once and the
token screen is skipped. Then the "Create owner" form (login, password twice)
posts `POST /api/setup {token, login, password}`, which creates the owner and the
host machine in one transaction (the same code as the environment path),
opens a session and returns the same body as `POST /api/login`. The rest of
the first run follows in the browser: **Choose a theme** (a tile per theme,
applied at once and saved to the profile; the setup pages themselves follow the
system light/dark setting), the v1 import wizard when a backup is waiting, and
finally **Create your first project** (a project with its home room and a
*general* room) with a short "How aimebu works" and a Spawn shortcut. With no
room selected the chat shows an empty-state card instead of the composer.

Personal interface preferences (theme, notification sound, volume and mute,
system events, debug inspector, composer formatting) are stored per user with
`GET`/`PUT /api/me/preferences`, so they follow the person across devices; the
browser keeps only a cache to paint before sign-in. Per-device layout
(collapsed panels and rooms) stays in the browser.
The login must be 3-32 lowercase letters, digits, `-` or `_` starting with a
letter (capitals are lowercased) and the password at least 12 characters, as
for invitations and for the environment variables.

The embedded supervisor, which needs the host machine key, starts in the
background right after the owner is created (reading each tool's version takes
seconds, and the setup request does not wait for it); until then the server only
serves setup. A v1 cutover works the same way:
the files are moved, the server starts, and setup creates the owner.

The token is single use and stops working the moment the owner exists (the
file is deleted and `/setup`, `POST /api/setup/token`
and `POST /api/setup` answer 404, and `GET /api/setup/status` answers `{open:false}`). Restarting the server without an owner mints
a new token and invalidates the old one. The token is compared in constant time
(surrounding spaces and a pasted newline are ignored). A wrong token answers
403, on the check as on the setup itself; after five wrong guesses from one
address further attempts, even with the right token, answer 429 with
`Retry-After` and back off exponentially (per IP). A
rejected login or password does not use the token up, so a typo does not lock
you out. Anyone who can read the log or `.setup_token` can create the
owner, so keep the server on loopback or behind `AIMEBU_ALLOW` until it is
done.

**From the environment (headless).** Set `AIMEBU_OWNER_LOGIN` and
`AIMEBU_OWNER_PASSWORD` before the first run (set both or neither; one alone
is refused). The same login and password rules apply (a login or password that fails them
stops the start with a message naming the rule). The command creates the owner and an approved host
machine in one transaction, writes the host machine key before that transaction
commits, and rolls back the owner and machine if the key cannot be written. It
never replaces an existing key file. The key is stored at
`server/host-machine.key` (0600) without printing the secret, and starts the
HTTP server. Later runs leave those rows alone.
If a crash leaves the key file behind but the database has no owner or machine,
startup explains that the orphaned key is safe to delete before retrying.
The default database is `~/.aimebu/server/aimebu.sqlite` (mode 0600);
`AIMEBU_CONFIG_DIR` selects another root. V2 keeps its `AIMEBU_*` environment
settings separate from v1's `AIMEBU_*` settings while using the same default
data directory.

Passwords use the standard library's PBKDF2-HMAC-SHA256 with a fresh 16-byte
salt and 600,000 iterations. The hash and parameters (algorithm, iterations,
salt length and salt) are stored separately; successful login upgrades older
parameters. [OWASP's Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
recommends 600,000 iterations for PBKDF2-HMAC-SHA256. Sixteen random bytes
exceed [NIST SP 800-63B's salt minimum](https://pages.nist.gov/800-63-4/sp800-63b.html).

Sessions use 256-bit random bearer tokens. SQLite stores only SHA-256 token
hashes. Session cookies are HttpOnly and SameSite=Strict, with Secure set on
TLS requests and on proxied requests whose `Host` is the host of an HTTPS
`AIMEBU_PUBLIC_URL`. The default lifetime is seven days and slides on
authenticated HTTP requests, but the stored expiry is moved only once it lags
the new one by more than a tenth of the lifetime (at least five minutes, at most
half the lifetime), so a page load's parallel requests do not each write. The
effective idle timeout is therefore between about 6 days 7 hours and 7 days.
A session also has an absolute lifetime of 30 days from when it was made
(`auth_session.created_at`): the expiry never slides past it, and the session
is refused after it however steadily it is used, so a stolen token ends. Signing
in again starts a new one. Sessions that existed when this was added start their
30 days at that migration. An open event socket checks session validity every 30 seconds
without renewing its expiry. Unsafe HTTP methods require `X-CSRF-Token`; WebSocket upgrades need
a valid session cookie and a same-origin `Origin`; behind a reverse proxy the
configured `AIMEBU_PUBLIC_URL` origin also counts as same-origin. Failed logins are throttled
per (login, client) and per client, with exponential backoff, so someone guessing at a login from
one address cannot lock the person out of it from another (behind a proxy listed in
`AIMEBU_TRUSTED_PROXIES`, the remote IP comes from `X-Forwarded-For`; see
[HTTPS](tls.md#reverse-proxy)), with the same invalid-login
error for unknown users and wrong passwords. Password verification pads older
hash policies to the strongest stored or current iteration count, including
the work done for unknown users. The throttle reserves in-flight attempts
atomically per login and client. A client is one IPv4 address or one IPv6 /64.
Five failures per login/client in a 15-minute window are allowed before
exponential backoff starts, and the client's own total across logins is
throttled the same way. A successful sign-in clears that login's failures for that
client but not the client's total, so signing in to one's own account does not
restore the budget for guessing at others. Once more than 50 guesses at one login
have failed across all clients, further guesses at it from a client that has already
failed at it are spaced a second apart: a fixed pause that never grows. A client
with no failures at that login is not held by it, so the person with the right
password gets through while a guesser elsewhere keeps the pause running. The trade
is that a guesser who spreads over many addresses gets each address's own free guesses
(five per 15 minutes) and the per-client backoff, not the pause: the aggregate slows
repeat guessers, it does not cap a very distributed one. Guesses
that cannot be right (an empty name or password, a name over 256 bytes) are
refused before any password work and count as failures. The throttle prunes expired
entries and caps its entry map; when it is full it forgets entries that have not
earned a delay first, oldest first, and keeps those serving a delay, so a flood of
one-guess clients cannot wash a penalty away.
Machine and agent keys are
256-bit random values prefixed `amk_` and `aak_`; only hashes are stored, and
revocation invalidates them.

`POST /api/login` accepts JSON `login` and `password` and returns
`user_id`, `csrf_token`, and `expires_at` (plus `first_run`, `owner` and `admin`, and `host_home` for the owner and admins; see api.md) while setting the session cookie.
An exact stored login wins. Otherwise, a trimmed, case-insensitive match is
accepted only when it identifies one account; ambiguous legacy names require
their exact spelling. New accounts store the lowercase form, and invitation
signup rejects names that would match an existing account under the same
Unicode-aware trimming and case rule. The fallback checks login names before
loading password hashes.
Send the token in `X-CSRF-Token` on mutations. `GET /api/me` returns the
current user and CSRF token; `POST /api/logout` revokes the session.

Settings → Server → Users & invites lets a project owner issue a single-use invitation for that
project, change member roles, or remove members. The first account is the
global owner and can also list all accounts, issue password-reset links, and
deactivate accounts. An invitation can grant viewer, operator, or owner access;
the issuer must still be a project owner when it is redeemed. Links expire
after one hour, carry a 256-bit random token in the URL fragment, and store
only its SHA-256 hash in SQLite. A new person chooses a login and password
(at least 12 characters). An existing user signs in before using a project
invitation so the grant attaches to that account. A reset link sets a new
password and revokes all prior sessions. Deactivation also revokes sessions
and blocks future sign-in; a project cannot lose its last active owner.

`POST /api/invites` creates a link. Opening it shows one card in place of the sign-in card: `POST /api/invites/inspect` tells the page what the
link is for (the project and role of a join link, or the account a reset link belongs to), so the card says "You are invited to <project> as <role>"
and asks for a login and password, or says "Choose a new password for <login>" with the account shown read-only, and has one action that redeems
the link and signs in. A person who already has an account switches to the sign-in card with "I already have an account" and uses the link from
inside afterwards. A link that cannot be used says so instead of offering fields. `POST /api/invites/redeem` accepts the
token plus new-account credentials, a new password for a reset, or an existing
session for a project grant. Successful redemption returns the stored login
for the follow-up sign-in. Signed-in redemption requires the session's CSRF
token. If automatic sign-in fails after redemption, the invitation form retries
sign-in without redeeming the spent token again. `GET /api/users` lists accounts
for the global owner and the admins;
`GET /api/users?project_id=...` lists a project owner's members.
`PUT` or `DELETE /api/projects/{project}/members/{user}` changes or removes
a membership. `POST /api/users/{user}/deactivate` is for the global owner, or an admin acting on an ordinary account (see below).
Viewers can read their project but cannot send in ordinary rooms, manage rooms,
or control agents. They can send, react, and attach files in a two-person
private chat while both people remain active project members and no agent has
joined. A viewer who is a peer in a private chat can answer its
pending questions and ordinary approvals; boundary admission requires an
owner. The usage limits are readable by a global admin too (profile names included, no e-mail, host, error text or credential state); the account details, the switcher and every change require the global owner.

A private chat between agents (started by a lead's `dm_agent`) belongs to no
project, so access is decided by the projects of the agents in it, and checked
on every request: reading needs the instance owner or a membership, at any
role, in the project of an agent still in the chat, and having joined it (the
chats one could join are listed by title only); writing needs owner or operator
in one of those projects, or the instance owner, so a viewer reads only. A
person's message wakes only the agents whose project they reach (owner or
operator there, or the instance owner); an agent of a project they only read, or
do not belong to, stays asleep (`message_not_woken` reason `no_reach`) though
the message is in the chat. A chat agent's questions go to the owners and
operators of the asking agent's project, and its approvals to that project's
owners, who see them in their Inbox; a question or approval nobody in the chat
could answer is refused rather than left to block the agent. Admins
get no implicit reach: like for any project's rooms, they add themselves to a
project first. Whether a lead may start a chat with an agent is likewise decided
by the server: every person whose message caused the lead's turn must be an
owner or operator of the target's project (or the instance owner), and when no
person caused it, an owner of the lead's own project must.

## Global admins

The owner is the first account. The owner can flag other accounts as **admins** (Settings → Server → Users & invites → Accounts, the *Admin* toggle that only
the owner sees; `PUT /api/users/{user}/admin {admin}`, migration adds `user.admin`). An admin has the owner's reach over people, projects
and global settings, and can read the list of machines and a harness's launchers, but cannot pair, approve or revoke a machine, change what a machine runs (launchers, harness models and providers, a harness's path, detecting a harness's models), nor reach provider logins, the account switcher, the quota switcher or the v1 import. A deactivated account has no reach whatever its flag
says; taking the flag away ends it at once.

| | owner | admin | project owner | operator / viewer |
| --- | --- | --- | --- | --- |
| create projects, change a project's folder | yes | yes | no | no |
| rename a project | yes | yes | their own project | no |
| list every account; invites into any project at any role; change anyone's project role | yes | yes (not the owner's or another admin's role) | their own project | no |
| password-reset link, deactivate | any account but the owner can be deactivated | ordinary accounts only | no | no |
| appoint or remove admins | yes | no | no | no |
| macros, message expiry, sounds, global memory, global roles/prompts, cleanup and disk report, settings export, the machine list and a harness's launchers (read-only), the usage limits (read-only: no e-mail, host, error text or credential state) | yes | yes | no | no |
| stop all agents on a machine (`POST /api/machines/{id}/agents/stop`, Settings → Agents → Machines): the agents they could stop one by one | yes | yes | no | no |
| machines (pairing, approval, revoking), what a machine runs (detecting the installed harnesses, saving, changing or removing a launcher; a harness's models, providers and path override; detecting a harness's models, which sends a provider key), account switcher and changing provider usages and credentials, quota switcher and limit guard, v1 import, settings preview/apply | yes | no | no | no |
| admit a paired machine for a project (`POST /api/projects/{project}/machine-folders/{machine}/admit`) | yes | yes | their own project | no |

Links follow their issuer: a password-reset link is rechecked when it is used (the issuer must still be allowed to act on that account, so
an admin who lost the flag or was deactivated, or whose target was promoted to admin meanwhile, cannot use an old link), and losing the
flag or the account spends every unused link the account issued. An admin creating a project becomes its owner member and picks its folder
on the host, as the owner does. An admin's settings export leaves out the quota switcher and limit guard sections.

**An admin must be trusted like the owner.** The things an admin may change are not harmless settings: global roles, prompts and memory shape what every
agent is told, and creating a project picks a folder on the host, so an admin steers agents that run on the host with the owner's logins. What the
admin cannot do is reach the owner's own account, other admins, pair or approve machines or reach provider logins; it can still act through the agents.

An admin who needs a project's rooms adds themself to it, as the owner does (membership still decides who sees a project's rooms and acts
in them). Admin is never implied for the owner's own account in the flag: the owner is an admin by being the owner, so the flag cannot be
set on it. System events that carry no project (credentials, quota switching) stay the owner's to see.

The web page reads the account's reach from the sign-in answers (`owner`, `admin`) and shows only what it can use: a viewer cannot write in or
spawn agents into an ordinary room (the composer and Spawn are off with a note saying why; a private chat between two members still works),
and anyone who is not the owner or an admin gets New project switched off and no Roles, Prompts, Macros, Machines or Harnesses settings tab (an admin has them, with pairing, approving and revoking machines, and everything that sets harnesses up, switched off and a reason). These are conveniences over the server's checks, which stay the rule.
