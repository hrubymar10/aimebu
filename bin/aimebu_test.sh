#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
WRAPPER="$SCRIPT_DIR/aimebu"
CACHE_BINARY="$REPO_DIR/aimebu-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)"

cleanup() {
    if [ -n "${TEST_CONFIG_DIR:-}" ]; then
        AIMEBU_CONFIG_DIR="$TEST_CONFIG_DIR" "$WRAPPER" server stop >/dev/null 2>&1 || true
    fi
    if [ -n "${TMPROOT_FORCE_VERSION:-}" ]; then
        rm -rf "$TMPROOT_FORCE_VERSION"
    fi
    if [ -n "${TMPROOT_FORCE_START:-}" ]; then
        rm -rf "$TMPROOT_FORCE_START"
    fi
    if [ -n "${TEST_CONFIG_DIR:-}" ]; then
        rm -rf "$TEST_CONFIG_DIR"
    fi
}

trap cleanup EXIT

TMPROOT_FORCE_VERSION="$(mktemp -d)"
AIMEBU_FORCE_BUILD=1 TMPDIR="$TMPROOT_FORCE_VERSION" "$WRAPPER" version >/dev/null
if find "$TMPROOT_FORCE_VERSION" -maxdepth 1 -type f -name 'aimebu-*' | grep -q .; then
    echo "forced version run leaked tmp binary" >&2
    exit 1
fi

TMPROOT_FORCE_START="$(mktemp -d)"
TEST_CONFIG_DIR="$(mktemp -d)"
TEST_PORT="$((20000 + RANDOM % 10000))"
AIMEBU_FORCE_BUILD=1 \
TMPDIR="$TMPROOT_FORCE_START" \
AIMEBU_CONFIG_DIR="$TEST_CONFIG_DIR" \
AIMEBU_PORT="$TEST_PORT" \
"$WRAPPER" server start >/dev/null

if find "$TMPROOT_FORCE_START" -maxdepth 1 -type f -name 'aimebu-*' | grep -q .; then
    echo "forced start run leaked tmp binary" >&2
    exit 1
fi

# Explicitly clear AIMEBU_FORCE_BUILD for the default-path checks below.
# The smoke test must be deterministic even when the caller exports
# AIMEBU_FORCE_BUILD=1 globally (e.g. in claude-docker container env).
unset AIMEBU_FORCE_BUILD

AIMEBU_CONFIG_DIR="$TEST_CONFIG_DIR" AIMEBU_PORT="$TEST_PORT" "$WRAPPER" server status | grep -q "aimebu is running"

"$WRAPPER" version >/dev/null
if [ ! -f "$CACHE_BINARY" ]; then
    echo "cached binary missing after default wrapper run" >&2
    exit 1
fi

# AIMEBU_FORCE_BUILD=0 must not trigger the force-build path. Only the
# literal value "1" forces a rebuild into tmp; any other value (including
# "0") keeps using the cached binary.
TMPROOT_FORCE_ZERO="$(mktemp -d)"
AIMEBU_FORCE_BUILD=0 TMPDIR="$TMPROOT_FORCE_ZERO" "$WRAPPER" version >/dev/null
if find "$TMPROOT_FORCE_ZERO" -maxdepth 1 -type f -name 'aimebu-*' | grep -q .; then
    echo "AIMEBU_FORCE_BUILD=0 unexpectedly triggered tmp build" >&2
    exit 1
fi
rm -rf "$TMPROOT_FORCE_ZERO"
