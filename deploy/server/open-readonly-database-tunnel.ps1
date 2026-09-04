[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$Server,

    [ValidateRange(1, 65535)]
    [int]$LocalPort = 15432,

    [string]$KeyPath = '',

    [string]$KnownHostsPath = ''
)

$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($KeyPath)) {
    $KeyPath = Join-Path $PSScriptRoot '..\..\key\mail-suite.pem'
}
if ([string]::IsNullOrWhiteSpace($KnownHostsPath)) {
    $KnownHostsPath = Join-Path $PSScriptRoot '..\..\.tmp\ssh_known_hosts'
}

# resolveRequiredFile 将调用方路径固定为已存在的本地文件，避免 SSH 静默使用其他身份或主机记录。
function resolveRequiredFile {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Description
    )

    try {
        return (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    }
    catch {
        throw "$Description 不存在：$Path"
    }
}

$resolvedKeyPath = resolveRequiredFile -Path $KeyPath -Description 'SSH 私钥'
$resolvedKnownHostsPath = resolveRequiredFile -Path $KnownHostsPath -Description 'SSH known-hosts 文件'
$sshTarget = "root@$Server"
$commonArguments = @(
    '-o', 'BatchMode=yes',
    '-o', 'StrictHostKeyChecking=yes',
    '-o', "UserKnownHostsFile=$resolvedKnownHostsPath",
    '-i', $resolvedKeyPath
)

# databaseIp 是本次隧道使用的 PostgreSQL 容器内网地址，容器重建后会在下次启动时重新读取。
$databaseIp = (& ssh @commonArguments $sshTarget "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' mail-suite-test-postgres-1").Trim()
if ($LASTEXITCODE -ne 0) {
    throw '读取远端 PostgreSQL 容器地址失败'
}

$parsedAddress = $null
if (-not [Net.IPAddress]::TryParse($databaseIp, [ref]$parsedAddress) -or
    $parsedAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
    throw '远端 PostgreSQL 容器未返回有效 IPv4 地址'
}

Write-Host "隧道已准备：数据库客户端请连接 127.0.0.1:$LocalPort。使用完成后按 Ctrl+C 断开。"
& ssh @commonArguments `
    '-o' 'ExitOnForwardFailure=yes' `
    '-o' 'ServerAliveInterval=30' `
    '-o' 'ServerAliveCountMax=3' `
    '-N' `
    '-L' "${LocalPort}:${databaseIp}:5432" `
    $sshTarget
if ($LASTEXITCODE -ne 0) {
    throw 'PostgreSQL SSH 隧道异常退出'
}
