#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly readonly_env="${service_root}/shared/postgres-readonly.env"

# require_root_and_files 校验执行身份、部署配置和运行目录边界。
require_root_and_files() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 配置数据库只读账号" >&2
    exit 1
  fi
  if [[ ! -f "${compose_file}" || ! -f "${runtime_env}" ]]; then
    echo "缺少 Compose 配置或数据库运行环境文件" >&2
    exit 1
  fi
}

# ensure_readonly_secret 首次生成只保存在远端受限文件中的随机密码。
ensure_readonly_secret() {
  if [[ -f "${readonly_env}" ]]; then
    return
  fi

  local password temporary_env
  password="$(openssl rand -hex 32)"
  temporary_env="$(mktemp "${service_root}/shared/postgres-readonly.env.XXXXXX")"
  chmod 0600 "${temporary_env}"
  printf 'MAIL_SUITE_POSTGRES_READONLY_PASSWORD=%s\n' "${password}" >"${temporary_env}"
  install -o root -g root -m 0600 "${temporary_env}" "${readonly_env}"
  rm -f "${temporary_env}"
  unset password
}

# compose 使用固定 project、受限环境文件和指定发布目录定位 PostgreSQL 容器。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# load_readonly_password 读取并校验远端生成的十六进制密码，不向标准输出回显。
load_readonly_password() {
  local password
  password="$(sed -n 's/^MAIL_SUITE_POSTGRES_READONLY_PASSWORD=//p' "${readonly_env}")"
  if [[ ! "${password}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "数据库只读账号凭据文件格式无效" >&2
    exit 1
  fi
  printf '%s' "${password}"
}

# configure_readonly_role 创建最小权限登录角色并收敛当前及未来表授权。
configure_readonly_role() {
  local container_id readonly_password
  container_id="$(compose ps -q postgres)"
  if [[ -z "${container_id}" || "$(docker inspect -f '{{.State.Running}}' "${container_id}")" != "true" ]]; then
    echo "PostgreSQL 容器未运行" >&2
    exit 1
  fi

  readonly_password="$(load_readonly_password)"
  docker exec -i \
    -e MAIL_SUITE_POSTGRES_READONLY_PASSWORD="${readonly_password}" \
    "${container_id}" \
    psql -X -v ON_ERROR_STOP=1 -U mail_suite -d mail_suite >/dev/null <<'SQL'
\getenv reader_password MAIL_SUITE_POSTGRES_READONLY_PASSWORD

DO $body$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mail_suite_reader') THEN
    CREATE ROLE mail_suite_reader LOGIN;
  END IF;
END
$body$;

SELECT format(
  'ALTER ROLE mail_suite_reader WITH LOGIN PASSWORD %L',
  :'reader_password'
) \gexec

ALTER ROLE mail_suite_reader
  NOSUPERUSER
  NOCREATEDB
  NOCREATEROLE
  NOINHERIT
  NOREPLICATION
  NOBYPASSRLS
  CONNECTION LIMIT 5;
ALTER ROLE mail_suite_reader SET default_transaction_read_only TO on;
ALTER ROLE mail_suite_reader SET statement_timeout TO '30s';
ALTER ROLE mail_suite_reader SET lock_timeout TO '5s';
ALTER ROLE mail_suite_reader SET idle_in_transaction_session_timeout TO '60s';

SELECT format('REVOKE %I FROM mail_suite_reader', parent.rolname)
FROM pg_auth_members
JOIN pg_roles AS parent ON parent.oid = pg_auth_members.roleid
WHERE pg_auth_members.member = (SELECT oid FROM pg_roles WHERE rolname = 'mail_suite_reader')
\gexec

REVOKE ALL PRIVILEGES ON DATABASE mail_suite FROM mail_suite_reader;
GRANT CONNECT ON DATABASE mail_suite TO mail_suite_reader;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM mail_suite_reader;
GRANT USAGE ON SCHEMA public TO mail_suite_reader;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM mail_suite_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO mail_suite_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE mail_suite IN SCHEMA public
  REVOKE ALL PRIVILEGES ON TABLES FROM mail_suite_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE mail_suite IN SCHEMA public
  GRANT SELECT ON TABLES TO mail_suite_reader;
SQL
  unset readonly_password
  echo "数据库只读账号已配置，密码保存在远端受限凭据文件中"
}

require_root_and_files
ensure_readonly_secret
configure_readonly_role
