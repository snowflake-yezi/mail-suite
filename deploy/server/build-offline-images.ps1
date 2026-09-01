param(
    [string]$OutputPath = ".tmp/mail-suite-images-20260831.tar"
)

$ErrorActionPreference = "Stop"
$imageTag = "test-20260831"
$postgresImage = "postgres:17.11-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73"
$postgresAlias = "mail-suite-postgres:17.11-alpine-18cfe3ef"
$composeFile = "deploy/server/compose.yaml"
$dockerConfig = ".tmp/docker-config"

# Build images from the server Compose definition and export a verifiable archive.
function BuildOfflineImages {
    $env:MAIL_SUITE_POSTGRES_PASSWORD = "build-validation-only"
    $env:MAIL_SUITE_IMAGE_TAG = $imageTag
    $env:MAIL_SUITE_VERSION = "test"
    $env:MAIL_SUITE_REVISION = "working-tree"
    try {
        docker --config $dockerConfig compose --project-directory . -f $composeFile build api worker migrator web
        if ($LASTEXITCODE -ne 0) { throw "Compose image build failed" }

        docker --config $dockerConfig pull $postgresImage
        if ($LASTEXITCODE -ne 0) { throw "PostgreSQL image pull failed" }

        docker --config $dockerConfig image tag $postgresImage $postgresAlias
        if ($LASTEXITCODE -ne 0) { throw "PostgreSQL image tag failed" }

        docker --config $dockerConfig image save --output $OutputPath `
            "mail-suite-api:$imageTag" `
            "mail-suite-worker:$imageTag" `
            "mail-suite-migrator:$imageTag" `
            "mail-suite-web:$imageTag" `
            $postgresAlias
        if ($LASTEXITCODE -ne 0) { throw "Offline image archive export failed" }
    }
    finally {
        Remove-Item Env:MAIL_SUITE_POSTGRES_PASSWORD -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_IMAGE_TAG -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_VERSION -ErrorAction SilentlyContinue
        Remove-Item Env:MAIL_SUITE_REVISION -ErrorAction SilentlyContinue
    }
}

BuildOfflineImages
Get-FileHash -Algorithm SHA256 $OutputPath
