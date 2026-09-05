#!/usr/bin/env bash
set -Eeuo pipefail

readonly service_root="/opt/mail-suite"
readonly release_dir="$(cd "${1:-.}" && pwd -P)"
readonly runtime_env="${service_root}/shared/runtime.env"
readonly webroot="${service_root}/shared/acme-webroot"
readonly readiness_file="${webroot}/.well-known/acme-challenge/mail-suite-ready"
readonly expected_ipv4="${MAIL_SUITE_EXPECTED_PUBLIC_IPV4:-}"
readonly acme_email="${MAIL_SUITE_ACME_EMAIL:-}"
readonly image_tag="${MAIL_SUITE_IMAGE_TAG:-test-20260905-rc7}"
readonly bootstrap_container="mail-suite-acme-bootstrap"
readonly -a hostnames=(
  "mail.test.snowye.fun"
  "idp.test.snowye.fun"
  "mx1.test.snowye.fun"
)
firewall_rule_added=0
bootstrap_started=0

# cleanup_bootstrap 停止临时 HTTP listener，并撤销只服务于 ACME 的临时 UFW 变更。
cleanup_bootstrap() {
  local exit_code=$?
  trap - EXIT
  if [[ "${bootstrap_started}" == "1" ]]; then
    docker rm -f "${bootstrap_container}" >/dev/null 2>&1 || true
  fi
  if [[ "${firewall_rule_added}" == "1" ]]; then
    ufw --force delete allow 80/tcp >/dev/null 2>&1 || true
  fi
  rm -f "${readiness_file}"
  exit "${exit_code}"
}
trap cleanup_bootstrap EXIT

# require_prerequisites 校验证书工具、联系信息、镜像和目录边界。
require_prerequisites() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 申请公网证书" >&2
    exit 1
  fi
  if [[ ! "${expected_ipv4}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
    echo "请通过 MAIL_SUITE_EXPECTED_PUBLIC_IPV4 提供目标 ECS IPv4" >&2
    exit 1
  fi
  if [[ ! "${acme_email}" =~ ^[^[:space:]@]+@[^[:space:]@]+$ ]]; then
    echo "请通过 MAIL_SUITE_ACME_EMAIL 提供 ACME 联系邮箱" >&2
    exit 1
  fi
  if [[ "$(certbot --version 2>&1)" != "certbot 2.9.0" ]]; then
    echo "目标机必须安装固定 certbot 2.9.0" >&2
    exit 1
  fi
  if ! command -v curl >/dev/null; then
    echo "目标机缺少 curl" >&2
    exit 1
  fi
  if ! docker image inspect "mail-suite-web:${image_tag}" >/dev/null 2>&1; then
    echo "缺少用于 ACME HTTP challenge 的固定 Web 镜像" >&2
    exit 1
  fi
  if [[ ! -f "${runtime_env}" ]]; then
    echo "缺少 runtime.env，请先生成运行材料" >&2
    exit 1
  fi
}

# verify_dns 要求每个 hostname 都唯一包含目标 ECS IPv4。
verify_dns() {
  local hostname addresses
  for hostname in "${hostnames[@]}"; do
    addresses="$(getent ahostsv4 "${hostname}" | awk '{print $1}' | sort -u)"
    if [[ "${addresses}" != "${expected_ipv4}" ]]; then
      echo "${hostname} 的 A 记录尚未唯一指向目标 ECS" >&2
      exit 1
    fi
  done
}

# start_http_challenge_server 使用待部署 Web 镜像提供最小 ACME webroot。
start_http_challenge_server() {
  local attempt
  if [[ -n "$(ss -H -lnt '( sport = :80 )')" ]]; then
    echo "宿主机 80 端口已被占用，拒绝覆盖现有 listener" >&2
    exit 1
  fi
  install -d -o root -g root -m 0755 "${webroot}/.well-known/acme-challenge"
  printf 'ready\n' >"${readiness_file}"
  chmod 0644 "${readiness_file}"
  docker run --rm -d \
    --name "${bootstrap_container}" \
    --read-only \
    --security-opt no-new-privileges:true \
    --tmpfs /run:uid=101,gid=101,mode=0755 \
    --tmpfs /var/cache/nginx:uid=101,gid=101,mode=0755 \
    --tmpfs /etc/nginx/conf.d:uid=101,gid=101,mode=0755 \
    -e MAIL_SUITE_API_UPSTREAM=http://127.0.0.1:9 \
    -p 80:8080 \
    -v "${webroot}/.well-known/acme-challenge:/usr/share/nginx/html/.well-known/acme-challenge:ro" \
    "mail-suite-web:${image_tag}" >/dev/null
  bootstrap_started=1
  for attempt in {1..20}; do
    if [[ "$(curl --fail --silent --show-error \
      -H 'Host: mail.test.snowye.fun' \
      "http://127.0.0.1/.well-known/acme-challenge/mail-suite-ready" 2>/dev/null || true)" == "ready" ]]; then
      rm -f "${readiness_file}"
      break
    fi
    if [[ "${attempt}" == "20" ]]; then
      echo "ACME 临时 HTTP listener 未在约定时间内就绪" >&2
      exit 1
    fi
    sleep 1
  done
  if ! ufw status | grep -Eq '(^|[[:space:]])80/tcp([[:space:]]|$).*ALLOW'; then
    ufw allow 80/tcp >/dev/null
    firewall_rule_added=1
  fi
}

# request_lineages 分别申请三条 ECDSA lineage，确保私钥互不复用。
request_lineages() {
  local hostname
  for hostname in "${hostnames[@]}"; do
    certbot certonly \
      --non-interactive \
      --agree-tos \
      --email "${acme_email}" \
      --webroot \
      --webroot-path "${webroot}" \
      --cert-name "${hostname}" \
      --domain "${hostname}" \
      --key-type ecdsa \
      --elliptic-curve secp384r1 \
      --keep-until-expiring
  done
}

# install_renewal_hook 让 Certbot timer 在续期后验证、安装并热加载全部证书。
install_renewal_hook() {
  install -d -o root -g root -m 0755 /etc/letsencrypt/renewal-hooks/deploy
  install -o root -g root -m 0755 \
    "${release_dir}/deploy/server/certbot-deploy-hook.sh" \
    /etc/letsencrypt/renewal-hooks/deploy/mail-suite
  systemctl enable --now certbot.timer
}

require_prerequisites
verify_dns
start_http_challenge_server
request_lineages
docker rm -f "${bootstrap_container}" >/dev/null
bootstrap_started=0
"${release_dir}/deploy/server/install-public-certificates.sh" "${release_dir}"
install_renewal_hook
echo "公网证书已申请，Certbot 自动续期 hook 已安装"
