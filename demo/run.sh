#!/bin/sh
# mesh7 demo control. Runs a mesh on :9191 and a console on :3118, next to
# anything already running; nothing outside demo/state is touched.
#
#   ./run.sh start      start the demo mesh (and the console if found)
#   ./run.sh rugpull    the CRM "updates itself"; restart the mesh to see it
#   ./run.sh verify     check the trace chain, then show a tampered copy failing
#   ./run.sh tamper     write state/tampered.jsonl, one approval rewritten into an allow
#   ./run.sh reset      stop and wipe demo state (pins, approvals, traces)
#   ./run.sh stop
#
# MESH7 (default: mesh7 on PATH) and CONSOLE_DIR (a built flux7-console
# frontend, default ~/flux7-console/frontend) can be overridden.
set -eu
cd "$(dirname "$0")"
DEMO="$PWD"
MESH7="${MESH7:-mesh7}"
CONSOLE_DIR="${CONSOLE_DIR:-$HOME/flux7-console/frontend}"
export MESH_TRACE_KEY="${MESH_TRACE_KEY:-demo-key-not-a-secret}"
mkdir -p state

# Wait for the old process to exit: it holds the SQLite lock and the port,
# and a health check would otherwise answer from it.
stop_pid() {
  [ -f "$1" ] || return 0
  pid="$(cat "$1")"; rm -f "$1"
  kill "$pid" 2>/dev/null || return 0
  i=0; while kill -0 "$pid" 2>/dev/null; do i=$((i+1)); [ $i -gt 40 ] && kill -9 "$pid" 2>/dev/null; sleep 0.25; done
}
stop_mesh() { stop_pid state/mesh.pid; }
stop_console() { stop_pid state/console.pid; }

start_mesh() {
  stop_mesh
  "$MESH7" serve --config config.yaml > state/mesh.log 2>&1 &
  echo $! > state/mesh.pid
  i=0; until curl -fs localhost:9191/health >/dev/null 2>&1; do
    i=$((i+1))
    if [ $i -gt 40 ] || ! kill -0 "$(cat state/mesh.pid)" 2>/dev/null; then tail -5 state/mesh.log; exit 1; fi
    sleep 0.25
  done
  echo "mesh    http://localhost:9191  ($("$MESH7" --version))"
}

start_console() {
  [ -d "$CONSOLE_DIR/.next" ] || { echo "console: no build in $CONSOLE_DIR (skipped)"; return; }
  stop_console
  (cd "$CONSOLE_DIR" && MESH_URL=http://localhost:9191 MESH_ADMIN_TOKEN= POLICY_DIR="$DEMO/policies" \
     ./node_modules/.bin/next start -H 127.0.0.1 -p 3118 > "$DEMO/state/console.log" 2>&1 &
   echo $! > "$DEMO/state/console.pid")
  echo "console http://localhost:3118/mesh/tools"
}

case "${1:-}" in
  start)   start_mesh; start_console ;;
  rugpull) touch state/rugpull; start_mesh; echo "the CRM changed its catalogue: see the banner on the Tools page" ;;
  tamper)
    cp state/traces.jsonl state/tampered.jsonl
    # Change one decision in place, as someone covering their tracks would:
    # the first approval becomes an allow, nothing else moves.
    python3 - <<'PY'
lines = open("state/tampered.jsonl").read().splitlines(keepends=True)
for i, l in enumerate(lines):
    if '"policy":"human_approval"' in l:
        lines[i] = l.replace('"policy":"human_approval"', '"policy":"allow"', 1)
        print(f"tampered line {i+1}: policy human_approval -> allow")
        break
open("state/tampered.jsonl", "w").write("".join(lines))
PY
    ;;
  verify)
    curl -s localhost:9191/traces/verify; echo
    "$0" tamper
    "$MESH7" trace verify state/tampered.jsonl || true ;;
  reset)   stop_mesh; stop_console; rm -rf state; echo "demo state wiped" ;;
  stop)    stop_mesh; stop_console; echo "stopped" ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
