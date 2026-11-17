#!/bin/sh
# Dev runner used by `make dev`. The server embeds its local supervisor.
# Children get TERM:
# background jobs of a non-interactive shell ignore INT.
set -u
bin=${1:?usage: dev-run.sh <aimebu-binary>}

# AIMEBU_DEV_CONFIG_DIR, AIMEBU_DEV_PORT, AIMEBU_DEV_TLS_PORT and
# AIMEBU_DEV_SENTRY_ENV are examples of the generic development overrides.
# Keep the wrapper's build flag and agent diagnostics, then remove the shell's
# production bus settings before applying any AIMEBU_DEV_* overrides.
for name in $(env | sed -n 's/^\(AIMEBU_[A-Z0-9_]*\)=.*/\1/p'); do
    case "$name" in
        AIMEBU_FORCE_BUILD|AIMEBU_AGENT_DEBUG|AIMEBU_DEV_*) ;;
        *) unset "$name" ;;
    esac
done
for name in $(env | sed -n 's/^\(AIMEBU_DEV_[A-Z0-9_]*\)=.*/\1/p'); do
    case "$name" in AIMEBU_DEV_SKIP_V1_PROBE) continue ;; esac
    suffix=${name#AIMEBU_DEV_}
    eval "export AIMEBU_${suffix}=\"\${${name}}\""
    unset "$name"
done

export AIMEBU_CONFIG_DIR="${AIMEBU_CONFIG_DIR:-$HOME/.aimebu-dev}"
export AIMEBU_PORT="${AIMEBU_PORT:-9897}"
export AIMEBU_TLS_PORT="${AIMEBU_TLS_PORT:-9896}"
export AIMEBU_SENTRY_ENV="${AIMEBU_SENTRY_ENV:-development}"
export AIMEBU_DEV_SKIP_V1_PROBE="${AIMEBU_DEV_SKIP_V1_PROBE:-1}"

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
