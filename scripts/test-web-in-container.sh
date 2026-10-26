#!/bin/sh
# The container side of `make test-web`: the repository is mounted read-only at /src, so it is copied to /work (a tmpfs) and the tests run
# from there. The tests only read the repository, but running them from a copy means that nothing can ever be written back to the checkout,
# and that the checkout's .git and tmp/ (not needed by the tests) are not carried along.
# The check of the image's CA roots runs first, before any test.
# usage: test-web-in-container.sh <name>...
set -eu
(cd /src && tar -cf - --exclude=./.git --exclude=./tmp .) | tar -xf - -C /work
cd /work
node scripts/check-node-ca.cjs
exec sh scripts/test-web.sh "$@"
