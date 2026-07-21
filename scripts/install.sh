#!/usr/bin/env bash
# Instalador do UpGuard Agent para Linux e macOS.
# Uso:
#   curl -sSL https://.../install.sh | sudo bash -s -- \
#        --client-id agt_xxx --client-secret sk_agt_xxx
#
# Flags:
#   --client-id       (obrigatório)
#   --client-secret   (obrigatório)
#   --server URL      (default https://api.upguard.com.br)
#   --interval SEG    (default 60)
#   --base-url URL    (onde baixar o binário; default releases do GitHub)
set -euo pipefail

CLIENT_ID=""
CLIENT_SECRET=""
SERVER="https://api.upguard.com.br"
INTERVAL="60"
BASE_URL="https://github.com/devshiftlabs/upguard-agent/releases/latest/download"

while [ $# -gt 0 ]; do
  case "$1" in
    --client-id) CLIENT_ID="$2"; shift 2;;
    --client-secret) CLIENT_SECRET="$2"; shift 2;;
    --server) SERVER="$2"; shift 2;;
    --interval) INTERVAL="$2"; shift 2;;
    --base-url) BASE_URL="$2"; shift 2;;
    *) echo "flag desconhecida: $1"; exit 1;;
  esac
done

[ -n "$CLIENT_ID" ] && [ -n "$CLIENT_SECRET" ] || { echo "erro: --client-id e --client-secret são obrigatórios"; exit 1; }
[ "$(id -u)" = "0" ] || { echo "erro: rode como root (sudo)"; exit 1; }

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux | darwin
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64";;
  arm64|aarch64) ARCH="arm64";;
  *) echo "arquitetura não suportada: $ARCH"; exit 1;;
esac

BIN_URL="$BASE_URL/upguard-agent-${OS}-${ARCH}"
BIN_PATH="/usr/local/bin/upguard-agent"

echo "Baixando $BIN_URL ..."
curl -fsSL "$BIN_URL" -o "$BIN_PATH"
chmod +x "$BIN_PATH"

# Config em /etc/upguard-agent/agent.env
mkdir -p /etc/upguard-agent
cat > /etc/upguard-agent/agent.env <<EOF
UPGUARD_CLIENT_ID=$CLIENT_ID
UPGUARD_CLIENT_SECRET=$CLIENT_SECRET
UPGUARD_SERVER_URL=$SERVER
UPGUARD_INTERVAL=$INTERVAL
EOF
chmod 600 /etc/upguard-agent/agent.env

if [ "$OS" = "linux" ]; then
  cat > /etc/systemd/system/upguard-agent.service <<EOF
[Unit]
Description=UpGuard Monitoring Agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/upguard-agent/agent.env
ExecStart=$BIN_PATH
Restart=always
RestartSec=10
DynamicUser=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable upguard-agent >/dev/null 2>&1 || true
  systemctl restart upguard-agent   # restart (não só start) para pegar binário novo em updates
  echo "OK — serviço systemd 'upguard-agent' ativo. Logs: journalctl -u upguard-agent -f"
else
  # macOS: launchd
  PLIST=/Library/LaunchDaemons/br.com.shiftlabs.upguard-agent.plist
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>br.com.shiftlabs.upguard-agent</string>
  <key>ProgramArguments</key><array><string>$BIN_PATH</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>UPGUARD_CLIENT_ID</key><string>$CLIENT_ID</string>
    <key>UPGUARD_CLIENT_SECRET</key><string>$CLIENT_SECRET</string>
    <key>UPGUARD_SERVER_URL</key><string>$SERVER</string>
    <key>UPGUARD_INTERVAL</key><string>$INTERVAL</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load -w "$PLIST"
  echo "OK — LaunchDaemon 'upguard-agent' carregado."
fi
