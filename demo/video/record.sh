#!/bin/sh
# Records the approval scene to out/approval.mp4, from a clean demo state.
#
#   ./record.sh
#
# Needs what ../run.sh needs (mesh7, a built console), plus ttyd, ffmpeg,
# node and a Chrome or Chromium (CHROME, default: the one Playwright caches).
# MESH7 and CONSOLE_DIR are passed through to run.sh.
set -eu
cd "$(dirname "$0")"
HERE="$PWD"
DEMO="$(cd .. && pwd)"
export MESH7="${MESH7:-mesh7}"
export CHROME="${CHROME:-$(ls -d "$HOME"/.cache/ms-playwright/chromium-*/chrome-linux64/chrome 2>/dev/null | tail -1)}"
mkdir -p out

[ -d node_modules/puppeteer-core ] || npm install --silent --no-audit --no-fund

"$DEMO/run.sh" reset >/dev/null
"$DEMO/run.sh" start

# The agent's terminal, served to the stage. A bare shell in demo/, with a
# short prompt and the same MESH7 as run.sh.
cat > out/shell.rc <<EOF
PS1='\[\033[2m\]demo \$\[\033[0m\] '
export MESH_TRACE_KEY=demo-key-not-a-secret
mesh7() { "\$MESH7" "\$@"; }
cd "$DEMO"
clear
EOF
ttyd -i 127.0.0.1 -p "${TTYD_PORT:-7691}" -W -o \
  -t fontSize=17 -t fontFamily='JetBrains Mono, monospace' -t disableLeaveAlert=true \
  -t 'theme={"background":"#0a0e13","foreground":"#c8d4df","cursor":"#62d6e0"}' \
  env MESH7="$MESH7" bash --noprofile --rcfile "$HERE/out/shell.rc" > out/ttyd.log 2>&1 &
TTYD=$!
trap 'kill $TTYD 2>/dev/null; "$DEMO/run.sh" stop >/dev/null' EXIT
sleep 1

TERM_URL="http://127.0.0.1:${TTYD_PORT:-7691}" node record.mjs
ffmpeg -loglevel error -y -i out/approval.webm \
  -c:v libx264 -pix_fmt yuv420p -crf 20 -preset slow -movflags +faststart out/approval.mp4
echo "$HERE/out/approval.mp4"
