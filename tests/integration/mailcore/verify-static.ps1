[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$verifyPath = Join-Path $repositoryRoot 'deploy\mail-core\verify.ps1'
$composePath = Join-Path $repositoryRoot 'deploy\mail-core\compose.yaml'
$fixturePath = Join-Path $PSScriptRoot 'fixtures\basic.eml'

# Assert-Contains 要求静态制品保留安全或协议验收所需的固定契约片段。
function Assert-Contains {
    param(
        [Parameter(Mandatory)]
        [string]$Text,
        [Parameter(Mandatory)]
        [string]$Pattern,
        [Parameter(Mandatory)]
        [string]$Message
    )

    if ($Text -notmatch $Pattern) {
        throw $Message
    }
}

$staticResult = & $verifyPath -StaticOnly | Out-String | ConvertFrom-Json
if (-not $staticResult.redaction) {
    throw 'Mail-core redaction regression did not run'
}
if ($staticResult.fixtureSha256 -notmatch '^[a-f0-9]{64}$') {
    throw 'Fixture canonicalization or SHA-256 calculation failed'
}

$verifySource = Get-Content -Raw -LiteralPath $verifyPath
if ($verifySource -match '(?m)-u\s+\$' -or $verifySource -match 'STALWART_PASSWORD=\$Password') {
    throw 'A credential is still passed through a curl or Docker command argument'
}
Assert-Contains -Text $verifySource -Pattern '\[switch\]\$Reset' -Message 'Explicit Reset switch is missing'
Assert-Contains -Text $verifySource -Pattern 'smtp250CrashDurability\s*=\s*''pending-fault-verification''' -Message 'SMTP 250 durability is overstated'
Assert-Contains -Text $verifySource -Pattern "'messageId'.*'from'.*'to'.*'textBody'.*'attachments'.*'bodyValues'" -Message 'Structured JMAP properties are incomplete'
Assert-Contains -Text $verifySource -Pattern 'RawSha256' -Message 'Raw JMAP blob hashing is missing'
Assert-Contains -Text $verifySource -Pattern "'get', 'Account'.*'id,name'" -Message 'Admin observed-state query is missing'
Assert-Contains -Text $verifySource -Pattern '\.AllowAutoRedirect\s*=\s*\$false' -Message 'Automatic JMAP redirects must remain disabled'
Assert-Contains -Text $verifySource -Pattern "(?s)PathAndQuery -eq '/\.well-known/jmap'.*status -eq 307.*location -eq '/jmap/session'" -Message 'The fixed JMAP discovery redirect guard is missing'
if ($verifySource -match 'isEnabled') {
    throw 'UserAccount must not use the Domain-only isEnabled field'
}
Assert-Contains -Text $verifySource -Pattern "'wrong-credential'" -Message 'Wrong-credential acceptance gate is not reported'
Assert-Contains -Text $verifySource -Pattern "'timeout'" -Message 'Timeout acceptance gate is not reported'

$compose = Get-Content -Raw -LiteralPath $composePath
if ([regex]::Matches($compose, '(?m)^\s+platform:\s+linux/amd64\s*$').Count -ne 2) {
    throw 'Both pinned images must declare linux/amd64'
}
foreach ($portLine in @(
    '127.0.0.1:${MAIL_CORE_SMTP_PORT:-20025}:25',
    '127.0.0.1:${MAIL_CORE_HTTPS_PORT:-28443}:443',
    '127.0.0.1:${MAIL_CORE_RECOVERY_PORT:-28080}:8080'
)) {
    if (-not $compose.Contains($portLine)) {
        throw "Compose loopback binding is missing: $portLine"
    }
}

$fixture = Get-Content -Raw -LiteralPath $fixturePath
Assert-Contains -Text $fixture -Pattern '(?m)^Message-ID: <mail-suite-poc-001@mail-suite\.test>$' -Message 'Fixture Message-ID is missing'
Assert-Contains -Text $fixture -Pattern '(?m)^Content-Disposition: attachment; filename="evidence\.txt"$' -Message 'Fixture attachment is missing'
Assert-Contains -Text $fixture -Pattern 'TWFpbCBTdWl0ZSBQb0MgYXR0YWNobWVudC4NCg==' -Message 'Fixture attachment content changed'

[pscustomobject]@{
    powershell = $PSVersionTable.PSVersion.ToString()
    redaction = $true
    commandArguments = 'secret-free'
    loopbackPorts = $true
    structuredFixture = $true
} | ConvertTo-Json
