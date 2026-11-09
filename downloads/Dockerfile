# Imagem do endereço de download do agente: nginx estático com a publicação
# montada por `upguard-release stage` (binários, SHA256SUMS, latest.json,
# assinatura e instaladores). Construída por scripts/publish-downloads.sh —
# o contexto do build é o diretório que o script prepara, não o repositório.
FROM nginxinc/nginx-unprivileged:1.30.6-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
# site/ = raiz servida (arquivos do root, só leitura para o nginx); a
# publicação fica em site/downloads/agent/.
COPY site/ /usr/share/nginx/html/

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
