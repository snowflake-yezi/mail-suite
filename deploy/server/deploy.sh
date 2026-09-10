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
  local required
  for required in \
    "${compose_file}" \
    "${release_dir}/deploy/server/nginx-public.conf.template" \
    "${release_dir}/deploy/server/prepare-runtime.sh" \
    "${release_dir}/deploy/server/prepare-public-certificates.sh" \
    "${release_dir}/deploy/server/install-public-certificates.sh" \
    "${release_dir}/deploy/server/certbot-deploy-hook.sh" \
    "${release_dir}/deploy/server/alidns-dns-hook.py" \
    "${release_dir}/deploy/server/alidns-dns-auth-hook.sh" \
    "${release_dir}/deploy/server/alidns-dns-cleanup-hook.sh" \
    "${release_dir}/deploy/server/configure-alidns-certificate-renewal.sh" \
    "${release_dir}/deploy/server/configure-readonly-database.sh" \
    "${release_dir}/deploy/server/configure-keycloak-database.sh" \
    "${release_dir}/deploy/server/configure-keycloak-amr.sh" \
    "${release_dir}/deploy/server/reconcile-keycloak-client.py" \
    "${release_dir}/deploy/server/bootstrap-stalwart.sh" \
    "${release_dir}/deploy/server/verify.sh" \
    "${release_dir}/deploy/server/config/keycloak-realm.template.json" \
    "${release_dir}/deploy/server/config/test-identity.template.json" \
    "${release_dir}/deploy/server/config/stalwart-bootstrap.json"; do
    if [[ ! -f "${required}" ]]; then
      echo "发布包缺少必要部署文件" >&2
      exit 1
    fi
  done
}

# install_certificate_hooks 同步固定共享 hook，避免 renewal 配置依赖历史 release。
install_certificate_hooks() {
  local hook_root="${service_root}/shared/acme-hooks"
  install -d -o root -g root -m 0755 "${hook_root}"
  install -o root -g root -m 0755 \
    "${release_dir}/deploy/server/alidns-dns-hook.py" \
    "${hook_root}/alidns-dns-hook.py"
  install -o root -g root -m 0755 \
    "${release_dir}/deploy/server/alidns-dns-auth-hook.sh" \
    "${hook_root}/alidns-dns-auth-hook.sh"
  install -o root -g root -m 0755 \
    "${release_dir}/deploy/server/alidns-dns-cleanup-hook.sh" \
    "${hook_root}/alidns-dns-cleanup-hook.sh"
  install -d -o root -g root -m 0755 /etc/letsencrypt/renewal-hooks/deploy
  install -o root -g root -m 0755 \
    "${release_dir}/deploy/server/certbot-deploy-hook.sh" \
    /etc/letsencrypt/renewal-hooks/deploy/mail-suite
}

# compose 使用固定 project、受限环境文件和当前发布目录。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# require_preloaded_images 防止部署在镜像缺失时回退到公网浮动拉取。
require_preloaded_images() {
  local image
  local -a images=(
    "mail-suite-api:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-worker:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-migrator:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-identity-bootstrap:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-web:${MAIL_SUITE_IMAGE_TAG}"
    "mail-suite-postgres:17.11-alpine-18cfe3ef"
    "mail-suite-keycloak:26.7.3-88943b6a"
    "mail-suite-stalwart:0.16.19-34e59515"
    "mail-suite-stalwart-cli:1.0.12-8831cc27"
  )
  for image in "${images[@]}"; do
    if ! docker image inspect "${image}" >/dev/null 2>&1; then
      echo "缺少固定镜像：${image}" >&2
      exit 1
    fi
  done
}

# ensure_external_images 在线构建模式也只拉取固定 digest，并创建与离线包一致的本地别名。
ensure_external_images() {
  local mapping source target
  local -a mappings=(
    "postgres:17.11-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73|mail-suite-postgres:17.11-alpine-18cfe3ef"
    "quay.io/keycloak/keycloak:26.7.3@sha256:88943b6ad06d6293a239f0dfca5acec64218c9b3ab327bf9c936acf408a6ae3b|mail-suite-keycloak:26.7.3-88943b6a"
    "ghcr.io/stalwartlabs/stalwart:v0.16.19@sha256:34e59515ef633e2353abbd9a12b0a232eb2174df9ac1ee820ed13e952cfd801d|mail-suite-stalwart:0.16.19-34e59515"
    "ghcr.io/stalwartlabs/cli:1.0.12@sha256:8831cc276c22e334bcf473fc7d28e21248d748c4bc1dac43c83738d3cbd3a9d5|mail-suite-stalwart-cli:1.0.12-8831cc27"
  )
  for mapping in "${mappings[@]}"; do
    source="${mapping%%|*}"
    target="${mapping#*|}"
    if ! docker image inspect "${target}" >/dev/null 2>&1; then
      docker pull "${source}"
      docker image tag "${source}" "${target}"
    fi
  done
}

# ensure_public_certificates 在 DNS 前置满足时申请三条独立公网证书。
ensure_public_certificates() {
  if [[ -f "${service_root}/shared/tls/current/mail.test.snowye.fun/fullchain.pem" &&
        -f "${service_root}/shared/tls/current/mail.test.snowye.fun/privkey.pem" &&
        -f "${service_root}/shared/tls/current/idp.test.snowye.fun/fullchain.pem" &&
        -f "${service_root}/shared/tls/current/idp.test.snowye.fun/privkey.pem" &&
        -f "${service_root}/shared/tls/current/mx1.test.snowye.fun/fullchain.pem" &&
        -f "${service_root}/shared/tls/current/mx1.test.snowye.fun/privkey.pem" ]]; then
    return
  fi
  if [[ -z "${MAIL_SUITE_ACME_EMAIL:-}" || -z "${MAIL_SUITE_EXPECTED_PUBLIC_IPV4:-}" ]]; then
    echo "缺少公网证书；请同时设置 MAIL_SUITE_ACME_EMAIL 和 MAIL_SUITE_EXPECTED_PUBLIC_IPV4" >&2
    exit 1
  fi
  bash "${release_dir}/deploy/server/prepare-public-certificates.sh" "${release_dir}"
}

# configure_public_firewall 在服务通过内部验证后放行唯一公网业务端口。
configure_public_firewall() {
  ufw allow 25/tcp >/dev/null
  ufw allow 80/tcp >/dev/null
  ufw allow 443/tcp >/dev/null
}

# verify_internal_oidc_discovery 在启动 API 前验证正式 issuer 的 Docker 内网 TLS 路径。
verify_internal_oidc_discovery() {
  local attempt discovery end_session_endpoint
  for attempt in {1..30}; do
    if discovery="$(compose exec -T web wget -q -O - \
      "https://idp.test.snowye.fun/realms/mail-suite-test/.well-known/openid-configuration")"; then
      end_session_endpoint="$(jq -r '.end_session_endpoint // empty' <<<"${discovery}")"
      if [[ "${end_session_endpoint}" == "https://idp.test.snowye.fun/realms/mail-suite-test/protocol/openid-connect/logout" ]]; then
        echo "OIDC Docker 内网 discovery 与 end-session 路径已就绪"
        return
      fi
    fi
    sleep 1
  done
  echo "OIDC Docker 内网 discovery 路径未在 30 秒内就绪" >&2
  exit 1
}

# wait_test_mailbox_operation 等待活动测试邮箱由真实 worker 与 mail-core 收敛。
wait_test_mailbox_operation() {
  local attempt mailbox_id postgres_id result
  mailbox_id="$(jq -er '.mailbox.mailbox_id' "${service_root}/shared/identity/test-identity.json")"
  postgres_id="$(compose ps -q postgres)"
  for attempt in {1..90}; do
    result="$({
      docker exec -i "${postgres_id}" \
        psql -X -qAt -v ON_ERROR_STOP=1 -v mailbox_id="${mailbox_id}" \
        -U mail_suite -d mail_suite <<'SQL'
SELECT CASE
  WHEN operations.status = 'succeeded'
    AND mailboxes.observed_status = 'active'
    AND mailboxes.observed_revision = mailboxes.revision
    AND mailboxes.observed_configuration_hash = operations.configuration_hash
    THEN 'ready'
  WHEN operations.status IN ('failed', 'dead', 'superseded')
    THEN 'terminal'
  ELSE 'waiting'
END
FROM mailboxes
LEFT JOIN operations
  ON operations.resource_id = mailboxes.id
 AND operations.kind = 'mailbox.provision'
WHERE mailboxes.id = :'mailbox_id'::uuid;
SQL
    } || true)"
    case "${result}" in
      ready)
        echo "活动测试邮箱 operation 已由 mail-core 收敛"
        return
        ;;
      terminal)
        echo "活动测试邮箱 operation 进入失败终态" >&2
        exit 1
        ;;
    esac
    sleep 2
  done
  echo "活动测试邮箱 operation 未在 180 秒内收敛" >&2
  exit 1
}

# deploy_release 构建或加载镜像、显式初始化依赖并等待完整服务集就绪。
deploy_release() {
  export MAIL_SUITE_IMAGE_TAG="${MAIL_SUITE_IMAGE_TAG:-test-20260905-rc7}"
  export MAIL_SUITE_VERSION="${MAIL_SUITE_VERSION:-test}"
  export MAIL_SUITE_REVISION="${MAIL_SUITE_REVISION:-working-tree}"
  export MAIL_SUITE_PREVIEW_PORT="${MAIL_SUITE_PREVIEW_PORT:-18444}"

  bash "${release_dir}/deploy/server/prepare-runtime.sh" "${release_dir}"
  if [[ "${MAIL_SUITE_SKIP_BUILD:-0}" == "1" ]]; then
    require_preloaded_images
  else
    compose build api worker migrator identity-bootstrap web
    ensure_external_images
    require_preloaded_images
  fi
  ensure_public_certificates
  compose config --quiet

  compose up -d postgres
  compose --profile tools run --rm migrator --up
  bash "${release_dir}/deploy/server/configure-readonly-database.sh" "${release_dir}"
  bash "${release_dir}/deploy/server/configure-keycloak-database.sh" "${release_dir}"
  bash "${release_dir}/deploy/server/bootstrap-stalwart.sh" "${release_dir}"

  compose up -d --no-recreate keycloak web
  verify_internal_oidc_discovery
  python3 "${release_dir}/deploy/server/reconcile-keycloak-client.py" \
    --deployment-root "${release_dir}" --apply
  bash "${release_dir}/deploy/server/configure-keycloak-amr.sh" "${release_dir}" --apply
  compose --profile tools run --rm identity-bootstrap --check --manifest /run/test-identity.json
  compose --profile tools run --rm identity-bootstrap --apply --manifest /run/test-identity.json
  compose stop api web
  compose up -d --wait postgres keycloak stalwart web api worker
  wait_test_mailbox_operation

  install_certificate_hooks
  bash "${release_dir}/deploy/server/verify.sh" "${release_dir}" --internal
  ln -sfn "${release_dir}" "${service_root}/current"
  configure_public_firewall
}

require_root_and_files
deploy_release
echo "完整服务集已切换；仍需从 ECS 外部执行公网 HTTPS 与 SMTP 验收"
