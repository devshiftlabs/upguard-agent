# Download próprio do agente e plano de transição

Objetivo: o binário do agente, o manifesto de versões e os instaladores passam a
ser servidos pelo próprio UpGuard (`https://upguard.com.br/downloads/agent/`),
para o repositório `devshiftlabs/upguard-agent` poder virar **privado** (regra da
casa) sem quebrar instalação nem auto-update.

## 1. Opção escolhida: imagem "agent-downloads" no ns `upguard` (Harbor + ArgoCD)

```
tag vX.Y.Z ─► release.yml ─► binários (8 plataformas) + latest.json assinado + SHA256SUMS + instaladores
                          └► imagem nginx harbor.shiftcloud.corp/upguard/agent-downloads:X.Y.Z
                          └► commit "Deploy agent-downloads vX.Y.Z" em kustomize/ ─► ArgoCD
agente / instalador ─► Cloudflare ─► nginx-dmz ─► NPM ─► upguard-frontend (nginx)
                                     location /downloads/agent/ ─► Service upguard-agent-downloads (ClusterIP)
```

| Opção | Por que não / por que sim |
|---|---|
| (a) backend servir `/api/agent/releases/latest` + binários de um volume | Precisa de um volume que o CI consiga escrever (PVC local-path preso a um nó, ou endpoint de upload com segredo novo no backend). Mistura release do agente com deploy do backend. Rollback = mexer em arquivos no volume. |
| **(b) imagem nginx estática publicada pelo pipeline (escolhida)** | Mesmo fluxo dos outros produtos (tag → Harbor → kustomize → ArgoCD). Nenhum segredo novo em servidor: o push usa o robô do Harbor já existente (`robot$upguard+upguard-ci`) como segredo do repo. Imagem imutável por versão; **rollback = voltar a tag** no `kustomize/deployment.yaml`. Não toca no nginx-dmz: a rota pública vem do nginx do frontend. |
| (c) diretório estático no nginx-dmz | O CI precisaria de SSH/escrita no servidor de borda (segredo novo no servidor mais exposto), publicação manual, sem histórico/rollback, fora do GitOps. |

Detalhes da (b):

- `downloads/Dockerfile` + `downloads/nginx.conf`: `nginxinc/nginx-unprivileged:1.30.6-alpine`, porta 8080,
  `latest.json`/assinatura/instaladores/`latest/` com `Cache-Control: no-cache`, `v<versão>/` com cache longo
  (a Cloudflare não serve manifesto velho e os binários versionados podem ficar em cache).
- `kustomize/` (Deployment 2 réplicas, não-root, raiz só leitura, Service ClusterIP) + `argocd-application.yaml`.
- Rota pública: `location ^~ /downloads/agent/` no `nginx.conf` do frontend (PR no `shiftlabs-upguard-web`),
  com resolução em tempo de execução (CoreDNS `10.43.0.10`): se o serviço não existir o frontend sobe normalmente
  e só `/downloads/agent/` responde 502.
- Alternativa sem passar pelo frontend: um NodePort no Service + `location /downloads/agent/` no host
  `upguard.com.br` do NPM/nginx-dmz. Não implementado (mudança manual na borda).

## 2. Formato publicado e verificação

```
/downloads/agent/latest.json        {"version":"1.8.0","files":{"linux-amd64":{"name","url":"v1.8.0/…","sha256","size"},…}}
/downloads/agent/latest.json.sig    ed25519(base64) dos bytes de latest.json
/downloads/agent/install.sh|.ps1
/downloads/agent/v1.8.0/upguard-agent-<os>-<arch>[.exe] + SHA256SUMS
/downloads/agent/latest/ -> v1.8.0  (usado pelos instaladores)
```

- `internal/release`: formato, `Stage` (monta o layout), `VerifyDir`, assinatura ed25519.
- `cmd/upguard-release`: `genkey`, `stage`, `verify` (usado pelo CI e pelo `scripts/publish-downloads.sh`).
- Agente (`update.go`): lê `latest.json` (limite 1 MiB), exige `latest.json.sig` se tiver chave embutida,
  baixa o binário para `<exe>.new` com limite de 256 MiB, confere **tamanho e SHA-256**, só então `rename` +
  reexec. Qualquer falha apaga o `.new`, registra no log e o agente segue monitorando. Atualização forçada
  pelo portal também passa pelo manifesto (e pela assinatura).
- Endereço configurável: `--update-url` / `UPGUARD_UPDATE_URL` (padrão `https://upguard.com.br/downloads/agent`,
  `off` desliga). Os instaladores aceitam `--update-url` / `-UpdateUrl`.
- Instaladores: baixam `SHA256SUMS` + binário de `.../latest/` e recusam se o SHA-256 não conferir.

**Por que a assinatura importa (medido no teste local):** só com SHA-256, quem controlar o endereço de download
(cluster, imagem, Cloudflare) troca binário **e** manifesto juntos e o agente instala. Com a chave embutida o
manifesto alterado é recusado ("assinatura do manifesto não confere"). Recomendação: gerar a chave **antes** de
taggear a v1.8.0, porque a versão que os hosts instalarem define se as próximas exigem assinatura.

## 3. O que o Luiz precisa criar (nada disso foi criado)

1. **Chave de assinatura** (recomendado antes da v1.8.0):
   `go run ./cmd/upguard-release genkey` → linha 2 (pública) em `internal/release/release.go`
   (`PublicKeyB64`, commit); linha 4 (privada, semente ed25519 de 32 bytes em base64) **só** como segredo do
   repositório `AGENT_SIGNING_KEY` (Settings → Secrets → Actions, nível de repositório: a org é Free e segredo de
   organização não chega a repo privado). Guardar uma cópia no Passbolt. O `stage` falha se a pública estiver
   embutida e o segredo faltar ou não corresponder.
2. **`HARBOR_USERNAME` / `HARBOR_PASSWORD`** no repositório `upguard-agent` (o robô `robot$upguard+upguard-ci`
   já existe e tem push no projeto `upguard`). Só são usados depois que o repo for privado.
3. **ArgoCD**: deploy key só-leitura no repo `upguard-agent` + secret `repo-upguard-agent` no ns `argocd`
   (igual aos outros `repo-*`), depois `kubectl apply -f argocd-application.yaml`. Enquanto o repo é público o
   ArgoCD lê sem chave, mas ela precisa existir antes de virar privado.
4. Conferir que o runner group dos self-hosted aceita o repositório (o job `publish-downloads` usa
   `[self-hosted, linux, docker]`).

## 4. Sequência de publicação

1. Merge do PR #1 (qualidade) e deste PR na `main` (sem tag).
2. Itens 1–3 da seção 3 (chave antes do passo 3).
3. `git tag v1.8.0 && git push origin v1.8.0`. Com o repo público o `release.yml` roda no runner do GitHub:
   testes, binários, publicação assinada e **GitHub Release** (binários + `SHA256SUMS` + `latest.json` +
   `latest.json.sig` + `upguard-agent-downloads-1.8.0.tar.gz`). O job `publish-downloads` é pulado (público).
4. No build-agent-1 (tem Docker, `gh` e rota para o Harbor), com `docker login harbor.shiftcloud.corp` feito:
   ```sh
   scripts/publish-downloads.sh --version 1.8.0 --from-release --push --kustomize
   git add kustomize/deployment.yaml && git commit -m "Deploy agent-downloads v1.8.0 [skip ci]" && git push
   ```
   O script confere manifesto/assinatura/SHA-256 antes de construir a imagem.
5. Aplicar o `argocd-application.yaml` e conferir o pod: `kubectl -n upguard get pods -l app=upguard-agent-downloads`.
6. Deploy do frontend com o PR `feat/download-agente` (location + instruções novas). Conferir ao vivo:
   `curl -s https://upguard.com.br/downloads/agent/latest.json` (JSON, não HTML) e
   `curl -sSL https://upguard.com.br/downloads/agent/install.sh | head`.
7. Os agentes v1.7.0 atualizam sozinhos para a v1.8.0 **pelo GitHub** em até 6 h (checagem no início do processo
   e a cada 6 h), ou na hora pelo botão "Atualizar agente" do portal. A v1.8.0 já consulta só o endereço novo.
8. Acompanhar (consulta só leitura, pelo pod do backend):
   ```sql
   SELECT agent_version, count(*) FILTER (WHERE last_seen_at > now() - interval '1 hour') AS ativos_1h, count(*)
   FROM agent_hosts GROUP BY 1 ORDER BY 1;
   ```
9. **Virar privado** quando todos os hosts ativos estiverem ≥ 1.8.0 (ou os que sobrarem tiverem sido
   reinstalados com o instalador novo). Depois disso o `release.yml` publica sozinho a partir da tag (job
   `publish-downloads` no self-hosted) e a GitHub Release deixa de ser criada.

## 5. Estado atual dos hosts (consulta só leitura em 08/11/2026 ~21h)

19 hosts cadastrados em 3 contas, todos Linux; **13 ativos** (última amostra < 1 h).

| Versão | Ativos | Parados | Observação |
|---|---|---|---|
| 1.7.0 | 12 | 3 (passbolt 23/09, ip-10-0-1-33 18/09, stg-minio 08/09) | atualizam sozinhos para a v1.8.0 enquanto o repo for público |
| 1.2.0 | 1 (`srv-neuro01`, servidor da NeuroClin na FWNet) | 3 (db-prod-01, web-prod-01, app-server-02, parados desde 21/07) | **a v1.2.0 não tem auto-update** (entrou na v1.3.0): reinstalar o `srv-neuro01` com o instalador novo |

Ativos: db-mongodb, k3s-master, nginx-dmz, k3s-tools, build-agent-1, db-mysql, pathoassist-vps (conta de outro
cliente), db-postgres, db-redis, db-tools, k3s-worker1, build-agent-2 (1.7.0) e srv-neuro01 (1.2.0).
Nenhum com atualização pendente nem pausado.

## 6. O que acontece com quem não atualizar antes de o repo virar privado

Código da v1.7.0 (`maybeUpdate`/`latestVersion`/`selfUpdate`): com o repo privado a API do GitHub responde 404,
`latestVersion` devolve erro e `maybeUpdate` **retorna em silêncio**; no "Atualizar agente" do portal o download
dá 404 e só aparece `falha ao auto-atualizar: download respondeu 404` no log. O laço de métricas não depende do
update: **o agente continua monitorando normalmente**, só fica parado na 1.7.0 até ser reinstalado
(`curl -sSL https://upguard.com.br/downloads/agent/install.sh | sudo bash -s -- --client-id … --client-secret …`).
Confirmado no teste local: o binário v1.7.0 oficial com o GitHub inalcançável e update forçado registrou a falha
e seguiu enviando métricas. A v1.2.0 nem tenta atualizar.

## 7. Rollback

- Endereço de download: voltar a tag em `kustomize/deployment.yaml` (o ArgoCD aplica). Agentes nunca fazem
  downgrade sozinhos (só atualizam para versão maior); "Atualizar agente" reinstala a versão publicada.
- Frontend: reverter o PR; os instaladores antigos do GitHub voltam a valer só enquanto o repo for público.
- Agente com problema: publicar uma versão maior corrigida (o auto-update leva em até 6 h).

## 8. Teste ponta a ponta (local, 08/11/2026)

Binários das 8 plataformas com uma chave de teste descartável, publicação assinada, imagem `agent-downloads`
rodando como no cluster (`--read-only`, `/tmp` tmpfs, usuário 101, sem capabilities), nginx do frontend
repassando `/downloads/agent/` e uma API falsa do UpGuard:

| Cenário | Resultado |
|---|---|
| agente 1.7.9 (código novo) → endereço novo | baixou 1.8.0, SHA-256 + assinatura conferidos, reexecutou e passou a reportar 1.8.0 (direto e pelo nginx do frontend) |
| binário adulterado (mesmo tamanho), manifesto íntegro | recusado ("SHA-256 do binário não confere — arquivo descartado"), normal e forçado; segue 1.7.9 |
| manifesto alterado para casar com o binário adulterado | agente com chave: recusado ("assinatura do manifesto não confere"); **agente sem chave: instalou** (motivo para gerar a chave antes da v1.8.0) |
| endereço novo fora do ar | só log, 4 envios de métricas em 50 s |
| v1.7.0 oficial com GitHub inalcançável + update forçado | `falha ao auto-atualizar`, 4 envios de métricas em 50 s |
| `install.sh` em contêiner Ubuntu | publicação íntegra: "SHA-256 conferido"; adulterada: "não confere — nada foi instalado" |
