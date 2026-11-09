#!/usr/bin/env bash
# Publica uma versão do agente no endereço de download próprio
# (https://upguard.com.br/downloads/agent): confere a publicação montada pelo
# release (manifesto, assinatura e SHA-256 de cada binário), constrói a imagem
# nginx upguard/agent-downloads e, opcionalmente, envia ao Harbor e atualiza a
# tag em kustomize/deployment.yaml (o ArgoCD aplica).
#
# Usado pelo release.yml (repo privado, runner self-hosted) e à mão no
# build-agent-1 durante a transição (repo ainda público, o release roda no
# runner do GitHub, que não alcança o Harbor):
#
#   scripts/publish-downloads.sh --version 1.8.0 --from-release --push --kustomize
#   git add kustomize/deployment.yaml && git commit -m "Deploy agent-downloads v1.8.0 [skip ci]" && git push
#
# Opções:
#   --version X       versão (sem o v) — obrigatório
#   --tarball ARQ     upguard-agent-downloads-X.tar.gz gerado pelo release
#   --from-release    baixa o tarball da GitHub Release vX (gh autenticado)
#   --push            envia a imagem ao registry (exige docker login antes)
#   --kustomize       troca a tag da imagem em kustomize/deployment.yaml
# Env: REGISTRY (default harbor.shiftcloud.corp), IMAGE (default upguard/agent-downloads),
#      DOCKER (default docker), REPO (default devshiftlabs/upguard-agent).
set -euo pipefail

VERSION="" TARBALL="" FROM_RELEASE=0 PUSH=0 KUSTOMIZE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2#v}"; shift 2;;
    --tarball) TARBALL="$2"; shift 2;;
    --from-release) FROM_RELEASE=1; shift;;
    --push) PUSH=1; shift;;
    --kustomize) KUSTOMIZE=1; shift;;
    *) echo "opção desconhecida: $1" >&2; exit 2;;
  esac
done
[ -n "$VERSION" ] || { echo "erro: --version é obrigatório" >&2; exit 2; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REGISTRY="${REGISTRY:-harbor.shiftcloud.corp}"
IMAGE="${IMAGE:-upguard/agent-downloads}"
DOCKER="${DOCKER:-docker}"
REPO="${REPO:-devshiftlabs/upguard-agent}"
REF="$REGISTRY/$IMAGE:$VERSION"
NAME="upguard-agent-downloads-$VERSION.tar.gz"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ "$FROM_RELEASE" = 1 ]; then
  gh release download "v$VERSION" -R "$REPO" -p "$NAME" -D "$WORK"
  TARBALL="$WORK/$NAME"
fi
[ -n "$TARBALL" ] && [ -f "$TARBALL" ] || { echo "erro: informe --tarball ou --from-release" >&2; exit 2; }

mkdir -p "$WORK/ctx/site"
tar -xzf "$TARBALL" -C "$WORK/ctx/site"
AGENT_DIR="$WORK/ctx/site/downloads/agent"

# Confere antes de publicar (assinatura exigida se o agente tem chave embutida).
if command -v go >/dev/null 2>&1; then
  (cd "$ROOT" && go run ./cmd/upguard-release verify -dir "$AGENT_DIR")
else
  "$DOCKER" run --rm -v "$ROOT":/src:ro -v "$AGENT_DIR":/pub:ro -w /src \
    "golang:1.27.3-alpine" go run ./cmd/upguard-release verify -dir /pub
fi
grep -q "\"version\": \"$VERSION\"" "$AGENT_DIR/latest.json" || { echo "erro: o tarball não é da versão $VERSION" >&2; exit 1; }

cp "$ROOT/downloads/Dockerfile" "$ROOT/downloads/nginx.conf" "$WORK/ctx/"
"$DOCKER" build -t "$REF" "$WORK/ctx"
echo "imagem $REF construída"

if [ "$PUSH" = 1 ]; then
  "$DOCKER" push "$REF"
fi
if [ "$KUSTOMIZE" = 1 ]; then
  sed -i.bak "s|image: $REGISTRY/$IMAGE:.*|image: $REF|" "$ROOT/kustomize/deployment.yaml"
  rm -f "$ROOT/kustomize/deployment.yaml.bak"
  grep -n "image:" "$ROOT/kustomize/deployment.yaml"
fi
