# Instalador do UpGuard Agent para Windows — 100% PowerShell (sem curl/bash).
#
# Uso (one-liner, PowerShell como Administrador):
#   iwr -useb https://upguard.com.br/downloads/agent/install.ps1 | iex
#   Install-UpGuardAgent -ClientId agt_xxx -ClientSecret sk_agt_xxx
#
# Ou baixando o arquivo:
#   .\install.ps1  ; Install-UpGuardAgent -ClientId agt_xxx -ClientSecret sk_agt_xxx
#
# O binário só é instalado se o SHA-256 conferir com o SHA256SUMS publicado.

function Install-UpGuardAgent {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string]$ClientId,
    [Parameter(Mandatory = $true)][string]$ClientSecret,
    [string]$Server = "https://api.upguard.com.br",
    [int]$Interval = 60,
    [string]$BaseUrl = "https://upguard.com.br/downloads/agent/latest",
    # Endereço do auto-update do agente ("off" desliga); vazio = padrão do agente.
    [string]$UpdateUrl = ""
  )

  $ErrorActionPreference = "Stop"
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocol]::Tls12

  # Exige Administrador (para criar o serviço).
  $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
  if (-not $isAdmin) { throw "Execute o PowerShell como Administrador." }

  # Detecta a arquitetura (amd64 ou arm64).
  $arch = "amd64"
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { $arch = "arm64" }
  $binName = "upguard-agent-windows-$arch.exe"
  $binUrl = "$BaseUrl/$binName"

  $installDir = Join-Path $env:ProgramFiles "UpGuardAgent"
  $binPath = Join-Path $installDir "upguard-agent.exe"
  New-Item -ItemType Directory -Force -Path $installDir | Out-Null

  # Baixa para um temporário e confere o SHA-256 antes de tocar no binário atual.
  Write-Host "Baixando $binUrl ..."
  $sums = (Invoke-WebRequest -Uri "$BaseUrl/SHA256SUMS" -UseBasicParsing).Content
  if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
  $expected = $null
  foreach ($line in ($sums -split "`n")) {
    $parts = $line.Trim() -split '\s+'
    if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -eq $binName) { $expected = $parts[0]; break }
  }
  if (-not $expected) { throw "$binName não está no SHA256SUMS publicado." }
  $tmp = Join-Path $installDir "upguard-agent.exe.download"
  Invoke-WebRequest -Uri $binUrl -OutFile $tmp -UseBasicParsing
  $got = (Get-FileHash -Path $tmp -Algorithm SHA256).Hash
  if ($got -ne $expected) {
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    throw "SHA-256 do binário não confere (recebido $got, esperado $expected) — nada foi instalado."
  }
  Write-Host "SHA-256 conferido: $got"

  # Variáveis de ambiente da máquina (lidas pelo serviço).
  [Environment]::SetEnvironmentVariable("UPGUARD_CLIENT_ID", $ClientId, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_CLIENT_SECRET", $ClientSecret, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_SERVER_URL", $Server, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_INTERVAL", "$Interval", "Machine")
  if ($UpdateUrl) { [Environment]::SetEnvironmentVariable("UPGUARD_UPDATE_URL", $UpdateUrl, "Machine") }

  # (Re)cria o serviço do Windows (inicia no boot, reinicia em falha). O
  # binário só é trocado com o serviço parado (no Windows o .exe em uso fica travado).
  $existing = Get-Service -Name "UpGuardAgent" -ErrorAction SilentlyContinue
  if ($existing) {
    Stop-Service UpGuardAgent -ErrorAction SilentlyContinue
    sc.exe delete UpGuardAgent | Out-Null
    Start-Sleep -Seconds 2
  }
  Move-Item -Path $tmp -Destination $binPath -Force
  New-Service -Name "UpGuardAgent" -BinaryPathName "`"$binPath`"" `
    -DisplayName "UpGuard Monitoring Agent" -StartupType Automatic | Out-Null
  # Reinício automático em caso de falha.
  sc.exe failure UpGuardAgent reset= 86400 actions= restart/10000/restart/10000/restart/10000 | Out-Null
  Start-Service UpGuardAgent

  Write-Host "OK — serviço 'UpGuardAgent' instalado e iniciado (arch=$arch)."
  Write-Host "Status: Get-Service UpGuardAgent   |   Parar: Stop-Service UpGuardAgent"
}

function Uninstall-UpGuardAgent {
  $ErrorActionPreference = "SilentlyContinue"
  Stop-Service UpGuardAgent
  sc.exe delete UpGuardAgent | Out-Null
  Remove-Item (Join-Path $env:ProgramFiles "UpGuardAgent") -Recurse -Force
  foreach ($v in "UPGUARD_CLIENT_ID","UPGUARD_CLIENT_SECRET","UPGUARD_SERVER_URL","UPGUARD_INTERVAL","UPGUARD_UPDATE_URL") {
    [Environment]::SetEnvironmentVariable($v, $null, "Machine")
  }
  Write-Host "UpGuard Agent removido."
}
