#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
if [[ -n "${1:-}" ]]; then
  readonly release_dir="$(cd "$1" && pwd -P)"
else
  readonly release_dir="$(readlink -f "${service_root}/current")"
fi
readonly verification_mode="${2:---full}"
readonly compose_file="${release_dir}/deploy/server/compose.yaml"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly readonly_env="${service_root}/shared/postgres-readonly.env"
readonly acme_root="${service_root}/shared/acme-alidns"
readonly acme_hook_root="${service_root}/shared/acme-hooks"
readonly acme_credentials="${acme_root}/credentials.json"
readonly acme_marker="${acme_root}/last-successful-staging"
readonly preview_port="${MAIL_SUITE_PREVIEW_PORT:-18444}"

export MAIL_SUITE_IMAGE_TAG="${MAIL_SUITE_IMAGE_TAG:-test-20260905-rc7}"
export MAIL_SUITE_VERSION="${MAIL_SUITE_VERSION:-test}"
export MAIL_SUITE_REVISION="${MAIL_SUITE_REVISION:-working-tree}"
export MAIL_SUITE_PREVIEW_PORT="${preview_port}"
stalwart_cli_env=""

# cleanup_temporary_files 保证验证失败时不遗留短期管理员凭据文件。
cleanup_temporary_files() {
  if [[ -n "${stalwart_cli_env}" && -f "${stalwart_cli_env}" ]]; then
    rm -f "${stalwart_cli_env}"
  fi
}
trap cleanup_temporary_files EXIT

if [[ "${verification_mode}" != "--internal" && "${verification_mode}" != "--full" ]]; then
  echo "验证模式只能是 --internal 或 --full" >&2
  exit 1
fi

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

# verify_test_mailbox_provisioning 核验活动 fixture 已收敛且停用 fixture 未创建 mail-core 任务。
verify_test_mailbox_provisioning() {
  local mailbox_id postgres_id result suspended_mailbox_id
  mailbox_id="$(jq -er '.mailbox.mailbox_id' "${service_root}/shared/identity/test-identity.json")"
  suspended_mailbox_id="$(
    jq -er '.suspended.mailbox_id' "${service_root}/shared/identity/test-identity.json"
  )"
  postgres_id="$(compose ps -q postgres)"
  result="$(
    docker exec -i "${postgres_id}" \
      psql -X -qAt -v ON_ERROR_STOP=1 \
      -v mailbox_id="${mailbox_id}" \
      -v suspended_mailbox_id="${suspended_mailbox_id}" \
      -U mail_suite -d mail_suite <<'SQL'
SELECT (
  SELECT count(*) = 1
    AND bool_and(operations.status = 'succeeded')
    AND bool_and(mailboxes.desired_status = 'active')
    AND bool_and(mailboxes.observed_status = 'active')
    AND bool_and(mailboxes.observed_revision = mailboxes.revision)
    AND bool_and(mailboxes.observed_configuration_hash = operations.configuration_hash)
  FROM mailboxes
  JOIN operations
    ON operations.resource_id = mailboxes.id
   AND operations.kind = 'mailbox.provision'
  WHERE mailboxes.id = :'mailbox_id'::uuid
)
AND NOT EXISTS (
  SELECT 1
  FROM operations
  WHERE resource_id = :'suspended_mailbox_id'::uuid
);
SQL
  )"
  if [[ "${result}" != "t" ]]; then
    echo "活动测试邮箱 operation 尚未收敛或停用 fixture 意外创建了任务" >&2
    exit 1
  fi
  echo "活动测试邮箱 operation 与停用 fixture 边界验证通过"
}

# runtime_value 精确读取唯一运行值，不执行或回显 env 文件内容。
runtime_value() {
  local name="$1" count
  count="$(grep -c "^${name}=" "${runtime_env}" || true)"
  if [[ "${count}" != "1" ]]; then
    return 1
  fi
  sed -n "s/^${name}=//p" "${runtime_env}"
}

# verify_secret_boundaries 核对运行 secret、身份文件和 recovery 清理状态。
verify_secret_boundaries() {
  local path expected recovery_value
  if [[ "$(stat -c '%a:%U:%G' "${runtime_env}" 2>/dev/null || true)" != "600:root:root" ]]; then
    echo "runtime.env 权限无效" >&2
    exit 1
  fi
  if grep -q '^MAIL_SUITE_STALWART_RECOVERY_ADMIN=' "${runtime_env}"; then
    echo "runtime.env 仍保留 Stalwart recovery 身份" >&2
    exit 1
  fi
  recovery_value="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(compose ps -q stalwart)" |
    sed -n 's/^STALWART_RECOVERY_ADMIN=//p')"
  if [[ -n "${recovery_value}" ]]; then
    echo "长期 Stalwart 容器仍持有 recovery 凭据" >&2
    exit 1
  fi

  for path in \
    "${service_root}/shared/secrets/worker/stalwart-admin-password" \
    "${service_root}/shared/secrets/worker/stalwart-mailbox-key"; do
    if [[ "$(stat -c '%a:%u:%g' "${path}" 2>/dev/null || true)" != "400:65532:65532" ]]; then
      echo "worker secret 权限无效" >&2
      exit 1
    fi
  done
  expected="400:1000:0"
  if [[ "$(stat -c '%a:%u:%g' "${service_root}/shared/identity/realm/mail-suite-test-realm.json" 2>/dev/null || true)" != "${expected}" ]]; then
    echo "Keycloak realm 文件权限无效" >&2
    exit 1
  fi
  if [[ "$(stat -c '%a:%u:%g' "${service_root}/shared/identity/test-identity.json" 2>/dev/null || true)" != "400:65532:65532" ]]; then
    echo "测试身份 manifest 权限无效" >&2
    exit 1
  fi
  echo "运行 secret、身份文件与 recovery 清理边界验证通过"
}

# verify_container_constraints 核对长期容器健康、内存、PID 和关键非 root 身份。
verify_container_constraints() {
  local service expected_memory expected_pids expected_user container_id actual
  while IFS='|' read -r service expected_memory expected_pids expected_user; do
    container_id="$(compose ps -q "${service}")"
    if [[ -z "${container_id}" ]]; then
      echo "长期服务缺少容器：${service}" >&2
      exit 1
    fi
    actual="$(docker inspect -f '{{.State.Health.Status}}|{{.HostConfig.Memory}}|{{.HostConfig.PidsLimit}}|{{.Config.User}}' "${container_id}")"
    if [[ "${actual}" != "healthy|${expected_memory}|${expected_pids}|${expected_user}" ]]; then
      echo "长期服务健康或资源边界无效：${service}" >&2
      exit 1
    fi
  done <<'CONTAINERS'
postgres|402653184|128|
keycloak|536870912|256|1000
stalwart|335544320|256|stalwart
api|134217728|64|65532:65532
worker|134217728|64|65532:65532
web|100663296|64|nginx
CONTAINERS
  echo "长期容器健康、内存、PID 与运行身份验证通过"
}

# verify_published_ports 核对只有 Web 和 SMTP 服务拥有宿主机发布端口。
verify_published_ports() {
  local service container_id published unexpected
  for service in postgres keycloak api worker; do
    container_id="$(compose ps -q "${service}")"
    published="$(docker port "${container_id}" 2>/dev/null || true)"
    if [[ -n "${published}" ]]; then
      echo "内部服务意外发布宿主机端口：${service}" >&2
      exit 1
    fi
  done
  published="$(docker port "$(compose ps -q stalwart)")"
  unexpected="$(awk '$1 != "25/tcp" {print}' <<<"${published}")"
  if [[ -z "${published}" || -n "${unexpected}" || "${published}" != *":25"* ]]; then
    echo "Stalwart 只能发布 SMTP 25" >&2
    exit 1
  fi
  published="$(docker port "$(compose ps -q web)")"
  if ! grep -Eq '^8080/tcp -> (0\.0\.0\.0|\[::\]):80$' <<<"${published}" ||
    ! grep -Eq '^8443/tcp -> (0\.0\.0\.0|\[::\]):443$' <<<"${published}" ||
    ! grep -Fxq "8443/tcp -> 127.0.0.1:${preview_port}" <<<"${published}"; then
    echo "Web 缺少预期的 HTTP、HTTPS 或回环预览端口" >&2
    exit 1
  fi
  unexpected="$(awk -v preview="${preview_port}" '
    $0 !~ /^8080\/tcp -> (0\.0\.0\.0|\[::\]):80$/ &&
    $0 !~ /^8443\/tcp -> (0\.0\.0\.0|\[::\]):443$/ &&
    $0 != "8443/tcp -> 127.0.0.1:" preview {print}
  ' <<<"${published}")"
  if [[ -n "${unexpected}" ]]; then
    echo "Web 发布了未批准的宿主机端口" >&2
    exit 1
  fi
  echo "宿主机发布端口边界验证通过"
}

# verify_keycloak_database_access 验证 Keycloak 使用独立数据库 owner 登录。
verify_keycloak_database_access() {
  local password container_id result
  password="$(runtime_value MAIL_SUITE_KEYCLOAK_DB_PASSWORD)" || {
    echo "缺少 Keycloak 数据库密码" >&2
    exit 1
  }
  container_id="$(compose ps -q postgres)"
  result="$(docker exec -e PGPASSWORD="${password}" "${container_id}" \
    psql -X -qAt -v ON_ERROR_STOP=1 -h 127.0.0.1 -U keycloak -d keycloak \
    -c "SELECT current_user = 'keycloak' AND current_database() = 'keycloak'")"
  unset password
  if [[ "${result}" != "t" ]]; then
    echo "Keycloak 独立数据库登录验证失败" >&2
    exit 1
  fi
  echo "Keycloak 独立数据库登录验证通过"
}

# verify_https_routes 验证主站、固定 issuer 和 IdP 管理路径阻断。
verify_https_routes() {
  local discovery issuer admin_status redirect_status
  curl --fail --silent --show-error \
    --resolve "mail.test.snowye.fun:${preview_port}:127.0.0.1" \
    "https://mail.test.snowye.fun:${preview_port}/health/live" >/dev/null
  curl --fail --silent --show-error \
    --resolve "mail.test.snowye.fun:${preview_port}:127.0.0.1" \
    "https://mail.test.snowye.fun:${preview_port}/health/ready" >/dev/null
  discovery="$(curl --fail --silent --show-error \
    --resolve "idp.test.snowye.fun:${preview_port}:127.0.0.1" \
    "https://idp.test.snowye.fun:${preview_port}/realms/mail-suite-test/.well-known/openid-configuration")"
  issuer="$(jq -r '.issuer // empty' <<<"${discovery}")"
  if [[ "${issuer}" != "https://idp.test.snowye.fun/realms/mail-suite-test" ]]; then
    echo "Keycloak discovery issuer 与公网契约不一致" >&2
    exit 1
  fi
  admin_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --resolve "idp.test.snowye.fun:${preview_port}:127.0.0.1" \
    "https://idp.test.snowye.fun:${preview_port}/admin/")"
  if [[ "${admin_status}" != "404" ]]; then
    echo "Keycloak 管理路径未被公网 Nginx 阻断" >&2
    exit 1
  fi
  redirect_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --resolve "mail.test.snowye.fun:80:127.0.0.1" \
    "http://mail.test.snowye.fun/")"
  if [[ "${redirect_status}" != "301" ]]; then
    echo "HTTP 入口没有固定跳转 HTTPS" >&2
    exit 1
  fi
  echo "主站 HTTPS、OIDC issuer 与 IdP 路径隔离验证通过"
}

# verify_stalwart_tls 使用永久管理员和系统 CA 做无跳过校验的内部请求。
verify_stalwart_tls() {
  local password
  password="$(<"${service_root}/shared/secrets/worker/stalwart-admin-password")"
  stalwart_cli_env="$(mktemp "${service_root}/shared/stalwart-verify.XXXXXX")"
  chmod 0600 "${stalwart_cli_env}"
  printf 'STALWART_URL=https://mx1.test.snowye.fun\nSTALWART_USER=admin@test.snowye.fun\nSTALWART_PASSWORD=%s\n' \
    "${password}" >"${stalwart_cli_env}"
  if ! docker run --rm --network mail-suite-test_backend --env-file "${stalwart_cli_env}" \
    mail-suite-stalwart-cli:1.0.12-8831cc27 --no-color get SystemSettings --json >/dev/null; then
    echo "Stalwart 内部严格 TLS 或管理员认证失败" >&2
    exit 1
  fi
  cleanup_temporary_files
  stalwart_cli_env=""
  unset password
  if ! timeout 15 openssl s_client \
    -starttls smtp \
    -connect 127.0.0.1:25 \
    -servername mx1.test.snowye.fun \
    -verify_hostname mx1.test.snowye.fun \
    -verify_return_error </dev/null >/dev/null 2>&1; then
    echo "SMTP STARTTLS 公共证书验证失败" >&2
    exit 1
  fi
  echo "Stalwart 管理 HTTPS 与 SMTP STARTTLS 证书验证通过"
}

# marker_value 从无秘密 staging 标记读取唯一键值，不执行文件内容。
marker_value() {
  local key="$1" count value
  count="$(awk -F= -v key="${key}" '$1 == key {count += 1} END {print count + 0}' "${acme_marker}")"
  if [[ "${count}" != "1" ]]; then
    echo "自动续期 staging 标记缺少唯一字段：${key}" >&2
    exit 1
  fi
  value="$(sed -n "s/^${key}=//p" "${acme_marker}")"
  printf '%s' "${value}"
}

# verify_certificate_renewal 验证 AliDNS 自动 hook、lineage、演练标记和 timer。
verify_certificate_renewal() {
  local hostname renewal_file source_file installed_file expected_hash marker_hash tombstone
  if [[ "$(stat -c '%a:%U:%G' "${acme_credentials}" 2>/dev/null || true)" != "600:root:root" ]]; then
    echo "AliDNS 凭据文件权限无效" >&2
    exit 1
  fi
  if ! jq -e '
    type == "object"
    and (keys == ["access_key_id", "access_key_secret"])
    and (.access_key_id | type == "string" and length >= 16 and length <= 128)
    and (.access_key_secret | type == "string" and length >= 16 and length <= 128)
  ' "${acme_credentials}" >/dev/null; then
    echo "AliDNS 凭据文件结构无效" >&2
    exit 1
  fi

  for source_file in \
    alidns-dns-hook.py \
    alidns-dns-auth-hook.sh \
    alidns-dns-cleanup-hook.sh; do
    installed_file="${acme_hook_root}/${source_file}"
    if [[ "$(stat -c '%a:%U:%G' "${installed_file}" 2>/dev/null || true)" != "755:root:root" ]] ||
      ! cmp -s "${release_dir}/deploy/server/${source_file}" "${installed_file}"; then
      echo "共享 AliDNS hook 与当前 release 不一致：${source_file}" >&2
      exit 1
    fi
  done
  if [[ "$(stat -c '%a:%U:%G' /etc/letsencrypt/renewal-hooks/deploy/mail-suite 2>/dev/null || true)" != "755:root:root" ]] ||
    ! cmp -s "${release_dir}/deploy/server/certbot-deploy-hook.sh" \
      /etc/letsencrypt/renewal-hooks/deploy/mail-suite; then
    echo "Certbot deploy hook 与当前 release 不一致" >&2
    exit 1
  fi

  for hostname in mail.test.snowye.fun idp.test.snowye.fun mx1.test.snowye.fun; do
    renewal_file="/etc/letsencrypt/renewal/${hostname}.conf"
    grep -Fxq 'authenticator = manual' "${renewal_file}"
    grep -Eq '^pref_challs = .*dns' "${renewal_file}"
    grep -Fxq "manual_auth_hook = ${acme_hook_root}/alidns-dns-auth-hook.sh" "${renewal_file}"
    grep -Fxq "manual_cleanup_hook = ${acme_hook_root}/alidns-dns-cleanup-hook.sh" "${renewal_file}"
  done
  if find "${acme_root}/state" -maxdepth 1 -type f -name '*.json' -print -quit 2>/dev/null |
    grep -q .; then
    echo "存在未清理的 AliDNS RecordId 状态" >&2
    exit 1
  fi
  for hostname in mail.test.snowye.fun idp.test.snowye.fun mx1.test.snowye.fun; do
    tombstone="${acme_root}/tombstones/${hostname}.json"
    if [[ "$(stat -c '%a:%U:%G' "${tombstone}" 2>/dev/null || true)" != "600:root:root" ]]; then
      echo "缺少受限的 AliDNS cleanup 检查材料：${hostname}" >&2
      exit 1
    fi
  done

  if [[ "$(stat -c '%a:%U:%G' "${acme_marker}" 2>/dev/null || true)" != "600:root:root" ]]; then
    echo "自动续期 staging 标记权限无效" >&2
    exit 1
  fi
  if [[ "$(marker_value version)" != "1" ]] ||
    [[ ! "$(marker_value verified_at)" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
    [[ "$(marker_value certbot_version)" != "2.9.0" ]] ||
    [[ "$(marker_value lineages)" != "mail.test.snowye.fun,idp.test.snowye.fun,mx1.test.snowye.fun" ]]; then
    echo "自动续期 staging 标记内容无效" >&2
    exit 1
  fi
  for source_file in alidns-dns-hook.py alidns-dns-auth-hook.sh alidns-dns-cleanup-hook.sh; do
    expected_hash="$(sha256sum "${acme_hook_root}/${source_file}" | awk '{print $1}')"
    case "${source_file}" in
      alidns-dns-hook.py) marker_hash="$(marker_value client_sha256)" ;;
      alidns-dns-auth-hook.sh) marker_hash="$(marker_value auth_sha256)" ;;
      alidns-dns-cleanup-hook.sh) marker_hash="$(marker_value cleanup_sha256)" ;;
    esac
    if [[ "${marker_hash}" != "${expected_hash}" ]]; then
      echo "自动续期 hook 未通过当前版本 staging 演练：${source_file}" >&2
      exit 1
    fi
  done
  if [[ "$(systemctl is-enabled certbot.timer)" != "enabled" ]] ||
    ! systemctl is-active --quiet certbot.timer; then
    echo "certbot.timer 未处于 enabled/active" >&2
    exit 1
  fi
  echo "AliDNS DNS-01 自动续期配置、staging 演练与 timer 验证通过"
}

# verify_public_firewall 确认公网放行集合只有 SSH、SMTP 和 Web。
verify_public_firewall() {
  local port address
  for port in 25 80 443; do
    if ! ufw status | grep -Eq "(^|[[:space:]])${port}/tcp([[:space:]]|$).*ALLOW"; then
      echo "UFW 缺少批准端口：${port}" >&2
      exit 1
    fi
  done
  while read -r address; do
    port="${address##*:}"
    case "${port}" in
      22|25|80|443) ;;
      *)
        echo "发现未批准的非回环 TCP listener：${port}" >&2
        exit 1
        ;;
    esac
  done < <(ss -H -lnt | awk '$4 ~ /^(0\.0\.0\.0|\[::\]|\*):/ {print $4}')
  echo "公网防火墙和监听端口集合验证通过"
}

compose ps
compose --profile tools run --rm migrator --status
compose --profile tools run --rm identity-bootstrap --check --manifest /run/test-identity.json
bash "${release_dir}/deploy/server/configure-keycloak-amr.sh" "${release_dir}" --check
verify_secret_boundaries
verify_container_constraints
verify_published_ports
verify_readonly_database_access
verify_test_mailbox_provisioning
verify_keycloak_database_access
verify_https_routes
verify_stalwart_tls
verify_certificate_renewal
if [[ "${verification_mode}" == "--full" ]]; then
  verify_public_firewall
fi
