[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$gitAttributesPath = Join-Path $repositoryRoot '.gitattributes'
$composePath = Join-Path $repositoryRoot 'deploy\server\compose.yaml'
$nginxPath = Join-Path $repositoryRoot 'deploy\server\nginx-public.conf.template'
$deployPath = Join-Path $repositoryRoot 'deploy\server\deploy.sh'
$prepareCertificatesPath = Join-Path $repositoryRoot 'deploy\server\prepare-public-certificates.sh'
$manualDnsRequestPath = Join-Path $repositoryRoot 'deploy\server\request-manual-dns-certificate.sh'
$manualDnsHookPath = Join-Path $repositoryRoot 'deploy\server\manual-dns-auth-hook.sh'
$aliDnsHookPath = Join-Path $repositoryRoot 'deploy\server\alidns-dns-hook.py'
$configureRenewalPath = Join-Path $repositoryRoot 'deploy\server\configure-alidns-certificate-renewal.sh'
$installCertificatesPath = Join-Path $repositoryRoot 'deploy\server\install-public-certificates.sh'
$bootstrapStalwartPath = Join-Path $repositoryRoot 'deploy\server\bootstrap-stalwart.sh'
$configureKeycloakAmrPath = Join-Path $repositoryRoot 'deploy\server\configure-keycloak-amr.sh'
$verifyPath = Join-Path $repositoryRoot 'deploy\server\verify.sh'

# Assert-Contains 要求服务器制品保留指定的安全或运行契约。
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

$gitAttributes = Get-Content -Raw -LiteralPath $gitAttributesPath
$compose = Get-Content -Raw -LiteralPath $composePath
$nginx = Get-Content -Raw -LiteralPath $nginxPath
$deploy = Get-Content -Raw -LiteralPath $deployPath
$prepareCertificates = Get-Content -Raw -LiteralPath $prepareCertificatesPath
$manualDnsRequest = Get-Content -Raw -LiteralPath $manualDnsRequestPath
$manualDnsHook = Get-Content -Raw -LiteralPath $manualDnsHookPath
$aliDnsHook = Get-Content -Raw -LiteralPath $aliDnsHookPath
$configureRenewal = Get-Content -Raw -LiteralPath $configureRenewalPath
$installCertificates = Get-Content -Raw -LiteralPath $installCertificatesPath
$bootstrapStalwart = Get-Content -Raw -LiteralPath $bootstrapStalwartPath
$configureKeycloakAmr = Get-Content -Raw -LiteralPath $configureKeycloakAmrPath
$verify = Get-Content -Raw -LiteralPath $verifyPath

# Linux 发布归档必须覆盖开发机的全局 autocrlf 设置，避免脚本在目标机解析失败。
Assert-Contains -Text $gitAttributes -Pattern '(?m)^\* text=auto eol=lf$' -Message 'Git text files must be normalized to LF for Linux release archives'
Assert-Contains -Text $compose -Pattern '(?m)^\s{2}backend:\r?\n\s{4}internal: true$' -Message 'Compose backend network must remain internal'
Assert-Contains -Text $compose -Pattern '(?m)^\s+- "25:25"$' -Message 'Stalwart SMTP port is not published'
Assert-Contains -Text $compose -Pattern '(?m)^\s+- "80:8080"$' -Message 'Web HTTP port is not published'
Assert-Contains -Text $compose -Pattern '(?m)^\s+- "443:8443"$' -Message 'Web HTTPS port is not published'
Assert-Contains -Text $compose -Pattern '(?ms)^\s{4}cap_drop:\r?\n\s{6}- ALL\r?\n\s{4}cap_add:\r?\n\s{6}- NET_BIND_SERVICE$' -Message 'Non-root Web must keep only the low-port bind capability'
Assert-Contains -Text $compose -Pattern '(?ms)^\s{2}web:.*?aliases:\r?\n\s{10}- idp\.test\.snowye\.fun\r?\n\s{10}- mail\.test\.snowye\.fun$' -Message 'Web backend aliases must cover both public HTTPS hostnames'
Assert-Contains -Text $compose -Pattern 'wget -q -O /dev/null https://mail\.test\.snowye\.fun/health/ready' -Message 'Web healthcheck must use the strict public hostname route'
if ($compose -match '--no-check-certificate') {
    throw 'Compose healthchecks must not bypass TLS certificate validation'
}
Assert-Contains -Text $compose -Pattern 'STALWART_RECOVERY_ADMIN: \$\{MAIL_SUITE_STALWART_RECOVERY_ADMIN:-\}' -Message 'Stalwart recovery value must default to empty'
Assert-Contains -Text $compose -Pattern 'MAIL_SUITE_STALWART_CA_FILE: /etc/ssl/certs/ca-certificates\.crt' -Message 'Worker strict public CA configuration is missing'
if ([regex]::Matches($compose, '(?m)^\s{4}healthcheck:$').Count -ne 5) {
    throw 'The five services without an image healthcheck must define one in Compose'
}
Assert-Contains -Text $compose -Pattern '/health/ready HTTP/1\.1\\r\\nHost: localhost' -Message 'Keycloak healthcheck must verify the management readiness HTTP status'
Assert-Contains -Text $compose -Pattern '(?m)^\s{6}start_period: 240s$' -Message 'Keycloak first-start readiness window is too short'
Assert-Contains -Text $compose -Pattern 'image: mail-suite-stalwart:0\.16\.19-34e59515' -Message 'Pinned Stalwart image healthcheck boundary changed'
if ($compose -match '(?m)^\s+- "(?:443|8080):(?:443|8080)"$') {
    throw 'Stalwart internal Admin, JMAP, or recovery port is published'
}

Assert-Contains -Text $nginx -Pattern '(?m)^\s*location \^~ /realms/mail-suite-test/ \{' -Message 'Public IdP realm route is missing'
Assert-Contains -Text $nginx -Pattern '(?s)server_name idp\.test\.snowye\.fun;.*location / \{\s*return 404;' -Message 'Public IdP default-deny route is missing'
Assert-Contains -Text $nginx -Pattern 'server api:8080 resolve;' -Message 'Nginx API runtime DNS resolution is missing'
Assert-Contains -Text $nginx -Pattern 'server keycloak:8080 resolve;' -Message 'Nginx Keycloak runtime DNS resolution is missing'
if ([regex]::Matches($nginx, '(?m)^\s*listen 443 ssl(?: default_server)?;$').Count -ne 3) {
    throw 'Every public TLS virtual host must also listen on Docker-internal port 443'
}
Assert-Contains -Text $deploy -Pattern 'compose exec -T web wget -q -O /dev/null.*\\\s*\r?\n\s*"https://idp\.test\.snowye\.fun/realms/mail-suite-test/\.well-known/openid-configuration"' -Message 'Pre-API internal OIDC discovery probe is missing'
Assert-Contains -Text $deploy -Pattern 'wait_test_mailbox_operation' -Message 'Deployment does not wait for the active test mailbox operation'
Assert-Contains -Text $deploy -Pattern "operations\.status IN \('failed', 'dead', 'superseded'\)" -Message 'Mailbox wait does not stop on terminal operation failure'
Assert-Contains -Text $deploy -Pattern 'configure-keycloak-amr\.sh.*--apply' -Message 'Deployment does not reconcile Keycloak OTP AMR before API startup'
Assert-Contains -Text $verify -Pattern 'configure-keycloak-amr\.sh.*--check' -Message 'Host verification does not check Keycloak OTP AMR'
Assert-Contains -Text $configureKeycloakAmr -Pattern 'default\.reference\.value' -Message 'OTP AMR reference value is missing'
Assert-Contains -Text $configureKeycloakAmr -Pattern 'reference_max_age="36000"' -Message 'OTP AMR maxAge does not match the SSO maximum lifespan'
Assert-Contains -Text $configureKeycloakAmr -Pattern 'KC_CLI_PASSWORD="\$\{KC_BOOTSTRAP_ADMIN_PASSWORD\}"' -Message 'Keycloak administrator password is not derived inside the existing container environment'
Assert-Contains -Text $configureKeycloakAmr -Pattern 'BEGIN TRANSACTION READ ONLY;' -Message 'Exact Keycloak authenticator config verification is not read-only'
Assert-Contains -Text $configureKeycloakAmr -Pattern 'authenticator_config_entry' -Message 'Exact Keycloak authenticator config values are not verified'
if ($configureKeycloakAmr -match '--password') {
    throw 'Keycloak administrator password must not be passed in command arguments'
}

foreach ($hostname in @('mail.test.snowye.fun', 'idp.test.snowye.fun', 'mx1.test.snowye.fun')) {
    Assert-Contains -Text $prepareCertificates -Pattern ([regex]::Escape('"' + $hostname + '"')) -Message "ACME hostname is missing: $hostname"
    Assert-Contains -Text $installCertificates -Pattern ([regex]::Escape('"' + $hostname + '"')) -Message "Certificate install hostname is missing: $hostname"
    Assert-Contains -Text $deploy -Pattern ([regex]::Escape("$hostname/privkey.pem")) -Message "Deployment does not require the private key: $hostname"
}
Assert-Contains -Text $prepareCertificates -Pattern 'trap - EXIT' -Message 'ACME cleanup trap is not recursion-safe'
Assert-Contains -Text $prepareCertificates -Pattern '--tmpfs /etc/nginx/conf\.d:uid=101,gid=101,mode=0755' -Message 'ACME Nginx rendered configuration tmpfs is missing'
Assert-Contains -Text $prepareCertificates -Pattern 'http://127\.0\.0\.1/\.well-known/acme-challenge/mail-suite-ready' -Message 'ACME local HTTP readiness probe is missing'
Assert-Contains -Text $manualDnsRequest -Pattern '--preferred-challenges dns' -Message 'Manual DNS certificate request is missing'
Assert-Contains -Text $manualDnsRequest -Pattern '--manual-auth-hook' -Message 'Manual DNS auth hook is not wired'
Assert-Contains -Text $manualDnsHook -Pattern '223\.5\.5\.5' -Message 'AliDNS public propagation check is missing'
Assert-Contains -Text $manualDnsHook -Pattern '8\.8\.8\.8' -Message 'Google public propagation check is missing'
Assert-Contains -Text $manualDnsHook -Pattern '1\.1\.1\.1' -Message 'Cloudflare public propagation check is missing'
Assert-Contains -Text $manualDnsHook -Pattern 'required_stable_checks=24' -Message 'Manual DNS propagation stability window is missing'
Assert-Contains -Text $manualDnsHook -Pattern 'chmod 0600' -Message 'Manual DNS challenge state is not restricted'
foreach ($hostname in @('mail.test.snowye.fun', 'idp.test.snowye.fun', 'mx1.test.snowye.fun')) {
    Assert-Contains -Text $aliDnsHook -Pattern ([regex]::Escape('"' + $hostname + '"')) -Message "AliDNS allowlist is missing: $hostname"
}
foreach ($resolver in @('223.5.5.5', '8.8.8.8', '1.1.1.1')) {
    Assert-Contains -Text $aliDnsHook -Pattern ([regex]::Escape('"' + $resolver + '"')) -Message "AliDNS public resolver is missing: $resolver"
}
Assert-Contains -Text $aliDnsHook -Pattern 'getpass\.getpass' -Message 'AliDNS credentials are not read from a hidden TTY prompt'
Assert-Contains -Text $aliDnsHook -Pattern 'method="POST"' -Message 'AliDNS credentials and challenge must not be placed in a GET URL'
Assert-Contains -Text $aliDnsHook -Pattern 'DescribeDomainRecordInfo' -Message 'AliDNS cleanup does not inspect its RecordId before deletion'
Assert-Contains -Text $aliDnsHook -Pattern 'record_matches_state' -Message 'AliDNS cleanup does not validate the current record content'
Assert-Contains -Text $aliDnsHook -Pattern 'DeleteDomainRecord' -Message 'AliDNS cleanup action is missing'
Assert-Contains -Text $aliDnsHook -Pattern 'validation_sha256' -Message 'AliDNS state does not use challenge digests'
Assert-Contains -Text $configureRenewal -Pattern 'certbot reconfigure' -Message 'Certbot lineage staging migration is missing'
Assert-Contains -Text $configureRenewal -Pattern '--manual-cleanup-hook' -Message 'Certbot automatic cleanup hook is not wired'
Assert-Contains -Text $configureRenewal -Pattern 'wait-cleanup' -Message 'Public TXT cleanup verification is missing'
Assert-Contains -Text $configureRenewal -Pattern 'systemctl enable --now certbot\.timer' -Message 'Certbot timer enablement is missing'
Assert-Contains -Text $configureRenewal -Pattern 'renewal-backups' -Message 'Certbot renewal configuration rollback is missing'
Assert-Contains -Text $deploy -Pattern 'install_certificate_hooks' -Message 'Deployment does not synchronize stable ACME hooks'
Assert-Contains -Text $verify -Pattern 'verify_certificate_renewal' -Message 'Host verification does not enforce automatic certificate renewal'
Assert-Contains -Text $installCertificates -Pattern 'validate_distinct_keys' -Message 'Certificate key isolation validation is missing'
Assert-Contains -Text $installCertificates -Pattern 'operation.*--rollback' -Message 'Certificate rollback operation is missing'
Assert-Contains -Text $installCertificates -Pattern 'trap cleanup_cli_environment EXIT' -Message 'Certificate reload credential cleanup is missing'
Assert-Contains -Text $verify -Pattern 'trap cleanup_temporary_files EXIT' -Message 'Verification credential cleanup is missing'
Assert-Contains -Text $verify -Pattern '\^8080/tcp -> .*:80\$' -Message 'Exact HTTP port validation is missing'
Assert-Contains -Text $verify -Pattern '\^8443/tcp -> .*:443\$' -Message 'Exact HTTPS port validation is missing'
Assert-Contains -Text $verify -Pattern "operations\.status = 'succeeded'" -Message 'Verification does not require a succeeded test mailbox operation'
Assert-Contains -Text $verify -Pattern "suspended_mailbox_id" -Message 'Verification does not keep the suspended fixture out of mail-core provisioning'

Assert-Contains -Text $bootstrapStalwart -Pattern "grep -v '\^MAIL_SUITE_STALWART_RECOVERY_ADMIN='" -Message 'Stalwart recovery cleanup is missing'
Assert-Contains -Text $bootstrapStalwart -Pattern 'trap cleanup_secret_file EXIT' -Message 'Stalwart bootstrap credential cleanup is missing'
if (($installCertificates + $bootstrapStalwart + $verify) -match '(?m)(?:--password|-p)\s+"?\$\{?(?:admin_)?password') {
    throw 'A Stalwart administrator credential is passed in command arguments'
}

foreach ($jsonPath in @(
    'deploy\server\config\keycloak-realm.template.json',
    'deploy\server\config\test-identity.template.json',
    'deploy\server\config\stalwart-bootstrap.json',
    'deploy\server\config\alidns-acme-policy.json'
)) {
    $null = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot $jsonPath) | ConvertFrom-Json
}
$aliDnsPolicy = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot 'deploy\server\config\alidns-acme-policy.json') | ConvertFrom-Json
$expectedAliDnsActions = @(
    'alidns:AddDomainRecord',
    'alidns:DeleteDomainRecord',
    'alidns:DescribeDomainRecordInfo',
    'alidns:DescribeDomainRecords'
)
if ($aliDnsPolicy.Statement.Count -ne 1 -or
    $aliDnsPolicy.Statement[0].Effect -ne 'Allow' -or
    $aliDnsPolicy.Statement[0].Resource -ne 'acs:alidns::<ALIYUN_ACCOUNT_ID>:domain/snowye.fun' -or
    (Compare-Object ($aliDnsPolicy.Statement[0].Action | Sort-Object) ($expectedAliDnsActions | Sort-Object))) {
    throw 'AliDNS RAM policy is broader than the approved domain and four actions'
}

[pscustomobject]@{
    backendNetwork = 'internal'
    publicPorts = @(25, 80, 443)
    healthchecks = 'five-compose-plus-stalwart-image'
    certificates = 'three-distinct-lineages'
    certificateRenewal = 'alidns-automated'
    releaseLineEndings = 'lf'
    manualDnsFallback = 'three-resolver-stable'
    recoveryCredential = 'one-time'
    temporaryCredentials = 'cleanup-trapped'
} | ConvertTo-Json
