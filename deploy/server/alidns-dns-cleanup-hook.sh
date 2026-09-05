#!/usr/bin/env bash
set -Eeuo pipefail

# 固定共享入口只清理 auth hook 状态文件持有的精确 RecordId。
exec /usr/bin/python3 /opt/mail-suite/shared/acme-hooks/alidns-dns-hook.py cleanup
