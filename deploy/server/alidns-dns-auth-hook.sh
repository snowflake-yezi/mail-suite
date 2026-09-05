#!/usr/bin/env bash
set -Eeuo pipefail

# 固定共享入口避免 Certbot renewal 配置绑定历史 release。
exec /usr/bin/python3 /opt/mail-suite/shared/acme-hooks/alidns-dns-hook.py auth
