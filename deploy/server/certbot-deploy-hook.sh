#!/usr/bin/env bash
set -Eeuo pipefail

readonly current_release="$(readlink -f /opt/mail-suite/current)"
exec "${current_release}/deploy/server/install-public-certificates.sh" "${current_release}" --reload
