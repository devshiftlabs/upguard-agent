# UpGuard Agent

Agente de monitoramento local (binário único, Go) que coleta métricas de
sistema — CPU, memória, disco, load, rede, uptime — e as envia para a sua conta
UpGuard, autenticado por **client-id / client-secret**.

- **Cross-platform**: Linux, macOS, Windows e FreeBSD/pfSense (amd64 + arm64).
- **Sem dependências** de runtime no host (binário estático).
- Roda como **serviço** (systemd / launchd / Windows Service / rc.d) e reinicia no boot.

## Instalação

Gere um par `client-id` / `client-secret` no painel do UpGuard (**Agentes → Nova
credencial**) e rode:

### Linux / macOS
```sh
curl -sSL https://.../install.sh | sudo bash -s -- \
  --client-id agt_xxx --client-secret sk_agt_xxx
```

### Windows (PowerShell como Administrador)
```powershell
iwr -useb https://.../install.ps1 | iex; `
  Install-UpGuardAgent -ClientId agt_xxx -ClientSecret sk_agt_xxx
```

### FreeBSD / pfSense (como root, shell)
O pfSense não traz `bash` — use `sh`. Se `curl` não existir, o instalador cai
para o `fetch` nativo:
```sh
fetch -o- https://.../install.sh | sh -s -- \
  --client-id agt_xxx --client-secret sk_agt_xxx
```
Instala um serviço **rc.d** (`upguard_agent`) supervisionado por `daemon(8)`
(reinicia em falha, inicia no boot). Logs: `tail -f /var/log/messages | grep upguard`.

> **Ressalva pfSense:** o script `/usr/local/etc/rc.d/upguard_agent.sh`, o binário
> e o `/etc/upguard-agent/agent.env` **não sobrevivem a um upgrade de firmware**
> do pfSense — reexecute o instalador após atualizar o pfSense.

## Configuração

O agente lê flags ou variáveis de ambiente:

| Flag | Env | Default |
|---|---|---|
| `--client-id` | `UPGUARD_CLIENT_ID` | — (obrigatório) |
| `--client-secret` | `UPGUARD_CLIENT_SECRET` | — (obrigatório) |
| `--server` | `UPGUARD_SERVER_URL` | `https://api.upguard.com.br` |
| `--interval` | `UPGUARD_INTERVAL` | `60` (segundos, mín. 10) |
| `--hostname` | `UPGUARD_HOSTNAME` | hostname do sistema |

## Build

Cross-compila para todos os alvos usando Docker (sem Go local):
```sh
./build.sh 1.0.0      # gera dist/upguard-agent-<os>-<arch>[.exe]
```

## Protocolo

`POST {server}/api/agent/metrics` com `Authorization: Basic base64(client_id:client_secret)`:
```json
{
  "host": { "hostname": "...", "os": "linux", "platform": "...", "agent_version": "1.0.0", "cpu_cores": 4, "mem_total_bytes": 0 },
  "metrics": { "cpu_percent": 0, "mem_percent": 0, "mem_used_bytes": 0, "disk_percent": 0, "disk_used_bytes": 0, "load1": 0, "load5": 0, "load15": 0, "net_rx_bytes": 0, "net_tx_bytes": 0, "uptime_seconds": 0 }
}
```
