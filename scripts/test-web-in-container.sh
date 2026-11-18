#!/bin/sh
# The container side of `make test-web`: the repository is mounted read-only at /src, and the tests run from a copy in /work (a tmpfs), so
# nothing can ever be written back to the checkout. The host pipes the tracked file list in (`git ls-files -z`); only those files are copied,
# so untracked local state (a checkout's .aimebu/ worktrees, node_modules, tmp/) cannot fill the 512 MB tmpfs. Tracked files missing from the
# working tree are skipped, as the tests could not read them anyway.
# The check of the image's CA roots runs first, before any test.
# usage: test-web-in-container.sh <name>...
set -eu
# One stdin, three scratch files: the NUL-separated list as it arrives, the newline form busybox tar's -T reads, and the files that exist.
cat >/tmp/test-web-files.z
tr '\0' '\n' </tmp/test-web-files.z >/tmp/test-web-files
: >/tmp/test-web-files.present
while IFS= read -r f; do
	if [ -e "/src/$f" ]; then
		printf '%s\n' "$f" >>/tmp/test-web-files.present
	fi
done </tmp/test-web-files
tar -C /src -cf - -T /tmp/test-web-files.present | tar -xf - -C /work
cd /work
node scripts/check-node-ca.cjs
exec sh scripts/test-web.sh "$@"
