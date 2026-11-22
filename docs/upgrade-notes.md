# Upgrade notes

What changes for a running install when it is upgraded to a build with the security hardening of this series, and what to do about it.
Each item names the document that describes the behaviour in full.

## Sign-in, setup, invites and pairing answer only expected host names

Sign-in, the first-run setup, invites and machine pairing now answer `421 unexpected_host` for a `Host` that is not `localhost` (or
`*.localhost`), an IP address, the host of `AIMEBU_PUBLIC_URL`, or a name listed in `AIMEBU_ALLOWED_HOSTS`. If you reach the server by a
LAN or VPN **name** (`studio.local`, a MagicDNS name), sign-in stops working until you list it:

```bash
export AIMEBU_ALLOWED_HOSTS=studio.local
```

A server reached by IP address, by `localhost` or through its public URL is not affected. See [HTTPS](tls.md#host-names).

## Remote supervisors paired over plain http

A supervisor on another machine that is paired against a plain-`http` server (not loopback) now refuses to run, and so does
`aimebu supervisor --pair` against one. Either serve the server over HTTPS and re-pair (or set `AIMEBU_URL` to the https address), or accept
the risk with `--insecure` (on the pairing command and on every later `aimebu supervisor`). The **Add machine** dialog shows a warning and the
`--insecure` command when the server is reached over plain http at such an address. The server's own local supervisor, which dials loopback, is not affected.
See [Supervisor](supervisor.md).

## Docker harnesses need a current harness-docker

Claude's and Codex's Docker variants now hand the agent's key to the harness by name (`HARNESS_DOCKER_FORWARD_ENV`) instead of putting it in
the harness's arguments, where `ps` showed it to every local user. A `harness-docker` that predates the `forward_env` feature is refused
with the way out (update harness-docker, then retry), as OpenCode's and Vibe's already were. The Settings → Harnesses row and
`aimebu doctor` say when a launcher is too old.

## What a machine runs is the owner's alone

Saving, changing or removing a launcher, a harness's models and providers, its path override, importing providers, auto-detecting which
harnesses are installed and detecting a harness's models are owner-only now; an admin the owner appointed gets 403 (the Settings controls are
disabled for them, with the reason). Listing the machines, their harnesses and a harness's launchers stays open to admins. See
[Auth](auth.md#global-admins).

## Sessions end after 30 days

A session now also has an absolute lifetime of 30 days from when it was made, on top of the sliding idle expiry, however often it is used.
Sessions that exist at the upgrade start their 30 days then (migration 94). Signing in again starts a new one. See [Auth](auth.md).

## Login throttling is per source

Wrong passwords are counted per (login, client) and per client, where a client is one IPv4 address or one IPv6 /64, so a guesser at one
address no longer locks the owner out from another. Behind a reverse proxy, set `AIMEBU_TRUSTED_PROXIES` so clients stay apart. See
[Auth](auth.md) and [HTTPS](tls.md#reverse-proxy).

## TLS: plain http redirects, and HSTS

With a certificate configured, the plain-http listener answers a request from another machine with a 308 to the same address on the HTTPS
port. Requests from this machine itself, the agents' toolbox calls (`/api/agent/...`) and the machine routes (`/api/machine...`) are served
as before. Responses served over HTTPS on port 443, or by a proxy for an `https` public URL, carry `Strict-Transport-Security` (never for
`localhost`, and not on another TLS port). See [HTTPS](tls.md#plain-http-next-to-https).

## Agent shared notes wait for the owner's approval

What an agent adds to the global shared notes (or changes in one) is stored inactive until the owner or an admin switches it on in the memory
viewer; until then it is in no agent's context and an agent cannot edit it. Agents can read and edit people's profiles only for members of
their own project, and their context carries only those members' profiles. Notes the owner or an admin writes, and imports, are active at
once. See [the contract](contract.md) (global memory).

## Smaller changes

- Starting, pausing and stopping an agent, reacting to a message as an agent, and opening an attachment now refuse a caller who may not before
  doing any work, and answer the same for something that does not exist as for something they may not see.
- A person may have at most 20 unsent attachment uploads, 100 MB together; a draft opens only for its uploader.
- Harness output is redacted for more kinds of credential before it is stored (see [Adapters](adapter.md)).
- `.aimebu/` is added to a repository's `info/exclude` when an agent's runtime state is written there.
- The app page is served with a Content-Security-Policy ([HTTPS](tls.md#content-security-policy)); while error reporting is on it names the
  report host. Mermaid diagrams render in strict mode with their links removed.
- A viewer no longer sees absolute paths on the machine's disk (a room's worktree, an agent card's blocker and grants), and the `usages/`
  folder is private (0700).
- A paired machine must be admitted for a project before its folder is read or a worktree is made there; the project's owner (or an admin)
  admits it from the spawn dialog or with `POST /api/projects/{project}/machine-folders/{machine}/admit`.
- A Copilot sign-in only links an `https` verification address.

## Agent keys are kept in memory

A supervisor no longer writes an agent's key to disk. The `supervisor/agent-tools/` folder an older supervisor wrote (one config per agent, each
holding a key), and the keys in Codex's `config.toml` and Pi's `mcp.json` under the runtime directories, are deleted when the new supervisor
starts. A supervisor that restarts is sent a **new** key for each agent it should run when it reconnects, and the old key stops working, so the
agents keep running across a restart with rotated keys. This needs the server and the supervisor both upgraded: an older supervisor keeps its
keys on disk and is sent none. Upgrade the server first. See [Supervisor](supervisor.md).

## "Only run agents in docker" (opt-in)

Settings → General → Agents has a new switch, off by default, that refuses native launchers: no agent is started or switched onto one, and an
agent already on one gets no turns until it is moved to a Docker launcher. Turn it on for an install that others can spawn agents on. See
[Supervisor](supervisor.md).
