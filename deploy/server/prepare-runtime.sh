#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly identity_root="${service_root}/shared/identity"
readonly realm_dir="${identity_root}/realm"
readonly realm_file="${realm_dir}/mail-suite-test-realm.json"
readonly manifest_file="${identity_root}/test-identity.json"
readonly worker_secret_dir="${service_root}/shared/secrets/worker"
readonly mailbox_key_file="${worker_secret_dir}/stalwart-mailbox-key"
readonly realm_template="${release_dir}/deploy/server/config/keycloak-realm.template.json"
readonly manifest_template="${release_dir}/deploy/server/config/test-identity.template.json"

# require_prerequisites 校验运行身份、发布模板和结构化 JSON 工具。
require_prerequisites() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 生成运行材料" >&2
    exit 1
  fi
  if [[ ! -f "${realm_template}" || ! -f "${manifest_template}" ]]; then
    echo "发布包缺少身份模板" >&2
    exit 1
  fi
  if ! command -v jq >/dev/null || ! command -v openssl >/dev/null; then
    echo "目标机缺少 jq 或 openssl" >&2
    exit 1
  fi
}

# random_hex 生成不会破坏 env 文件边界的 64 字符随机值。
random_hex() {
  openssl rand -hex 32
}

# random_base64 生成解码后为 32 字节的标准 Base64 值。
random_base64() {
  openssl rand -base64 32 | tr -d '\n'
}

# random_base32 生成 Keycloak TOTP 使用的无填充 Base32 secret。
random_base32() {
  openssl rand 20 | base32 | tr -d '=\n'
}

# random_recovery_admin 生成 Stalwart 首次 bootstrap 使用的一次性 basic-auth 身份。
random_recovery_admin() {
  printf 'recovery:%s' "$(random_hex)"
}

# runtime_value 按精确变量名读取唯一值，不执行 env 文件内容。
runtime_value() {
  local name="$1" count
  count="$(grep -c "^${name}=" "${runtime_env}" || true)"
  if [[ "${count}" != "1" ]]; then
    return 1
  fi
  sed -n "s/^${name}=//p" "${runtime_env}"
}

# append_runtime_value 原子追加单个已验证不会包含换行的运行值。
append_runtime_value() {
  local name="$1" value="$2" temporary
  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    echo "拒绝写入包含换行的运行配置" >&2
    exit 1
  fi
  temporary="$(mktemp "${service_root}/shared/runtime.env.XXXXXX")"
  chmod 0600 "${temporary}"
  cp "${runtime_env}" "${temporary}"
  printf '%s=%s\n' "${name}" "${value}" >>"${temporary}"
  install -o root -g root -m 0600 "${temporary}" "${runtime_env}"
  rm -f "${temporary}"
}

# ensure_runtime_value 保留已有稳定 secret，仅在首次缺失时生成。
ensure_runtime_value() {
  local name="$1" generator="$2" existing value
  if existing="$(runtime_value "${name}")"; then
    if [[ -z "${existing}" ]]; then
      echo "运行配置 ${name} 为空" >&2
      exit 1
    fi
    return
  fi
  if grep -q "^${name}=" "${runtime_env}"; then
    echo "运行配置 ${name} 重复" >&2
    exit 1
  fi
  value="$(${generator})"
  append_runtime_value "${name}" "${value}"
  unset value
}

# ensure_runtime_environment 创建并补齐完整服务集所需的稳定 secret。
ensure_runtime_environment() {
  install -d -o root -g root -m 0750 "${service_root}/shared"
  if [[ ! -f "${runtime_env}" ]]; then
    local initial
    initial="$(mktemp "${service_root}/shared/runtime.env.XXXXXX")"
    chmod 0600 "${initial}"
    printf 'MAIL_SUITE_POSTGRES_PASSWORD=%s\n' "$(random_hex)" >"${initial}"
    install -o root -g root -m 0600 "${initial}" "${runtime_env}"
    rm -f "${initial}"
  fi
  if [[ "$(stat -c '%a:%U:%G' "${runtime_env}")" != "600:root:root" ]]; then
    echo "runtime.env 权限必须是 0600 root:root" >&2
    exit 1
  fi

  ensure_runtime_value MAIL_SUITE_KEYCLOAK_DB_PASSWORD random_hex
  ensure_runtime_value MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD random_hex
  ensure_runtime_value MAIL_SUITE_TEST_USER_PASSWORD random_hex
  ensure_runtime_value MAIL_SUITE_TEST_ADMIN_OTP_SECRET random_base32
  ensure_runtime_value MAIL_SUITE_OIDC_CLIENT_SECRET random_hex
  ensure_runtime_value MAIL_SUITE_AUTH_SECRET_PEPPER random_base64
  ensure_runtime_value MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY random_base64

  if [[ ! -f "${worker_secret_dir}/stalwart-admin-password" ]]; then
    ensure_runtime_value MAIL_SUITE_STALWART_RECOVERY_ADMIN random_recovery_admin
  fi
}

# ensure_worker_mailbox_key 创建 worker 唯一可读的邮箱凭据派生密钥。
ensure_worker_mailbox_key() {
  install -d -o 65532 -g 65532 -m 0500 "${worker_secret_dir}"
  if [[ ! -f "${mailbox_key_file}" ]]; then
    local temporary
    temporary="$(mktemp "${service_root}/shared/stalwart-mailbox-key.XXXXXX")"
    chmod 0600 "${temporary}"
    random_base64 >"${temporary}"
    install -o 65532 -g 65532 -m 0400 "${temporary}" "${mailbox_key_file}"
    rm -f "${temporary}"
  fi
  if [[ "$(stat -c '%a:%u:%g' "${mailbox_key_file}")" != "400:65532:65532" ]]; then
    echo "Stalwart mailbox key 权限无效" >&2
    exit 1
  fi
}

# new_uuid 从内核随机源生成不进入日志的稳定测试标识。
new_uuid() {
  tr '[:upper:]' '[:lower:]' </proc/sys/kernel/random/uuid
}

# generate_identity_files 一次性生成相互一致的 realm 和控制面 manifest。
generate_identity_files() {
  if [[ -f "${realm_file}" && -f "${manifest_file}" ]]; then
    jq -e '.realm == "mail-suite-test"' "${realm_file}" >/dev/null
    jq -e '.oidc_issuer == "https://idp.test.snowye.fun/realms/mail-suite-test"' "${manifest_file}" >/dev/null
    return
  fi
  if [[ -e "${realm_file}" || -e "${manifest_file}" ]]; then
    echo "身份运行材料不完整，拒绝部分重建" >&2
    exit 1
  fi

  install -d -o root -g root -m 0751 "${identity_root}"
  install -d -o 1000 -g 0 -m 0500 "${realm_dir}"

  export MAIL_SUITE_BUILD_CLIENT_SECRET="$(runtime_value MAIL_SUITE_OIDC_CLIENT_SECRET)"
  export MAIL_SUITE_BUILD_TEST_PASSWORD="$(runtime_value MAIL_SUITE_TEST_USER_PASSWORD)"
  export MAIL_SUITE_BUILD_ADMIN_OTP="$(runtime_value MAIL_SUITE_TEST_ADMIN_OTP_SECRET)"
  export MAIL_SUITE_BUILD_MAILBOX_SUBJECT="$(new_uuid)"
  export MAIL_SUITE_BUILD_ADMIN_SUBJECT="$(new_uuid)"
  export MAIL_SUITE_BUILD_UNMAPPED_SUBJECT="$(new_uuid)"
  export MAIL_SUITE_BUILD_SUSPENDED_SUBJECT="$(new_uuid)"
  export MAIL_SUITE_BUILD_MFA_SUBJECT="$(new_uuid)"
  export MAIL_SUITE_BUILD_TENANT_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_DOMAIN_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_MAILBOX_PRINCIPAL_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_MAILBOX_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_ADMIN_PRINCIPAL_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_SUSPENDED_PRINCIPAL_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_SUSPENDED_MAILBOX_ID="$(new_uuid)"
  export MAIL_SUITE_BUILD_MFA_PRINCIPAL_ID="$(new_uuid)"

  local realm_temporary manifest_temporary
  realm_temporary="$(mktemp "${service_root}/shared/keycloak-realm.XXXXXX")"
  manifest_temporary="$(mktemp "${service_root}/shared/test-identity.XXXXXX")"
  chmod 0600 "${realm_temporary}" "${manifest_temporary}"
  jq '
    .clients[0].secret = env.MAIL_SUITE_BUILD_CLIENT_SECRET |
    .users[0].id = env.MAIL_SUITE_BUILD_MAILBOX_SUBJECT |
    .users[1].id = env.MAIL_SUITE_BUILD_ADMIN_SUBJECT |
    .users[2].id = env.MAIL_SUITE_BUILD_UNMAPPED_SUBJECT |
    .users[3].id = env.MAIL_SUITE_BUILD_SUSPENDED_SUBJECT |
    .users[4].id = env.MAIL_SUITE_BUILD_MFA_SUBJECT |
    .users[].credentials[0].value = env.MAIL_SUITE_BUILD_TEST_PASSWORD |
    .users[1].credentials[1].secretData = ({value: env.MAIL_SUITE_BUILD_ADMIN_OTP} | tojson)
  ' "${realm_template}" >"${realm_temporary}"
  jq '
    .tenant.id = env.MAIL_SUITE_BUILD_TENANT_ID |
    .domain.id = env.MAIL_SUITE_BUILD_DOMAIN_ID |
    .mailbox.principal_id = env.MAIL_SUITE_BUILD_MAILBOX_PRINCIPAL_ID |
    .mailbox.subject = env.MAIL_SUITE_BUILD_MAILBOX_SUBJECT |
    .mailbox.mailbox_id = env.MAIL_SUITE_BUILD_MAILBOX_ID |
    .administrator.principal_id = env.MAIL_SUITE_BUILD_ADMIN_PRINCIPAL_ID |
    .administrator.subject = env.MAIL_SUITE_BUILD_ADMIN_SUBJECT |
    .suspended.principal_id = env.MAIL_SUITE_BUILD_SUSPENDED_PRINCIPAL_ID |
    .suspended.subject = env.MAIL_SUITE_BUILD_SUSPENDED_SUBJECT |
    .suspended.mailbox_id = env.MAIL_SUITE_BUILD_SUSPENDED_MAILBOX_ID |
    .mfa_insufficient.principal_id = env.MAIL_SUITE_BUILD_MFA_PRINCIPAL_ID |
    .mfa_insufficient.subject = env.MAIL_SUITE_BUILD_MFA_SUBJECT |
    .unmapped_subject = env.MAIL_SUITE_BUILD_UNMAPPED_SUBJECT
  ' "${manifest_template}" >"${manifest_temporary}"

  install -o 1000 -g 0 -m 0400 "${realm_temporary}" "${realm_file}"
  install -o 65532 -g 65532 -m 0400 "${manifest_temporary}" "${manifest_file}"
  rm -f "${realm_temporary}" "${manifest_temporary}"
  unset MAIL_SUITE_BUILD_CLIENT_SECRET MAIL_SUITE_BUILD_TEST_PASSWORD MAIL_SUITE_BUILD_ADMIN_OTP
  unset MAIL_SUITE_BUILD_MAILBOX_SUBJECT MAIL_SUITE_BUILD_ADMIN_SUBJECT MAIL_SUITE_BUILD_UNMAPPED_SUBJECT
  unset MAIL_SUITE_BUILD_SUSPENDED_SUBJECT MAIL_SUITE_BUILD_MFA_SUBJECT MAIL_SUITE_BUILD_TENANT_ID
  unset MAIL_SUITE_BUILD_DOMAIN_ID MAIL_SUITE_BUILD_MAILBOX_PRINCIPAL_ID MAIL_SUITE_BUILD_MAILBOX_ID
  unset MAIL_SUITE_BUILD_ADMIN_PRINCIPAL_ID MAIL_SUITE_BUILD_SUSPENDED_PRINCIPAL_ID
  unset MAIL_SUITE_BUILD_SUSPENDED_MAILBOX_ID MAIL_SUITE_BUILD_MFA_PRINCIPAL_ID
}

require_prerequisites
ensure_runtime_environment
ensure_worker_mailbox_key
generate_identity_files
echo "运行 secret、Keycloak realm 和受控身份 manifest 已就绪"
