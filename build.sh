#!/usr/bin/env bash
# Cross-compila o agente para Linux/macOS/Windows (amd64+arm64) usando um
# container golang — não requer Go instalado no host.
set -euo pipefail
VERSION="${1:-1.0.0}"
cd "$(dirname "$0")"
mkdir -p dist

DOCKER="${DOCKER:-docker}"
"$DOCKER" run --rm -v "$PWD":/src -w /src golang:1.22-alpine sh -c "
  set -e
  apk add --no-cache git >/dev/null
  go mod tidy
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
    os=\${target%/*}; arch=\${target#*/}
    ext=''; [ \"\$os\" = windows ] && ext='.exe'
    echo \"building \$os/\$arch\"
    CGO_ENABLED=0 GOOS=\$os GOARCH=\$arch go build \
      -ldflags \"-s -w -X main.version=$VERSION\" \
      -o dist/upguard-agent-\$os-\$arch\$ext .
  done
"
echo '--- artefatos ---'
ls -la dist/
