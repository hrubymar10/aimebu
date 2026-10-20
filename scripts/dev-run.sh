#!/bin/sh
# Dev runner used by `make dev`. The server embeds its local supervisor.
# Children get TERM:
# background jobs of a non-interactive shell ignore INT.
set -u
bin=${1:?usage: dev-run.sh <aimebu-binary>}

"$bin" server &
server=$!
stop() {
	kill -TERM "$server" 2>/dev/null
	wait
	exit 0
}
trap stop INT TERM

wait "$server"
stop
