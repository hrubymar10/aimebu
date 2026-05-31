# Fleet Command

`aimebu fleet` launches a named bundle of shell commands. It is for the
common project-start workflow where you always open the same set of terminal
agent sessions from the same working tree.

## CLI

```bash
aimebu fleet                 # list configured fleets and agent counts
aimebu fleet default         # launch default with the current directory
aimebu fleet default ~/src/aimebu
```

## Platform

Fleet launching is macOS-only in v1. It uses `osascript` and Terminal.app to
open each configured command in a new Terminal window. Running
`aimebu fleet <name>` on Linux or Windows exits with a clear error.

The optional path is expanded to a canonical absolute path before
substitution. When omitted, aimebu uses the current working directory.
Canonicalization resolves symlinks in the deepest existing path prefix and
then preserves any not-yet-created tail components.

Fleet names must be 1-64 characters, start with a lowercase letter or digit,
and then use only lowercase letters, digits, dots, underscores, or dashes.

Each fleet stores one or more agent command strings. In v1, each command must
keep **Open in Terminal window** enabled. **Auto-set cwd** defaults on, but it
can be disabled when the command already handles its own working directory.

Supported placeholders:

- `${AIMEBU_FLEET_PATH}` — canonical absolute target path.
- `${AIMEBU_FLEET_NAME}` — fleet name.
- `${AIMEBU_FLEET_AGENT_INDEX}` — zero-based agent index.

## How Wrapping Works

Write the inner command only:

```text
aimebu agent --auto-room --assume-role leader -- claude-docker
```

Before launching, aimebu wraps the command for Terminal.app. When **Auto-set
cwd** is enabled, aimebu also prepends the resolved target directory:

```text
osascript -e 'tell application "Terminal" to do script "cd /absolute/path && aimebu agent --auto-room --assume-role leader -- claude-docker"'
```

aimebu performs plain string replacement for placeholders; it does not set
environment variables for them. Double quotes (`"`) and backslashes (`\`) in
the resolved command are escaped before insertion into the AppleScript
`do script "..."` string.

Launching is best-effort: aimebu tries every command, prints any command that
failed to start, and exits with the number of failed starts.

If you previously stored full `osascript ...` commands in a fleet, strip each
row down to the inner command. aimebu now does the Terminal.app wrapping for
you.

## Examples

Claude Code leader:

```text
aimebu agent --auto-room --assume-role leader -- claude-docker
```

Codex worker:

```text
aimebu agent --auto-room --assume-role worker -- codex-docker
```

pi reviewer:

```text
aimebu agent --auto-room --assume-role reviewer -- pi-docker
```

## Settings

Fleets are edited in Settings -> Fleets in the web UI. The editor supports
adding, renaming, deleting, and clipboard import/export. **Copy fleet** copies
one fleet as an importable JSON envelope. **Copy fleets JSON** copies every
fleet. **Import fleets JSON** reads an envelope from the clipboard and falls
back to a paste textarea when clipboard read is unavailable.

```json
{
  "version": 1,
  "fleets": {
    "default": {
      "agents": [
        {
          "command": "aimebu agent --auto-room --assume-role leader -- claude-docker",
          "wrap_terminal": true,
          "auto_set_cwd": true
        },
        {
          "command": "cd ~/src/other && aimebu agent --auto-room -- codex-docker",
          "wrap_terminal": true,
          "auto_set_cwd": false
        }
      ]
    }
  }
}
```

Importing never overwrites an existing fleet name. Collisions are renamed with
`-2`, `-3`, and so on.

## Storage And Prune

Fleets are stored in `~/.aimebu/server/fleet.json` with file mode `0600`.
Plain `aimebu prune` / `aimebu prune -y` preserves this file. `aimebu prune
-a` deletes it with the rest of user settings.

Fleet commands run with your full shell privileges. Treat imported fleet JSON
like a shell script: only import fleets you trust.
