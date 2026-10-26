#!/bin/sh
# Runs the browser-free web unit tests, one `node scripts/test-<name>.cjs` after the other, with a start and an end line for each, and stops
# at the first one that fails. It is what `make test-web` runs, with the local node or, in a container, the pinned image's. The tests only
# read the repository: they must run from a read-only checkout, so nothing here may write into it.
# usage: scripts/test-web.sh <name>...
set -eu
total=$#
n=0
for name in "$@"; do
	n=$((n + 1))
	started=$(date +%s)
	printf '[web %d/%d] start  %s\n' "$n" "$total" "$name"
	node "scripts/test-$name.cjs"
	printf '[web %d/%d] ok     %s (%ds)\n' "$n" "$total" "$name" $(($(date +%s) - started))
done
