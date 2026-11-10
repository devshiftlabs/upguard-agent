#!/bin/sh
# Instalador do UpGuard Agent para Linux, macOS e FreeBSD/pfSense.
# POSIX sh (sem bashismos) — roda em bash, dash e no /bin/sh do FreeBSD/pfSense.
# Uso:
#   curl -sSL https://upguard.com.br/downloads/agent/install.sh | sudo sh -s -- \
#        --client-id agt_xxx --client-secret sk_agt_xxx
#   # No pfSense (já é root, e pode não ter curl):
#   #   fetch -o- https://upguard.com.br/downloads/agent/install.sh | \
#   #   sh -s -- --client-id agt_xxx --client-secret sk_agt_xxx
#
# Flags:
#   --client-id       (obrigatório)
#   --client-secret   (obrigatório)
#   --server URL      (default https://api.upguard.com.br)
#   --interval SEG    (default 60)
#   --base-url URL    (onde baixar o binário e o SHA256SUMS; default
#                      https://upguard.com.br/downloads/agent/latest)
#   --update-url URL  (endereço do auto-update do agente; "off" desliga;
#                      default o do próprio agente)
#
# O binário só é instalado se o SHA-256 conferir com o SHA256SUMS publicado.
set -eu

CLIENT_ID=""
CLIENT_SECRET=""
SERVER="https://api.upguard.com.br"
INTERVAL="60"
BASE_URL="https://upguard.com.br/downloads/agent/latest"
UPDATE_URL=""

while [ $# -gt 0 ]; do
  case "$1" in
    --client-id) CLIENT_ID="$2"; shift 2;;
    --client-secret) CLIENT_SECRET="$2"; shift 2;;
    --server) SERVER="$2"; shift 2;;
    --interval) INTERVAL="$2"; shift 2;;
    --base-url) BASE_URL="$2"; shift 2;;
    --update-url) UPDATE_URL="$2"; shift 2;;
    *) echo "flag desconhecida: $1"; exit 1;;
  esac
done

[ -n "$CLIENT_ID" ] && [ -n "$CLIENT_SECRET" ] || { echo "erro: --client-id e --client-secret são obrigatórios"; exit 1; }
[ "$(id -u)" = "0" ] || { echo "erro: rode como root (sudo)"; exit 1; }

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux | darwin | freebsd
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64";;
  arm64|aarch64) ARCH="arm64";;
  *) echo "arquitetura não suportada: $ARCH"; exit 1;;
esac

BIN_NAME="upguard-agent-${OS}-${ARCH}"
BIN_URL="$BASE_URL/$BIN_NAME"
BIN_PATH="/usr/local/bin/upguard-agent"

# download URL -> arquivo. Usa curl se existir, senão fetch (padrão no FreeBSD).
download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v fetch >/dev/null 2>&1; then
    fetch -q -o "$2" "$1"
  else
    echo "erro: nem curl nem fetch disponíveis para baixar o binário"; exit 1
  fi
}

# sha256_of ARQUIVO -> hash em hexadecimal (sha256sum no Linux, shasum no
# macOS, sha256 no FreeBSD, openssl como último recurso).
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  elif command -v sha256 >/dev/null 2>&1; then sha256 -q "$1"
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | awk '{print $NF}'
  fi
}

echo "Baixando $BIN_URL ..."
# Baixa para um temp e move (rename atômico) — evita "Text file busy" (ETXTBSY)
# quando o binário já está em execução (update do agente).
TMP_BIN="$(mktemp)"
TMP_SUMS="$(mktemp)"
trap 'rm -f "$TMP_BIN" "$TMP_SUMS"' EXIT
download "$BASE_URL/SHA256SUMS" "$TMP_SUMS"
download "$BIN_URL" "$TMP_BIN"
EXPECTED="$(awk -v f="$BIN_NAME" '$2 == f || $2 == "*" f {print $1}' "$TMP_SUMS" | head -n 1)"
GOT="$(sha256_of "$TMP_BIN")"
[ -n "$EXPECTED" ] || { echo "erro: $BIN_NAME não está no SHA256SUMS publicado"; exit 1; }
[ -n "$GOT" ] || { echo "erro: nenhuma ferramenta de SHA-256 (sha256sum/shasum/sha256/openssl) disponível"; exit 1; }
if [ "$GOT" != "$EXPECTED" ]; then
  echo "erro: SHA-256 do binário não confere (recebido $GOT, esperado $EXPECTED) — nada foi instalado"
  exit 1
fi
echo "SHA-256 conferido: $GOT"
chmod +x "$TMP_BIN"
mv -f "$TMP_BIN" "$BIN_PATH"

# Config em /etc/upguard-agent/agent.env
mkdir -p /etc/upguard-agent
cat > /etc/upguard-agent/agent.env <<EOF
UPGUARD_CLIENT_ID=$CLIENT_ID
UPGUARD_CLIENT_SECRET=$CLIENT_SECRET
UPGUARD_SERVER_URL=$SERVER
UPGUARD_INTERVAL=$INTERVAL
EOF
[ -z "$UPDATE_URL" ] || echo "UPGUARD_UPDATE_URL=$UPDATE_URL" >> /etc/upguard-agent/agent.env
chmod 600 /etc/upguard-agent/agent.env

case "$OS" in
linux)
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
# Roda como root para enxergar serviços do sistema (systemctl) e todos os discos.

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable upguard-agent >/dev/null 2>&1 || true
  systemctl restart upguard-agent   # restart (não só start) para pegar binário novo em updates
  echo "OK — serviço systemd 'upguard-agent' ativo. Logs: journalctl -u upguard-agent -f"
  ;;

darwin)
  # macOS: launchd
  PLIST=/Library/LaunchDaemons/br.com.shiftlabs.upguard-agent.plist
  UPDATE_XML=""
  [ -z "$UPDATE_URL" ] || UPDATE_XML="<key>UPGUARD_UPDATE_URL</key><string>$UPDATE_URL</string>"
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
    $UPDATE_XML
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load -w "$PLIST"
  echo "OK — LaunchDaemon 'upguard-agent' carregado."
  ;;

freebsd)
  # FreeBSD/pfSense: serviço rc.d supervisionado por daemon(8) (reinício em falha).
  # O pfSense só executa no boot scripts rc.d terminados em ".sh"; no FreeBSD puro
  # o nome sem sufixo é o convencional. Detectamos o pfSense pelo ident do kernel.
  RC_NAME="upguard_agent"
  RC_FILE="/usr/local/etc/rc.d/${RC_NAME}"
  if [ "$(uname -i 2>/dev/null || true)" = "pfSense" ] || [ -f /usr/local/sbin/pfSsh.php ]; then
    RC_FILE="/usr/local/etc/rc.d/${RC_NAME}.sh"
  fi
  mkdir -p /usr/local/etc/rc.d
  cat > "$RC_FILE" <<EOF
#!/bin/sh
#
# PROVIDE: ${RC_NAME}
# REQUIRE: NETWORKING
# KEYWORD: shutdown
#
# UpGuard Monitoring Agent — serviço rc.d (FreeBSD / pfSense).

. /etc/rc.subr

name="${RC_NAME}"
rcvar="${RC_NAME}_enable"

# pidfile = PID do supervisor daemon(8); o rc.subr para o serviço matando-o (o
# supervisor então encerra o agente e NÃO o reinicia).
pidfile="/var/run/\${name}.pid"
command="/usr/sbin/daemon"
agent_bin="${BIN_PATH}"
# -r reinicia o agente se ele cair; -S envia stdout/stderr ao syslog (tag=name);
# -P grava o PID do supervisor; -p grava o PID do agente.
command_args="-r -S -T \${name} -P \${pidfile} -p /var/run/\${name}_child.pid \${agent_bin}"

start_precmd="${RC_NAME}_precmd"
${RC_NAME}_precmd()
{
	# Exporta as credenciais/config para o processo do agente.
	if [ -r /etc/upguard-agent/agent.env ]; then
		set -a
		. /etc/upguard-agent/agent.env
		set +a
	fi
}

load_rc_config "\$name"
: \${${RC_NAME}_enable:="YES"}
# O pfSense executa scripts *.sh no boot sem passar argumento — assume "start".
run_rc_command "\${1:-start}"
EOF
  chmod +x "$RC_FILE"
  # Habilita no rc.conf (no-op no pfSense, mas correto no FreeBSD puro).
  sysrc "${RC_NAME}_enable=YES" >/dev/null 2>&1 || true
  # restart (não só start) para pegar binário novo em reinstalações/updates.
  service "$RC_NAME" restart 2>/dev/null || service "$RC_NAME" start
  echo "OK — serviço rc.d '${RC_NAME}' ativo ($RC_FILE). Logs: tail -f /var/log/messages | grep ${RC_NAME}"
  ;;

*)
  echo "erro: SO não suportado: $OS"; exit 1
  ;;
esac
