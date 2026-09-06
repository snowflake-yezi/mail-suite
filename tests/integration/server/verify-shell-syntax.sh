#!/usr/bin/env bash
set -Eeuo pipefail

# 真实解析全部服务器脚本，防止静态文本断言漏掉 Bash 语法错误。
readonly repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
for script in "${repository_root}"/deploy/server/*.sh; do
  bash -n "${script}"
done
echo "Server Bash syntax checks passed"
