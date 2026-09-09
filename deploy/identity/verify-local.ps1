[CmdletBinding()]
param(
    [string]$RuntimeEnvironment = "",
    [switch]$Running
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# Read-EnvironmentFile 解析本脚本生成的简单 KEY=VALUE 文件，不输出值。
function Read-EnvironmentFile {
    param([string]$Path)
    $values = @{}
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        $separator = $line.IndexOf('=')
        if ($separator -le 0) {
            throw "身份运行环境文件格式无效"
        }
        $values[$line.Substring(0, $separator)] = $line.Substring($separator + 1)
    }
    return $values
}

# Invoke-PinnedTLSGet 只信任本轮临时证书并读取本地 HTTPS JSON。
function Invoke-PinnedTLSGet {
    param(
        [string]$URI,
        [string]$CertificatePath
    )
    $expectedCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($CertificatePath)
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.ServerCertificateCustomValidationCallback = {
        param($request, $certificate, $chain, $errors)
        return $null -ne $certificate -and $certificate.GetCertHashString() -eq $expectedCertificate.GetCertHashString()
    }
    $client = [Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(10)
    try {
        $response = $client.GetAsync($URI).GetAwaiter().GetResult()
        if (-not $response.IsSuccessStatusCode) {
            throw "本地 HTTPS 验证返回非成功状态"
        }
        return $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    }
    finally {
        $client.Dispose()
        $handler.Dispose()
        $expectedCertificate.Dispose()
    }
}

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
if ([string]::IsNullOrWhiteSpace($RuntimeEnvironment)) {
    $RuntimeEnvironment = Join-Path $repositoryRoot ".tmp/identity/runtime.env"
}
$runtimeEnvironmentPath = [IO.Path]::GetFullPath($RuntimeEnvironment)
$expectedPrefix = [IO.Path]::GetFullPath((Join-Path $repositoryRoot ".tmp")).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $runtimeEnvironmentPath.StartsWith($expectedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "身份运行环境文件必须位于仓库 .tmp 下"
}
if (-not (Test-Path -LiteralPath $runtimeEnvironmentPath -PathType Leaf)) {
    throw "身份运行环境文件不存在"
}
$environment = Read-EnvironmentFile -Path $runtimeEnvironmentPath
$requiredKeys = @(
    "COMPOSE_PROJECT_NAME",
    "MAIL_SUITE_KEYCLOAK_IMAGE",
    "MAIL_SUITE_IDENTITY_RUNTIME_DIR",
    "MAIL_SUITE_CONTROL_DB_PASSWORD",
    "MAIL_SUITE_KEYCLOAK_DB_PASSWORD",
    "MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD",
    "MAIL_SUITE_TEST_USER_PASSWORD",
    "MAIL_SUITE_TEST_ADMIN_OTP_SECRET",
    "MAIL_SUITE_OIDC_CLIENT_SECRET",
    "MAIL_SUITE_AUTH_SECRET_PEPPER",
    "MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY"
)
foreach ($key in $requiredKeys) {
    if (-not $environment.ContainsKey($key) -or [string]::IsNullOrWhiteSpace($environment[$key])) {
        throw "身份运行环境缺少必需值"
    }
}
if ($environment["MAIL_SUITE_KEYCLOAK_IMAGE"] -notmatch '^[a-z0-9][a-z0-9._/-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*@sha256:[0-9a-f]{64}$') {
    throw "Keycloak 镜像没有同时固定版本和 digest"
}

$runtimeRoot = [IO.Path]::GetFullPath($environment["MAIL_SUITE_IDENTITY_RUNTIME_DIR"])
if (-not $runtimeRoot.StartsWith($expectedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "身份运行目录越出仓库 .tmp"
}
$manifestPath = Join-Path $runtimeRoot "test-identity.json"
$realmPath = Join-Path $runtimeRoot "realm/mail-suite-local-realm.json"
$keycloakCertificatePath = Join-Path $runtimeRoot "tls/keycloak/server.crt"
$keycloakCAPath = Join-Path $runtimeRoot "tls/keycloak/ca.crt"
$keycloakPrivateKeyPath = Join-Path $runtimeRoot "tls/keycloak/server.key"
$webCertificatePath = Join-Path $runtimeRoot "tls/web/server.crt"
$webPrivateKeyPath = Join-Path $runtimeRoot "tls/web/server.key"
@(
    $manifestPath,
    $realmPath,
    $keycloakCertificatePath,
    $keycloakCAPath,
    $keycloakPrivateKeyPath,
    $webCertificatePath,
    $webPrivateKeyPath
) | ForEach-Object {
    if (-not (Test-Path -LiteralPath $_ -PathType Leaf)) {
        throw "本地身份运行材料不完整"
    }
}

$manifest = Get-Content -LiteralPath $manifestPath -Encoding utf8 -Raw | ConvertFrom-Json
$realm = Get-Content -LiteralPath $realmPath -Encoding utf8 -Raw | ConvertFrom-Json
$realmSubjects = @($realm.users | ForEach-Object { $_.id })
$mappedSubjects = @(
    $manifest.mailbox.subject,
    $manifest.administrator.subject,
    $manifest.suspended.subject,
    $manifest.mfa_insufficient.subject,
    $manifest.unmapped_subject
)
if ($manifest.oidc_issuer -ne "https://idp.127-0-0-1.sslip.io:18443/realms/mail-suite-local" -or
    @($mappedSubjects | Where-Object { $_ -notin $realmSubjects }).Count -ne 0) {
    throw "受控 manifest 与临时 Keycloak realm 不一致"
}

$client = @($realm.clients | Where-Object { $_.clientId -eq "mail-suite-local" })
$administrator = @($realm.users | Where-Object { $_.id -eq $manifest.administrator.subject })
$mfaInsufficient = @($realm.users | Where-Object { $_.id -eq $manifest.mfa_insufficient.subject })
if ($client.Count -ne 1 -or $administrator.Count -ne 1 -or $mfaInsufficient.Count -ne 1) {
    throw "Keycloak client 或 MFA 测试身份配置无效"
}
$amrMapper = @($client[0].protocolMappers | Where-Object { $_.protocolMapper -eq "oidc-amr-mapper" })
$staticMfaMapper = @($client[0].protocolMappers | Where-Object {
    $_.protocolMapper -eq "oidc-usermodel-attribute-mapper" -and
    ($_.config."claim.name" -eq "acr" -or $_.config."claim.name" -eq "amr")
})
$administratorOTP = @($administrator[0].credentials | Where-Object { $_.type -eq "otp" })
$mfaInsufficientOTP = @($mfaInsufficient[0].credentials | Where-Object { $_.type -eq "otp" })
if ($client.Count -ne 1 -or
    @($client[0].redirectUris).Count -ne 1 -or
    $client[0].redirectUris -ne "https://mail.127-0-0-1.sslip.io:18444/api/v1/auth/callback" -or
    $client[0].attributes."post.logout.redirect.uris" -ne "https://mail.127-0-0-1.sslip.io:18444/" -or
    $amrMapper.Count -ne 1 -or
    $staticMfaMapper.Count -ne 0 -or
    $administratorOTP.Count -ne 1 -or
    $mfaInsufficientOTP.Count -ne 0) {
    throw "Keycloak client、跨 hostname 或真实 OTP fixture 配置无效"
}
$otpSecretData = $administratorOTP[0].secretData | ConvertFrom-Json
$otpCredentialData = $administratorOTP[0].credentialData | ConvertFrom-Json
if ($otpSecretData.value -ne $environment["MAIL_SUITE_TEST_ADMIN_OTP_SECRET"] -or
    $otpCredentialData.subType -ne "totp" -or
    $otpCredentialData.algorithm -ne "HmacSHA1" -or
    $otpCredentialData.digits -ne 6 -or
    $otpCredentialData.period -ne 30 -or
    $realm.otpPolicyType -ne "totp" -or
    $realm.otpPolicyAlgorithm -ne "HmacSHA1") {
    throw "管理员 TOTP 凭据与受限运行秘密不一致"
}

$keycloakCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($keycloakCertificatePath)
$webCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($webCertificatePath)
try {
    if ($keycloakCertificate.NotAfter.ToUniversalTime() -le [DateTime]::UtcNow -or
        $keycloakCertificate.NotAfter.ToUniversalTime() -gt [DateTime]::UtcNow.AddDays(8) -or
        $webCertificate.NotAfter.ToUniversalTime() -le [DateTime]::UtcNow -or
        $webCertificate.NotAfter.ToUniversalTime() -gt [DateTime]::UtcNow.AddDays(8)) {
        throw "本地 TLS 证书有效期不满足短期约束"
    }
    if ($keycloakCertificate.GetPublicKeyString() -eq $webCertificate.GetPublicKeyString()) {
        throw "Keycloak 与 Mail Suite 不得复用 TLS 私钥"
    }
    if ($keycloakCertificate.GetNameInfo(
            [Security.Cryptography.X509Certificates.X509NameType]::DnsName,
            $false
        ) -ne "idp.127-0-0-1.sslip.io" -or
        $webCertificate.GetNameInfo(
            [Security.Cryptography.X509Certificates.X509NameType]::DnsName,
            $false
        ) -ne "mail.127-0-0-1.sslip.io") {
        throw "本地 TLS 证书 hostname 不满足 Cookie 隔离边界"
    }
}
finally {
    $keycloakCertificate.Dispose()
    $webCertificate.Dispose()
}

$composeFile = Join-Path $PSScriptRoot "compose.yaml"
$composeConfigurationJSON = & docker compose --env-file $runtimeEnvironmentPath -f $composeFile --profile tools config --format json
if ($LASTEXITCODE -ne 0) {
    throw "本地身份 Compose 配置无效"
}
$composeConfiguration = ($composeConfigurationJSON | Out-String) | ConvertFrom-Json
$apiVolumeTargets = @($composeConfiguration.services.api.volumes | ForEach-Object { $_.target })
$keycloakVolumeTargets = @($composeConfiguration.services.keycloak.volumes | ForEach-Object { $_.target })
$webVolumeTargets = @($composeConfiguration.services.web.volumes | ForEach-Object { $_.target })
$keycloakAliases = @($composeConfiguration.services.keycloak.networks."identity-backend".aliases)
$issuerURI = [Uri]$composeConfiguration.services.api.environment.MAIL_SUITE_OIDC_ISSUER
$webOriginURI = [Uri]$composeConfiguration.services.api.environment.MAIL_SUITE_AUTH_TRUSTED_ORIGIN
$postLogoutURI = [Uri]$composeConfiguration.services.api.environment.MAIL_SUITE_OIDC_POST_LOGOUT_REDIRECT_URI
if ($issuerURI.Host -eq $webOriginURI.Host -or
    $postLogoutURI.AbsoluteUri -ne "https://mail.127-0-0-1.sslip.io:18444/" -or
    $postLogoutURI.GetLeftPart([UriPartial]::Authority) -ne $webOriginURI.AbsoluteUri.TrimEnd('/') -or
    $apiVolumeTargets.Count -ne 1 -or
    $apiVolumeTargets[0] -ne "/run/identity-idp-trust" -or
    "/run/identity-keycloak-tls" -notin $keycloakVolumeTargets -or
    "/run/identity-web-tls" -in $keycloakVolumeTargets -or
    "/run/identity-web-tls" -notin $webVolumeTargets -or
    "/run/identity-keycloak-tls" -in $webVolumeTargets -or
    "idp.127-0-0-1.sslip.io" -notin $keycloakAliases) {
    throw "Compose hostname、issuer 网络或证书最小挂载边界无效"
}

$previousDatabaseURL = $env:MAIL_SUITE_DATABASE_URL
try {
    $env:MAIL_SUITE_DATABASE_URL = "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable"
    & go run ./src/backend/cmd/identity-bootstrap --check --manifest $manifestPath
    if ($LASTEXITCODE -ne 0) {
        throw "受控测试身份 manifest 校验失败"
    }
}
finally {
    $env:MAIL_SUITE_DATABASE_URL = $previousDatabaseURL
}

if ($Running) {
    $discovery = Invoke-PinnedTLSGet -URI "https://idp.127-0-0-1.sslip.io:18443/realms/mail-suite-local/.well-known/openid-configuration" -CertificatePath $keycloakCertificatePath | ConvertFrom-Json
    if ($discovery.issuer -ne $manifest.oidc_issuer -or
        $discovery.end_session_endpoint -ne "https://idp.127-0-0-1.sslip.io:18443/realms/mail-suite-local/protocol/openid-connect/logout") {
        throw "Keycloak discovery issuer 或 end-session endpoint 与受控配置不一致"
    }
    $session = Invoke-PinnedTLSGet -URI "https://mail.127-0-0-1.sslip.io:18444/api/v1/session" -CertificatePath $webCertificatePath | ConvertFrom-Json
    if ($session.authenticated -ne $false) {
        throw "全新浏览器请求必须返回匿名会话"
    }

    $containerDiscoveryJSON = & docker compose --env-file $runtimeEnvironmentPath -f $composeFile --profile tools run --rm --no-deps -T issuer-probe
    if ($LASTEXITCODE -ne 0) {
        throw "API 网络无法使用相同 HTTPS issuer"
    }
    $containerDiscovery = ($containerDiscoveryJSON | Out-String) | ConvertFrom-Json
    if ($containerDiscovery.issuer -ne $manifest.oidc_issuer) {
        throw "容器内 discovery issuer 与浏览器 issuer 不一致"
    }

    & docker compose --env-file $runtimeEnvironmentPath -f $composeFile exec -T keycloak bash -ec "test -r /run/identity-keycloak-tls/server.key && test ! -e /run/identity-web-tls/server.key"
    if ($LASTEXITCODE -ne 0) {
        throw "Keycloak 证书挂载权限或隔离无效"
    }
    & docker compose --env-file $runtimeEnvironmentPath -f $composeFile exec -T web sh -ec "test -r /run/identity-web-tls/server.key && test ! -e /run/identity-keycloak-tls/server.key"
    if ($LASTEXITCODE -ne 0) {
        throw "Web 证书挂载权限或隔离无效"
    }
}

Write-Output "本地身份配置、临时 TLS 和受控 manifest 验证通过；未输出任何秘密"
