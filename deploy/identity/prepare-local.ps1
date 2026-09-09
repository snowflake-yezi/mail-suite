[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$KeycloakImage,
    [string]$RuntimeDirectory = ""
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# Assert-KeycloakImage 要求镜像同时固定可读版本 tag 和不可变 manifest digest。
function Assert-KeycloakImage {
    param([string]$Image)
    if ($Image -notmatch '^[a-z0-9][a-z0-9._/-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*@sha256:[0-9a-f]{64}$') {
        throw "Keycloak 镜像必须使用 repository:version@sha256:<64 位小写十六进制>"
    }
}

# New-HexSecret 生成只含十六进制字符的运行时密码，避免 env 文件转义歧义。
function New-HexSecret {
    param([int]$Bytes = 32)
    $buffer = [byte[]]::new($Bytes)
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $generator.GetBytes($buffer)
    }
    finally {
        $generator.Dispose()
    }
    return (($buffer | ForEach-Object { $_.ToString("x2") }) -join "")
}

# New-Base64Secret 生成 Go 运行配置要求的标准 Base64 随机值。
function New-Base64Secret {
    $buffer = [byte[]]::new(32)
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $generator.GetBytes($buffer)
    }
    finally {
        $generator.Dispose()
    }
    return [Convert]::ToBase64String($buffer)
}

# New-Base32Secret 生成兼容 TOTP authenticator 的 160 bit Base32 秘密。
function New-Base32Secret {
    $alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
    $buffer = [byte[]]::new(32)
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $generator.GetBytes($buffer)
    }
    finally {
        $generator.Dispose()
    }
    return (($buffer | ForEach-Object { $alphabet[[int]($_ -band 31)] }) -join "")
}

# Protect-PrivatePath 将秘密文件限制为当前操作系统账号可读写。
function Protect-PrivatePath {
    param([string]$Path)
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        $currentIdentity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
        & icacls.exe $Path /inheritance:r /grant:r "${currentIdentity}:(F)" | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "无法限制临时身份文件权限"
        }
        return
    }
    & chmod 600 $Path
    if ($LASTEXITCODE -ne 0) {
        throw "无法限制临时身份文件权限"
    }
}

# ConvertTo-Pem 将 DER 字节编码为兼容 Nginx、Keycloak 与 Go 的 ASCII PEM。
function ConvertTo-Pem {
    param(
        [string]$Label,
        [byte[]]$Bytes
    )
    $body = [Convert]::ToBase64String($Bytes, [Base64FormattingOptions]::InsertLineBreaks)
    return "-----BEGIN $Label-----`r`n$body`r`n-----END $Label-----`r`n"
}

# New-LocalTLSMaterial 为一个本地 hostname 生成独立的短期证书和私钥。
function New-LocalTLSMaterial {
    param(
        [string]$Hostname,
        [IO.DirectoryInfo]$OutputDirectory
    )
    $rsa = [Security.Cryptography.RSA]::Create(3072)
    try {
        $certificateRequest = [Security.Cryptography.X509Certificates.CertificateRequest]::new(
            "CN=$Hostname",
            $rsa,
            [Security.Cryptography.HashAlgorithmName]::SHA256,
            [Security.Cryptography.RSASignaturePadding]::Pkcs1
        )
        $san = [Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
        $san.AddDnsName($Hostname)
        $certificateRequest.CertificateExtensions.Add($san.Build())
        $certificateRequest.CertificateExtensions.Add(
            [Security.Cryptography.X509Certificates.X509BasicConstraintsExtension]::new($false, $false, 0, $true)
        )
        $certificateRequest.CertificateExtensions.Add(
            [Security.Cryptography.X509Certificates.X509KeyUsageExtension]::new(
                [Security.Cryptography.X509Certificates.X509KeyUsageFlags]::DigitalSignature,
                $true
            )
        )
        $certificate = $certificateRequest.CreateSelfSigned(
            [DateTimeOffset]::UtcNow.AddMinutes(-5),
            [DateTimeOffset]::UtcNow.AddDays(7)
        )
        try {
            $certificatePem = ConvertTo-Pem -Label "CERTIFICATE" -Bytes $certificate.Export(
                [Security.Cryptography.X509Certificates.X509ContentType]::Cert
            )
            if ($rsa -is [Security.Cryptography.RSACng]) {
                $privateKeyBytes = $rsa.Key.Export([Security.Cryptography.CngKeyBlobFormat]::Pkcs8PrivateBlob)
            }
            else {
                $privateKeyBytes = $rsa.ExportPkcs8PrivateKey()
            }
            $privateKeyPem = ConvertTo-Pem -Label "PRIVATE KEY" -Bytes $privateKeyBytes
        }
        finally {
            $certificate.Dispose()
        }
    }
    finally {
        $rsa.Dispose()
    }

    $certificatePath = Join-Path $OutputDirectory.FullName "server.crt"
    $caPath = Join-Path $OutputDirectory.FullName "ca.crt"
    $privateKeyPath = Join-Path $OutputDirectory.FullName "server.key"
    [IO.File]::WriteAllText($certificatePath, $certificatePem, [Text.Encoding]::ASCII)
    [IO.File]::WriteAllText($caPath, $certificatePem, [Text.Encoding]::ASCII)
    [IO.File]::WriteAllText($privateKeyPath, $privateKeyPem, [Text.Encoding]::ASCII)
    return [ordered]@{
        CertificatePath = $certificatePath
        CAPath = $caPath
        PrivateKeyPath = $privateKeyPath
    }
}

Assert-KeycloakImage -Image $KeycloakImage
$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$temporaryRoot = [IO.Path]::GetFullPath((Join-Path $repositoryRoot ".tmp"))
if ([string]::IsNullOrWhiteSpace($RuntimeDirectory)) {
    $RuntimeDirectory = Join-Path $temporaryRoot "identity"
}
$runtimeRoot = [IO.Path]::GetFullPath($RuntimeDirectory)
$temporaryPrefix = $temporaryRoot.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $runtimeRoot.StartsWith($temporaryPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "身份运行目录必须位于仓库 .tmp 下"
}
if ((Test-Path -LiteralPath $runtimeRoot) -and (Get-ChildItem -LiteralPath $runtimeRoot -Force | Select-Object -First 1)) {
    throw "身份运行目录已包含文件；请先显式停止并清理上一轮环境"
}

$tlsDirectory = New-Item -ItemType Directory -Force -Path (Join-Path $runtimeRoot "tls")
$keycloakTLSDirectory = New-Item -ItemType Directory -Force -Path (Join-Path $tlsDirectory.FullName "keycloak")
$webTLSDirectory = New-Item -ItemType Directory -Force -Path (Join-Path $tlsDirectory.FullName "web")
$realmDirectory = New-Item -ItemType Directory -Force -Path (Join-Path $runtimeRoot "realm")

$keycloakTLS = New-LocalTLSMaterial -Hostname "idp.127-0-0-1.sslip.io" -OutputDirectory $keycloakTLSDirectory
$webTLS = New-LocalTLSMaterial -Hostname "mail.127-0-0-1.sslip.io" -OutputDirectory $webTLSDirectory

$subjects = [ordered]@{
    mailbox = [Guid]::NewGuid().ToString()
    administrator = [Guid]::NewGuid().ToString()
    unmapped = [Guid]::NewGuid().ToString()
    suspended = [Guid]::NewGuid().ToString()
    mfaInsufficient = [Guid]::NewGuid().ToString()
}
$clientSecret = New-HexSecret
$userPassword = New-HexSecret

# New-TestUser 构造只存在于临时 realm 的假身份，ID 即稳定 OIDC subject。
function New-TestUser {
    param(
        [string]$ID,
        [string]$Username
    )
    return [ordered]@{
        id = $ID
        username = $Username
        enabled = $true
        emailVerified = $true
        email = "${Username}@mail-suite.example.test"
        credentials = @(
            [ordered]@{ type = "password"; value = $userPassword; temporary = $false }
        )
        requiredActions = @()
    }
}

# New-OTPCredential 构造 Keycloak 导入格式的真实 TOTP 凭据。
function New-OTPCredential {
    param([string]$Secret)
    return [ordered]@{
        type = "otp"
        userLabel = "Mail Suite local TOTP"
        secretData = ([ordered]@{ value = $Secret } | ConvertTo-Json -Compress)
        credentialData = ([ordered]@{
            subType = "totp"
            digits = 6
            counter = 0
            period = 30
            algorithm = "HmacSHA1"
        } | ConvertTo-Json -Compress)
    }
}

$administratorOTPSecret = New-Base32Secret
$administratorUser = New-TestUser -ID $subjects.administrator -Username "administrator"
$administratorUser.credentials += (New-OTPCredential -Secret $administratorOTPSecret)

$realm = [ordered]@{
    realm = "mail-suite-local"
    enabled = $true
    sslRequired = "external"
    registrationAllowed = $false
    resetPasswordAllowed = $false
    rememberMe = $false
    bruteForceProtected = $true
    otpPolicyType = "totp"
    otpPolicyAlgorithm = "HmacSHA1"
    otpPolicyInitialCounter = 0
    otpPolicyDigits = 6
    otpPolicyLookAheadWindow = 1
    otpPolicyPeriod = 30
    otpPolicyCodeReusable = $false
    clients = @(
        [ordered]@{
            clientId = "mail-suite-local"
            name = "Mail Suite local integration"
            enabled = $true
            protocol = "openid-connect"
            clientAuthenticatorType = "client-secret"
            secret = $clientSecret
            publicClient = $false
            standardFlowEnabled = $true
            directAccessGrantsEnabled = $false
            serviceAccountsEnabled = $false
            redirectUris = @("https://mail.127-0-0-1.sslip.io:18444/api/v1/auth/callback")
            webOrigins = @("https://mail.127-0-0-1.sslip.io:18444")
            attributes = [ordered]@{
                "post.logout.redirect.uris" = "https://mail.127-0-0-1.sslip.io:18444/"
            }
            protocolMappers = @(
                [ordered]@{
                    name = "mail-suite-amr"
                    protocol = "openid-connect"
                    protocolMapper = "oidc-amr-mapper"
                    consentRequired = $false
                    config = [ordered]@{
                        "id.token.claim" = "true"
                        "access.token.claim" = "false"
                    }
                }
            )
        }
    )
    users = @(
        (New-TestUser -ID $subjects.mailbox -Username "mailbox"),
        $administratorUser,
        (New-TestUser -ID $subjects.unmapped -Username "unmapped"),
        (New-TestUser -ID $subjects.suspended -Username "suspended"),
        (New-TestUser -ID $subjects.mfaInsufficient -Username "mfa-insufficient")
    )
}

$manifest = [ordered]@{
    schema_version = 1
    oidc_issuer = "https://idp.127-0-0-1.sslip.io:18443/realms/mail-suite-local"
    tenant = [ordered]@{
        id = [Guid]::NewGuid().ToString()
        name = "OIDC 本地集成测试租户"
    }
    domain = [ordered]@{
        id = [Guid]::NewGuid().ToString()
        name = "mail-suite.example.test"
    }
    mailbox = [ordered]@{
        principal_id = [Guid]::NewGuid().ToString()
        subject = $subjects.mailbox
        mailbox_id = [Guid]::NewGuid().ToString()
        local_part = "mailbox"
        display_name = "Mailbox Fixture"
    }
    administrator = [ordered]@{
        principal_id = [Guid]::NewGuid().ToString()
        subject = $subjects.administrator
        display_name = "Administrator Fixture"
    }
    suspended = [ordered]@{
        principal_id = [Guid]::NewGuid().ToString()
        subject = $subjects.suspended
        mailbox_id = [Guid]::NewGuid().ToString()
        local_part = "suspended"
        display_name = "Suspended Fixture"
    }
    mfa_insufficient = [ordered]@{
        principal_id = [Guid]::NewGuid().ToString()
        subject = $subjects.mfaInsufficient
        display_name = "MFA Insufficient Fixture"
    }
    unmapped_subject = $subjects.unmapped
}

$realmPath = Join-Path $realmDirectory.FullName "mail-suite-local-realm.json"
$manifestPath = Join-Path $runtimeRoot "test-identity.json"
[IO.File]::WriteAllText($realmPath, ($realm | ConvertTo-Json -Depth 20), [Text.UTF8Encoding]::new($false))
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 20), [Text.UTF8Encoding]::new($false))

$runtimePathForCompose = $runtimeRoot.Replace([IO.Path]::DirectorySeparatorChar, '/')
$runtimeEnvironment = @(
    "COMPOSE_PROJECT_NAME=mail-suite-identity-$(([Guid]::NewGuid().ToString('N')).Substring(0, 12))",
    "MAIL_SUITE_KEYCLOAK_IMAGE=$KeycloakImage",
    "MAIL_SUITE_IDENTITY_RUNTIME_DIR=$runtimePathForCompose",
    "MAIL_SUITE_CONTROL_DB_PASSWORD=$(New-HexSecret)",
    "MAIL_SUITE_KEYCLOAK_DB_PASSWORD=$(New-HexSecret)",
    "MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD=$(New-HexSecret)",
    "MAIL_SUITE_TEST_USER_PASSWORD=$userPassword",
    "MAIL_SUITE_TEST_ADMIN_OTP_SECRET=$administratorOTPSecret",
    "MAIL_SUITE_OIDC_CLIENT_SECRET=$clientSecret",
    "MAIL_SUITE_AUTH_SECRET_PEPPER=$(New-Base64Secret)",
    "MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY=$(New-Base64Secret)"
)
$runtimeEnvironmentPath = Join-Path $runtimeRoot "runtime.env"
[IO.File]::WriteAllLines($runtimeEnvironmentPath, $runtimeEnvironment, [Text.UTF8Encoding]::new($false))

@(
    $keycloakTLS.CertificatePath,
    $keycloakTLS.CAPath,
    $keycloakTLS.PrivateKeyPath,
    $webTLS.CertificatePath,
    $webTLS.CAPath,
    $webTLS.PrivateKeyPath,
    $realmPath,
    $manifestPath,
    $runtimeEnvironmentPath
) |
    ForEach-Object { Protect-PrivatePath -Path $_ }

Write-Output "本地身份运行材料已生成到 .tmp/identity；未启动容器或写入数据库"
