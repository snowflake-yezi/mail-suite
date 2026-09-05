[CmdletBinding()]
param(
    [switch]$Keep,
    [switch]$Reset,
    [switch]$StaticOnly
)

$ErrorActionPreference = 'Stop'

$projectName = 'mail-suite-mail-core-poc'
$scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$repositoryRoot = (Resolve-Path (Join-Path $scriptRoot '..\..')).Path
$composePath = Join-Path $scriptRoot 'compose.yaml'
$bootstrapPath = Join-Path $scriptRoot 'config\bootstrap.json'
$fixturePath = Join-Path $repositoryRoot 'tests\integration\mailcore\fixtures\basic.eml'
$secretDirectory = Join-Path $repositoryRoot '.tmp\mail-core-poc-secrets'
$composeEnvironmentPath = Join-Path $secretDirectory 'compose.env'
$cliEnvironmentPath = Join-Path $secretDirectory 'cli.env'
$credentialsPath = Join-Path $secretDirectory 'credentials.env'
$cliImage = 'ghcr.io/stalwartlabs/cli:1.0.12@sha256:8831cc276c22e334bcf473fc7d28e21248d748c4bc1dac43c83738d3cbd3a9d5'
$mailCapability = 'urn:ietf:params:jmap:mail'
$expectedMessageId = 'mail-suite-poc-001@mail-suite.test'
$expectedFrom = 'sender@outside.test'
$expectedTo = 'receiver@mail-suite.test'
$expectedSubject = 'Mail Suite PoC'
$expectedTextBody = 'This message belongs only to the isolated Mail Suite Stalwart PoC.'
$expectedAttachmentName = 'evidence.txt'
$expectedAttachmentType = 'text/plain'
$expectedAttachmentBytes = [System.Text.Encoding]::ASCII.GetBytes("Mail Suite PoC attachment.`r`n")

# Get-ConfiguredPort 读取并校验验证脚本与 Compose 共用的回环高位端口。
function Get-ConfiguredPort {
    param(
        [Parameter(Mandatory)]
        [string]$Name,
        [Parameter(Mandatory)]
        [int]$Default
    )

    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) {
        return $Default
    }
    $port = 0
    if (-not [int]::TryParse($value, [ref]$port) -or $port -lt 1024 -or $port -gt 65535) {
        throw "$Name must be an integer between 1024 and 65535"
    }
    return $port
}

# Protect-SensitiveText 脱敏 JSON、HTTP Authorization、CLI 与环境变量常见秘密格式。
function Protect-SensitiveText {
    param(
        [AllowEmptyString()]
        [string]$Text
    )

    if ([string]::IsNullOrEmpty($Text)) {
        return $Text
    }
    $safe = [regex]::Replace(
        $Text,
        '(?i)("(?:password|secret|token|authorization)"\s*:\s*)"(?:\\.|[^"\\])*"',
        '$1"[redacted]"'
    )
    $safe = [regex]::Replace(
        $safe,
        '(?im)(\bAuthorization\s*:\s*)(?:Basic|Bearer)\s+[^\s,;]+',
        '$1[redacted]'
    )
    $safe = [regex]::Replace(
        $safe,
        '(?im)(\b[A-Za-z0-9_.-]*(?:password|secret|token)\b\s*[:=]\s*)"[^"\r\n]*"',
        '$1[redacted]'
    )
    return [regex]::Replace(
        $safe,
        '(?im)(\b[A-Za-z0-9_.-]*(?:password|secret|token)\b\s*[:=]\s*)[^\s,;}\r\n]+',
        '$1[redacted]'
    )
}

# Assert-RedactionRegression 防止异常文本再次泄漏 JSON、Basic/Bearer 或 CLI 秘密值。
function Assert-RedactionRegression {
    $sample = @'
{"secret":"json-secret","password": "json-password"}
Authorization: Basic QWxhZGRpbjpvcGVuLXNlc2FtZQ==
Authorization: Bearer bearer-token
secret: "cli-secret"
STALWART_PASSWORD=env-secret
'@
    $safe = Protect-SensitiveText -Text $sample
    foreach ($marker in @(
        'json-secret',
        'json-password',
        'QWxhZGRpbjpvcGVuLXNlc2FtZQ==',
        'bearer-token',
        'cli-secret',
        'env-secret'
    )) {
        if ($safe.Contains($marker)) {
            throw "Sensitive-output redaction regression for marker: $marker"
        }
    }
    if ($safe -notmatch '\[redacted\]') {
        throw 'Sensitive-output redaction did not emit a redaction marker'
    }
}

# Invoke-Docker 将 Docker 非零退出统一转换为脱敏后的 PowerShell 失败。
function Invoke-Docker {
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments,
        [string]$StandardInput
    )

    $ErrorActionPreference = 'Continue'
    if ($PSBoundParameters.ContainsKey('StandardInput')) {
        $output = $StandardInput | & docker @Arguments 2>&1
    } else {
        $output = & docker @Arguments 2>&1
    }
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        $safeOutput = Protect-SensitiveText -Text ($output -join "`n")
        $prefixLength = [Math]::Min(4, $Arguments.Count - 1)
        throw "Docker command failed: docker $($Arguments[0..$prefixLength] -join ' '); $safeOutput"
    }
    return $output
}

# Test-PocDockerState 只读判断固定 Compose project 是否已有容器、网络或卷。
function Test-PocDockerState {
    $commands = @(
        @('ps', '--all', '--quiet', '--filter', "label=com.docker.compose.project=$projectName"),
        @('network', 'ls', '--quiet', '--filter', "label=com.docker.compose.project=$projectName"),
        @('volume', 'ls', '--quiet', '--filter', "label=com.docker.compose.project=$projectName")
    )
    foreach ($arguments in $commands) {
        $ErrorActionPreference = 'Continue'
        $output = & docker @arguments 2>$null
        if ($LASTEXITCODE -ne 0) {
            throw 'Docker state inspection failed'
        }
        if (@($output | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) }).Count -gt 0) {
            return $true
        }
    }
    return $false
}

# Remove-SecretDirectory 仅删除脚本拥有的三个已知秘密文件和空目录。
function Remove-SecretDirectory {
    if (-not (Test-Path -LiteralPath $secretDirectory)) {
        return
    }
    foreach ($path in @($composeEnvironmentPath, $cliEnvironmentPath, $credentialsPath)) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path -Force
        }
    }
    $unexpected = @(Get-ChildItem -LiteralPath $secretDirectory -Force)
    if ($unexpected.Count -gt 0) {
        throw "Refusing to remove secret directory with unexpected files: $secretDirectory"
    }
    Remove-Item -LiteralPath $secretDirectory -Force
}

# New-RestrictedSecretDirectory 创建仅当前 Windows 身份可访问的 Git 忽略目录。
function New-RestrictedSecretDirectory {
    if (Test-Path -LiteralPath $secretDirectory) {
        throw "Restricted secret directory already exists; inspect it or run with -Reset: $secretDirectory"
    }
    $parent = Split-Path -Parent $secretDirectory
    $null = New-Item -ItemType Directory -Force -Path $parent
    $null = New-Item -ItemType Directory -Path $secretDirectory

    $currentSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
    $acl = Get-Acl -LiteralPath $secretDirectory
    $acl.SetAccessRuleProtection($true, $false)
    $inheritance = [System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor
        [System.Security.AccessControl.InheritanceFlags]::ObjectInherit
    $rule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $currentSid,
        [System.Security.AccessControl.FileSystemRights]::FullControl,
        $inheritance,
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow
    )
    $acl.SetAccessRule($rule)
    Set-Acl -LiteralPath $secretDirectory -AclObject $acl

    $effectiveAcl = Get-Acl -LiteralPath $secretDirectory
    $foreignAllows = @($effectiveAcl.Access | Where-Object {
        $_.AccessControlType -eq [System.Security.AccessControl.AccessControlType]::Allow -and
        $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value -ne $currentSid.Value
    })
    if ($foreignAllows.Count -gt 0) {
        throw 'Restricted secret directory retained an unexpected allow rule'
    }
}

# Assert-EnvironmentValue 防止换行把一个秘密扩展成额外环境变量。
function Assert-EnvironmentValue {
    param(
        [Parameter(Mandatory)]
        [string]$Name,
        [AllowEmptyString()]
        [string]$Value
    )

    if ($Value -match '[\r\n]') {
        throw "$Name contains a forbidden line break"
    }
}

# Write-RestrictedEnvironmentFile 在受限目录内写入无 BOM 的 Docker 环境文件。
function Write-RestrictedEnvironmentFile {
    param(
        [Parameter(Mandatory)]
        [string]$Path,
        [Parameter(Mandatory)]
        [System.Collections.IDictionary]$Values
    )

    $lines = foreach ($entry in $Values.GetEnumerator()) {
        Assert-EnvironmentValue -Name ([string]$entry.Key) -Value ([string]$entry.Value)
        "$($entry.Key)=$($entry.Value)"
    }
    $encoding = [System.Text.UTF8Encoding]::new($false)
    [System.IO.File]::WriteAllText($Path, (($lines -join "`n") + "`n"), $encoding)
}

# Write-ComposeEnvironment 同步验证客户端端口，并按阶段注入或清空 recovery 凭据。
function Write-ComposeEnvironment {
    param(
        [AllowEmptyString()]
        [string]$RecoveryAdmin
    )

    Write-RestrictedEnvironmentFile -Path $composeEnvironmentPath -Values ([ordered]@{
        MAIL_CORE_PUBLIC_URL = $publicUrl
        MAIL_CORE_SMTP_PORT = [string]$smtpPort
        MAIL_CORE_HTTPS_PORT = [string]$httpsPort
        MAIL_CORE_RECOVERY_PORT = [string]$recoveryPort
        MAIL_CORE_RECOVERY_ADMIN = $RecoveryAdmin
    })
}

# Invoke-StalwartCli 通过短生命周期容器和受限 env 文件调用固定版本管理客户端。
function Invoke-StalwartCli {
    param(
        [Parameter(Mandatory)]
        [string]$Url,
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [Parameter(Mandatory)]
        [string[]]$Arguments,
        [string]$StandardInput
    )

    Write-RestrictedEnvironmentFile -Path $cliEnvironmentPath -Values ([ordered]@{
        STALWART_URL = $Url
        STALWART_USER = $Username
        STALWART_PASSWORD = $Password
    })
    $dockerArguments = @(
        'run', '--rm', '-i',
        '--network', "${projectName}_default",
        '--env-file', $cliEnvironmentPath,
        $cliImage,
        '--no-color'
    ) + $Arguments
    try {
        if ($PSBoundParameters.ContainsKey('StandardInput')) {
            return Invoke-Docker -Arguments $dockerArguments -StandardInput $StandardInput
        }
        return Invoke-Docker -Arguments $dockerArguments
    } finally {
        Write-RestrictedEnvironmentFile -Path $cliEnvironmentPath -Values ([ordered]@{
            STALWART_URL = ''
            STALWART_USER = ''
            STALWART_PASSWORD = ''
        })
    }
}

# Get-StalwartAccountState 通过 Admin API 读取指定账户的当前观测状态。
function Get-StalwartAccountState {
    param(
        [Parameter(Mandatory)]
        [string]$AccountId,
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password
    )

    $output = Invoke-StalwartCli `
        -Url 'https://stalwart:443' `
        -Username $Username `
        -Password $Password `
        -Arguments @('--insecure', 'get', 'Account', $AccountId, '--fields', 'id,name', '--json')
    $json = $output | Where-Object {
        ([string]$_).TrimStart().StartsWith('{') -or ([string]$_).TrimStart().StartsWith('[')
    } | Select-Object -First 1
    if ([string]::IsNullOrWhiteSpace([string]$json)) {
        throw "Stalwart account get did not return JSON: account=$AccountId"
    }
    $account = $json | ConvertFrom-Json
    if ($account -is [Array]) {
        if ($account.Count -ne 1) {
            throw "Stalwart account get returned an unexpected item count: account=$AccountId count=$($account.Count)"
        }
        $account = $account[0]
    }
    if ([string]$account.id -ne $AccountId) {
        throw "Stalwart account get returned a different account: expected=$AccountId actual=$($account.id)"
    }
    return $account
}

# Assert-StalwartAccountState 核对 Admin 观测到的账户标识与名称。
function Assert-StalwartAccountState {
    param(
        [Parameter(Mandatory)]
        [string]$AccountId,
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password
    )

    $account = Get-StalwartAccountState `
        -AccountId $AccountId `
        -Username $Username `
        -Password $Password
    if ([string]::IsNullOrWhiteSpace([string]$account.name)) {
        throw "Stalwart account get returned an empty name: account=$AccountId"
    }
}

# Wait-HttpHealth 等待 Stalwart HTTP/HTTPS 健康端点进入可用状态。
function Wait-HttpHealth {
    param(
        [Parameter(Mandatory)]
        [string]$Url,
        [int]$TimeoutSeconds = 60
    )

    $ErrorActionPreference = 'Continue'
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        & curl.exe -kfsS --noproxy '*' --max-time 3 $Url *> $null
        if ($LASTEXITCODE -eq 0) {
            return
        }
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)
    throw "Stalwart health endpoint did not become ready: $Url"
}

# Read-SmtpResponse 读取 SMTP 单行或多行响应并返回终止行。
function Read-SmtpResponse {
    param(
        [Parameter(Mandatory)]
        [System.IO.StreamReader]$Reader
    )

    do {
        $line = $Reader.ReadLine()
        if ($null -eq $line) {
            throw 'SMTP server closed the connection unexpectedly'
        }
    } while ($line.Length -ge 4 -and $line[3] -eq '-')
    return $line
}

# Send-SmtpCommand 写入单个 SMTP 命令并返回服务端终止响应行。
function Send-SmtpCommand {
    param(
        [Parameter(Mandatory)]
        [System.IO.StreamWriter]$Writer,
        [Parameter(Mandatory)]
        [System.IO.StreamReader]$Reader,
        [Parameter(Mandatory)]
        [string]$Command
    )

    $Writer.WriteLine($Command)
    try {
        return Read-SmtpResponse -Reader $Reader
    } catch {
        throw "SMTP command did not complete: $Command"
    }
}

# Open-SmtpSession 创建受超时保护的本机 SMTP 会话并完成 EHLO。
function Open-SmtpSession {
    $client = [System.Net.Sockets.TcpClient]::new()
    try {
        $client.ReceiveTimeout = 30000
        $client.SendTimeout = 30000
        $client.Connect('127.0.0.1', $smtpPort)
        $stream = $client.GetStream()
        $reader = [System.IO.StreamReader]::new($stream, [System.Text.Encoding]::ASCII, $false, 1024, $true)
        $writer = [System.IO.StreamWriter]::new($stream, [System.Text.Encoding]::ASCII, 1024, $true)
        $writer.NewLine = "`r`n"
        $writer.AutoFlush = $true
        $greeting = Read-SmtpResponse -Reader $reader
        if ($greeting -notmatch '^220') {
            throw "Unexpected SMTP greeting: $greeting"
        }
        $ehlo = Send-SmtpCommand -Writer $writer -Reader $reader -Command 'EHLO poc.local'
        if ($ehlo -notmatch '^250') {
            throw "SMTP EHLO failed: $ehlo"
        }
        return [pscustomobject]@{ Client = $client; Stream = $stream; Reader = $reader; Writer = $writer }
    } catch {
        $client.Dispose()
        throw
    }
}

# Close-SmtpSession 有界关闭测试 SMTP 会话及其底层资源。
function Close-SmtpSession {
    param(
        [Parameter(Mandatory)]
        $Session
    )

    try {
        $Session.Writer.WriteLine('QUIT')
        $null = Read-SmtpResponse -Reader $Session.Reader
    } finally {
        $Session.Reader.Dispose()
        $Session.Writer.Dispose()
        $Session.Stream.Dispose()
        $Session.Client.Dispose()
    }
}

# Test-SmtpRecipient 验证指定收件人在 MAIL FROM 后得到的 RCPT 响应。
function Test-SmtpRecipient {
    param(
        [Parameter(Mandatory)]
        [string]$Recipient
    )

    $session = Open-SmtpSession
    try {
        $mailResponse = Send-SmtpCommand -Writer $session.Writer -Reader $session.Reader -Command 'MAIL FROM:<sender@outside.test>'
        if ($mailResponse -notmatch '^250') {
            throw "SMTP MAIL FROM failed: $mailResponse"
        }
        return Send-SmtpCommand -Writer $session.Writer -Reader $session.Reader -Command "RCPT TO:<$Recipient>"
    } finally {
        Close-SmtpSession -Session $session
    }
}

# Get-CanonicalFixtureBytes 将固定 EML 规范化为 SMTP DATA 使用的 CRLF 字节。
function Get-CanonicalFixtureBytes {
    $encoding = [System.Text.UTF8Encoding]::new($false)
    $text = [System.IO.File]::ReadAllText($fixturePath, $encoding)
    $normalized = $text.Replace("`r`n", "`n").Replace("`r", "`n").TrimEnd([char[]]"`n")
    return ,$encoding.GetBytes(($normalized.Replace("`n", "`r`n") + "`r`n"))
}

# Deliver-Fixture 投递固定无敏感内容 EML，并返回 DATA 接受响应。
function Deliver-Fixture {
    $session = Open-SmtpSession
    try {
        $mailResponse = Send-SmtpCommand -Writer $session.Writer -Reader $session.Reader -Command 'MAIL FROM:<sender@outside.test>'
        $rcptResponse = Send-SmtpCommand -Writer $session.Writer -Reader $session.Reader -Command 'RCPT TO:<receiver@mail-suite.test>'
        if ($mailResponse -notmatch '^250' -or $rcptResponse -notmatch '^250') {
            throw "SMTP envelope failed: MAIL=$mailResponse RCPT=$rcptResponse"
        }
        $dataResponse = Send-SmtpCommand -Writer $session.Writer -Reader $session.Reader -Command 'DATA'
        if ($dataResponse -notmatch '^354') {
            throw "SMTP DATA command failed: $dataResponse"
        }
        $fixtureText = [System.Text.Encoding]::UTF8.GetString((Get-CanonicalFixtureBytes))
        $fixtureLines = $fixtureText -split "`r`n"
        for ($index = 0; $index -lt ($fixtureLines.Count - 1); $index++) {
            $line = $fixtureLines[$index]
            if ($line.StartsWith('.')) {
                $session.Writer.WriteLine(".$line")
            } else {
                $session.Writer.WriteLine($line)
            }
        }
        $session.Writer.WriteLine('.')
        return Read-SmtpResponse -Reader $session.Reader
    } finally {
        Close-SmtpSession -Session $session
    }
}

# Invoke-LocalHttpsRequest 在进程内设置 Basic Authorization，不把凭据交给命令行。
function Invoke-LocalHttpsRequest {
    param(
        [Parameter(Mandatory)]
        [ValidateSet('GET', 'POST')]
        [string]$Method,
        [Parameter(Mandatory)]
        [string]$PathAndQuery,
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [string]$JsonBody,
        [switch]$AllowJmapDiscoveryRedirect
    )

    if (-not $PathAndQuery.StartsWith('/')) {
        throw 'Local HTTPS path must be absolute'
    }
    $request = [System.Net.HttpWebRequest]::Create("https://127.0.0.1:$httpsPort$PathAndQuery")
    $request.Method = $Method
    $request.Host = $publicUri.Authority
    $request.Timeout = 15000
    $request.ReadWriteTimeout = 15000
    $request.AllowAutoRedirect = $false
    $request.ServerCertificateValidationCallback = { $true }
    $credentialBytes = [System.Text.Encoding]::UTF8.GetBytes("${Username}:$Password")
    $request.Headers['Authorization'] = 'Basic ' + [Convert]::ToBase64String($credentialBytes)
    if ($PSBoundParameters.ContainsKey('JsonBody')) {
        $bodyBytes = [System.Text.Encoding]::UTF8.GetBytes($JsonBody)
        $request.ContentType = 'application/json'
        $request.ContentLength = $bodyBytes.Length
        $requestStream = $request.GetRequestStream()
        try {
            $requestStream.Write($bodyBytes, 0, $bodyBytes.Length)
        } finally {
            $requestStream.Dispose()
        }
    }
    try {
        $response = [System.Net.HttpWebResponse]$request.GetResponse()
    } catch [System.Net.WebException] {
        $status = 'transport-error'
        $location = $null
        if ($null -ne $_.Exception.Response) {
            $status = [int]$_.Exception.Response.StatusCode
            $location = $_.Exception.Response.Headers['Location']
            $_.Exception.Response.Dispose()
        }
        if ($AllowJmapDiscoveryRedirect -and $Method -eq 'GET' -and
            $PathAndQuery -eq '/.well-known/jmap' -and $status -eq 307 -and
            $location -eq '/jmap/session') {
            return ,(Invoke-LocalHttpsRequest `
                -Method GET `
                -PathAndQuery '/jmap/session' `
                -Username $Username `
                -Password $Password)
        }
        throw "HTTPS request failed: $Method $PathAndQuery status=$status"
    }
    try {
        $memory = [System.IO.MemoryStream]::new()
        try {
            $response.GetResponseStream().CopyTo($memory)
            $bytes = $memory.ToArray()
        } finally {
            $memory.Dispose()
        }
        return ,$bytes
    } finally {
        $response.Dispose()
    }
}

# Convert-JsonResponse 将本机 HTTPS 字节响应解析为 JSON 对象。
function Convert-JsonResponse {
    param(
        [Parameter(Mandatory)]
        [byte[]]$Bytes
    )

    return [System.Text.Encoding]::UTF8.GetString($Bytes) | ConvertFrom-Json
}

# Assert-JmapPublicUrl 确保服务端模板不会把测试凭据发送到预期 origin 之外。
function Assert-JmapPublicUrl {
    param(
        [Parameter(Mandatory)]
        [string]$Url
    )

    $uri = [Uri]$Url
    if (-not $uri.IsAbsoluteUri -or $uri.Scheme -ne 'https' -or
        $uri.DnsSafeHost -ne $publicUri.DnsSafeHost -or $uri.Port -ne $httpsPort) {
        throw "JMAP returned an unexpected public URL: $Url"
    }
    return $uri.PathAndQuery
}

# Invoke-Jmap 调用 session 公布的 JMAP API endpoint 并解析 JSON 响应。
function Invoke-Jmap {
    param(
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [Parameter(Mandatory)]
        [string]$ApiPath,
        [Parameter(Mandatory)]
        [hashtable]$Request
    )

    $requestJson = $Request | ConvertTo-Json -Depth 12 -Compress
    $raw = Invoke-LocalHttpsRequest -Method POST -PathAndQuery $ApiPath -Username $Username -Password $Password -JsonBody $requestJson
    return Convert-JsonResponse -Bytes $raw
}

# Get-JmapMethodPayload 校验 JMAP method response 名称并拒绝协议错误响应。
function Get-JmapMethodPayload {
    param(
        [Parameter(Mandatory)]
        $Response,
        [Parameter(Mandatory)]
        [string]$ExpectedName
    )

    if ($null -eq $Response.methodResponses -or $Response.methodResponses.Count -eq 0) {
        throw "JMAP response did not contain $ExpectedName"
    }
    $name = [string]$Response.methodResponses[0][0]
    if ($name -eq 'error') {
        $errorType = [string]$Response.methodResponses[0][1].type
        throw "JMAP method failed: $ExpectedName type=$errorType"
    }
    if ($name -ne $ExpectedName) {
        throw "Unexpected JMAP method response: expected=$ExpectedName actual=$name"
    }
    return $Response.methodResponses[0][1]
}

# Get-JmapEmail 读取结构化邮件字段、正文值、附件和原始 blob 标识。
function Get-JmapEmail {
    param(
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [Parameter(Mandatory)]
        [string]$ApiPath,
        [Parameter(Mandatory)]
        [string]$AccountId,
        [Parameter(Mandatory)]
        [string]$MessageId
    )

    $get = @{
        using = @('urn:ietf:params:jmap:core', $mailCapability)
        methodCalls = @(, @(
            'Email/get',
            @{
                accountId = $AccountId
                ids = @($MessageId)
                properties = @('id', 'blobId', 'messageId', 'from', 'to', 'subject', 'textBody', 'attachments', 'bodyValues')
                bodyProperties = @('partId', 'blobId', 'size', 'name', 'type', 'charset', 'disposition')
                fetchTextBodyValues = $true
                maxBodyValueBytes = 65536
            },
            'get-1'
        ))
    }
    $response = Invoke-Jmap -Username $Username -Password $Password -ApiPath $ApiPath -Request $get
    $payload = Get-JmapMethodPayload -Response $response -ExpectedName 'Email/get'
    if ($payload.notFound.Count -gt 0 -or $payload.list.Count -ne 1) {
        throw "JMAP Email/get did not return exactly one message: $MessageId"
    }
    return $payload.list[0]
}

# Find-DeliveredMessage 轮询 JMAP 并按固定 Message-ID 返回邮件标识。
function Find-DeliveredMessage {
    param(
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [Parameter(Mandatory)]
        [string]$ApiPath,
        [Parameter(Mandatory)]
        [string]$AccountId,
        [int]$TimeoutSeconds = 30
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        $query = @{
            using = @('urn:ietf:params:jmap:core', $mailCapability)
            methodCalls = @(, @(
                'Email/query',
                @{ accountId = $AccountId; filter = @{}; sort = @(, @{ property = 'receivedAt'; isAscending = $false }); limit = 20 },
                'query-1'
            ))
        }
        $response = Invoke-Jmap -Username $Username -Password $Password -ApiPath $ApiPath -Request $query
        $payload = Get-JmapMethodPayload -Response $response -ExpectedName 'Email/query'
        foreach ($id in @($payload.ids)) {
            $message = Get-JmapEmail -Username $Username -Password $Password -ApiPath $ApiPath -AccountId $AccountId -MessageId $id
            $headerIds = @($message.messageId | ForEach-Object { ([string]$_).Trim('<', '>') })
            if ($headerIds -contains $expectedMessageId) {
                return [string]$message.id
            }
        }
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)
    throw 'Delivered fixture did not appear in JMAP before timeout'
}

# Get-JmapDownloadBytes 展开 JMAP downloadUrl 模板并从固定本机 origin 下载 blob。
function Get-JmapDownloadBytes {
    param(
        [Parameter(Mandatory)]
        [string]$Template,
        [Parameter(Mandatory)]
        [string]$AccountId,
        [Parameter(Mandatory)]
        [string]$BlobId,
        [Parameter(Mandatory)]
        [string]$Name,
        [Parameter(Mandatory)]
        [string]$Type,
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password
    )

    $url = $Template.Replace('{accountId}', [Uri]::EscapeDataString($AccountId))
    $url = $url.Replace('{blobId}', [Uri]::EscapeDataString($BlobId))
    $url = $url.Replace('{name}', [Uri]::EscapeDataString($Name))
    $url = $url.Replace('{type}', [Uri]::EscapeDataString($Type))
    $path = Assert-JmapPublicUrl -Url $url
    return ,(Invoke-LocalHttpsRequest -Method GET -PathAndQuery $path -Username $Username -Password $Password)
}

# Get-Sha256Hex 返回字节内容的小写 SHA-256，供重启前后稳定性比较。
function Get-Sha256Hex {
    param(
        [Parameter(Mandatory)]
        [byte[]]$Bytes
    )

    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($sha256.ComputeHash($Bytes))).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha256.Dispose()
    }
}

# Assert-ByteContent 比较下载附件与 fixture 声明的确定性字节。
function Assert-ByteContent {
    param(
        [Parameter(Mandatory)]
        [byte[]]$Actual,
        [Parameter(Mandatory)]
        [byte[]]$Expected,
        [Parameter(Mandatory)]
        [string]$Label
    )

    if ($Actual.Length -ne $Expected.Length) {
        throw "$Label length mismatch: actual=$($Actual.Length) expected=$($Expected.Length)"
    }
    for ($index = 0; $index -lt $Expected.Length; $index++) {
        if ($Actual[$index] -ne $Expected[$index]) {
            throw "$Label content mismatch at byte $index"
        }
    }
}

# Assert-JmapMessage 校验结构化字段、正文、附件内容，并返回 raw blob 哈希。
function Assert-JmapMessage {
    param(
        [Parameter(Mandatory)]
        [string]$Username,
        [Parameter(Mandatory)]
        [string]$Password,
        [Parameter(Mandatory)]
        [string]$ApiPath,
        [Parameter(Mandatory)]
        [string]$DownloadTemplate,
        [Parameter(Mandatory)]
        [string]$AccountId,
        [Parameter(Mandatory)]
        [string]$MessageId
    )

    $message = Get-JmapEmail -Username $Username -Password $Password -ApiPath $ApiPath -AccountId $AccountId -MessageId $MessageId
    $headerIds = @($message.messageId | ForEach-Object { ([string]$_).Trim('<', '>') })
    if ($headerIds -notcontains $expectedMessageId) {
        throw 'JMAP Message-ID did not match the fixture'
    }
    if (@($message.from)[0].email -ne $expectedFrom -or @($message.to)[0].email -ne $expectedTo) {
        throw 'JMAP From/To did not match the fixture'
    }
    if ($message.subject -ne $expectedSubject) {
        throw 'JMAP subject did not match the fixture'
    }

    $actualBody = $null
    foreach ($part in @($message.textBody)) {
        $bodyProperty = $message.bodyValues.PSObject.Properties | Where-Object { $_.Name -eq $part.partId }
        if ($null -ne $bodyProperty -and $null -ne $bodyProperty.Value.value -and
            -not $bodyProperty.Value.isTruncated) {
            $actualBody = ([string]$bodyProperty.Value.value).Replace("`r`n", "`n").TrimEnd([char[]]"`n")
            break
        }
    }
    if ($actualBody -ne $expectedTextBody) {
        throw 'JMAP text body did not match the fixture'
    }

    $attachment = @($message.attachments | Where-Object { $_.name -eq $expectedAttachmentName }) | Select-Object -First 1
    if ($null -eq $attachment -or $attachment.type -ne $expectedAttachmentType -or
        $attachment.disposition -ne 'attachment' -or [string]::IsNullOrWhiteSpace([string]$attachment.blobId)) {
        throw 'JMAP attachment metadata did not match the fixture'
    }
    $attachmentBytes = Get-JmapDownloadBytes `
        -Template $DownloadTemplate `
        -AccountId $AccountId `
        -BlobId $attachment.blobId `
        -Name $expectedAttachmentName `
        -Type $expectedAttachmentType `
        -Username $Username `
        -Password $Password
    Assert-ByteContent -Actual $attachmentBytes -Expected $expectedAttachmentBytes -Label 'JMAP attachment'

    if ([string]::IsNullOrWhiteSpace([string]$message.blobId)) {
        throw 'JMAP message did not expose a raw blob identifier'
    }
    $rawBytes = Get-JmapDownloadBytes `
        -Template $DownloadTemplate `
        -AccountId $AccountId `
        -BlobId $message.blobId `
        -Name 'mail-suite-poc.eml' `
        -Type 'message/rfc822' `
        -Username $Username `
        -Password $Password
    return [pscustomobject]@{
        RawSha256 = Get-Sha256Hex -Bytes $rawBytes
        AttachmentSha256 = Get-Sha256Hex -Bytes $attachmentBytes
    }
}

$smtpPort = Get-ConfiguredPort -Name 'MAIL_CORE_SMTP_PORT' -Default 20025
$httpsPort = Get-ConfiguredPort -Name 'MAIL_CORE_HTTPS_PORT' -Default 28443
$recoveryPort = Get-ConfiguredPort -Name 'MAIL_CORE_RECOVERY_PORT' -Default 28080
$configuredPublicUrl = [Environment]::GetEnvironmentVariable('MAIL_CORE_PUBLIC_URL')
if ([string]::IsNullOrWhiteSpace($configuredPublicUrl)) {
    $configuredPublicUrl = "https://mx.mail-suite.test:$httpsPort"
}
$publicUri = [Uri]$configuredPublicUrl
if (-not $publicUri.IsAbsoluteUri -or $publicUri.Scheme -ne 'https' -or $publicUri.Port -ne $httpsPort -or
    $publicUri.AbsolutePath -ne '/' -or -not [string]::IsNullOrEmpty($publicUri.Query)) {
    throw 'MAIL_CORE_PUBLIC_URL must be an HTTPS origin whose port matches MAIL_CORE_HTTPS_PORT'
}
$publicUrl = $publicUri.AbsoluteUri.TrimEnd('/')

Assert-RedactionRegression
if ($StaticOnly) {
    [pscustomobject]@{
        powershell = $PSVersionTable.PSVersion.ToString()
        redaction = $true
        smtpPort = $smtpPort
        httpsPort = $httpsPort
        recoveryPort = $recoveryPort
        publicUrl = $publicUrl
        fixtureSha256 = Get-Sha256Hex -Bytes (Get-CanonicalFixtureBytes)
    } | ConvertTo-Json
    return
}

$baseComposeArguments = @('compose', '--project-name', $projectName, '-f', $composePath)
$secretDirectoryCreated = $false
$recoveryContainerHasSecret = $false
$composeArguments = $null
$recoveryPassword = [guid]::NewGuid().ToString('N') + 'Aa1!'
$mailboxPassword = [guid]::NewGuid().ToString('N') + 'Bb2!'

try {
    $hasExistingState = Test-PocDockerState
    if ($hasExistingState -and -not $Reset) {
        throw "PoC state already exists for project '$projectName'; rerun with -Reset to remove it explicitly"
    }
    if ($Reset) {
        Write-Host "Reset boundary: project=$projectName volumes=${projectName}_stalwart_config,${projectName}_stalwart_data"
        $null = Invoke-Docker -Arguments ($baseComposeArguments + @('down', '--volumes', '--remove-orphans'))
        Remove-SecretDirectory
    } elseif (Test-Path -LiteralPath $secretDirectory) {
        throw "Restricted secret directory already exists; inspect it or rerun with -Reset: $secretDirectory"
    }

    New-RestrictedSecretDirectory
    $secretDirectoryCreated = $true
    Write-ComposeEnvironment -RecoveryAdmin "admin:$recoveryPassword"
    $composeArguments = @(
        'compose', '--env-file', $composeEnvironmentPath,
        '--project-name', $projectName, '-f', $composePath
    )
    $null = Invoke-Docker -Arguments ($composeArguments + @('config', '--quiet'))
    $recoveryContainerHasSecret = $true
    $null = Invoke-Docker -Arguments ($composeArguments + @('up', '-d', '--wait', 'stalwart'))
    Wait-HttpHealth -Url "http://127.0.0.1:$recoveryPort/healthz/live"

    $bootstrapJson = Get-Content -Raw -LiteralPath $bootstrapPath
    $bootstrapOutput = Invoke-StalwartCli `
        -Url 'http://stalwart:8080' `
        -Username 'admin' `
        -Password $recoveryPassword `
        -Arguments @('update', 'Bootstrap', '--stdin') `
        -StandardInput $bootstrapJson
    $bootstrapText = $bootstrapOutput -join "`n"
    $adminUser = [regex]::Match($bootstrapText, 'username:\s*"([^"]+)"').Groups[1].Value
    $adminPassword = [regex]::Match($bootstrapText, 'secret:\s*"([^"]+)"').Groups[1].Value
    if (-not $adminUser -or -not $adminPassword) {
        throw 'Bootstrap did not return a permanent administrator credential'
    }
    Write-RestrictedEnvironmentFile -Path $credentialsPath -Values ([ordered]@{
        STALWART_ADMIN_USER = $adminUser
        STALWART_ADMIN_PASSWORD = $adminPassword
        STALWART_MAILBOX_USER = 'receiver@mail-suite.test'
        STALWART_MAILBOX_PASSWORD = $mailboxPassword
    })

    Write-ComposeEnvironment -RecoveryAdmin ''
    $null = Invoke-Docker -Arguments ($composeArguments + @('up', '-d', '--force-recreate', '--wait', 'stalwart'))
    $recoveryContainerHasSecret = $false
    Wait-HttpHealth -Url "https://127.0.0.1:$httpsPort/healthz/live"

    $domainOutput = Invoke-StalwartCli `
        -Url 'https://stalwart:443' `
        -Username $adminUser `
        -Password $adminPassword `
        -Arguments @('--insecure', 'query', 'Domain', '--where', 'name=mail-suite.test', '--fields', 'id,name', '--json')
    $domainJson = $domainOutput | Where-Object {
        ([string]$_).TrimStart().StartsWith('{') -or ([string]$_).TrimStart().StartsWith('[')
    } | Select-Object -First 1
    if ([string]::IsNullOrWhiteSpace([string]$domainJson)) {
        throw 'Stalwart domain query did not return JSON'
    }
    $domain = $domainJson | ConvertFrom-Json
    if ($domain.name -ne 'mail-suite.test') {
        throw 'Bootstrap did not provision the reserved test domain'
    }

    $accountJson = [ordered]@{
        name = 'receiver'
        domainId = $domain.id
        credentials = @{ '0' = @{ '@type' = 'Password'; secret = $mailboxPassword } }
    } | ConvertTo-Json -Depth 6 -Compress
    $accountOutput = Invoke-StalwartCli `
        -Url 'https://stalwart:443' `
        -Username $adminUser `
        -Password $adminPassword `
        -Arguments @('--insecure', 'create', 'Account/User', '--stdin') `
        -StandardInput $accountJson
    $accountText = $accountOutput -join "`n"
    $accountId = [regex]::Match($accountText, 'Created Account\s+(\S+)').Groups[1].Value
    if (-not $accountId) {
        throw 'Stalwart did not create the test mailbox account'
    }
    Assert-StalwartAccountState `
        -AccountId $accountId `
        -Username $adminUser `
        -Password $adminPassword

    $unknownRcpt = Test-SmtpRecipient -Recipient 'unknown@mail-suite.test'
    $relayRcpt = Test-SmtpRecipient -Recipient 'external@outside.test'
    $activeRcpt = Test-SmtpRecipient -Recipient 'receiver@mail-suite.test'
    if ($unknownRcpt -notmatch '^5' -or $relayRcpt -notmatch '^5' -or $activeRcpt -notmatch '^250') {
        throw "Unexpected SMTP RCPT responses: unknown=$unknownRcpt relay=$relayRcpt active=$activeRcpt"
    }
    $dataResponse = Deliver-Fixture
    if ($dataResponse -notmatch '^250') {
        throw "Stalwart did not accept the fixture DATA: $dataResponse"
    }

    $sessionBytes = Invoke-LocalHttpsRequest `
        -Method GET `
        -PathAndQuery '/.well-known/jmap' `
        -Username 'receiver@mail-suite.test' `
        -Password $mailboxPassword `
        -AllowJmapDiscoveryRedirect
    $jmapSession = Convert-JsonResponse -Bytes $sessionBytes
    $accountProperty = $jmapSession.primaryAccounts.PSObject.Properties | Where-Object { $_.Name -eq $mailCapability }
    $jmapAccountId = $accountProperty.Value
    if (-not $jmapAccountId) {
        $primaryKeys = @($jmapSession.primaryAccounts.PSObject.Properties.Name) -join ','
        $capabilityKeys = @($jmapSession.capabilities.PSObject.Properties.Name) -join ','
        throw "JMAP session did not expose a primary mail account; primary=$primaryKeys capabilities=$capabilityKeys"
    }
    $jmapApiPath = Assert-JmapPublicUrl -Url $jmapSession.apiUrl
    $null = Assert-JmapPublicUrl -Url $jmapSession.downloadUrl.Replace('{accountId}', 'account').Replace('{blobId}', 'blob').Replace('{name}', 'name').Replace('{type}', 'type')
    $messageId = Find-DeliveredMessage `
        -Username 'receiver@mail-suite.test' `
        -Password $mailboxPassword `
        -ApiPath $jmapApiPath `
        -AccountId $jmapAccountId
    $beforeRestart = Assert-JmapMessage `
        -Username 'receiver@mail-suite.test' `
        -Password $mailboxPassword `
        -ApiPath $jmapApiPath `
        -DownloadTemplate $jmapSession.downloadUrl `
        -AccountId $jmapAccountId `
        -MessageId $messageId

    $null = Invoke-Docker -Arguments ($composeArguments + @('restart', 'stalwart'))
    Wait-HttpHealth -Url "https://127.0.0.1:$httpsPort/healthz/live"
    $afterRestart = Assert-JmapMessage `
        -Username 'receiver@mail-suite.test' `
        -Password $mailboxPassword `
        -ApiPath $jmapApiPath `
        -DownloadTemplate $jmapSession.downloadUrl `
        -AccountId $jmapAccountId `
        -MessageId $messageId
    if ($afterRestart.RawSha256 -ne $beforeRestart.RawSha256) {
        throw 'Raw JMAP blob SHA-256 changed after a normal restart'
    }

    $null = Invoke-StalwartCli `
        -Url 'https://stalwart:443' `
        -Username $adminUser `
        -Password $adminPassword `
        -Arguments @('--insecure', 'delete', 'Account', '--ids', $accountId)
    $deletedRcpt = Test-SmtpRecipient -Recipient 'receiver@mail-suite.test'
    if ($deletedRcpt -notmatch '^5') {
        throw "Deleted mailbox was still accepted: $deletedRcpt"
    }
    $recreatedAccountOutput = Invoke-StalwartCli `
        -Url 'https://stalwart:443' `
        -Username $adminUser `
        -Password $adminPassword `
        -Arguments @('--insecure', 'create', 'Account/User', '--stdin') `
        -StandardInput $accountJson
    $recreatedAccountText = $recreatedAccountOutput -join "`n"
    $recreatedAccountId = [regex]::Match($recreatedAccountText, 'Created Account\s+(\S+)').Groups[1].Value
    if (-not $recreatedAccountId) {
        throw 'Stalwart did not recreate the test mailbox account'
    }
    Assert-StalwartAccountState `
        -AccountId $recreatedAccountId `
        -Username $adminUser `
        -Password $adminPassword
    $recreatedRcpt = Test-SmtpRecipient -Recipient 'receiver@mail-suite.test'
    if ($recreatedRcpt -notmatch '^250') {
        throw "Recreated mailbox was not accepted: $recreatedRcpt"
    }

    [pscustomobject]@{
        stalwartVersion = '0.16.19'
        architecture = 'linux/amd64'
        domain = $domain.name
        unknownRecipient = $unknownRcpt
        openRelay = $relayRcpt
        activeRecipient = $activeRcpt
        dataAccepted = $dataResponse
        messageId = $expectedMessageId
        structuredContentVerified = $true
        attachmentSha256 = $beforeRestart.AttachmentSha256
        fixtureSha256 = Get-Sha256Hex -Bytes (Get-CanonicalFixtureBytes)
        rawBlobSha256 = $beforeRestart.RawSha256
        normalRestartRawHashStable = $true
        deletedRecipient = $deletedRcpt
        recreatedRecipient = $recreatedRcpt
        smtp250CrashDurability = 'pending-fault-verification'
        pendingAcceptance = @(
            'wrong-credential',
            'timeout',
            'read-only-storage',
            'dependency-stop',
            'partial-config',
            'mailbox-suspend-restore',
            'dual-instance-fault-domain'
        )
        credentialsFile = $(if ($Keep) { $credentialsPath } else { $null })
    } | ConvertTo-Json -Depth 4
} finally {
    if ($null -ne $composeArguments) {
        if (-not $Keep) {
            try {
                $null = Invoke-Docker -Arguments ($composeArguments + @('down', '--volumes', '--remove-orphans'))
            } catch {
                Write-Warning 'PoC cleanup failed; run the documented docker compose down command.'
            }
        } elseif ($recoveryContainerHasSecret) {
            try {
                $null = Invoke-Docker -Arguments ($composeArguments + @('rm', '--stop', '--force', 'stalwart'))
            } catch {
                Write-Warning 'Recovery container cleanup failed; do not retain a container with recovery credentials.'
            }
        }
    }
    if ($secretDirectoryCreated -and -not $Keep) {
        try {
            Remove-SecretDirectory
        } catch {
            Write-Warning "Restricted secret cleanup failed: $secretDirectory"
        }
    } elseif ($secretDirectoryCreated -and $Keep) {
        Write-Warning "PoC state retained; restricted credentials are stored at: $credentialsPath"
    }
}
