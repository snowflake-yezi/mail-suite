#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"

# require_root_and_files 校验部署身份和发布包完整性。
require_root_and_files() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 执行部署脚本" >&2
    exit 1
  fi
  if [[ ! -f "${compose_file}" ]]; then
    echo "发布包缺少 deploy/server/compose.yaml" >&2
    exit 1
  fi
}

# ensure_runtime_secret 仅在首次部署时创建不回显的数据库密码。
ensure_runtime_secret() {
  if [[ -f "${runtime_env}" ]]; then
    return
  fi

  local password temporary_env
  password="$(openssl rand -hex 32)"
  temporary_env="$(mktemp "${service_root}/shared/runtime.env.XXXXXX")"
  chmod 0600 "${temporary_env}"
  printf 'MAIL_SUITE_POSTGRES_PASSWORD=%s\n' "${password}" >"${temporary_env}"
  install -o root -g root -m 0600 "${temporary_env}" "${runtime_env}"
  rm -f "${temporary_env}"
  unset password
}

# compose 使用固定 project、受限环境文件和当前发布目录。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# deploy_release 构建镜像、显式迁移并等待本机预览入口就绪。
deploy_release() {
  export MAIL_SUITE_IMAGE_TAG="${MAIL_SUITE_IMAGE_TAG:-test-20260831}"
  export MAIL_SUITE_VERSION="${MAIL_SUITE_VERSION:-test}"
  export MAIL_SUITE_REVISION="${MAIL_SUITE_REVISION:-working-tree}"
  export MAIL_SUITE_PREVIEW_PORT="${MAIL_SUITE_PREVIEW_PORT:-18080}"

  compose config --quiet
  if [[ "${MAIL_SUITE_SKIP_BUILD:-0}" == "1" ]]; then
    require_preloaded_images
  else
    compose build
  fi
  compose up -d postgres
  compose --profile tools run --rm migrator --up
  compose up -d --wait postgres api worker web
  ln -sfn "${release_dir}" "${service_root}/current"
}

# require_preloaded_images 防止离线部署在镜像缺失时回退到公网拉取。
require_preloaded_images() {
  local image
  local -a images=(
    "mail-suite-api:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-worker:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-migrator:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-identity-bootstrap:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-web:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-postgres:17.11-alpine-18cfe3ef"
  )
  for image in "${images[@]}"; do
    if ! docker image inspect "${image}" >/dev/null 2>&1; then
      echo "缺少离线镜像：${image}" >&2
      exit 1
    fi
  done
}

require_root_and_files
ensure_runtime_secret
deploy_release
