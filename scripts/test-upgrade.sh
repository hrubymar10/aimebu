#!/bin/sh
# Build and exercise a released binary against a candidate on disposable data.
set -eu
usage() { echo 'usage: scripts/test-upgrade.sh <old-tag> [<new-ref>] | --from-backup <archive> [<new-ref>]' >&2; exit 2; }
[ "$#" -ge 1 ] && [ "$#" -le 3 ] || usage
mode=upgrade
if [ "$1" = --from-backup ]; then
    [ "$#" -ge 2 ] || usage
    mode=backup
    source=$2
    ref=${3:-HEAD}
    [ -f "$source" ] || { echo 'backup archive not found' >&2; exit 2; }
    source=$(cd "$(dirname "$source")" && pwd -P)/$(basename "$source")
else
    source=$1
    ref=${2:-HEAD}
    git rev-parse -q --verify "refs/tags/$source^{commit}" >/dev/null || { echo 'old tag not found' >&2; exit 2; }
fi
root=$(git rev-parse --show-toplevel)
git -C "$root" rev-parse -q --verify "$ref^{commit}" >/dev/null || { echo 'new ref not found' >&2; exit 2; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aimebu-upgrade-test.XXXXXXXX")
oldtree=
newtree=
cleanup() {
    if [ -n "$newtree" ]; then git -C "$root" worktree remove --force "$newtree" >/dev/null 2>&1 || true; fi
    if [ -n "$oldtree" ]; then git -C "$root" worktree remove --force "$oldtree" >/dev/null 2>&1 || true; fi
    rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
mkdir -p "$tmp/home" "$tmp/codex" "$tmp/claude" "$tmp/config" "$tmp/scratch"
# Every subprocess inherits these roots, including go build and the smoke test.
real_data_root=${AIMEBU_CONFIG_DIR:-$HOME/.aimebu}
export AIMEBU_REAL_DATA_ROOT="$real_data_root"
host_gopath=${GOPATH:-$(go env GOPATH)}
host_gocache=${GOCACHE:-$(go env GOCACHE)}
host_gomodcache=${GOMODCACHE:-$(go env GOMODCACHE)}
export HOME="$tmp/home" CODEX_HOME="$tmp/codex" CLAUDE_CONFIG_DIR="$tmp/claude"
export AIMEBU_CONFIG_DIR="$tmp/config" AIMEBU_SENTRY_DSN=off
export GOPATH="$host_gopath" GOCACHE="$host_gocache" GOMODCACHE="$host_gomodcache"
export AIMEBU_WEB_BUNDLE=off
newtree=$tmp/new-source
git -C "$root" worktree add --detach "$newtree" "$ref" >/dev/null
(cd "$newtree" && go build -o "$tmp/aimebu-new" ./cmd/aimebu)
echo 'PASS candidate build'
if [ "$mode" = upgrade ]; then
    oldtree=$tmp/old-source
    git -C "$root" worktree add --detach "$oldtree" "$source" >/dev/null
    (cd "$oldtree" && go build -o "$tmp/aimebu-old" ./cmd/aimebu)
    echo 'PASS old tag build'
    sh "$root/scripts/upgrade-smoke.sh" "$tmp/aimebu-old" "$tmp/aimebu-new" "$tmp/scratch"
else
    node "$root/scripts/test-upgrade-backup.cjs" "$tmp/aimebu-new" "$source" "$tmp"
fi
