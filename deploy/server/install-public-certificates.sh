#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly tls_root="${service_root}/shared/tls"
readonly current_root="${tls_root}/current"
readonly archive_root="${tls_root}/releases"
readonly current_marker="${tls_root}/current-release"
readonly previous_marker="${tls_root}/previous-release"
readonly operation="${2:-}"
readonly -a hostnames=(
  "mail.test.snowye.fun"
  "idp.test.snowye.fun"
  "mx1.test.snowye.fun"
)
cli_env=""

# cleanup_cli_environment 保证 Stalwart 热加载使用的短期凭据文件不会残留。
cleanup_cli_environment() {
  if [[ -n "${cli_env}" && -f "${cli_env}" ]]; then
    rm -f "${cli_env}"
  fi
}
trap cleanup_cli_environment EXIT

# require_prerequisites 校验身份、证书来源和固定服务脚本边界。
require_prerequisites() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 安装公网证书" >&2
    exit 1
  fi
  if [[ ! -f "${release_dir}/deploy/server/compose.yaml" ]]; then
    echo "发布目录缺少服务器 Compose" >&2
    exit 1
  fi
  if [[ "${operation}" != "" && "${operation}" != "--reload" && "${operation}" != "--rollback" ]]; then
    echo "证书安装脚本只接受可选的 --reload 或 --rollback" >&2
    exit 1
  fi
  command -v openssl >/dev/null
}

# validate_archive 校验回滚标记和快照内三条证书的完整性。
validate_archive() {
  local archive_id="$1" archive_dir hostname certificate private_key certificate_key key_key
  if [[ ! "${archive_id}" =~ ^[0-9]{8}T[0-9]{6}Z-[0-9]+$ ]]; then
    echo "证书快照标记无效" >&2
    exit 1
  fi
  archive_dir="${archive_root}/${archive_id}"
  if [[ ! -d "${archive_dir}" ]]; then
    echo "证书回滚快照不存在" >&2
    exit 1
  fi
  for hostname in "${hostnames[@]}"; do
    certificate="${archive_dir}/${hostname}/fullchain.pem"
    private_key="${archive_dir}/${hostname}/privkey.pem"
    if [[ ! -f "${certificate}" || ! -f "${private_key}" ]]; then
      echo "证书回滚快照不完整：${hostname}" >&2
      exit 1
    fi
    openssl x509 -in "${certificate}" -noout -checkhost "${hostname}" >/dev/null
    if ! openssl x509 -in "${certificate}" -noout -checkend 86400 >/dev/null; then
      echo "证书回滚快照将在 24 小时内过期：${hostname}" >&2
      exit 1
    fi
    certificate_key="$(certificate_public_key_fingerprint "${certificate}")"
    key_key="$(private_key_fingerprint "${private_key}")"
    if [[ -z "${certificate_key}" || "${certificate_key}" != "${key_key}" ]]; then
      echo "证书回滚快照密钥不匹配：${hostname}" >&2
      exit 1
    fi
  done
  validate_distinct_keys "${archive_dir}"
}

# certificate_public_key_fingerprint 计算证书公钥摘要但不输出私钥内容。
certificate_public_key_fingerprint() {
  local certificate="$1"
  openssl x509 -in "${certificate}" -pubkey -noout |
    openssl pkey -pubin -outform DER 2>/dev/null |
    openssl dgst -sha256 -r | awk '{print $1}'
}

# private_key_fingerprint 计算私钥对应公钥摘要，供配对和隔离检查。
private_key_fingerprint() {
  local private_key="$1"
  openssl pkey -in "${private_key}" -pubout -outform DER 2>/dev/null |
    openssl dgst -sha256 -r | awk '{print $1}'
}

# validate_lineage 验证 SAN、有效期、链文件和私钥配对。
validate_lineage() {
  local hostname="$1" live_dir certificate private_key certificate_key key_key
  live_dir="/etc/letsencrypt/live/${hostname}"
  certificate="$(readlink -f "${live_dir}/fullchain.pem")"
  private_key="$(readlink -f "${live_dir}/privkey.pem")"
  if [[ ! -f "${certificate}" || ! -f "${private_key}" ]]; then
    echo "缺少 ${hostname} 的 Certbot lineage" >&2
    exit 1
  fi
  openssl x509 -in "${certificate}" -noout -checkhost "${hostname}" >/dev/null
  if ! openssl x509 -in "${certificate}" -noout -checkend 1209600 >/dev/null; then
    echo "${hostname} 证书有效期不足 14 天" >&2
    exit 1
  fi
  certificate_key="$(certificate_public_key_fingerprint "${certificate}")"
  key_key="$(private_key_fingerprint "${private_key}")"
  if [[ -z "${certificate_key}" || "${certificate_key}" != "${key_key}" ]]; then
    echo "${hostname} 证书与私钥不匹配" >&2
    exit 1
  fi
}

# validate_distinct_keys 要求三个公网职责使用互不复用的私钥。
validate_distinct_keys() {
  local source_root="${1:-/etc/letsencrypt/live}" mail_key idp_key mx_key
  mail_key="$(private_key_fingerprint "${source_root}/${hostnames[0]}/privkey.pem")"
  idp_key="$(private_key_fingerprint "${source_root}/${hostnames[1]}/privkey.pem")"
  mx_key="$(private_key_fingerprint "${source_root}/${hostnames[2]}/privkey.pem")"
  if [[ "${mail_key}" == "${idp_key}" || "${mail_key}" == "${mx_key}" || "${idp_key}" == "${mx_key}" ]]; then
    echo "公网 hostname 不得复用证书私钥" >&2
    exit 1
  fi
}

# archive_certificates 把本次 Certbot 结果保存为可精确回滚的只读快照。
archive_certificates() {
  local archive_id="$1" archive_dir hostname
  archive_dir="${archive_root}/${archive_id}"
  install -d -o root -g root -m 0700 "${archive_dir}"
  for hostname in "${hostnames[@]}"; do
    install -d -o root -g root -m 0700 "${archive_dir}/${hostname}"
    install -o root -g root -m 0444 \
      "/etc/letsencrypt/live/${hostname}/fullchain.pem" \
      "${archive_dir}/${hostname}/fullchain.pem"
    install -o root -g root -m 0400 \
      "/etc/letsencrypt/live/${hostname}/privkey.pem" \
      "${archive_dir}/${hostname}/privkey.pem"
  done
}

# publish_archive 原子替换稳定挂载目录内的证书文件并保留前一版本标记。
publish_archive() {
  local archive_id="$1" hostname owner temporary_cert temporary_key old_marker marker_temp
  install -d -o root -g root -m 0755 "${tls_root}" "${current_root}"
  for hostname in "${hostnames[@]}"; do
    owner=101
    if [[ "${hostname}" == "mx1.test.snowye.fun" ]]; then
      owner=2000
    fi
    install -d -o "${owner}" -g "${owner}" -m 0500 "${current_root}/${hostname}"
    temporary_cert="$(mktemp "${current_root}/${hostname}/fullchain.pem.XXXXXX")"
    temporary_key="$(mktemp "${current_root}/${hostname}/privkey.pem.XXXXXX")"
    install -o "${owner}" -g "${owner}" -m 0444 \
      "${archive_root}/${archive_id}/${hostname}/fullchain.pem" "${temporary_cert}"
    install -o "${owner}" -g "${owner}" -m 0400 \
      "${archive_root}/${archive_id}/${hostname}/privkey.pem" "${temporary_key}"
    mv -f "${temporary_cert}" "${current_root}/${hostname}/fullchain.pem"
    mv -f "${temporary_key}" "${current_root}/${hostname}/privkey.pem"
  done

  old_marker="$(cat "${current_marker}" 2>/dev/null || true)"
  if [[ -n "${old_marker}" && -d "${archive_root}/${old_marker}" ]]; then
    printf '%s\n' "${old_marker}" >"${previous_marker}"
    chmod 0600 "${previous_marker}"
  fi
  marker_temp="$(mktemp "${tls_root}/current-release.XXXXXX")"
  printf '%s\n' "${archive_id}" >"${marker_temp}"
  chmod 0600 "${marker_temp}"
  mv -f "${marker_temp}" "${current_marker}"
}

# compose 只使用受限 env 文件定位当前服务，不展开配置到输出。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${service_root}/shared/runtime.env" \
    -f "${release_dir}/deploy/server/compose.yaml" \
    "$@"
}

# reload_running_services 在文件验证后分别热加载 Nginx 与 Stalwart TLS。
reload_running_services() {
  local web_id stalwart_id admin_password
  web_id="$(compose ps -q web 2>/dev/null || true)"
  if [[ -n "${web_id}" && "$(docker inspect -f '{{.State.Running}}' "${web_id}")" == "true" ]]; then
    compose exec -T web nginx -t
    compose exec -T web nginx -s reload
  fi

  stalwart_id="$(compose ps -q stalwart 2>/dev/null || true)"
  if [[ -n "${stalwart_id}" && "$(docker inspect -f '{{.State.Running}}' "${stalwart_id}")" == "true" ]]; then
    if [[ ! -f "${service_root}/shared/secrets/worker/stalwart-admin-password" ]]; then
      echo "Stalwart 正在运行但永久管理员 secret 缺失" >&2
      exit 1
    fi
    admin_password="$(<"${service_root}/shared/secrets/worker/stalwart-admin-password")"
    cli_env="$(mktemp "${service_root}/shared/stalwart-cli.XXXXXX")"
    chmod 0600 "${cli_env}"
    printf 'STALWART_URL=https://mx1.test.snowye.fun\nSTALWART_USER=admin@test.snowye.fun\nSTALWART_PASSWORD=%s\n' \
      "${admin_password}" >"${cli_env}"
    docker run --rm --network mail-suite-test_backend --env-file "${cli_env}" \
      mail-suite-stalwart-cli:1.0.12-8831cc27 --no-color \
      create Action/ReloadTlsCertificates --json '{}' >/dev/null
    cleanup_cli_environment
    cli_env=""
    unset admin_password
  fi
}

require_prerequisites
if [[ "${operation}" == "--rollback" ]]; then
  rollback_id="$(cat "${previous_marker}" 2>/dev/null || true)"
  validate_archive "${rollback_id}"
  publish_archive "${rollback_id}"
  reload_running_services
  echo "已回滚到上一公网证书快照：${rollback_id}"
  exit 0
fi
for hostname in "${hostnames[@]}"; do
  validate_lineage "${hostname}"
done
validate_distinct_keys
install -d -o root -g root -m 0700 "${archive_root}"
archive_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
archive_certificates "${archive_id}"
publish_archive "${archive_id}"
if [[ "${operation}" == "--reload" ]]; then
  reload_running_services
fi
echo "三个独立公网证书已验证并安装：${archive_id}"
