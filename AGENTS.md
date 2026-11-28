# aimebu v2 contributor instructions

## Documentation

Before changing code, read this file, README.md, and the relevant files in
docs/. Update documentation in the same change whenever behavior, commands,
or the public contract change. `docs/contract.md` is the implementation
contract; changing it requires the room leader's approval.

## Dependencies

Use the Go standard library by default. The only preapproved external
dependencies are `github.com/goccy/go-json`, `github.com/coder/websocket`,
`modernc.org/sqlite` and `github.com/pelletier/go-toml/v2` (MIT; it reads the person's
Codex `config.toml`), and add each only when implementation needs it.
`github.com/air-verse/air` is approved as a `tool` directive for `make dev`
only (GPL-3.0; it is never imported or linked into aimebu).
`github.com/getsentry/sentry-go` and `.../sentry-go/otel` (MIT) and `go.opentelemetry.io/otel`, `.../otel/sdk` and `.../otel/trace` (Apache-2.0) are
approved for `internal/telemetry` only (owner approval 2026-11-06), with the vendored Sentry browser bundle in `web/vendor/sentry/`.
`github.com/evanw/esbuild` (MIT, v0.28.2) is approved for `internal/server/web_bundle.go`: the server bundles the web UI with it, as a library, when it starts
(owner approval 2026-11-07).
Ask before adding, removing, or updating any other dependency.

For JSON, always import `github.com/goccy/go-json` (package `json`), never
`encoding/json`. Keep it in the third-party import group.

## Tests and commits

Never commit a red tree, but test in proportion while iterating: run
`go test -race` on the changed packages, `make test-web`, and the
`scripts/test-*-page.cjs` for the touched area; run `scripts/test-ui.cjs`
only when a change touches the chat or app-wide flows it covers. The full
gate — `make gate`, which is `make test-full` plus `make test-ui` twice as two
lanes side by side — runs once, on the squashed tip, before it is pushed.
Whoever adds a package's first `_test.go` also runs `go test ./internal/testenv`.
Commit messages must use a conventional subject, a blank line, and a wrapped
body explaining the reason and tradeoffs. Do not add AI attribution or
override the configured Git author.

For concurrent agent work, use only your assigned worktree. Run test commands
under the shared test lock for their full duration: `scripts/test-lock.sh <command>`, which queues first come first served and
prints who holds the lock and your position while it waits (plain `flock /tmp/aimebu-test.lock <command>` still works, on the same lock, but it
is not fair and shows nothing). Use an
isolated temporary `HOME`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, and
`AIMEBU_CONFIG_DIR` so tests cannot alter real credentials or bus state;
preserve `GOPATH`, `GOCACHE`, and `GOMODCACHE` from the host environment.
Write substantive commit messages through a shell heredoc. Before committing,
check added lines and the message against the workspace prohibited-substring
list. Report the exact test command and result, branch, commit SHA, and parent
SHA in the handoff.

## Live-reload dev server

`make dev` runs `go tool air`. It rebuilds `./tmp/aimebu` on Go or web changes and
restarts `scripts/dev-run.sh`, which runs `aimebu server`. The server starts
or adopts its local supervisor, a detached process of its own that keeps
running across the restarts air makes. Environment variables
(`AIMEBU_BIND`, `AIMEBU_OWNER_LOGIN`, …) pass through from the shell.
`make dev` exports `AIMEBU_SENTRY_DSN=off`: a sandbox run can never report to
Sentry unless `AIMEBU_DEV_SENTRY_DSN` sets a DSN explicitly (then the
`development` environment tag applies). Scripts that start a scratch server set
it too; `scripts/test-sentry-off.cjs` fails when one does not.
Point it at a deploy worktree that only moves when a reviewed commit is
checked out, never at a worktree with work in progress.
