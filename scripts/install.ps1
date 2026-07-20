# Instalador do UpGuard Agent para Windows — 100% PowerShell (sem curl/bash).
#
# Uso (one-liner, PowerShell como Administrador):
#   iwr -useb https://raw.githubusercontent.com/devshiftlabs/upguard-agent/main/scripts/install.ps1 | iex
#   Install-UpGuardAgent -ClientId agt_xxx -ClientSecret sk_agt_xxx
#
# Ou baixando o arquivo:
#   .\install.ps1  ; Install-UpGuardAgent -ClientId agt_xxx -ClientSecret sk_agt_xxx

function Install-UpGuardAgent {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string]$ClientId,
    [Parameter(Mandatory = $true)][string]$ClientSecret,
    [string]$Server = "https://api.upguard.com.br",
    [int]$Interval = 60,
    [string]$BaseUrl = "https://github.com/devshiftlabs/upguard-agent/releases/latest/download"
  )

  $ErrorActionPreference = "Stop"
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocol]::Tls12

  # Exige Administrador (para criar o serviço).
  $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
  if (-not $isAdmin) { throw "Execute o PowerShell como Administrador." }

  # Detecta a arquitetura (amd64 ou arm64).
  $arch = "amd64"
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { $arch = "arm64" }
  $binUrl = "$BaseUrl/upguard-agent-windows-$arch.exe"

  $installDir = Join-Path $env:ProgramFiles "UpGuardAgent"
  $binPath = Join-Path $installDir "upguard-agent.exe"
  New-Item -ItemType Directory -Force -Path $installDir | Out-Null

  Write-Host "Baixando $binUrl ..."
  Invoke-WebRequest -Uri $binUrl -OutFile $binPath -UseBasicParsing

  # Variáveis de ambiente da máquina (lidas pelo serviço).
  [Environment]::SetEnvironmentVariable("UPGUARD_CLIENT_ID", $ClientId, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_CLIENT_SECRET", $ClientSecret, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_SERVER_URL", $Server, "Machine")
  [Environment]::SetEnvironmentVariable("UPGUARD_INTERVAL", "$Interval", "Machine")

  # (Re)cria o serviço do Windows (inicia no boot, reinicia em falha).
  $existing = Get-Service -Name "UpGuardAgent" -ErrorAction SilentlyContinue
  if ($existing) {
    Stop-Service UpGuardAgent -ErrorAction SilentlyContinue
    sc.exe delete UpGuardAgent | Out-Null
    Start-Sleep -Seconds 2
  }
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
  foreach ($v in "UPGUARD_CLIENT_ID","UPGUARD_CLIENT_SECRET","UPGUARD_SERVER_URL","UPGUARD_INTERVAL") {
    [Environment]::SetEnvironmentVariable($v, $null, "Machine")
  }
  Write-Host "UpGuard Agent removido."
}
