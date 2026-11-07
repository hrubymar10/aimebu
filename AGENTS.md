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
Ask before adding, removing, or updating any other dependency.

For JSON, always import `github.com/goccy/go-json` (package `json`), never
`encoding/json`. Keep it in the third-party import group.

## Tests and commits

Never commit a red tree, but test in proportion while iterating: run
`go test -race` on the changed packages, `make test-web`, and the
`scripts/test-*-page.cjs` for the touched area; run `scripts/test-ui.cjs`
only when a change touches the chat or app-wide flows it covers. The full
gate — `make test-full` plus `make test-ui` twice — runs once, on the
squashed tip, before it is pushed. Commit messages must use a conventional
subject, a blank line, and a wrapped body explaining the reason and
tradeoffs. Do not add AI attribution or override the configured Git author.

For concurrent agent work, use only your assigned worktree. Run test commands
under the shared workspace `flock` gate for their full duration. Use an
isolated temporary `HOME`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, and
`AIMEBU2_CONFIG_DIR` so tests cannot alter real credentials or bus state;
preserve `GOPATH`, `GOCACHE`, and `GOMODCACHE` from the host environment.
Write substantive commit messages through a shell heredoc. Before committing,
check added lines and the message against the workspace prohibited-substring
list. Report the exact test command and result, branch, commit SHA, and parent
SHA in the handoff.

## Live-reload dev server

`make dev` runs `go tool air`. It rebuilds `./tmp/aimebu` on Go or web changes and
restarts `scripts/dev-run.sh`, which runs `aimebu server` with its embedded
local supervisor. Environment variables
(`AIMEBU2_BIND`, `AIMEBU2_OWNER_LOGIN`, …) pass through from the shell.
Point it at a deploy worktree that only moves when a reviewed commit is
checked out, never at a worktree with work in progress.
