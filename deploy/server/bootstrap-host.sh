#!/usr/bin/env bash
set -Eeuo pipefail

readonly docker_version="29.1.3-0ubuntu3~24.04.2"
readonly compose_version="2.40.3+ds1-0ubuntu1~24.04.1"
readonly buildx_version="0.30.1-0ubuntu1~24.04.1"
readonly service_root="/opt/mail-suite"
readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly release_root="$(cd "${script_dir}/../.." && pwd)"
readonly registry_mirror="${MAIL_SUITE_REGISTRY_MIRROR:-}"

# require_root 确保软件包、防火墙和系统目录变更只由 root 执行。
require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "请使用 root 执行主机引导脚本" >&2
    exit 1
  fi
}

# require_ubuntu_noble 防止把固定包版本误装到其他发行版。
require_ubuntu_noble() {
  # shellcheck disable=SC1091
  source /etc/os-release
  if [[ "${ID:-}" != "ubuntu" || "${VERSION_CODENAME:-}" != "noble" ]]; then
    echo "该脚本仅支持 Ubuntu 24.04 (noble)" >&2
    exit 1
  fi
}

# require_registry_mirror 只接受不含路径、凭据或查询参数的 HTTPS 加速地址。
require_registry_mirror() {
  if [[ ! "${registry_mirror}" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]]; then
    echo "请通过 MAIL_SUITE_REGISTRY_MIRROR 提供有效的 HTTPS registry mirror" >&2
    exit 1
  fi
}

# install_runtime 刷新索引、应用安全更新并安装固定版本容器运行时。
install_runtime() {
  local daemon_config
  apt-get update
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get upgrade -y
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y \
    "docker.io=${docker_version}" \
    "docker-compose-v2=${compose_version}" \
    "docker-buildx=${buildx_version}"

  install -d -o root -g root -m 0755 /etc/docker
  daemon_config="$(mktemp)"
  sed "s|__MAIL_SUITE_REGISTRY_MIRROR__|${registry_mirror}|g" \
    "${release_root}/deploy/server/daemon.json" >"${daemon_config}"
  install -o root -g root -m 0644 "${daemon_config}" /etc/docker/daemon.json
  rm -f "${daemon_config}"
  systemctl enable --now docker
  systemctl restart docker
}

# prepare_service_directories 隔离版本目录与不会随发布覆盖的运行秘密。
prepare_service_directories() {
  install -d -o root -g root -m 0750 \
    "${service_root}" \
    "${service_root}/releases" \
    "${service_root}/shared"
}

# configure_firewall 先保留当前 SSH 路径，再启用默认拒绝入站策略。
configure_firewall() {
  ufw allow OpenSSH
  ufw default deny incoming
  ufw default allow outgoing
  ufw --force enable
}

require_root
require_ubuntu_noble
require_registry_mirror
install_runtime
prepare_service_directories
configure_firewall
