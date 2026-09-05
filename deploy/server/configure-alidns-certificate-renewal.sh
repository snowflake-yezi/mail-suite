#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly operation="${2:---configure-if-missing}"
readonly source_root="${release_dir}/deploy/server"
readonly hook_root="${service_root}/shared/acme-hooks"
readonly acme_root="${service_root}/shared/acme-alidns"
readonly credentials_file="${acme_root}/credentials.json"
readonly marker_file="${acme_root}/last-successful-staging"
readonly auth_hook="${hook_root}/alidns-dns-auth-hook.sh"
readonly cleanup_hook="${hook_root}/alidns-dns-cleanup-hook.sh"
readonly deploy_hook="/etc/letsencrypt/renewal-hooks/deploy/mail-suite"
readonly -a hostnames=(
  "mail.test.snowye.fun"
  "idp.test.snowye.fun"
  "mx1.test.snowye.fun"
)
backup_dir=""

# restore_renewal_configuration 在 staging 迁移失败时恢复三条原配置并保持 timer 停止。
restore_renewal_configuration() {
  local exit_code=$? hostname
  trap - EXIT
  if [[ "${exit_code}" -ne 0 && -n "${backup_dir}" ]]; then
    for hostname in "${hostnames[@]}"; do
      cp --preserve=mode,ownership,timestamps \
        "${backup_dir}/${hostname}.conf" \
        "/etc/letsencrypt/renewal/${hostname}.conf"
    done
    rm -f "${marker_file}"
    echo "自动续期迁移失败；renewal 配置已恢复，certbot.timer 保持停止" >&2
  fi
  exit "${exit_code}"
}
trap restore_renewal_configuration EXIT

# require_prerequisites 校验 root、固定工具版本、发布制品和既有 lineage。
require_prerequisites() {
  local required hostname
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 配置 AliDNS 自动续期" >&2
    exit 1
  fi
  case "${operation}" in
    --configure-if-missing | --rotate-credentials) ;;
    *)
      echo "操作只能是 --configure-if-missing 或 --rotate-credentials" >&2
      exit 1
      ;;
  esac
  if [[ "$(certbot --version 2>&1)" != "certbot 2.9.0" ]]; then
    echo "目标机必须安装固定 certbot 2.9.0" >&2
    exit 1
  fi
  for required in python3 dig systemctl sha256sum; do
    if ! command -v "${required}" >/dev/null; then
      echo "目标机缺少自动续期依赖：${required}" >&2
      exit 1
    fi
  done
  for required in \
    "${source_root}/alidns-dns-hook.py" \
    "${source_root}/alidns-dns-auth-hook.sh" \
    "${source_root}/alidns-dns-cleanup-hook.sh" \
    "${source_root}/certbot-deploy-hook.sh"; do
    if [[ ! -f "${required}" ]]; then
      echo "发布包缺少自动续期制品" >&2
      exit 1
    fi
  done
  for hostname in "${hostnames[@]}"; do
    if [[ ! -f "/etc/letsencrypt/renewal/${hostname}.conf" ||
          ! -d "/etc/letsencrypt/live/${hostname}" ]]; then
      echo "缺少待迁移的证书 lineage：${hostname}" >&2
      exit 1
    fi
  done
}

# install_stable_hooks 把 Certbot hook 安装到不依赖历史 release 的共享路径。
install_stable_hooks() {
  install -d -o root -g root -m 0700 "${acme_root}"
  install -d -o root -g root -m 0755 "${hook_root}"
  install -o root -g root -m 0755 \
    "${source_root}/alidns-dns-hook.py" \
    "${hook_root}/alidns-dns-hook.py"
  install -o root -g root -m 0755 \
    "${source_root}/alidns-dns-auth-hook.sh" \
    "${auth_hook}"
  install -o root -g root -m 0755 \
    "${source_root}/alidns-dns-cleanup-hook.sh" \
    "${cleanup_hook}"
  install -d -o root -g root -m 0755 /etc/letsencrypt/renewal-hooks/deploy
  install -o root -g root -m 0755 \
    "${source_root}/certbot-deploy-hook.sh" \
    "${deploy_hook}"
}

# configure_credentials_if_required 仅通过目标机 TTY 隐藏录入或轮换凭据。
configure_credentials_if_required() {
  if [[ "${operation}" == "--rotate-credentials" || ! -f "${credentials_file}" ]]; then
    /usr/bin/python3 "${hook_root}/alidns-dns-hook.py" configure
  fi
  /usr/bin/python3 "${hook_root}/alidns-dns-hook.py" check
}

# backup_renewal_configuration 保存迁移前配置以支持失败自动恢复。
backup_renewal_configuration() {
  local hostname backup_root
  backup_root="${acme_root}/renewal-backups"
  install -d -o root -g root -m 0700 "${backup_root}"
  backup_dir="$(mktemp -d "${backup_root}/staging-XXXXXXXX")"
  chmod 0700 "${backup_dir}"
  for hostname in "${hostnames[@]}"; do
    cp --preserve=mode,ownership,timestamps \
      "/etc/letsencrypt/renewal/${hostname}.conf" \
      "${backup_dir}/${hostname}.conf"
  done
}

# reconfigure_lineages 由 Certbot 自身逐条执行 staging 续期并保存自动 hook 参数。
reconfigure_lineages() {
  local hostname
  if systemctl is-active --quiet certbot.service; then
    echo "certbot.service 正在运行，请等待本轮结束后重试" >&2
    exit 1
  fi
  systemctl stop certbot.timer
  rm -f "${marker_file}"
  backup_renewal_configuration
  for hostname in "${hostnames[@]}"; do
    certbot reconfigure \
      --non-interactive \
      --cert-name "${hostname}" \
      --authenticator manual \
      --preferred-challenges dns \
      --manual-auth-hook "${auth_hook}" \
      --manual-cleanup-hook "${cleanup_hook}"
  done
}

# verify_renewal_configuration 精确核对三条 lineage 已固定为自动 DNS hook。
verify_renewal_configuration() {
  local hostname renewal_file
  for hostname in "${hostnames[@]}"; do
    renewal_file="/etc/letsencrypt/renewal/${hostname}.conf"
    grep -Fxq "authenticator = manual" "${renewal_file}"
    grep -Eq '^pref_challs = .*dns' "${renewal_file}"
    grep -Fxq "manual_auth_hook = ${auth_hook}" "${renewal_file}"
    grep -Fxq "manual_cleanup_hook = ${cleanup_hook}" "${renewal_file}"
  done
  if find "${acme_root}/state" -maxdepth 1 -type f -name '*.json' -print -quit 2>/dev/null |
    grep -q .; then
    echo "staging 演练后仍有未清理的 AliDNS RecordId 状态" >&2
    exit 1
  fi
}

# publish_success_marker 记录 staging 结果和 hook 摘要，不保存 AccessKey 或 challenge。
publish_success_marker() {
  local temporary
  temporary="$(mktemp "${acme_root}/.last-successful-staging.XXXXXXXX")"
  chmod 0600 "${temporary}"
  {
    printf 'version=1\n'
    printf 'verified_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'certbot_version=2.9.0\n'
    printf 'lineages=mail.test.snowye.fun,idp.test.snowye.fun,mx1.test.snowye.fun\n'
    printf 'client_sha256=%s\n' "$(sha256sum "${hook_root}/alidns-dns-hook.py" | awk '{print $1}')"
    printf 'auth_sha256=%s\n' "$(sha256sum "${auth_hook}" | awk '{print $1}')"
    printf 'cleanup_sha256=%s\n' "$(sha256sum "${cleanup_hook}" | awk '{print $1}')"
  } >"${temporary}"
  chown root:root "${temporary}"
  mv -f "${temporary}" "${marker_file}"
}

require_prerequisites
install_stable_hooks
configure_credentials_if_required
reconfigure_lineages
verify_renewal_configuration
/usr/bin/python3 "${hook_root}/alidns-dns-hook.py" wait-cleanup
publish_success_marker
systemctl enable --now certbot.timer
if [[ "$(systemctl is-enabled certbot.timer)" != "enabled" ]] ||
  ! systemctl is-active --quiet certbot.timer; then
  echo "certbot.timer 未处于 enabled/active" >&2
  exit 1
fi
echo "三条证书已通过 AliDNS DNS-01 staging 演练并启用自动续期"
