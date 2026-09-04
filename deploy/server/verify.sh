#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(readlink -f "${service_root}/current")"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly readonly_env="${service_root}/shared/postgres-readonly.env"
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

# load_readonly_password 读取并校验只读账号密码，不向验证输出回显。
load_readonly_password() {
  local password
  if [[ "$(stat -c '%a:%U:%G' "${readonly_env}" 2>/dev/null || true)" != "600:root:root" ]]; then
    echo "数据库只读账号凭据文件权限无效" >&2
    exit 1
  fi
  password="$(sed -n 's/^MAIL_SUITE_POSTGRES_READONLY_PASSWORD=//p' "${readonly_env}")"
  if [[ ! "${password}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "数据库只读账号凭据文件格式无效" >&2
    exit 1
  fi
  printf '%s' "${password}"
}

# verify_readonly_database_access 验证网络隔离、登录、只读权限和未来表默认授权。
verify_readonly_database_access() {
  local container_id database_ip default_grant permission_result published_ports readonly_password
  container_id="$(compose ps -q postgres)"
  readonly_password="$(load_readonly_password)"

  published_ports="$(docker port "${container_id}" 2>/dev/null || true)"
  if [[ -n "${published_ports}" || -n "$(ss -H -lnt '( sport = :5432 or sport = :15432 )')" ]]; then
    echo "服务器不得发布或监听 PostgreSQL 端口" >&2
    exit 1
  fi
  if ufw status | grep -Eq '(^|[[:space:]])(5432|15432)(/tcp)?([[:space:]]|$)'; then
    echo "UFW 不得放行 PostgreSQL 端口" >&2
    exit 1
  fi
  if [[ "$(docker network inspect -f '{{.Internal}}' mail-suite-test_backend)" != "true" ]]; then
    echo "PostgreSQL backend 网络必须保持 internal" >&2
    exit 1
  fi
  database_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "mail-suite-test_backend").IPAddress}}' "${container_id}")"
  if [[ ! "${database_ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
    echo "PostgreSQL 容器内网地址无效" >&2
    exit 1
  fi

  permission_result="$(
    docker exec -i \
      -e PGPASSWORD="${readonly_password}" \
      "${container_id}" \
      psql -X -qAt -v ON_ERROR_STOP=1 -h 127.0.0.1 -U mail_suite_reader -d mail_suite <<'SQL'
SELECT current_user = 'mail_suite_reader'
  AND current_database() = 'mail_suite'
  AND current_setting('default_transaction_read_only')::boolean
  AND current_setting('statement_timeout')::interval = interval '30 seconds'
  AND current_setting('lock_timeout')::interval = interval '5 seconds'
  AND current_setting('idle_in_transaction_session_timeout')::interval = interval '60 seconds'
  AND has_schema_privilege(current_user, 'public', 'USAGE')
  AND NOT has_schema_privilege(current_user, 'public', 'CREATE')
  AND NOT has_database_privilege(current_user, current_database(), 'CREATE')
  AND COALESCE((
    SELECT bool_and(
      has_table_privilege(current_user, format('%I.%I', schemaname, tablename), 'SELECT')
      AND NOT has_table_privilege(
        current_user,
        format('%I.%I', schemaname, tablename),
        'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'
      )
    )
    FROM pg_tables
    WHERE schemaname = 'public'
  ), false)
  AND (
    SELECT NOT rolsuper
      AND NOT rolcreatedb
      AND NOT rolcreaterole
      AND NOT rolinherit
      AND NOT rolreplication
      AND NOT rolbypassrls
      AND rolconnlimit = 5
    FROM pg_roles
    WHERE rolname = current_user
  )
  AND NOT EXISTS (
    SELECT 1
    FROM pg_auth_members
    WHERE member = (SELECT oid FROM pg_roles WHERE rolname = current_user)
  );
SQL
  )"
  if [[ "${permission_result}" != "t" ]]; then
    echo "数据库只读账号权限验证失败" >&2
    exit 1
  fi

  if docker exec \
    -e PGPASSWORD="${readonly_password}" \
    "${container_id}" \
    psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -U mail_suite_reader -d mail_suite \
    -c 'UPDATE tenants SET updated_at = updated_at WHERE false' >/dev/null 2>&1; then
    echo "数据库只读账号意外获得写权限" >&2
    exit 1
  fi

  default_grant="$(
    docker exec -i "${container_id}" psql -X -qAt -v ON_ERROR_STOP=1 -U mail_suite -d mail_suite <<'SQL'
SELECT EXISTS (
  SELECT 1
  FROM pg_default_acl
  JOIN pg_roles AS owner ON owner.oid = pg_default_acl.defaclrole
  JOIN pg_namespace ON pg_namespace.oid = pg_default_acl.defaclnamespace
  CROSS JOIN LATERAL aclexplode(pg_default_acl.defaclacl) AS privilege
  JOIN pg_roles AS grantee ON grantee.oid = privilege.grantee
  WHERE owner.rolname = 'mail_suite'
    AND pg_namespace.nspname = 'public'
    AND pg_default_acl.defaclobjtype = 'r'
    AND grantee.rolname = 'mail_suite_reader'
    AND privilege.privilege_type = 'SELECT'
    AND NOT privilege.is_grantable
);
SQL
  )"
  if [[ "${default_grant}" != "t" ]]; then
    echo "数据库未来表默认只读授权验证失败" >&2
    exit 1
  fi

  unset readonly_password
  echo "PostgreSQL 只读账号与服务器网络隔离验证通过"
}

compose ps
curl --fail --silent --show-error --write-out '\n' "http://127.0.0.1:${preview_port}/health/live"
curl --fail --silent --show-error --write-out '\n' "http://127.0.0.1:${preview_port}/health/ready"
compose --profile tools run --rm migrator --status
verify_readonly_database_access
