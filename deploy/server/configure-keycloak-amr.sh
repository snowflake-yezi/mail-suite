#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly operation="${2:---apply}"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly realm_name="mail-suite-test"
readonly browser_flow="browser"
readonly otp_provider="auth-otp-form"
readonly config_alias="mail-suite-browser-otp-amr"
readonly reference_value="otp"
readonly reference_max_age="36000"
readonly kcadm_config="/tmp/mail-suite-kcadm-amr-${$}.config"

# compose 使用固定 project 和受限环境文件访问现有 Keycloak 容器。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# cleanup_cli_session 删除只存在于 Keycloak 容器 tmpfs 的短期 Admin CLI token。
cleanup_cli_session() {
  compose exec -T keycloak rm -f "${kcadm_config}" >/dev/null 2>&1 || true
}

# start_cli_session 从容器既有环境派生 CLI 环境登录，避免密码出现在宿主命令参数或日志。
start_cli_session() {
  if ! compose exec -T keycloak bash -lc '
    set -eu
    umask 077
    rm -f "$1"
    export KC_CLI_PASSWORD="${KC_BOOTSTRAP_ADMIN_PASSWORD}"
    /opt/keycloak/bin/kcadm.sh config credentials \
      --config "$1" \
      --server http://127.0.0.1:8080 \
      --realm master \
      --user "${KC_BOOTSTRAP_ADMIN_USERNAME}" >/dev/null 2>&1
    unset KC_CLI_PASSWORD
  ' bash "${kcadm_config}"; then
    echo "Keycloak Admin CLI 登录失败" >&2
    exit 1
  fi
  if [[ "$(compose exec -T keycloak stat -c '%a' "${kcadm_config}")" != "600" ]]; then
    echo "Keycloak Admin CLI 临时配置权限无效" >&2
    exit 1
  fi
}

# kc_get 读取受限 Admin REST 资源。
kc_get() {
  compose exec -T keycloak /opt/keycloak/bin/kcadm.sh get \
    --config "${kcadm_config}" "$@"
}

# kc_create 创建受限 Admin REST 资源。
kc_create() {
  compose exec -T keycloak /opt/keycloak/bin/kcadm.sh create \
    --config "${kcadm_config}" "$@"
}

# kc_update 更新本脚本拥有的 authenticator config。
kc_update() {
  compose exec -T keycloak /opt/keycloak/bin/kcadm.sh update \
    --config "${kcadm_config}" "$@"
}

# kc_delete 删除已精确验证归属的 authenticator config。
kc_delete() {
  compose exec -T keycloak /opt/keycloak/bin/kcadm.sh delete \
    --config "${kcadm_config}" "$@"
}

# read_realm_boundary 只读取验证 SSO/flow 边界所需字段，避免输出 client 或用户秘密。
read_realm_boundary() {
  kc_get "realms/${realm_name}" \
    --fields realm,browserFlow,ssoSessionIdleTimeout,ssoSessionMaxLifespan
}

# read_browser_executions 读取当前 browser flow 的 execution 列表。
read_browser_executions() {
  kc_get "realms/${realm_name}/authentication/flows/${browser_flow}/executions"
}

# read_authenticator_config 以只读事务读取 REST 会脱敏的精确键值，禁止直接写 Keycloak 表。
read_authenticator_config() {
  local config_id="$1" postgres_id
  if [[ ! "${config_id}" =~ ^[0-9a-f-]{36}$ ]]; then
    echo "Keycloak authenticator config 标识格式无效" >&2
    return 1
  fi
  postgres_id="$(compose ps -q postgres)"
  if [[ -z "${postgres_id}" ]]; then
    echo "PostgreSQL 容器未运行" >&2
    return 1
  fi
  docker exec -i "${postgres_id}" \
    psql -X -qAt -v ON_ERROR_STOP=1 -v config_id="${config_id}" \
      -U mail_suite -d keycloak <<'SQL'
BEGIN TRANSACTION READ ONLY;
SELECT json_build_object(
  'id', authenticator_config.id,
  'alias', authenticator_config.alias,
  'config', COALESCE(
    json_object_agg(
      authenticator_config_entry.name,
      authenticator_config_entry.value
    ) FILTER (WHERE authenticator_config_entry.name IS NOT NULL),
    '{}'::json
  )
)
FROM authenticator_config
LEFT JOIN authenticator_config_entry
  ON authenticator_config_entry.authenticator_id = authenticator_config.id
WHERE authenticator_config.id = :'config_id'
GROUP BY authenticator_config.id, authenticator_config.alias;
COMMIT;
SQL
}

# validate_realm_boundary 固定 SSO 最大生命周期，避免 AMR 先过期但 SSO 不重新挑战。
validate_realm_boundary() {
  local realm_json="$1"
  if ! jq -e \
    --arg realm "${realm_name}" \
    --arg flow "${browser_flow}" \
    --argjson max_age "${reference_max_age}" \
    '.realm == $realm
      and .browserFlow == $flow
      and .ssoSessionMaxLifespan == $max_age' \
    <<<"${realm_json}" >/dev/null; then
    echo "Keycloak realm、browser flow 或 SSO 最大生命周期不符合 MFA 契约" >&2
    return 1
  fi
}

# find_otp_execution 要求当前 browser flow 中恰好一个 OTP execution。
find_otp_execution() {
  local executions_json="$1" count execution_id config_id
  count="$(jq -r --arg provider "${otp_provider}" \
    '[.[] | select(.providerId == $provider)] | length' <<<"${executions_json}")"
  if [[ "${count}" != "1" ]]; then
    echo "Keycloak browser flow 必须恰好包含一个 OTP execution" >&2
    return 1
  fi
  execution_id="$(jq -er --arg provider "${otp_provider}" \
    '.[] | select(.providerId == $provider) | .id' <<<"${executions_json}")"
  config_id="$(jq -r --arg provider "${otp_provider}" \
    '.[] | select(.providerId == $provider) | (.authenticationConfig // "")' \
    <<<"${executions_json}")"
  if [[ ! "${execution_id}" =~ ^[0-9a-f-]{36}$ ]] ||
    [[ -n "${config_id}" && ! "${config_id}" =~ ^[0-9a-f-]{36}$ ]]; then
    echo "Keycloak OTP execution 标识格式无效" >&2
    return 1
  fi
  printf '%s|%s' "${execution_id}" "${config_id}"
}

# config_is_exact 判断配置是否完全由本契约的 alias 和两个固定值组成。
config_is_exact() {
  local config_json="$1"
  jq -e \
    --arg alias "${config_alias}" \
    --arg reference "${reference_value}" \
    --arg max_age "${reference_max_age}" \
    '.alias == $alias
      and (.config | type == "object")
      and .config["default.reference.value"] == $reference
      and .config["default.reference.maxAge"] == $max_age
      and ((.config | keys | sort) == ["default.reference.maxAge", "default.reference.value"])' \
    <<<"${config_json}" >/dev/null
}

# config_is_owned 只按保留 alias 判断是否允许 reconcile 修正漂移。
config_is_owned() {
  local config_json="$1"
  jq -e --arg alias "${config_alias}" '.alias == $alias' <<<"${config_json}" >/dev/null
}

# create_owned_config 为无配置的 OTP execution 创建 AMR reference。
create_owned_config() {
  local execution_id="$1"
  kc_create "realms/${realm_name}/authentication/executions/${execution_id}/config" \
    -s "alias=${config_alias}" \
    -s "config.\"default.reference.value\"=${reference_value}" \
    -s "config.\"default.reference.maxAge\"=${reference_max_age}" >/dev/null 2>&1
}

# update_owned_config 将本脚本 alias 下的漂移恢复为精确契约。
update_owned_config() {
  local config_id="$1"
  kc_update "realms/${realm_name}/authentication/config/${config_id}" \
    -s "alias=${config_alias}" \
    -s "config.\"default.reference.value\"=${reference_value}" \
    -s "config.\"default.reference.maxAge\"=${reference_max_age}" >/dev/null 2>&1
}

# delete_owned_config 回滚已精确验证的 AMR config。
delete_owned_config() {
  local config_id="$1"
  kc_delete "realms/${realm_name}/authentication/config/${config_id}" >/dev/null
}

# inspect_current_config 返回 execution id、config id 和完整 config JSON。
inspect_current_config() {
  local executions_json ids execution_id config_id config_json=""
  if ! executions_json="$(read_browser_executions)"; then
    return 1
  fi
  if ! ids="$(find_otp_execution "${executions_json}")"; then
    return 1
  fi
  execution_id="${ids%%|*}"
  config_id="${ids#*|}"
  if [[ -n "${config_id}" ]]; then
    if ! config_json="$(read_authenticator_config "${config_id}")"; then
      return 1
    fi
  fi
  jq -cn \
    --arg execution_id "${execution_id}" \
    --arg config_id "${config_id}" \
    --argjson config "${config_json:-null}" \
    '{execution_id: $execution_id, config_id: $config_id, config: $config}'
}

# apply_config 创建缺失配置、修正自有漂移，并拒绝覆盖未知配置。
apply_config() {
  local state execution_id config_id config_json
  if ! state="$(inspect_current_config)"; then
    return 1
  fi
  execution_id="$(jq -r '.execution_id' <<<"${state}")"
  config_id="$(jq -r '.config_id' <<<"${state}")"
  if [[ -z "${config_id}" ]]; then
    if ! create_owned_config "${execution_id}"; then
      return 1
    fi
  else
    config_json="$(jq -c '.config' <<<"${state}")"
    if config_is_exact "${config_json}"; then
      return
    fi
    if ! config_is_owned "${config_json}"; then
      echo "Keycloak OTP execution 已绑定未知 authenticator config，拒绝覆盖" >&2
      return 1
    fi
    if ! update_owned_config "${config_id}"; then
      return 1
    fi
  fi
  check_config
}

# check_config 验证目标 execution 已绑定精确 AMR 配置。
check_config() {
  local state config_id config_json
  if ! state="$(inspect_current_config)"; then
    return 1
  fi
  config_id="$(jq -r '.config_id' <<<"${state}")"
  if [[ -z "${config_id}" ]]; then
    echo "Keycloak OTP execution 尚未绑定 AMR config" >&2
    return 1
  fi
  config_json="$(jq -c '.config' <<<"${state}")"
  if ! config_is_exact "${config_json}"; then
    echo "Keycloak OTP AMR config 与固定契约不一致" >&2
    return 1
  fi
}

# rollback_config 只删除精确匹配的自有配置，漂移时停止。
rollback_config() {
  local state config_id config_json after
  if ! state="$(inspect_current_config)"; then
    return 1
  fi
  config_id="$(jq -r '.config_id' <<<"${state}")"
  if [[ -z "${config_id}" ]]; then
    return
  fi
  config_json="$(jq -c '.config' <<<"${state}")"
  if ! config_is_exact "${config_json}"; then
    echo "Keycloak OTP AMR config 已漂移，拒绝自动回滚" >&2
    return 1
  fi
  if ! delete_owned_config "${config_id}"; then
    return 1
  fi
  if ! after="$(inspect_current_config)"; then
    return 1
  fi
  if [[ -n "$(jq -r '.config_id' <<<"${after}")" ]]; then
    echo "Keycloak OTP AMR config 回滚后仍存在" >&2
    return 1
  fi
}

# main 校验发布边界，建立短期会话并执行单一操作。
main() {
  if [[ "$(id -u)" -ne 0 ]] || [[ ! -f "${compose_file}" ]] || [[ ! -f "${runtime_env}" ]]; then
    echo "请使用 root 从有效发布目录配置 Keycloak AMR" >&2
    exit 1
  fi
  if ! command -v jq >/dev/null; then
    echo "目标机缺少 jq" >&2
    exit 1
  fi
  case "${operation}" in
    --apply|--check|--rollback) ;;
    *)
      echo "操作只能是 --apply、--check 或 --rollback" >&2
      exit 1
      ;;
  esac

  trap cleanup_cli_session EXIT
  start_cli_session
  validate_realm_boundary "$(read_realm_boundary)"
  case "${operation}" in
    --apply)
      apply_config
      echo "Keycloak OTP AMR config 已收敛"
      ;;
    --check)
      check_config
      echo "Keycloak OTP AMR config 验证通过"
      ;;
    --rollback)
      rollback_config
      echo "Keycloak OTP AMR config 已回滚"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
