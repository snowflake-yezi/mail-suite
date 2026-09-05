#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly bootstrap_file="${release_dir}/deploy/server/config/stalwart-bootstrap.json"
readonly worker_secret_dir="${service_root}/shared/secrets/worker"
readonly admin_password_file="${worker_secret_dir}/stalwart-admin-password"
readonly cli_image="mail-suite-stalwart-cli:1.0.12-8831cc27"
cli_env=""

# cleanup_secret_file 保证临时 CLI 环境文件不会在成功或失败后残留。
cleanup_secret_file() {
  if [[ -n "${cli_env}" && -f "${cli_env}" ]]; then
    rm -f "${cli_env}"
  fi
}
trap cleanup_secret_file EXIT

# compose 使用固定 project 和受限 env 文件操作当前发布。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# runtime_value 精确读取唯一 env 值，不执行受限文件内容。
runtime_value() {
  local name="$1" count
  count="$(grep -c "^${name}=" "${runtime_env}" || true)"
  if [[ "${count}" != "1" ]]; then
    return 1
  fi
  sed -n "s/^${name}=//p" "${runtime_env}"
}

# write_cli_environment 创建不在命令参数中暴露 Basic Auth 的短期 env 文件。
write_cli_environment() {
  local url="$1" username="$2" password="$3"
  cleanup_secret_file
  cli_env="$(mktemp "${service_root}/shared/stalwart-cli.XXXXXX")"
  chmod 0600 "${cli_env}"
  printf 'STALWART_URL=%s\nSTALWART_USER=%s\nSTALWART_PASSWORD=%s\n' \
    "${url}" "${username}" "${password}" >"${cli_env}"
}

# run_cli 在 internal backend 网络调用固定版本客户端。
run_cli() {
  docker run --rm -i \
    --network mail-suite-test_backend \
    --env-file "${cli_env}" \
    "${cli_image}" \
    --no-color "$@"
}

# remove_recovery_value 原子删除 runtime.env 中的一次性 recovery 身份。
remove_recovery_value() {
  local temporary
  temporary="$(mktemp "${service_root}/shared/runtime.env.XXXXXX")"
  chmod 0600 "${temporary}"
  grep -v '^MAIL_SUITE_STALWART_RECOVERY_ADMIN=' "${runtime_env}" >"${temporary}"
  install -o root -g root -m 0600 "${temporary}" "${runtime_env}"
  rm -f "${temporary}"
}

# bootstrap_fresh_state 从 recovery 身份创建永久管理员并立即销毁 recovery 配置。
bootstrap_fresh_state() {
  local recovery recovery_username recovery_password bootstrap_output admin_username admin_password
  recovery="$(runtime_value MAIL_SUITE_STALWART_RECOVERY_ADMIN)" || {
    echo "首次 Stalwart bootstrap 缺少 recovery 身份" >&2
    exit 1
  }
  if [[ "${recovery}" != *:* ]]; then
    echo "Stalwart recovery 身份格式无效" >&2
    exit 1
  fi
  recovery_username="${recovery%%:*}"
  recovery_password="${recovery#*:}"

  compose up -d --wait stalwart
  write_cli_environment "http://stalwart:8080" "${recovery_username}" "${recovery_password}"
  bootstrap_output="$(run_cli update Bootstrap --stdin <"${bootstrap_file}")"
  admin_username="$(sed -n 's/.*username:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${bootstrap_output}")"
  admin_password="$(sed -n 's/.*secret:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"${bootstrap_output}")"
  if [[ "${admin_username}" != "admin@test.snowye.fun" || -z "${admin_password}" ]]; then
    echo "Stalwart bootstrap 未返回预期永久管理员" >&2
    exit 1
  fi
  install -d -o 65532 -g 65532 -m 0500 "${worker_secret_dir}"
  local password_temporary
  password_temporary="$(mktemp "${service_root}/shared/stalwart-admin-password.XXXXXX")"
  chmod 0600 "${password_temporary}"
  printf '%s\n' "${admin_password}" >"${password_temporary}"
  install -o 65532 -g 65532 -m 0400 "${password_temporary}" "${admin_password_file}"
  rm -f "${password_temporary}"
  unset recovery recovery_password admin_password bootstrap_output
  remove_recovery_value
  compose up -d --wait --force-recreate stalwart
}

# ensure_recovery_removed 清理异常中断后可能遗留的一次性 recovery 配置。
ensure_recovery_removed() {
  if grep -q '^MAIL_SUITE_STALWART_RECOVERY_ADMIN=' "${runtime_env}"; then
    remove_recovery_value
    compose up -d --wait --force-recreate stalwart
  else
    compose up -d --wait stalwart
  fi
}

# install_file_backed_certificate 幂等绑定 mx 证书并设为无 SNI 时的默认 TLS 证书。
install_file_backed_certificate() {
  local admin_password
  admin_password="$(<"${admin_password_file}")"
  write_cli_environment "https://mx1.test.snowye.fun" "admin@test.snowye.fun" "${admin_password}"
  run_cli --insecure apply --stdin >/dev/null <<'JSON'
{"@type":"upsert","object":"Certificate","matchOn":["subjectAlternativeNames"],"value":{"mail-suite-mx":{"certificate":{"@type":"File","filePath":"/run/public-tls/fullchain.pem"},"privateKey":{"@type":"File","filePath":"/run/public-tls/privkey.pem"},"subjectAlternativeNames":["mx1.test.snowye.fun"]}}}
{"@type":"update","object":"SystemSettings","id":"singleton","value":{"defaultCertificateId":"#mail-suite-mx"}}
JSON
  run_cli --insecure create Action/ReloadTlsCertificates --json '{}' >/dev/null
  run_cli get SystemSettings --json >/dev/null
  unset admin_password
}

if [[ "$(id -u)" -ne 0 || ! -f "${compose_file}" || ! -f "${runtime_env}" || ! -f "${bootstrap_file}" ]]; then
  echo "请使用 root 从有效发布目录初始化 Stalwart" >&2
  exit 1
fi
if [[ ! -f "${service_root}/shared/tls/current/mx1.test.snowye.fun/fullchain.pem" ]]; then
  echo "缺少已验证的 mx1.test.snowye.fun 公网证书" >&2
  exit 1
fi
if [[ ! -f "${admin_password_file}" ]]; then
  bootstrap_fresh_state
else
  ensure_recovery_removed
fi
if [[ "$(stat -c '%a:%u:%g' "${admin_password_file}")" != "400:65532:65532" ]]; then
  echo "Stalwart 永久管理员 secret 权限无效" >&2
  exit 1
fi
install_file_backed_certificate
echo "Stalwart 已完成无 recovery 凭据 bootstrap、证书绑定和严格 TLS 复核"
