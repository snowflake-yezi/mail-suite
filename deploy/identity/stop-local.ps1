[CmdletBinding()]
param(
    [string]$RuntimeEnvironment = "",
    [switch]$RemoveVolumes
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
if ([string]::IsNullOrWhiteSpace($RuntimeEnvironment)) {
    $RuntimeEnvironment = Join-Path $repositoryRoot ".tmp/identity/runtime.env"
}
$runtimeEnvironmentPath = [IO.Path]::GetFullPath($RuntimeEnvironment)
$expectedPrefix = [IO.Path]::GetFullPath((Join-Path $repositoryRoot ".tmp")).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $runtimeEnvironmentPath.StartsWith($expectedPrefix, [StringComparison]::OrdinalIgnoreCase) -or
    -not (Test-Path -LiteralPath $runtimeEnvironmentPath -PathType Leaf)) {
    throw "只允许停止由仓库 .tmp 运行文件标识的身份环境"
}

$arguments = @("compose", "--env-file", $runtimeEnvironmentPath, "-f", (Join-Path $PSScriptRoot "compose.yaml"), "down", "--remove-orphans")
if ($RemoveVolumes) {
    $arguments += "--volumes"
}
& docker @arguments
if ($LASTEXITCODE -ne 0) {
    throw "停止本地身份环境失败"
}

Write-Output $(if ($RemoveVolumes) { "本地身份容器和隔离数据卷已移除；.tmp 私密材料仍保留" } else { "本地身份容器已停止；隔离数据卷和 .tmp 私密材料仍保留" })
