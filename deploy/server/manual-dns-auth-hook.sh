#!/usr/bin/env bash
set -Eeuo pipefail

readonly state_root="/opt/mail-suite/shared/acme-manual"
readonly domain="${CERTBOT_DOMAIN:-}"
readonly validation="${CERTBOT_VALIDATION:-}"
readonly record="_acme-challenge.${domain}"
readonly challenge_file="${state_root}/${domain}.challenge"
readonly required_stable_checks=24
temporary=""

# cleanup_temporary_state 删除尚未原子发布的 challenge 临时文件。
cleanup_temporary_state() {
  if [[ -n "${temporary}" && -f "${temporary}" ]]; then
    rm -f "${temporary}"
  fi
}
trap cleanup_temporary_state EXIT

# require_challenge 校验 Certbot 输入只落在批准的三个测试主机名内。
require_challenge() {
  case "${domain}" in
    mail.test.snowye.fun | idp.test.snowye.fun | mx1.test.snowye.fun) ;;
    *)
      echo "拒绝未批准域名的 DNS-01 challenge" >&2
      exit 1
      ;;
  esac
  if [[ -z "${validation}" || "${validation}" == *$'\n'* || "${validation}" == *$'\r'* ]]; then
    echo "DNS-01 challenge 值无效" >&2
    exit 1
  fi
  if ! command -v dig >/dev/null; then
    echo "目标机缺少 dig" >&2
    exit 1
  fi
}

# resolver_has_value 要求指定公共解析器返回完整且精确的 TXT challenge。
resolver_has_value() {
  local resolver="$1"
  dig +time=5 +tries=1 +short TXT "${record}" "@${resolver}" |
    tr -d '"' |
    grep -Fxq "${validation}"
}

# publish_challenge 原子发布只允许 root 读取的人工 DNS 操作材料。
publish_challenge() {
  install -d -o root -g root -m 0700 "${state_root}"
  temporary="$(mktemp "${state_root}/.${domain}.XXXXXX")"
  chmod 0600 "${temporary}"
  printf 'record=%s\nvalue=%s\n' "${record}" "${validation}" >"${temporary}"
  mv -f "${temporary}" "${challenge_file}"
  temporary=""
}

# wait_for_public_dns 要求三家公共解析器连续两分钟看到 TXT 后再放行 ACME 校验。
wait_for_public_dns() {
  local attempt stable_checks=0
  for attempt in {1..480}; do
    if resolver_has_value 223.5.5.5 && \
      resolver_has_value 8.8.8.8 && \
      resolver_has_value 1.1.1.1; then
      ((stable_checks += 1))
      if [[ "${stable_checks}" -ge "${required_stable_checks}" ]]; then
        return
      fi
    else
      stable_checks=0
    fi
    sleep 5
  done
  echo "DNS-01 challenge 在 40 分钟内未达到公共解析连续稳定门槛" >&2
  exit 1
}

require_challenge
publish_challenge
wait_for_public_dns
