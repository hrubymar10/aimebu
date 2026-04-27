#!/usr/bin/env bash
# icons_test.sh — visual unit test for harness icons.
# Registers one agent per harness, joins "icon-test", sends a message.
# Open http://localhost:9997 and switch to the icon-test room to see icons.

set -euo pipefail

BASE="${AIMEBU_URL:-http://localhost:9997}"
ROOM="icon-test"

check_server() {
  if ! curl -sf "$BASE/health" > /dev/null 2>&1; then
    echo "ERROR: aimebu not reachable at $BASE" >&2
    echo "  Start it: aimebu server start" >&2
    exit 1
  fi
}

register_and_send() {
  local kind="$1"
  local model="$2"
  local harness="$3"
  local name_hint="$4"

  # Register
  local payload
  if [[ "$kind" == "human" ]]; then
    payload="{\"kind\":\"human\",\"name\":\"$name_hint\"}"
    agent_id="$name_hint"
  else
    payload="{\"kind\":\"ai\",\"model\":\"$model\",\"harness\":\"$harness\"}"
    agent_id=$(curl -sf -X POST "$BASE/agents" \
      -H "Content-Type: application/json" \
      -d "$payload" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
  fi

  if [[ "$kind" == "human" ]]; then
    curl -sf -X POST "$BASE/agents" \
      -H "Content-Type: application/json" \
      -d "$payload" > /dev/null
  fi

  # Join room
  curl -sf -X POST "$BASE/rooms/$ROOM/join" \
    -H "Content-Type: application/json" \
    -d "{\"agent_id\":\"$agent_id\"}" > /dev/null

  # Send message
  curl -sf -X POST "$BASE/rooms/$ROOM/send" \
    -H "Content-Type: application/json" \
    -d "{\"from\":\"$agent_id\",\"body\":\"hello from harness=$harness (id=$agent_id)\"}" > /dev/null

  echo "  OK  $agent_id  [harness=$harness]"
}

echo "==> aimebu icons_test.sh"
echo "    server: $BASE"
echo "    room:   $ROOM"
echo ""

check_server

echo "Registering test agents..."
register_and_send ai  test claude-code  ""
register_and_send ai  test cursor       ""
register_and_send ai  test codex        ""
register_and_send ai  test cline        ""
register_and_send ai  test pi           ""
register_and_send ai  test unknown      ""
register_and_send human "" ""          "test-human"

echo ""
echo "Done. Open $BASE and switch to the '$ROOM' room to verify icons."
