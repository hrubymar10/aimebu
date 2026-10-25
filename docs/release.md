# Releasing aimebu 2.0.0 (draft plan)

Status: draft of the 2.0.0 release plan. Everything below was checked against the tree at
`207a2667` unless marked **open**. Nothing here adds a dependency or changes code.

## 1. Versioning

Where the version lives today:

- `cmd/aimebu/main.go`: `var version = "aimebu v2 (development)"`, a plain
  string overridden at build time with `-ldflags "-X main.version=..."`.
- `aimebu version` prints `resolveVersion()`. With an override it prints the
  string unchanged. Without one it appends the short VCS revision, commit time
  and `dirty` from `runtime/debug` build info (a `go build` inside a checkout
  prints `aimebu v2 (development) 7fce1dbcb00c 2026-10-20T15:34:02Z`).
- Nothing else reads it. The server API and the web UI do not show the binary
  version. The embedded web assets are cache-busted by a content hash
  (`internal/server/static_assets.go`), not by the version.
- There is no `VERSION` file, changelog, or Go-source constant. Git has one tag,
  `v0.0.0`.
- The Go module path is `github.com/hrubymar10/aimebu`, the same as v1, so a
  v2 tag is not importable as a Go module (Go requires a `/v2` suffix for a
  major version 2 or higher tag on a module with a `go.mod`). That is fine
  because the repo ships a binary, not a library; do not add `/v2` to the module
  path.

Proposal:

1. The release version is the git tag `v2.0.0` (semver). The tag is the single
   source of truth.
2. The build passes `-X 'main.version=aimebu v2.0.0'`, derived from the tag, so
   `aimebu version` prints `aimebu v2.0.0`. Verified: that exact flag works
   (tested with a throwaway build).
3. Dev builds keep the default string plus VCS suffix, so a dirty or untagged
   binary is never mistaken for a release.
4. **Open:** should `aimebu version` also carry the commit for a release
   build (for example `aimebu v2.0.0 (7fce1db)`)? Today an override suppresses
   the VCS suffix.
5. Pre-releases use the same mechanism: `v2.0.0-rc.1`.

## 2. Builds

The primary install builds from source on the user's machine (section 3), so
nothing below is on the critical path. Prebuilt binaries are an optional extra.
The cross-compile checks matter because they prove the tree builds CGo-free on
every platform the wrapper may run on.

Targets: macOS arm64 and amd64, Linux amd64 and arm64.

CGo-free: **yes, verified.** The only cgo-capable dependency would be SQLite,
and v2 uses `modernc.org/sqlite` (pure Go). No file in the tree has
`import "C"`. With `CGO_ENABLED=0` all four targets cross-compile from one
Linux host:

```sh
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X 'main.version=aimebu v2.0.0'" \
    -o dist/aimebu-${t%/*}-${t#*/} ./cmd/aimebu
done
```

Result of that loop: four binaries of 22 to 24 MB each, the darwin one a valid
Mach-O arm64 executable, and the linux-arm64 one ran `aimebu version` and printed
`aimebu v2.0.0`. The darwin and linux-amd64 binaries were built but not run (no
such host here); run them on real machines before publishing.

Notes:

- The web UI is embedded with `//go:embed web` (`embed.go`) and `web/` is plain
  files with no build step, so the Go build is the whole build. (`scripts/build-icons.cjs`
  is a manual asset step, not part of the build.)
- `go.mod` says `go 1.26.1`; CI uses `go-version-file: go.mod`, so releases build
  with the same toolchain as CI.
- `github.com/air-verse/air` is a `tool` directive for `make dev` only. It is
  not linked into the binary.
- A single `aimebu` binary is the whole product: server, `supervisor`,
  `agent-tools`, `switcher`, `doctor`, `prune`, `sessions`, `usages`, `fe`. There is no
  separate supervisor artifact to ship.
- Runtime prerequisites are not bundled and must be in the release notes: the
  harness CLIs (Claude Code, Codex, Pi, OpenCode) the owner wants, and for token
  auto-refresh `harness-docker-ctrl` plus a reachable `docker` daemon (see
  `docs/migrating-from-v1.md`, section 0).
- Signing: macOS binaries stay **unsigned**, as in v1; signing and
  notarization are not a goal. A prebuilt binary downloaded through a browser
  gets a quarantine flag, so any release that ships binaries documents the
  workaround: `xattr -d com.apple.quarantine ./aimebu`. Downloads through
  `gh release download` or `curl` are not quarantined, and a binary built by
  `bin/aimebu` never is.
- Add a `SHA256SUMS` file to any release that ships binaries.

Build tooling options (none is added by this plan):

- A shell script plus the `gh` CLI. Matches the repo's no-new-tools stance; the
  loop above is nearly all of it.
- GoReleaser (a standalone release tool, not a Go dependency of the module). I
  believe it is MIT-licensed and actively maintained, but I did not verify
  either in this plan; check the license and last release date before adopting.
  It would replace the script with a config file and also produce checksums and
  release notes.

Recommendation: none needed for 2.0.0 if prebuilt binaries are not published.
If they are, a script in `scripts/release.sh` (not written yet).

## 3. Distribution

Decided: development stays in the **private** `hrubymar10/aimebu_v2`. At
stable, `v2` is squashed or rebased into the public v1 origin
(`hrubymar10/aimebu`), which then becomes v2. Distribution is from that public
repo.

**Primary path, as in v1: clone the repo and run `bin/aimebu`**, a bash wrapper
that builds the binary on the user's machine on first use and re-uses the
cached build afterwards. There is nothing prebuilt to sign, and the macOS
quarantine problem does not arise for a locally built binary.

**v2 has `bin/aimebu`** and a README Install section. The wrapper (tested by
`cmd/aimebu/binwrapper_test.go`):

- Builds `./cmd/aimebu` into `<repo>/aimebu-<os>-<arch>` on first run, then
  `exec`s it (`/aimebu-*` is in `.gitignore`).
- `AIMEBU_FORCE_BUILD=1` rebuilds into a per-run temp binary that is removed on
  exit, and forwards INT/TERM; any other value leaves the cache alone. The
  name does not clash with the dev `AIMEBU2_*` variables and fits the final
  `AIMEBU_*` names (decision B1), so it can stay.
- It passes `-X 'main.version=aimebu $(git describe --tags --always --dirty)'`
  when git metadata exists, since v2's `version` is a full string
  (`aimebu v2 (development)`) that `resolveVersion` compares with its default;
  without git metadata it keeps the default so the VCS suffix still appears.
- It needs a Go toolchain at least as new as `go.mod` (`go 1.26.1`); with the
  default `GOTOOLCHAIN=auto` an older Go downloads the right one. The README
  states this.
- It resolves symlinks to its own location, so `bin/aimebu` can be symlinked
  onto PATH or its `bin/` directory added to PATH.

README Install section: Manual (clone, add `bin/` to PATH, `aimebu
version`) first, with the Go toolchain requirement.

### Running as a service

A service manager wants the process in the foreground and does its own restarts, so it runs `aimebu server`, not `server start`
(which detaches). The server writes `server/aimebu.log` itself, so the manager's output can go to its own journal as well. Both
definitions below are examples, not verified here: set the path to the binary and the environment (`AIMEBU2_CONFIG_DIR`, the
owner variables for a first start, bind and allowlist) to the installation's own, and move a root that still holds v1's files
with a foreground `aimebu server` first.

launchd (`~/Library/LaunchAgents/dev.aimebu.server.plist`, loaded with `launchctl bootstrap gui/$(id -u) <file>`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.aimebu.server</string>
  <key>ProgramArguments</key><array><string>/usr/local/bin/aimebu</string><string>server</string></array>
  <key>EnvironmentVariables</key><dict><key>AIMEBU2_CONFIG_DIR</key><string>/Users/me/.aimebu2</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ExitTimeOut</key><integer>60</integer>
</dict></plist>
```

systemd (`~/.config/systemd/user/aimebu.service`, then `systemctl --user enable --now aimebu`):

```ini
[Unit]
Description=aimebu v2 server

[Service]
ExecStart=/usr/local/bin/aimebu server
Environment=AIMEBU2_CONFIG_DIR=%h/.aimebu2
Restart=on-failure
TimeoutStopSec=60

[Install]
WantedBy=default.target
```

Stop such a service with its manager (`launchctl bootout`, `systemctl --user stop`), not with `aimebu server stop`, which would only
have the manager start it again. Both give a clean shutdown a minute (the server stops its agents and closes the database before it exits). A Homebrew formula's
`service` block takes the same `aimebu server` command.

Secondary options, none required for 2.0.0 and none verified here:

- **Homebrew:** v1 shipped a HEAD-only formula in `hrubymar10/tap` (`brew install
  --HEAD aimebu`). It lives outside this repo and builds from `master`, so it
  picks up v2 at the cutover. Check that the formula's build command and service
  definition still fit (`aimebu server`) and update it for the new defaults.
- **`go install github.com/hrubymar10/aimebu/cmd/aimebu@latest`:** documented
  in v1. Be careful: the repo has a `v0.0.0` tag, and a `v2.x` tag on a module
  path without `/v2` is not a valid module version. `@latest` therefore likely
  resolves to the old `v0.0.0` commit, not v2. Check it before keeping the line
  in the README, or document `@master` instead. Binaries from `go install` print
  the VCS-stamped development version, not `v2.0.0`.
- **Prebuilt binaries on GitHub Releases** (section 2): at most an option. They
  would need the `xattr -d com.apple.quarantine ./aimebu` workaround on macOS
  because binaries stay unsigned and signing/notarization is not a goal. If
  published, add `SHA256SUMS`. The release can also just be a tag plus notes.
- `aimebu update --check` is parked by the owner and not a release
  blocker; v2 has no self-update.

### Homebrew formula draft

This is the proposed HEAD-only `Formula/aimebu.rb` for the existing tap after
the public repo cutover. It is text for review, not a change to the tap. The
formula builds the v2 command from `./cmd/aimebu` and runs the foreground
`aimebu server` process under `brew services`. v2 also has `aimebu server
start`, which detaches, but a service manager needs the running server process
to supervise, so the formula uses the foreground command (see [Running as a
service](#running-as-a-service)).

```ruby
class Aimebu < Formula
  desc "Rooms and agents for project work"
  homepage "https://github.com/hrubymar10/aimebu"
  license "MIT"
  head "https://github.com/hrubymar10/aimebu.git", branch: "master"

  depends_on "go" => :build

  def install
    system "go", "build", "-trimpath", "-o", bin/"aimebu", "./cmd/aimebu"
  end

  service do
    run [opt_bin/"aimebu", "server"]
    keep_alive true
    environment_variables AIMEBU2_BIND: "127.0.0.1",
                          AIMEBU2_PORT: "9897"
  end

  test do
    assert_match "aimebu", shell_output("#{bin}/aimebu version")
  end
end
```

The `AIMEBU2_*` names and port 9897 match the current development build. It
uses the default `~/.aimebu2` data root and browser owner setup; no owner
password belongs in a formula. Before publishing the tap, apply B1's final
environment-variable names, data root and port to this draft. Keep the
service command as `aimebu server` after that rename. A service that must
reach harness CLIs outside Homebrew's service `PATH` needs that path configured
on the host.

After the tap has been updated, the v1 install flow remains `brew tap
hrubymar10/tap`, `brew trust hrubymar10/tap`, then `brew install --HEAD
aimebu`. `brew services start aimebu` registers and starts the service at
login; `brew services run aimebu` starts it without registration. The formula
and those commands need a macOS and Linux smoke check before the tap is
published. See Homebrew's [formula service
syntax](https://docs.brew.sh/Formula-Cookbook.html#service-files) and
[`brew services` behavior](https://docs.brew.sh/Manpage#services-subcommand).

**Before the public push:** keep v1 reachable. The v1 history on `master` is
replaced by v2, so tag or branch its last commit first (for example tag
`v1-final` and keep a `v1` branch), and decide whether `master` is force-pushed
or v2 lands as a merge. That is an owner decision; the plan does not assume
either. Check the squashed history and the repo contents are fine to publish.

**Rehearsal:** cut `v2.0.0-rc.1` on the private `aimebu_v2` first and install
from a fresh clone with `bin/aimebu` on both macOS and Linux.

## 4. Upgrade path from v1

The two guides already cover the procedure; the release notes link to them and
do not repeat it:

- [`docs/migrating-from-v1.md`](migrating-from-v1.md): the user steps (verify
  token refresh is idle, stop v1, start v2 on the old data folder, import,
  rollback by restoring the backup).
- [`docs/import-v1.md`](import-v1.md): what is imported and how the backup works.

Points the release notes must state plainly:

1. The v1 data folder is moved into `.old-v1-bkp/` on first v2 start, so back up
   first and do not start v2 until v1 is stopped.
2. Message numbers are preserved (v2 reserves v1's highest number).
3. The credential model is the same token-only design as v1's latest release,
   so a v1 install with **Auto-refresh tokens** on needs the idle-journal check
   from section 0 before cutover.
4. Port and folder defaults: at stable v2 uses `~/.aimebu`, `AIMEBU_*` and port
   9997 (blocker B1 below), so it takes over v1's folder and port in place. Today
   the development build defaults to `~/.aimebu2` and port 9897.

## 5. Release notes

Structure for `RELEASE_NOTES.md` or the GitHub release body:

1. One-paragraph summary: what v2 is (rooms for humans and agents, projects,
   machines and supervisors, accounts, harness-managed agents).
2. What changed from v1: the user-visible differences, taken from `README.md`
   and `docs/migrating-from-v1.md`.
3. Upgrade: link to the migration guide; list the pre-flight checks.
4. Install: clone and `bin/aimebu` (Go toolchain required), plus the optional
   routes above. Requirements: supported OS and architectures, harness CLIs,
   docker for token refresh. If binaries are published, the quarantine
   workaround.
5. Known issues: whatever is still open at tag time from section 6.
6. Checksums.

There is no changelog in the repo. The `v2` branch carries v1's history plus
per-subsystem `feat(v2)` commits on top of the v1 removal, so commit messages
are too coarse to generate notes from. Write them by hand from the README and
docs.

## 6. What still blocks 2.0.0

Release blockers that can be checked in the tree:

- **The cutover runbook** ([cutover runbook](cutover-runbook.md)) exists but
  must be rehearsed on a copy of real v1 data (checklist step 3).
- **CI.** `.github/workflows/test.yml` exists; confirm it is enabled and green
  on the private `aimebu_v2` repo before tagging.
- **This plan:** the release script (not written yet), the notes (section 5)
  and the tag.
- **Findings from the first-run test** on real data: fixed or consciously
  deferred before the tag.

Blockers that are decided but not done (all in the plan only; no code changed
by this document):

- **B1 Switch the defaults before stable.** Decided: before stable, v2 moves from
  the development defaults to `~/.aimebu`, `AIMEBU_*` environment variables and
  port 9997. Today the code still has `~/.aimebu2`, `AIMEBU2_*` and 9897
  (`cmd/aimebu/sessions.go:60-67`, `env_help.go`, `tls.go`, the server and
  supervisor), and README, `docs/import-v1.md`, `docs/api.md`, `docs/auth.md`,
  `docs/doctor.md`, `docs/prune.md` and `docs/contract.md` all name the dev paths.
  The change must also settle the cutover checks that assume two roots
  (`aimebu doctor` compares `~/.aimebu` and `~/.aimebu2`; the migration guide
  says "same-root cutover" with `AIMEBU2_CONFIG_DIR`), and it breaks anyone
  already running the dev build, who needs a one-time move of their folder.
  This is a ticket of its own; it blocks 2.0.0.
- **B2 Public repo cutover** (section 3): tag the last v1 commit, squash or
  rebase `v2` into `hrubymar10/aimebu`, and check the published history.
- **B3 The README says "development"** in several places and the version string
  is `aimebu v2 (development)`; both must read as a release at tag time.
- **B4 `bin/aimebu` and the README Install section** (section 3): done; the
  wrapper, the README Install section and the wrapper smoke tests exist.

**macOS signing.** Decided: not a goal. The primary install builds locally, so
there is nothing to sign; the quarantine workaround is documented only for
optional prebuilt binaries.

Not blockers (can ship after 2.0.0):

- README wording that still says "development" is covered by B3; other docs
  polish can follow the tag.
- Larger features and rewrites, and automatic worktree cleanup.

## 7. Release checklist

1. Blockers in section 6 closed or consciously deferred, including B1 (defaults
   switched to `~/.aimebu`, `AIMEBU_*`, 9997) and B2 (public repo plan).
2. On `v2`, clean tree: `make test-full` green (`fmt-check`, `vet`, `test-race`,
   `test-web`); `make test-ui` green.
3. The cutover runbook rehearsed on a copy of real v1 data.
4. Docs read as release docs: no "development build", "until cutover" or
   `AIMEBU2_*` wording left, and `docs/migrating-from-v1.md` matches the final
   defaults. The README Install section is the clone + `bin/aimebu` path.
5. From a fresh clone on macOS and on Linux: `bin/aimebu version` builds and
   prints `aimebu v2.0.0` at the tag, a second run reuses the cache,
   `AIMEBU_FORCE_BUILD=1` rebuilds, and `aimebu doctor` runs, with a throwaway
   config root (set the config-dir variable of the final defaults, `HOME`,
   `CODEX_HOME` and `CLAUDE_CONFIG_DIR` to temp dirs so no real credentials are
   touched). Run the section 2 cross-compile loop once to prove the CGo-free
   build on every target.
6. Rehearse on the private repo: tag `v2.0.0-rc.1` on `aimebu_v2` and install
   from a fresh clone of it.
7. Preserve v1 (tag its last commit, keep a `v1` branch), move `v2` into
   `hrubymar10/aimebu`, and tag `v2.0.0` on the release commit (signed per the
   repo's commit policy); push the tag. Create the GitHub release with the
   notes (binaries and `SHA256SUMS` only if the optional route is taken).
8. Install from a fresh clone of the public repo at the tag and confirm
   `aimebu version`. Re-check the Homebrew formula and the `go install` line.
9. Update the Homebrew tap if it is kept.
10. Point v1 users at the migration guide; v1 stays reachable at its tag and `v1` branch.
