#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"

# compose 使用固定 project 和受限 env 文件定位共享 PostgreSQL。
compose() {
  docker compose \
    --project-directory "${release_dir}" \
    --env-file "${runtime_env}" \
    -f "${compose_file}" \
    "$@"
}

# load_keycloak_password 读取唯一随机密码且不向输出回显。
load_keycloak_password() {
  local count password
  count="$(grep -c '^MAIL_SUITE_KEYCLOAK_DB_PASSWORD=' "${runtime_env}" || true)"
  password="$(sed -n 's/^MAIL_SUITE_KEYCLOAK_DB_PASSWORD=//p' "${runtime_env}")"
  if [[ "${count}" != "1" || ! "${password}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Keycloak 数据库密码配置无效" >&2
    exit 1
  fi
  printf '%s' "${password}"
}

# configure_keycloak_database 幂等创建独立 owner、数据库并复核最小角色属性。
configure_keycloak_database() {
  local container_id password verified
  container_id="$(compose ps -q postgres)"
  if [[ -z "${container_id}" || "$(docker inspect -f '{{.State.Running}}' "${container_id}")" != "true" ]]; then
    echo "PostgreSQL 容器未运行" >&2
    exit 1
  fi
  password="$(load_keycloak_password)"
  docker exec -i \
    -e MAIL_SUITE_KEYCLOAK_DB_PASSWORD="${password}" \
    "${container_id}" \
    psql -X -q -v ON_ERROR_STOP=1 -U mail_suite -d mail_suite >/dev/null <<'SQL'
\getenv keycloak_password MAIL_SUITE_KEYCLOAK_DB_PASSWORD

DO $body$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keycloak') THEN
    CREATE ROLE keycloak LOGIN;
  END IF;
END
$body$;

SELECT format('ALTER ROLE keycloak WITH LOGIN PASSWORD %L', :'keycloak_password') \gexec
ALTER ROLE keycloak
  NOSUPERUSER
  NOCREATEDB
  NOCREATEROLE
  NOINHERIT
  NOREPLICATION
  NOBYPASSRLS
  CONNECTION LIMIT 20;

SELECT 'CREATE DATABASE keycloak OWNER keycloak'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'keycloak')
\gexec

REVOKE ALL PRIVILEGES ON DATABASE keycloak FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE keycloak TO keycloak;
SQL

  verified="$(docker exec -i "${container_id}" psql -X -qAt -v ON_ERROR_STOP=1 -U mail_suite -d mail_suite <<'SQL'
SELECT EXISTS (
  SELECT 1
  FROM pg_database
  JOIN pg_roles ON pg_roles.oid = pg_database.datdba
  WHERE pg_database.datname = 'keycloak'
    AND pg_roles.rolname = 'keycloak'
)
AND EXISTS (
  SELECT 1
  FROM pg_roles
  WHERE rolname = 'keycloak'
    AND rolcanlogin
    AND NOT rolsuper
    AND NOT rolcreatedb
    AND NOT rolcreaterole
    AND NOT rolinherit
    AND NOT rolreplication
    AND NOT rolbypassrls
    AND rolconnlimit = 20
)
AND NOT EXISTS (
  SELECT 1
  FROM pg_auth_members
  WHERE member = (SELECT oid FROM pg_roles WHERE rolname = 'keycloak')
);
SQL
)"
  unset password
  if [[ "${verified}" != "t" ]]; then
    echo "Keycloak 数据库隔离验证失败" >&2
    exit 1
  fi
  echo "Keycloak 独立数据库与最小权限 owner 已配置"
}

if [[ "$(id -u)" -ne 0 || ! -f "${compose_file}" || ! -f "${runtime_env}" ]]; then
  echo "请使用 root 从有效发布目录配置 Keycloak 数据库" >&2
  exit 1
fi
configure_keycloak_database
