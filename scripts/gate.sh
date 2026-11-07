#!/bin/sh
# The full gate, as two lanes that run side by side: `make test-full` (format, vet, the Go tests under -race, the web unit tests) and `make
# test-ui` (the Chromium tests), the second GATE_UI_RUNS times in a row (default 2: AGENTS.md asks for two green runs on the tip that is pushed).
# Each lane writes its own log under tmp/ and prints its tail when it ends; the gate is green only when both lanes are. The lanes share nothing
# (every test owns its ports and temporary directories), so they only compete for CPU: the whole gate takes about as long as the UI lane alone.
# usage: scripts/gate.sh        (via `make gate`; WEB_TEST_RUNNER, UI_JOBS and GO_TEST_SHARDS pass through to the lanes)
set -u
runs=${GATE_UI_RUNS:-2}
mkdir -p tmp
stamp=$(date +%Y%m%d-%H%M%S)
full_log=tmp/gate-full-$stamp.log
ui_log=tmp/gate-ui-$stamp.log
started=$(date +%s)

( s=$(date +%s); make test-full > "$full_log" 2>&1; code=$?; echo "lane test-full: exit $code in $(( $(date +%s) - s ))s" >> "$full_log"; exit $code ) &
full_pid=$!
( s=$(date +%s); code=0
  n=1
  while [ "$n" -le "$runs" ]; do
    echo "== make test-ui, run $n of $runs" >> "$ui_log"
    make test-ui >> "$ui_log" 2>&1 || { code=$?; break; }
    n=$((n + 1))
  done
  echo "lane test-ui: exit $code in $(( $(date +%s) - s ))s" >> "$ui_log"; exit $code ) &
ui_pid=$!

wait "$full_pid"; full=$?
wait "$ui_pid"; ui=$?
elapsed=$(( $(date +%s) - started ))

for log in "$full_log" "$ui_log"; do
  echo "---- $log (last lines)"
  tail -n 6 "$log"
done
echo "---- gate: test-full exit $full, test-ui exit $ui, $elapsed s ($full_log, $ui_log)"
[ "$full" -eq 0 ] && [ "$ui" -eq 0 ]
