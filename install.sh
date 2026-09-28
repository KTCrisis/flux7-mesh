#!/bin/sh
# Install mesh7 (the proxy) and mesh (the approval CLI) from the latest
# GitHub release, and optionally run mesh7 as a service.
#
#   curl -fsSL https://raw.githubusercontent.com/KTCrisis/flux7-mesh/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --service        # + systemd user service
#   curl -fsSL .../install.sh | sh -s -- --system         # + system service (sudo)
#
# Options:
#   --prefix DIR     where the binaries go (default ~/.local/bin, /usr/local/bin as root)
#   --config PATH    config the service serves (default ~/.config/mesh7/config.yaml,
#                    a starter one is written if missing)
#   --service        install and start a systemd user service
#   --system         install and start a systemd system service (uses sudo)
#   --version vX.Y.Z install that release instead of the latest (v0.17.0 or later)
set -eu

REPO="KTCrisis/flux7-mesh"

usage() {
  cat <<'USAGE'
install.sh [--prefix DIR] [--config PATH] [--service | --system] [--version vX.Y.Z]
  --prefix DIR     where mesh7 and mesh go (default ~/.local/bin, /usr/local/bin as root)
  --config PATH    config the service serves (default ~/.config/mesh7/config.yaml)
  --service        install and start a systemd user service
  --system         install and start a systemd system service (uses sudo)
  --version vX.Y.Z install that release instead of the latest (v0.17.0 or later)
USAGE
}
PREFIX=""
CONFIG="${HOME}/.config/mesh7/config.yaml"
SERVICE=""
VERSION="${MESH7_VERSION:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix)  PREFIX="$2"; shift 2 ;;
    --config)  CONFIG="$2"; shift 2 ;;
    --service) SERVICE="user"; shift ;;
    --system)  SERVICE="system"; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "install.sh: unknown option $1" >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows download mesh7_windows_amd64.zip from the releases page" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

if [ -z "$PREFIX" ]; then
  if [ "$(id -u)" = 0 ]; then PREFIX=/usr/local/bin; else PREFIX="${HOME}/.local/bin"; fi
fi

ASSET="mesh7_${OS}_${ARCH}.tar.gz"
if [ -n "$VERSION" ]; then
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
else
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
fi

command -v curl >/dev/null 2>&1 || die "curl is required"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "Downloading ${URL}"
curl -fsSL "$URL" -o "$TMP/mesh7.tar.gz" || die "download failed: ${URL}"
tar -xzf "$TMP/mesh7.tar.gz" -C "$TMP"

mkdir -p "$PREFIX"
for bin in mesh7 mesh; do
  [ -f "$TMP/$bin" ] || die "$bin missing from the archive (releases before v0.17.0 did not ship mesh)"
  install -m 0755 "$TMP/$bin" "$PREFIX/$bin"
done
say "Installed $("$PREFIX/mesh7" --version) and mesh to ${PREFIX}"

case ":${PATH}:" in
  *":${PREFIX}:"*) ;;
  *) say "Note: ${PREFIX} is not on your PATH. Add it, for instance:"
     say "  echo 'export PATH=\"${PREFIX}:\$PATH\"' >> ~/.profile" ;;
esac

[ -n "$SERVICE" ] || {
  say ""
  say "Next: point Claude Code at it"
  say "  claude mcp add mesh7 -- ${PREFIX}/mesh7 --mcp --config /path/to/config.yaml"
  say "Docs: https://docs.flux7.art/mesh7/getting-started/"
  exit 0
}

[ "$OS" = linux ] || die "--service and --system use systemd (Linux). On macOS run: mesh7 serve --config ${CONFIG}"
command -v systemctl >/dev/null 2>&1 || die "systemctl not found; run: mesh7 serve --config ${CONFIG}"

# A service needs a config to serve. Write a starter one, never overwrite.
if [ ! -f "$CONFIG" ]; then
  mkdir -p "$(dirname "$CONFIG")"
  cat > "$CONFIG" <<CFG
# mesh7 config: add your MCP servers and rules, see
# https://docs.flux7.art/mesh7/configuration/
port: 9090
approval:
  channel: queue          # a service has no terminal: answer with 'mesh approve <id>'

mcp_servers: []

policies:
  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
CFG
  say "Wrote a starter config to ${CONFIG} (denies everything until you add rules)"
fi

UNIT_EXEC="${PREFIX}/mesh7 serve --config ${CONFIG}"

if [ "$SERVICE" = user ]; then
  UNIT_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/mesh7.service" <<UNIT
[Unit]
Description=flux7-mesh governance proxy (mesh7)
Documentation=https://docs.flux7.art/mesh7/deployment-modes/
After=network.target

[Service]
ExecStart=${UNIT_EXEC}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT
  if systemctl --user daemon-reload 2>/dev/null && systemctl --user enable --now mesh7; then
    say "Started the user service: systemctl --user status mesh7"
    say "To keep it running after logout: loginctl enable-linger ${USER:-\$USER}"
  else
    die "the systemd user manager is not available here (common on WSL). Re-run with --system."
  fi
else
  command -v sudo >/dev/null 2>&1 || [ "$(id -u)" = 0 ] || die "--system needs root or sudo"
  SUDO=""; [ "$(id -u)" = 0 ] || SUDO=sudo
  RUN_AS="$(id -un)"
  $SUDO tee /etc/systemd/system/mesh7.service >/dev/null <<UNIT
[Unit]
Description=flux7-mesh governance proxy (mesh7)
Documentation=https://docs.flux7.art/mesh7/deployment-modes/
After=network.target

[Service]
User=${RUN_AS}
ExecStart=${UNIT_EXEC}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now mesh7
  say "Started the system service: systemctl status mesh7"
fi

say ""
say "Claude Code then relays to it:"
say "  claude mcp add mesh7 -- ${PREFIX}/mesh7 --mcp --config ${CONFIG}"
say "Approvals: mesh approve <id>   Docs: https://docs.flux7.art/mesh7/deployment-modes/"
