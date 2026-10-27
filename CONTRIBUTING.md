# Contributing to aimebu v2

This guide gets you from a fresh clone to a running, tested change, starting
with the web UI. It is about workflow; behaviour and APIs live in
[README.md](README.md), [AGENTS.md](AGENTS.md) (the rules for agents working
in this repo, which apply to people too) and [docs/](docs/). If a command here
stops working, fix this file in the same commit as the change that broke it.

## Prerequisites

- **Go**: the version in `go.mod` (`go 1.26.1` or newer). Dependencies are the
  standard library plus the three approved in `AGENTS.md`; ask before adding,
  removing or updating any other.
- **Docker**, for `make test-web` and so `make test-full`: the web unit tests run in a pinned `node`
  image (see "The web tests in Docker" below), so a machine needs no node of its own for them.
  `WEB_TEST_RUNNER=local` runs them with a local **Node.js** instead (a current release; the tests are
  plain `node` scripts with no `npm install` step and no `package.json`, verified on Node 24). The
  browser scripts (`make test-ui`, the audits) always use the local node.
- **Chromium and `puppeteer-core`**, for `make test-ui`, the audit scripts,
  and icon builds. Install `puppeteer-core` globally
  (`npm install -g puppeteer-core`);
  the scripts find it on the normal module path or in
  `/usr/local/lib/node_modules`. The browser scripts use Chromium or Chrome
  from `/Applications` on macOS and common `/usr/bin` paths on Linux. Set
  `CHROMIUM_PATH` to the executable when it lives elsewhere. They run headless
  with `--no-sandbox`.
- `git`, and `make`.

## Run it locally with an isolated environment

Never develop against your real config directory or credentials. The server
and some tests read `~/.codex`, `~/.claude` and the v2 config root, so give
every session its own:

```sh
T=$(mktemp -d)
mkdir -p "$T/home"
export HOME="$T/home" CODEX_HOME="$T/codex" CLAUDE_CONFIG_DIR="$T/claude" \
       AIMEBU2_CONFIG_DIR="$T/cfg" AIMEBU2_PORT=9885
make dev
```

If you change `HOME`, keep your Go caches by exporting `GOPATH`, `GOCACHE` and
`GOMODCACHE` from `go env` first, or the build starts from scratch.

`make dev` runs the server under `air` (`.air.toml`). It rebuilds `./tmp/aimebu`
when a `.go`, `.html`, `.css`, `.js`, `.mjs` or `.json` file changes and restarts the
server, so edit a file under `web/`, wait a second or two, and reload the
browser. Ctrl-C stops it. If you start it without a terminal, stop it by its
process ID, never with `pkill -f`: that pattern also matches your own shell.
The build carries the checkout's `git describe` as its version, which the page header shows
(`GET /api/buildinfo`), so you can tell which build you are looking at.

Pick a free port with `AIMEBU2_PORT` so you do not collide with another
instance. `AIMEBU2_BIND` (default `127.0.0.1`) and `AIMEBU2_ALLOW` control who
can connect; `aimebu help` lists every `AIMEBU2_*` variable.

### First run: the setup token

A fresh config directory has no owner. Unless you set both
`AIMEBU2_OWNER_LOGIN` and `AIMEBU2_OWNER_PASSWORD`, the server prints a
single-use setup token, a shortcut link and the token file's path:

```
No owner exists yet. Open the setup page and enter this setup token (...):
  setup_...
The token is also in /tmp/tmp.XXXX/cfg/.setup_token
Shortcut that fills the token in for you:
  http://127.0.0.1:9885/setup?token=setup_...
```

Open the shortcut (or open `http://127.0.0.1:9885/`, paste the token), then
create the owner with a login and a password of at least 12 characters. The
token is valid until the server restarts. To skip the page in a scratch
environment, start with `AIMEBU2_OWNER_LOGIN` and `AIMEBU2_OWNER_PASSWORD`
set. See [docs/auth.md](docs/auth.md).

For UI work without agents, the web app has mock switches such as
`/?mock_inbox=1`, `/?mock_agents=1` and `/?mock_recovery=1`; grep `mock_` in
`web/js` for the full set.

## Which test command to run

| Command | What it runs | Use it |
| --- | --- | --- |
| `make test` | `go test ./...`, with progress output | while iterating on Go code |
| `make test-web` | the browser-free web unit tests (`scripts/test-*.cjs`, run in Node's `vm` over the module source), in Docker (`WEB_TEST_RUNNER=local` for the local node) | while iterating on `web/js`; fast, no browser |
| `make test-ui` | builds the server, starts a scratch one and drives Chromium through the UI; then `scripts/test-harness-page.cjs`, the Preact Harnesses page in Chromium with held, reordered and failing responses | after any visible UI change; needs Chromium and `puppeteer-core` |
| `make test-race` | `go test -race ./...`, with progress output | after touching concurrency, the store, WebSocket or supervisor code |
| `make test-full` | `fmt-check`, `vet`, `test-race`, `test-web` | before every commit; it is also what CI runs |

`test-ui` is not part of `test-full`, so run it yourself for UI changes. Two
more scripts check the UI without a server and exit non-zero on findings:
`node scripts/phone-audit.cjs` (tap targets, overlap and reachability at phone
and tablet sizes) and `node scripts/a11y-audit.cjs` (accessible names,
contrast, focus rings).

A new web unit test is a `scripts/test-<name>.cjs` file; add it to the
`WEB_TESTS` list in the `Makefile`, or it never runs.

### Reading the test output

`make test` and `make test-race` run `go test -json` through
`scripts/gotestprogress` (the standard library and the JSON package the repo already
uses, no new dependency; `make` builds it into `tmp/`).
Plain `go test ./...` prints nothing for a package until the whole package is
done, so a stuck test looks like silence. The runner prints:

```
[12:00:01] start  github.com/hrubymar10/aimebu/internal/store
[12:03:10] ok     github.com/hrubymar10/aimebu/internal/store (3m09s)  [4/23 packages]
[12:03:10] slow   github.com/hrubymar10/aimebu/internal/server TestSomething (41s)
[12:03:40] still running after 3m39s, nothing new for 30s  [4/23 packages]
         running github.com/hrubymar10/aimebu/cmd/aimebu TestServerFoo for 2m10s
[12:08:01] HANG?  github.com/hrubymar10/aimebu/cmd/aimebu TestServerFoo has been running for 5m00s
           kill -QUIT 12345    # aimebu.test
```

- `start` / `ok` / `FAIL` per package, with its duration and an `X/Y packages` count
  (`cached` marks a result that was not re-run). A package with no test files only counts.
- A failing test prints right away with everything it logged, and a package that fails
  without a failing test of its own (a build error, a timeout, a crash) prints its output
  and names the tests that never finished. The end of the run lists every failure again,
  the three slowest packages, and the exit status, which is `go test`'s (non-zero on any failure).
- `slow` names a passing top-level test that took 30 s or more (`-slow`).
- **`still running`** appears whenever nothing has happened for 30 s and lists what is running
  and for how long. A package that is only compiling or between tests says so. This is the line
  that tells you where a hung run is stuck.
- **`HANG?`** appears once for a test that has been running for 5 minutes (a normal
  package takes about 5 to 10 minutes under `-race`, but one test should not). It prints the
  `kill -QUIT <pid>` command for the test binary: `SIGQUIT` makes a Go program print the stack
  of every goroutine and exit, the dump appears in that package's FAIL output, and the
  goroutine that is waiting (on a channel, a lock, a socket) shows what the test is waiting for.
  `kill -USR1 <pid of gotestprogress>` does the same for every package that has a test running for
  30 s or more; `-quit-on-hang` does it by itself when the 5 minutes pass.

Flags go in `PROGRESS_FLAGS`, for example `make test-race PROGRESS_FLAGS='-quiet 10s -hang 2m'`
(`-quiet`, `-slow`, `-hang`, `-quit-on-hang`). `make test PLAIN=1` runs plain `go test`.
`make test-web` prints a `[web n/N]` start and end line with the duration for each script.

Tests wait on servers, subprocesses and channels, and a wait with no deadline is how a
test hangs without a word. `cmd/aimebu/testwait_test.go` has the helpers: `receive` waits on
a channel for a limit and dumps the goroutines when it gives up, and `testHTTP` is a client
whose requests have a deadline (`http.DefaultClient` has none). Use them for a new wait, and
make the limit generous: it is for a hang, not for a slow machine.

Run tests in the isolated environment above. When several people or agents
share one machine, run the slow commands (`test-ui`, `test-full`) one at a
time; the shared workspace gate is `flock` on `~/xcode/.aimebu-v2-gate.lock`.
Redirect long output to a file and search that, rather than re-running.

Every Go package with tests needs a `TestMain` that calls `testenv.Main(m)`;
packages with custom setup call `testenv.Isolate()` before any test work.
The helper moves home and credential directories to a temporary root and
preserves the effective Go cache paths before changing `HOME`. Tests in the
adapter, supervisor, server, and command packages also call
`testenv.RestrictPATH` after isolation. It exposes only basic tools and skips
machine-wide harness install directories during discovery. Put a fake
executable on the test's
`PATH` when a case needs a harness; never run a real login or launcher.

### The web tests in Docker

`make test-web` runs the scripts in Docker by default, so every machine uses the same toolchain, and
says so on its first line. It is one container for all the scripts, from the digest-pinned image
`WEB_TEST_NODE_IMAGE` (`node:24.22.1-alpine3.24@sha256:06d7adf3…`; override the variable to change it;
the first run pulls it, the only time it needs the network). The repository is mounted read-only at
`/src` and copied (without `.git` and `tmp/`) to a tmpfs at `/work`, where the scripts
run, so nothing can be written back to the checkout. The container has no network, a read-only root
file system, no capabilities, and your user id. The scripts have no npm dependencies and only read
the repository (a script that needs a scratch file uses `os.tmpdir()`). The repository must be in a
folder Docker may share: Docker Desktop and OrbStack share paths under `$HOME`, not `/tmp`.

Before the scripts, the container runs `scripts/check-node-ca.cjs`, which fails when node's bundled store or
the image's system store holds one of the compromised CA roots (2026-10) it names by SHA-256 fingerprint:
a new pin of `WEB_TEST_NODE_IMAGE` must pass it.

`WEB_TEST_RUNNER=local` runs `scripts/test-web.sh` with the `node` on `PATH` instead. There is no
automatic fallback in either direction: with Docker missing or not running, `make test-web` (and
`make test-full`) stops with a message naming `WEB_TEST_RUNNER=local`; with `local` and no node, it
says to install node or drop the variable. `test-ui` and the audits are not affected: they need
Chromium and the local node.

## The `web/` layout

The UI is ES modules with no bundler and no build step. Most of it is
hand-written DOM code; pages are moving to Preact + htm one at a time (see
[docs/design/frontend-preact.md](docs/design/frontend-preact.md)). `web/` is
embedded into the Go binary (`embed.go`) and served as is; the server adds a
content version to every relative module import (`./x.js`, `../x.js`) and to
the import map, so each module has exactly one URL.

- `web/index.html`: the page shell and the static markup of dialogs and
  panels. Its import map names the vendored Preact runtime; then it loads
  `js/app.js` as a module.
- `web/js/`: one module per concern. `app.js` boots the page and wires the
  rest; `api.js` holds the session (CSRF token, current user, rights) and the
  `request` helper every call uses; `events.js` is the live event stream.
  Main areas: `sidebar.js` (rooms), `chat.js` and `composer*.js` (messages and
  input), `inbox*.js` (the Inbox), `agents.js` and `room-members.js` (agent
  cards and profile), `machines.js`, `settings*.js` and the `*-settings.js`
  pages (Settings), `setup.js` and `welcome.js` (first run), `preferences.js`
  (per-user preferences saved through `/api/me/preferences`).
  Shared UI pieces: `icon-button.js`, `dialogs.js`, `settings-row.js`,
  `popover-focus.js`, `dialog-focus.js`, `notice.js`, `live-region.js`.
  Logic that can run without a DOM (`*-format.js`, `settings-backup-format.js`,
  `preferences.js` and similar) is loaded straight into Node's `vm` by the
  `scripts/test-*.cjs` tests, so keep browser APIs out of those modules.
- Preact code: `web/js/ui.js` is the only module that imports Preact, htm and signals
  (`scripts/test-preact-vendor.cjs` checks it); components import `html`,
  `render`, the hooks and the signals from it. A page's shared state is
  signals the page owns (see the design doc's State section). `web/js/components/` holds shared pieces
  (`IconButton`, `SettingsRow`, `SettingsGroup`: the same markup as
  `icon-button.js` and `settings-row.js`), `web/js/hooks/` shared hooks, and
  `web/js/pages/<page>/` one page each: its components, a `model.js` with the
  page's pure logic (import-free, tested in `vm`), and a `page.js` adapter that
  gives the settings shell its tab and section.
  Markup is written with `html` tagged templates:
  ``html`<button class="btn" onClick=${save}>Save</button>` ``. A container
  belongs to Preact or to hand-written code, never both. A Preact page keeps
  the ids, classes and text the page it replaced had, so `test-ui` and the
  stylesheet keep working.
- `web/style.css`: the original base stylesheet; it is small and mostly legacy.
  `web/theme-v1.css`: the stylesheet that matters, which carries the colour
  variables (themes switch through `data-theme` on `<html>`) and nearly all of
  the layout. Put new styles in `theme-v1.css`, using its variables rather than
  fixed colours, so every theme keeps working.
- `web/icons/`: SVG icons plus the app and favicon images. An icon is drawn
  through a CSS mask in the current text colour: add the file, add an
  `.icon-mask-<name>` rule next to the others in `theme-v1.css`, and use it
  with `iconButton('<name>', 'Label')`. Raster icons are built by
  `node scripts/build-icons.cjs` from the masters in `assets/`.
- `web/vendor/`: third-party files (Mermaid, the emoji database, syntax
  highlighting, the editor, Preact, htm and signals). Never edit them by hand. `web/vendor/README.md`
  records each file's source, version, checksum and licence; update it in the
  same commit as any change here, including when a new icon comes from a
  catalogue. `scripts/test-svg-headers.cjs` fails if an SVG still carries a
  catalogue's download header.
- `web/sounds/` and `web/manifest.webmanifest`: built-in notification sounds
  and the installable-app manifest.

## UI conventions

- **Icon buttons, never text buttons.** An action is an SVG icon with a
  tooltip (`title`) and an accessible name (`aria-label`). Use `iconButton()`
  or `setGlyph()` from `icon-button.js`, which set all three. Reuse an existing
  glyph from `web/icons/` before asking for a new one. Text appears only on
  dialog actions (Cancel, OK) and in places that already use labelled buttons.
- **In-app dialogs, never the browser's.** Do not call `confirm()`, `prompt()`
  or `alert()`. Use `confirmDialog`, `promptDialog`, `formDialog` and
  `noticeDialog` from `dialogs.js`; they return promises, queue one at a time,
  and handle focus and Escape. Destructive confirmations pass `danger: true`.
- **One spacing system in Settings.** Build settings pages from the shared
  row: `settingsRow({ label, desc, control })` in `settings-row.js` produces
  `.settings-group > .settings-row`, and one block of CSS styles it (see the
  `#settings-dialog` rules in `theme-v1.css`). Do not add one-off margins or
  paddings to a single page; change the shared rule if the spacing is wrong.
- **Phone layouts.** The phone breakpoint is `max-width: 700px`, and phone
  navigation is the bottom tab bar from `mobile-tabs.js`. Tap targets must be
  at least 40 px; run `node scripts/phone-audit.cjs` after any layout change.
  Controls that appear on hover must also appear on focus and always on touch
  screens.
- **Accessibility.** Every control has an accessible name, every focusable
  element a visible focus ring, and text keeps its contrast in every theme;
  `node scripts/a11y-audit.cjs` checks all three, and also that every button
uses one of the shared classes (`.btn`, `.icon-btn`, `.row-btn`).
- **Plain words for users.** Say "Not responding", not an internal state name,
  and never show raw JSON.
- **Experiments.** A/B questions run behind the top-bar pill in
  `ui-variant.js`; its header comment explains how to define one. None runs
  now, and a page moved to Preact is not one: it replaces its legacy module in
  the same change.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/):
`type(scope): short imperative summary` (types `feat`, `fix`, `refactor`,
`perf`, `test`, `docs`, `build`, `ci`, `chore`; keep the subject near 72
characters, no trailing period), then a blank line, then a body wrapped at about
72 characters that explains why: the failure, the choice you made, the
trade-off. A subject-only message is not enough for a non-trivial change.
Write the message through a heredoc so the wrapping survives.

Update the documentation in the same commit as the change that makes it stale:
README, `docs/`, this file, `web/vendor/README.md`. Never commit a red tree:
run `make test-full` first (and `make test-ui` for UI changes). Commit as
yourself with plain `git commit`; do not override the author.

## The pre-push hook

The shared pre-push guard runs on `git push`. It is installed into the clone's
`.git/hooks`, so it is not part of the repository; a clone without it simply
skips these checks. It checks, for every commit that is not on a remote yet:

1. **Identity**: author and committer both match the repository's configured
   `user.name` and `user.email`.
2. **Signature**: the commit carries a good GPG signature (status `G` or `U`).
3. **Forbidden words**: the commit message and every added line or path are
   matched against a list of private names kept in the hook; a match asks you
   to confirm at the terminal, so keep such names out of commits and docs.

The hook does not run tests. `make test-full` before committing is on you, and
CI runs it again on every push and pull request. A repository can add its own
extra checks in an executable `.git/hooks/pre-push.local`.
