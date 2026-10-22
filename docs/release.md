# Releasing aimebu 2.0.0 (draft plan)

Status: draft of the 2.0.0 release plan. Everything below was checked against the tree at
`7fce1db` unless marked **open**. Nothing here adds a dependency or changes code.

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
  `agent-tools`, `switcher`, `doctor`, `prune`, `sessions`, `usages`. There is no
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
If they are, the script in `scripts/release.sh`.

## 3. Distribution

Decided: development stays in the **private** `hrubymar10/aimebu_v2`. At
stable, `v2` is squashed or rebased into the public v1 origin
(`hrubymar10/aimebu`), which then becomes v2. Distribution is from that public
repo.

**Primary path, as in v1: clone the repo and run `bin/aimebu`**, a bash wrapper
that builds the binary on the user's machine on first use and re-uses the
cached build afterwards. There is nothing prebuilt to sign, and the macOS
quarantine problem does not arise for a locally built binary.

**v2 does not have `bin/aimebu` yet** (`git ls-tree v2` has no `bin/`; v1 `master`
has it, and v2's `.gitignore` already ignores the `/aimebu-*` cache name it
writes). The v2 README has no Install section either. Both are blockers (B4,
section 6). The v1 wrapper carries over almost unchanged:

- Builds `./cmd/aimebu` (same path in v2) into `<repo>/aimebu-<os>-<arch>` on
  first run, then `exec`s it.
- `AIMEBU_FORCE_BUILD=1` rebuilds into a per-run temp binary that is removed on
  exit, and forwards INT/TERM; any other value leaves the cache alone. The
  name does not clash with the dev `AIMEBU2_*` variables and fits the final
  `AIMEBU_*` names (decision B1), so it can stay.
- It must change in one place: v1 passes `-X main.version=$(git describe ...)`,
  but v2's `version` is a full string (`aimebu v2 (development)`) that
  `resolveVersion` compares with its default, so the wrapper must pass
  `-X "main.version=aimebu $(git describe --tags --always --dirty)"`. Otherwise
  `aimebu version` prints a bare tag or hash.
- It needs a Go toolchain at least as new as `go.mod` (`go 1.26.1`); with the
  default `GOTOOLCHAIN=auto` an older Go downloads the right one. State this in
  the README.
- v1 resolves its location with `dirname "$0"`, which breaks when `bin/aimebu`
  is symlinked onto PATH. v1 documents adding the `bin/` directory to PATH
  instead; keep that, or resolve the link.

README Install section for 2.0.0: Manual (clone, add `bin/` to PATH, `aimebu
version`) first, with the Go toolchain requirement.

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
`aimebu server` process under `brew services`. The v1 `server start` daemon
command is not a v2 command, and the service manager needs the running server
process to supervise.

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

There is no changelog in the repo. The `v2` branch carries v1's history plus 15
per-subsystem `feat(v2)` commits on top of the v1 removal, so commit messages
are too coarse to generate notes from. Write them by hand from the README and
docs.

## 6. What still blocks 2.0.0

Checked against the open project-board items labelled `v2` on 2026-10-20 (all in
Backlog unless noted).

Release blockers (proposal):

| Ticket | Why it blocks |
| --- | --- |
| v1 to v2 cutover plan for real data (high) | The real switch has not happened; the runbook is not written, approved or rehearsed. |
| Findings from the owner's first-run test (high) | The ticket's own done-when is "all findings fixed or consciously deferred". |
| Release 2.0.0 | This plan, plus the script, notes and tag. |
| Contributor onboarding for v2 (high) | A docs deliverable the ticket calls for before more people join; confirm whether it gates 2.0.0 or only the repo going wider. |
| v2 dev setup on the new repo (high) | CI on the private repo and the pre-push hook. CI exists (`.github/workflows/test.yml`, `make test-full`); confirm it is actually enabled and green on `aimebu_v2`. |
| Flaky tests under load (medium) | The release gate is `make test-full`; three tests fail intermittently under load. |
| Switching away from an empty or logged-out active login (medium) | A user-facing credential dead end observed in v1; `docs/switcher.md` already describes switching to an empty profile, so verify whether v2 is already fixed and close the ticket. |

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
- **B4 `bin/aimebu` and the README Install section** (section 3): v2 has neither.
  Port the v1 wrapper with the version-flag change, add an Install section, and
  add a test (a throwaway-clone smoke: first run builds, second run reuses the
  cache, `AIMEBU_FORCE_BUILD=1` leaves no temp file).

**macOS signing.** Decided: not a goal. The primary install builds locally, so
there is nothing to sign; the quarantine workaround is documented only for
optional prebuilt binaries.

Not blockers (label `v2`, can ship after 2.0.0, or already labelled `v2.1`):

- Where v1's `general` room goes in import (low, but it touches the
  import wizard users hit on day one; the owner may promote it).
- README inbox sound wording, the Firefox check of the settings pages and the
  trash icon (all low polish; the README wording is a one-line docs fix worth
  doing before the tag).
- Frontend rewrite to Preact, autopilots, per-room notification settings,
  native mobile app, `aimebu update --check`, slash commands, usage-budget
  alerts, SVG attachments.
- Automatic worktree cleanup (explicitly "after 2.0.0", label `v2.1`) and the
  error-tracking decision (`v2.1`).

## 7. Release checklist

1. Blockers in section 6 closed or consciously deferred, including B1 (defaults
   switched to `~/.aimebu`, `AIMEBU_*`, 9997), B2 (public repo plan) and B4
   (`bin/aimebu` plus README Install).
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
