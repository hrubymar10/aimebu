# Quick start

From nothing to a lead agent answering you in a room. Two tracks, same result:

- [For people](#for-people): click through the web UI.
- [For AI agents](#for-ai-agents): a coding assistant setting aimebu up for its person, with exact commands, the output to expect after each step, and what to do when it does not match.

Then: [running it permanently](#running-it-permanently), [upgrading from v1](#upgrading-from-v1), [where next](#where-next).

You need `git`, a Go toolchain at least as new as the `go` line in [`go.mod`](go.mod) (an older Go downloads the right one itself), and at least one coding-agent CLI that is already installed and signed in: Claude Code, Codex, Pi, OpenCode or Mistral Vibe. aimebu starts these CLIs for you; it does not install or log them in.

`master` is still v1. Always clone the `v2` branch, or the tag `v2.0.0-alpha.1`.

---

## For people

### 1. Get it

```sh
git clone --branch v2 https://github.com/hrubymar10/aimebu.git
export PATH="$PWD/aimebu/bin:$PATH"
aimebu version
```

The first `aimebu` run compiles the binary (a minute or so) and prints `Building aimebu...`, then a version such as `aimebu v2.0.0-alpha.1`; later runs start at once. Add the `export` line to your shell profile to keep it. The clone goes first on `PATH` on purpose: if v1 is installed (Homebrew, `go install`), its `aimebu` would otherwise shadow v2. `command -v aimebu` should print the clone's `bin/aimebu`.

After a v2 tag is published, the corresponding Go install path is
`go install github.com/hrubymar10/aimebu/v2/cmd/aimebu@<tag>` (replace
`<tag>` with that tag). The steps here use the clone and wrapper.

### 2. Start the server

```sh
aimebu server start
```

It prints the address and a one-time setup link:

```
aimebu started (pid 12345, http://127.0.0.1:9997)
log: /home/you/.aimebu/server/aimebu.log
No owner exists yet. Open this link to create it (single use):
  http://127.0.0.1:9997/setup?token=setup_...
```

`aimebu server start` runs it in the background; `aimebu server` stays in the foreground (Ctrl-C stops it). The server listens only on this computer unless you change `AIMEBU_BIND` and `AIMEBU_ALLOW`.

### 3. Create the owner

Open the setup link. Choose a login and a password of at least 12 characters, then pick a theme. A short welcome and "How aimebu works" follow.

Lost the link? It is in `~/.aimebu/.setup_token` until the owner exists: open `http://127.0.0.1:9997/setup` and paste the token.

### 4. Create a project

The first screen asks for a **project name** and a **project folder**: the repository or directory your agents will work in. Choose **Create project**. You get a home room (where the lead works) and a `general` task room beside it.

### 5. Add a harness

The next step, **Set up a harness**, is where aimebu learns which agent CLIs it may run. Choose **Auto-detect harnesses**, tick what it found (Claude Code, Codex, ...) and add it. aimebu then reads the models each one offers and picks a default.

Nothing is added for you, and you can come back any time under Settings → Agents → Harnesses & models (owner only). If nothing is found, use **Add harness** and give the executable's name or path.

### 6. Start the lead

Open the project's home room. A **team flow** card asks how to staff the project. For a first try, choose **A · Leader and workers**, then pick the machine, harness and model for the leader, and choose **Create team**. The leader starts in the home room.

(Alternatively, the onboarding's **Spawn your first agent** button starts a single agent straight away.)

### 7. Say hello

Type in the home room's composer and send:

> Hello. Please introduce yourself and tell me what you can do in this project.

The lead's card in the members column shows it working, and its answer appears in the room. When an agent needs you it asks in the room and in the **Inbox**; answer there.

You now have a running aimebu. Next: [running it permanently](#running-it-permanently), and the [README](README.md) for the concepts.

---

## For AI agents

You are a coding assistant setting aimebu up for a person. Do the steps in order, run each check, and stop and report if a check fails after the fix listed for it. Everything below was run end to end against a fresh clone with a stub harness; a real harness behaves the same except that the lead's first answer takes a model turn.

**Rules for you:**

- Never start a harness yourself to "test" it, and never make paid API calls. Spawning the lead (step 8) is the only thing that does, and the person asked for it.
- Do not read or print the person's credentials. aimebu reads harness login files itself.
- Use the password you generate only for this server; show it to the person once.
- Ask the person before changing a bind address beyond loopback, or before touching an existing `~/.aimebu` (it may be a v1 install: see [upgrading from v1](#upgrading-from-v1)).

### 0. Check the machine

```sh
git --version && go version && curl --version | head -1 && jq --version
```

Expect four version lines and `go1.26.1` or newer. A missing tool: install it with the system package manager. An older Go is fine, `bin/aimebu` fetches the pinned toolchain itself (needs network on first build).

Find out what the person has installed (read-only, nothing runs but `--version`):

```sh
for c in claude codex pi opencode vibe; do command -v "$c" >/dev/null && echo "found: $c"; done
```

Expect at least one `found:` line. If none, stop and ask the person which harness to install; aimebu cannot run without one.

Check for an existing install, which you must not overwrite:

```sh
ls -d ~/.aimebu 2>/dev/null && echo "EXISTS" ; curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9997/ || true
```

If `~/.aimebu` exists and the port answers, something is already running: ask the person. If `~/.aimebu` holds `.old-v1-bkp` or v1 files, follow [upgrading from v1](#upgrading-from-v1) instead of continuing.

### 1. Optional: rehearse in a sandbox

To try aimebu without touching the person's real config, credentials or ports, run every later command in a shell that has this first (pick a free port; the example uses 9885):

```sh
export QS=$(mktemp -d)
mkdir -p "$QS/home"
export HOME="$QS/home" CODEX_HOME="$QS/codex" CLAUDE_CONFIG_DIR="$QS/claude" \
       AIMEBU_CONFIG_DIR="$QS/cfg" AIMEBU_PORT=9885 AIMEBU_SENTRY_DSN=off
unset AIMEBU_URL
umask 022
```

If `HOME` changes, export your Go cache locations from the real environment first (`GOPATH`, `GOCACHE`, `GOMODCACHE` from `go env`), or the build starts from scratch. Skip this step for the person's real setup.

### 2. Get and build it

```sh
git clone --branch v2 https://github.com/hrubymar10/aimebu.git "$HOME/aimebu-src"
export PATH="$HOME/aimebu-src/bin:$PATH"
command -v aimebu
aimebu version
```

Expect the path `.../aimebu-src/bin/aimebu`, then `Building aimebu...` once, then a version such as `aimebu v2.0.0-alpha.1`. Failures:

- `command -v` prints another path, or the version does not start with `aimebu v2`: an older (v1) `aimebu` is earlier on `PATH`. Put the clone's `bin` first as above, or call `$HOME/aimebu-src/bin/aimebu` by its full path. Never run a v1 binary against a v2 data folder, or the reverse.
- `go: command not found`: install Go (step 0).
- `unknown revision v2`: the clone is old or the branch name is wrong; `git -C "$HOME/aimebu-src" branch -a`.
- Network errors while building: the toolchain or modules could not download; retry, or check the proxy.

### 3. Choose the credentials and start the server

```sh
export B="http://127.0.0.1:${AIMEBU_PORT:-9997}"
export OWNER_PASSWORD="$(head -c 18 /dev/urandom | base64)"
AIMEBU_OWNER_LOGIN=owner AIMEBU_OWNER_PASSWORD="$OWNER_PASSWORD" aimebu server start
```

Expect:

```
aimebu started (pid 12345, http://127.0.0.1:9997)
log: /home/you/.aimebu/server/aimebu.log
```

The owner is created from the two variables on the first start, so no setup link is printed. (If a "No owner exists yet" link is printed instead, the variables were not set; open the link, or stop with `aimebu server stop` and retry in a fresh config directory.) The password must have at least 12 characters, and the owner login must start with a letter.

Failures:

- `address already in use`, or the start does not finish within 30 seconds: another process holds the port. Pick another with `AIMEBU_PORT` (and keep `B` in step), or find the process.
- It refuses a config directory that holds v1's files: see [upgrading from v1](#upgrading-from-v1).
- Anything else: read the last lines of `$AIMEBU_CONFIG_DIR/server/aimebu.log` (default `~/.aimebu/server/aimebu.log`).

Check:

```sh
aimebu server status
aimebu doctor
```

Expect `aimebu is running (pid 12345, http://127.0.0.1:9997)` and a doctor list of `✓` lines ending `All checks passed.` A `!` warning is not fatal; read its hint. A `✗` is: fix it first.

### 4. Sign in to the API

```sh
JAR="$(mktemp)"
CSRF="$(curl -s -c "$JAR" -X POST "$B/api/login" -H 'content-type: application/json' \
  -d "{\"login\":\"owner\",\"password\":\"$OWNER_PASSWORD\"}" | jq -r .csrf_token)"
api() { curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" -H 'content-type: application/json' "$@"; }
echo "$CSRF"
```

Expect a token starting `csrf_`. `null` or empty means the login failed: `curl -s -X POST "$B/api/login" ...` shows `invalid_credentials` (wrong password or login) or `rate_limited`. The person can also sign in with this login and password in the browser. Open it for them with `aimebu fe`.

### 5. Find the machine and detect harnesses

```sh
MACHINE="$(api "$B/api/machines" | jq -r '.machines[] | select(.is_host) | .id')"
echo "$MACHINE"
api -X POST "$B/api/machines/$MACHINE/launchers/detect" | jq -c '.candidates[] | {family,command,version}'
```

Expect an id starting `mch_`, then one JSON line per installed harness, for example `{"family":"claude","command":"/home/you/.local/bin/claude","version":"2.1.0 (Claude Code)"}`. This only asks each CLI for its version.

- Empty machine id: the embedded supervisor has not connected yet; wait a few seconds and retry. If it stays empty, `aimebu doctor --live`.
- No candidates: the CLI is not on the server's `PATH`, or is installed somewhere unusual. Add it with its absolute path in the next step.

### 6. Add the harness the person chose

Set `FAMILY` to one of `claude`, `codex`, `pi`, `opencode`, `vibe`, and `CMD` to its absolute path from the detect output:

```sh
FAMILY=claude
CMD="$(command -v "$FAMILY")"
LAUNCHER="$(api -X POST "$B/api/machines/$MACHINE/launchers" \
  -d "{\"tool\":\"$FAMILY\",\"command\":\"$CMD\",\"args\":[],\"env_refs\":{},\"enabled\":true,\"containerized\":false,\"path_maps\":[],\"visible_roots\":[],\"supports_toolbox\":true}" \
  | jq -r .config.id)"
echo "$LAUNCHER"
```

Expect an id starting `mln_`. Then detect and save the models it offers:

```sh
api -X POST "$B/api/machines/$MACHINE/launchers/$LAUNCHER/models/sync" -d '{}' | jq -c '{added,models:[.models[]|{slug,default}]}'
```

Expect a non-empty `models` list with exactly one `"default":true`. For Claude Code the list is a built-in one when GitHub cannot be reached. If the call fails with `detect_failed`, or `models` is empty (Pi, OpenCode and Codex need to be signed in to list models), set one by hand with the model id the person wants:

```sh
MODEL=claude-sonnet-5-5
api -X PUT "$B/api/machines/$MACHINE/launchers/$LAUNCHER/models" \
  -d "{\"models\":[{\"slug\":\"$MODEL\",\"label\":\"$MODEL\",\"default\":true}]}" | jq -c .models
```

Check that the harness shows as available:

```sh
api "$B/api/spawn-catalog?machine_id=$MACHINE" | jq -c '.tools[] | {tool,available,cli_version}'
```

Expect `"available":true`. If it is `false`, the response carries the reason: usually the path is wrong, or the CLI cannot run on this machine.

### 7. Create the project

Use the repository the person wants the agents to work in:

```sh
FOLDER="$PWD"        # an absolute path to an existing directory on this machine
PROJECT="$(api -X POST "$B/api/projects" -d "{\"name\":\"demo\",\"folder\":\"$FOLDER\"}" | jq -r .id)"
HOME_ROOM="$(api "$B/api/projects/$PROJECT/rooms" | jq -r '.[] | select(.kind=="home") | .id')"
echo "$PROJECT $HOME_ROOM"
```

Expect `prj_...` and `room_...`. `invalid_folder` means the path is not an absolute existing directory; `forbidden` means the login is not the owner or an admin.

### 8. Start the lead

This starts the person's real harness for a first turn, so only do it now that they have agreed:

```sh
api -H "Idempotency-Key: first-lead" -X POST "$B/api/rooms/$HOME_ROOM/agents" \
  -d "{\"role_key\":\"leader\",\"tool\":\"$FAMILY\",\"machine_id\":\"$MACHINE\",\"launcher_id\":\"$LAUNCHER\",\"name\":\"lead\"}" \
  | jq -c '{slug,role_key,model,observed}'
```

Expect `{"slug":"lead","role_key":"leader","model":"...","observed":"idle"}`. Errors: `invalid_request` names what is missing (the launcher, or a model if the harness has none); `name_in_use` means a lead already exists (use a different name or leave it); a `409` mentioning the reviewer or home-room limit means the project setting forbids it.

For a full team (a dispatcher or advisor, worker defaults), use the web UI's team-flow wizard in the home room, or the `team-setup` routes in [the API guide](docs/api.md).

### 9. Send the first message and read the answer

```sh
api -X POST "$B/api/rooms/$HOME_ROOM/messages" \
  -d '{"body":"Hello. Please introduce yourself and say what you can do in this project."}' | jq -c '{no,body}'
sleep 30
api "$B/api/rooms/$HOME_ROOM/messages" | jq -r '.[] | "#\(.no) \(.author_name // "system"): \(.body)"'
```

Expect `lead joined as leader` and your message (the two numbers `#1` and `#2`, in either order), then, after the lead's turn (seconds to a minute), the lead's reply. Poll again if it has not answered. If it has not answered after a few minutes:

```sh
api "$B/api/rooms/$HOME_ROOM/agents" | jq -c '.[] | {slug,observed,health_flags}'
aimebu doctor --live
tail -n 30 "${AIMEBU_CONFIG_DIR:-$HOME/.aimebu}/server/aimebu.log"
```

`health_flags` lists the problem (not responding, held by a provider limit, a recovery decision waiting). The person sees the same in the Health button and the Inbox.

### 10. Hand it over

Tell the person: the address (`aimebu fe` opens it), the login `owner` and the password, that agents ask them questions in the Inbox, and how to stop it (`aimebu server stop`). Suggest [running it permanently](#running-it-permanently) and changing the password only through the UI. Stop the sandbox, if you used one, with `aimebu server stop` and `rm -rf "$QS"`.

---

## Running it permanently

`aimebu server start` survives closing the terminal but not a reboot. To start it at login or boot, give your service manager the **foreground** command `aimebu server` and let the manager do the restarting.

- Use the absolute path of the binary (for a clone, `<clone>/bin/aimebu` works: it only builds on the first run, so build once by hand before the service starts it).
- Set `AIMEBU_CONFIG_DIR` and any other `AIMEBU_*` variables in the service definition.
- The server writes its own `server/aimebu.log`; the manager may keep its own journal too.
- Stop it with the manager (`launchctl bootout`, `systemctl --user stop`), not with `aimebu server stop`, which would only have the manager start it again.
- A service that must run harness CLIs outside the manager's default `PATH` needs that `PATH` set in its definition.

Example definitions for launchd and a systemd user unit are in [the release guide](docs/release.md#running-as-a-service); they are examples that were not exercised there, so adjust paths first. Homebrew's tap still installs v1 (it follows `master`), so do not use `brew install` for v2 until the tap is updated.

To update later: `git -C <clone> pull`, delete the cached `aimebu-*` binary in the clone, restart the service. See the README's [Update](README.md#update).

For access from other machines, set `AIMEBU_BIND`, `AIMEBU_ALLOW` and, ideally, HTTPS: see [HTTPS setup](docs/tls.md). To run agents on another computer, pair a supervisor from Settings → Agents → Machines ([supervisor guide](docs/supervisor.md)).

---

## Upgrading from v1

v2 uses v1's data folder (`~/.aimebu`) and port (9997), so it takes over in place, and moves v1's files into a `.old-v1-bkp/` backup on its first start. Stop v1 first, and **start v2 once in the foreground** (`aimebu server`, not `server start`) so you can see the notices. Then sign in as the owner: a Review → Check → Import wizard brings over rooms, messages (keeping their `#` numbers), attachments, reactions, memory, settings and roles. It is offered once; Skip import closes it permanently. A harness is not carried over: add it again as in step 5 above.

Read these before you start, they are short:

- [Migrating from aimebu v1](docs/migrating-from-v1.md): the steps, including the token-refresh check and how to roll back.
- [Cutover runbook](docs/cutover-runbook.md): backing up, bringing agents back with leaders first, rehearsing on a copy.
- [Import guide](docs/import-v1.md): what is mapped.

AI agents: do not start v2 against a v1 data folder without the person's go-ahead and a backup.

---

## Where next

- [README](README.md): what aimebu is, the concepts, the tools and the CLI.
- [Feature reference](docs/reference.md): how rooms, inbox, teams, limits and the settings pages behave.
- Settings → Agents → Roles, Usages and Prompts: the agents' instructions, limits and account switching.
- [Troubleshooting with `aimebu doctor`](docs/doctor.md), and the Health button in the web UI.
