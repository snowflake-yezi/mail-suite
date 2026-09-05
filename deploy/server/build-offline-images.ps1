param(
    [string]$OutputPath = ".tmp/mail-suite-images-20260905-rc7.tar"
)

$ErrorActionPreference = "Stop"
$imageTag = "test-20260905-rc7"
$postgresImage = "postgres:17.11-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73"
$postgresAlias = "mail-suite-postgres:17.11-alpine-18cfe3ef"
$keycloakImage = "quay.io/keycloak/keycloak:26.7.3@sha256:88943b6ad06d6293a239f0dfca5acec64218c9b3ab327bf9c936acf408a6ae3b"
$keycloakAlias = "mail-suite-keycloak:26.7.3-88943b6a"
$stalwartImage = "ghcr.io/stalwartlabs/stalwart:v0.16.19@sha256:34e59515ef633e2353abbd9a12b0a232eb2174df9ac1ee820ed13e952cfd801d"
$stalwartAlias = "mail-suite-stalwart:0.16.19-34e59515"
$stalwartCliImage = "ghcr.io/stalwartlabs/cli:1.0.12@sha256:8831cc276c22e334bcf473fc7d28e21248d748c4bc1dac43c83738d3cbd3a9d5"
$stalwartCliAlias = "mail-suite-stalwart-cli:1.0.12-8831cc27"
$composeFile = "deploy/server/compose.yaml"
$dockerConfig = ".tmp/docker-config"

# Build images from the server Compose definition and export a verifiable archive.
function BuildOfflineImages {
    $env:MAIL_SUITE_POSTGRES_PASSWORD = "build-validation-only"
    $env:MAIL_SUITE_KEYCLOAK_DB_PASSWORD = "build-validation-only"
    $env:MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD = "build-validation-only"
    $env:MAIL_SUITE_OIDC_CLIENT_SECRET = "build-validation-only"
    $env:MAIL_SUITE_AUTH_SECRET_PEPPER = "YnVpbGQtdmFsaWRhdGlvbi1vbmx5LWJ1aWxkLXZhbGlkYXRpb24="
    $env:MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY = "YnVpbGQtdmFsaWRhdGlvbi1vbmx5LWJ1aWxkLXZhbGlkYXRpb24="
    $env:MAIL_SUITE_IMAGE_TAG = $imageTag
    $env:MAIL_SUITE_VERSION = "test"
    $env:MAIL_SUITE_REVISION = "working-tree"
    try {
        docker --config $dockerConfig compose --project-directory . -f $composeFile build api worker migrator identity-bootstrap web
        if ($LASTEXITCODE -ne 0) { throw "Compose image build failed" }

        docker --config $dockerConfig pull $postgresImage
        if ($LASTEXITCODE -ne 0) { throw "PostgreSQL image pull failed" }

        docker --config $dockerConfig pull $keycloakImage
        if ($LASTEXITCODE -ne 0) { throw "Keycloak image pull failed" }

        docker --config $dockerConfig pull $stalwartImage
        if ($LASTEXITCODE -ne 0) { throw "Stalwart image pull failed" }

        docker --config $dockerConfig pull $stalwartCliImage
        if ($LASTEXITCODE -ne 0) { throw "Stalwart CLI image pull failed" }

        docker --config $dockerConfig image tag $postgresImage $postgresAlias
        if ($LASTEXITCODE -ne 0) { throw "PostgreSQL image tag failed" }

        docker --config $dockerConfig image tag $keycloakImage $keycloakAlias
        if ($LASTEXITCODE -ne 0) { throw "Keycloak image tag failed" }

        docker --config $dockerConfig image tag $stalwartImage $stalwartAlias
        if ($LASTEXITCODE -ne 0) { throw "Stalwart image tag failed" }

        docker --config $dockerConfig image tag $stalwartCliImage $stalwartCliAlias
        if ($LASTEXITCODE -ne 0) { throw "Stalwart CLI image tag failed" }

        docker --config $dockerConfig image save --output $OutputPath `
            "mail-suite-api:$imageTag" `
            "mail-suite-worker:$imageTag" `
            "mail-suite-migrator:$imageTag" `
            "mail-suite-identity-bootstrap:$imageTag" `
            "mail-suite-web:$imageTag" `
            $postgresAlias `
            $keycloakAlias `
            $stalwartAlias `
            $stalwartCliAlias
        if ($LASTEXITCODE -ne 0) { throw "Offline image archive export failed" }
    }
    finally {
        Remove-Item Env:MAIL_SUITE_POSTGRES_PASSWORD -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_KEYCLOAK_DB_PASSWORD -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_OIDC_CLIENT_SECRET -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_AUTH_SECRET_PEPPER -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_IMAGE_TAG -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_VERSION -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_REVISION -ErrorAction SilentlyContinue
    }
}

BuildOfflineImages
Get-FileHash -Algorithm SHA256 $OutputPath
