[CmdletBinding()]
param([string]$RuntimeEnvironment = "")

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
if ([string]::IsNullOrWhiteSpace($RuntimeEnvironment)) {
    $RuntimeEnvironment = Join-Path $repositoryRoot ".tmp/identity/runtime.env"
}
$runtimeEnvironmentPath = [IO.Path]::GetFullPath($RuntimeEnvironment)
$composeFile = Join-Path $PSScriptRoot "compose.yaml"

# Invoke-Compose 执行固定 identity compose，并在首个失败步骤停止。
function Invoke-Compose {
    param([string[]]$Arguments)
    & docker compose --env-file $runtimeEnvironmentPath -f $composeFile @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "本地身份 Compose 步骤失败"
    }
}

& (Join-Path $PSScriptRoot "verify-local.ps1") -RuntimeEnvironment $runtimeEnvironmentPath
Invoke-Compose -Arguments @("up", "-d", "--wait", "control-postgres", "keycloak-postgres", "keycloak")
Invoke-Compose -Arguments @("--profile", "tools", "run", "--rm", "migrator", "--up")
Invoke-Compose -Arguments @("--profile", "tools", "run", "--rm", "identity-bootstrap", "--apply", "--manifest", "/run/identity/test-identity.json")
Invoke-Compose -Arguments @("up", "-d", "--build", "--wait", "api", "web")
& (Join-Path $PSScriptRoot "verify-local.ps1") -RuntimeEnvironment $runtimeEnvironmentPath -Running

Write-Output "本地身份环境已启动：https://mail.127-0-0-1.sslip.io:18444"
