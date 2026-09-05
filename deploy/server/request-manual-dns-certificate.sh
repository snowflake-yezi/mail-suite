#!/usr/bin/env bash
set -Eeuo pipefail

readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly hostname="${2:-}"
readonly acme_email="${MAIL_SUITE_ACME_EMAIL:-}"
readonly auth_hook="${release_dir}/deploy/server/manual-dns-auth-hook.sh"
readonly challenge_file="/opt/mail-suite/shared/acme-manual/${hostname}.challenge"

# require_prerequisites 将人工 DNS 签发限制在批准的测试域名与固定 Certbot 版本。
require_prerequisites() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 申请人工 DNS 证书" >&2
    exit 1
  fi
  case "${hostname}" in
    mail.test.snowye.fun | idp.test.snowye.fun | mx1.test.snowye.fun) ;;
    *)
      echo "人工 DNS 证书域名不在批准范围" >&2
      exit 1
      ;;
  esac
  if [[ ! "${acme_email}" =~ ^[^[:space:]@]+@[^[:space:]@]+$ ]]; then
    echo "请通过 MAIL_SUITE_ACME_EMAIL 提供 ACME 联系邮箱" >&2
    exit 1
  fi
  if [[ "$(certbot --version 2>&1)" != "certbot 2.9.0" ]]; then
    echo "目标机必须安装固定 certbot 2.9.0" >&2
    exit 1
  fi
  if [[ ! -x "${auth_hook}" ]]; then
    echo "人工 DNS auth hook 缺失或不可执行" >&2
    exit 1
  fi
}

require_prerequisites
rm -f "${challenge_file}"
certbot certonly \
  --non-interactive \
  --agree-tos \
  --email "${acme_email}" \
  --manual \
  --preferred-challenges dns \
  --manual-auth-hook "${auth_hook}" \
  --cert-name "${hostname}" \
  --domain "${hostname}" \
  --key-type ecdsa \
  --elliptic-curve secp384r1 \
  --keep-until-expiring
echo "人工 DNS 公共证书已签发：${hostname}"
