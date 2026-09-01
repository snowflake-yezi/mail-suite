#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(readlink -f "${service_root}/current")"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly preview_port="${MAIL_SUITE_PREVIEW_PORT:-18080}"

export MAIL_SUITE_IMAGE_TAG="${MAIL_SUITE_IMAGE_TAG:-test-20260831}"
export MAIL_SUITE_VERSION="${MAIL_SUITE_VERSION:-test}"
export MAIL_SUITE_REVISION="${MAIL_SUITE_REVISION:-working-tree}"
export MAIL_SUITE_PREVIEW_PORT="${preview_port}"

# compose 只读取受限环境文件，不输出展开后的秘密配置。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

compose ps
curl --fail --silent --show-error --write-out '\n' "http://127.0.0.1:${preview_port}/health/live"
curl --fail --silent --show-error --write-out '\n' "http://127.0.0.1:${preview_port}/health/ready"
compose --profile tools run --rm migrator --status
