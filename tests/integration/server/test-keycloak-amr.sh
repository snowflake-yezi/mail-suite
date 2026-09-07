#!/usr/bin/env bash
set -Eeuo pipefail

readonly repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
source "${repository_root}/deploy/server/configure-keycloak-amr.sh"

readonly fixture_root="$(mktemp -d)"
trap 'rm -rf "${fixture_root}"' EXIT
realm_state='{"realm":"mail-suite-test","browserFlow":"browser","ssoSessionIdleTimeout":1800,"ssoSessionMaxLifespan":36000}'

# reset_fixture 创建一个没有 execution config 的有效 browser flow。
reset_fixture() {
  printf '%s' '[{"id":"11111111-1111-1111-1111-111111111111","providerId":"auth-otp-form"}]' \
    >"${fixture_root}/executions.json"
  rm -f "${fixture_root}/config.json" "${fixture_root}/action"
}

# read_realm_boundary 返回测试冻结的 SSO/flow 边界。
read_realm_boundary() {
  printf '%s' "${realm_state}"
}

# read_browser_executions 返回当前伪 execution 状态。
read_browser_executions() {
  cat "${fixture_root}/executions.json"
}

# read_authenticator_config 返回当前伪 config。
read_authenticator_config() {
  cat "${fixture_root}/config.json"
}

# attach_config 把伪 config id 关联到唯一 OTP execution。
attach_config() {
  jq '. = [{
    id: "11111111-1111-1111-1111-111111111111",
    providerId: "auth-otp-form",
    authenticationConfig: "22222222-2222-2222-2222-222222222222"
  }]' "${fixture_root}/executions.json" >"${fixture_root}/executions.next"
  mv "${fixture_root}/executions.next" "${fixture_root}/executions.json"
}

# create_owned_config 模拟 Admin REST 创建并记录动作。
create_owned_config() {
  printf '%s' create >"${fixture_root}/action"
  printf '%s' '{"id":"22222222-2222-2222-2222-222222222222","alias":"mail-suite-browser-otp-amr","config":{"default.reference.value":"otp","default.reference.maxAge":"36000"}}' \
    >"${fixture_root}/config.json"
  attach_config
}

# update_owned_config 模拟修复自有 alias 下的配置漂移。
update_owned_config() {
  printf '%s' update >"${fixture_root}/action"
  printf '%s' '{"id":"22222222-2222-2222-2222-222222222222","alias":"mail-suite-browser-otp-amr","config":{"default.reference.value":"otp","default.reference.maxAge":"36000"}}' \
    >"${fixture_root}/config.json"
}

# delete_owned_config 模拟精确回滚并解除 execution 关联。
delete_owned_config() {
  printf '%s' delete >"${fixture_root}/action"
  rm -f "${fixture_root}/config.json"
  reset_fixture
  printf '%s' delete >"${fixture_root}/action"
}

# expect_failure 要求给定操作安全失败。
expect_failure() {
  if "$@" >/dev/null 2>&1; then
    echo "Expected operation to fail: $*" >&2
    exit 1
  fi
}

reset_fixture
validate_realm_boundary "$(read_realm_boundary)"
apply_config
[[ "$(<"${fixture_root}/action")" == "create" ]]
check_config

rm -f "${fixture_root}/action"
apply_config
[[ ! -e "${fixture_root}/action" ]]

jq '.config["default.reference.maxAge"] = "1"' "${fixture_root}/config.json" \
  >"${fixture_root}/config.next"
mv "${fixture_root}/config.next" "${fixture_root}/config.json"
apply_config
[[ "$(<"${fixture_root}/action")" == "update" ]]

jq '.alias = "operator-owned"' "${fixture_root}/config.json" >"${fixture_root}/config.next"
mv "${fixture_root}/config.next" "${fixture_root}/config.json"
expect_failure apply_config
expect_failure rollback_config

jq '.alias = "mail-suite-browser-otp-amr"' "${fixture_root}/config.json" \
  >"${fixture_root}/config.next"
mv "${fixture_root}/config.next" "${fixture_root}/config.json"
rollback_config
[[ "$(<"${fixture_root}/action")" == "delete" ]]
expect_failure check_config

reset_fixture
printf '%s' '[]' >"${fixture_root}/executions.json"
expect_failure inspect_current_config
printf '%s' '[
  {"id":"11111111-1111-1111-1111-111111111111","providerId":"auth-otp-form"},
  {"id":"33333333-3333-3333-3333-333333333333","providerId":"auth-otp-form"}
]' >"${fixture_root}/executions.json"
expect_failure inspect_current_config

expect_failure validate_realm_boundary \
  '{"realm":"mail-suite-test","browserFlow":"browser","ssoSessionMaxLifespan":36001}'

echo "Keycloak OTP AMR create, idempotence, drift, ownership and rollback checks passed"
