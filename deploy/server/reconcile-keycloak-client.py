#!/usr/bin/env python3
"""幂等维护测试 Keycloak client 的精确前台退出回跳属性。"""

from __future__ import annotations

import argparse
import copy
import ipaddress
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
from typing import Any
from urllib import error, parse, request


SERVICE_ROOT = Path("/opt/mail-suite")
RELEASES_ROOT = SERVICE_ROOT / "releases"
RUNTIME_ENV = SERVICE_ROOT / "shared" / "runtime.env"
ROLLBACK_MARKER = SERVICE_ROOT / "shared" / "keycloak-client-post-logout.rollback.json"
REALM = "mail-suite-test"
CLIENT_ID = "mail-suite-test"
ADMIN_USERNAME = "mail-suite-ops"
POST_LOGOUT_REDIRECT_URI = "https://mail.test.snowye.fun/"
POST_LOGOUT_ATTRIBUTE = "post.logout.redirect.uris"
MAXIMUM_RESPONSE_BYTES = 1 << 20


class ReconcileFailure(RuntimeError):
    """保存可公开输出且不包含上游响应或秘密的稳定失败阶段。"""

    def __init__(self, stage: str) -> None:
        super().__init__(stage)
        self.stage = stage


class NoRedirectHandler(request.HTTPRedirectHandler):
    """拒绝 Admin REST 重定向，防止凭据被转发到意外地址。"""

    def redirect_request(
        self,
        req: request.Request,
        fp: Any,
        code: int,
        msg: str,
        headers: Any,
        newurl: str,
    ) -> None:
        return None


class KeycloakAdminClient:
    """通过宿主到 Keycloak Docker bridge 的内部 HTTP 执行有界 Admin REST。"""

    def __init__(self, base_url: str, password: str, opener: Any | None = None) -> None:
        parsed = parse.urlsplit(base_url)
        if (
            parsed.scheme != "http"
            or parsed.port != 8080
            or not parsed.hostname
            or parsed.path
            or parsed.query
            or parsed.fragment
        ):
            raise ReconcileFailure("admin_endpoint")
        try:
            address = ipaddress.ip_address(parsed.hostname)
        except ValueError as exc:
            raise ReconcileFailure("admin_endpoint") from exc
        if address.version != 4 or not address.is_private:
            raise ReconcileFailure("admin_endpoint")
        if not password:
            raise ReconcileFailure("runtime_secret")
        self.base_url = base_url
        self.password = password
        self.opener = opener or request.build_opener(NoRedirectHandler())
        self.access_token = ""

    def authenticate(self) -> None:
        """在请求体中交换短期管理 token，不把密码放进 URL 或进程参数。"""

        form = parse.urlencode(
            {
                "grant_type": "password",
                "client_id": "admin-cli",
                "username": ADMIN_USERNAME,
                "password": self.password,
            }
        ).encode("ascii")
        payload = self._request_json(
            "/realms/master/protocol/openid-connect/token",
            method="POST",
            body=form,
            content_type="application/x-www-form-urlencoded",
            authenticated=False,
            stage="admin_auth",
        )
        token = payload.get("access_token") if isinstance(payload, dict) else None
        if not isinstance(token, str) or not token:
            raise ReconcileFailure("admin_auth")
        self.access_token = token

    def find_unique_client(self) -> str:
        """按固定 clientId 查询并要求唯一内部 client 标识。"""

        query = parse.urlencode({"clientId": CLIENT_ID})
        payload = self._request_json(
            f"/admin/realms/{REALM}/clients?{query}", stage="client_lookup"
        )
        if not isinstance(payload, list) or len(payload) != 1:
            raise ReconcileFailure("client_lookup")
        internal_id = payload[0].get("id") if isinstance(payload[0], dict) else None
        if not isinstance(internal_id, str) or not internal_id:
            raise ReconcileFailure("client_lookup")
        return internal_id

    def get_client(self, internal_id: str) -> dict[str, Any]:
        """读取完整 client representation 供保留字段的 read-modify-write。"""

        payload = self._request_json(
            f"/admin/realms/{REALM}/clients/{parse.quote(internal_id, safe='')}",
            stage="client_read",
        )
        if not isinstance(payload, dict):
            raise ReconcileFailure("client_read")
        return payload

    def update_client(self, internal_id: str, representation: dict[str, Any]) -> None:
        """将只变更目标属性的完整 representation 写回固定 client。"""

        body = json.dumps(
            representation, ensure_ascii=True, separators=(",", ":")
        ).encode("utf-8")
        self._request_json(
            f"/admin/realms/{REALM}/clients/{parse.quote(internal_id, safe='')}",
            method="PUT",
            body=body,
            content_type="application/json",
            stage="client_update",
            allow_empty=True,
        )

    def _request_json(
        self,
        path: str,
        *,
        method: str = "GET",
        body: bytes | None = None,
        content_type: str | None = None,
        authenticated: bool = True,
        stage: str,
        allow_empty: bool = False,
    ) -> Any:
        """执行禁止重定向、限制响应大小且错误脱敏的 JSON 请求。"""

        headers = {"Accept": "application/json"}
        if content_type:
            headers["Content-Type"] = content_type
        if authenticated:
            if not self.access_token:
                raise ReconcileFailure(stage)
            headers["Authorization"] = f"Bearer {self.access_token}"
        http_request = request.Request(
            self.base_url + path, data=body, headers=headers, method=method
        )
        try:
            with self.opener.open(http_request, timeout=10) as response:
                payload = response.read(MAXIMUM_RESPONSE_BYTES + 1)
        except (error.HTTPError, error.URLError, TimeoutError, OSError) as exc:
            raise ReconcileFailure(stage) from exc
        if len(payload) > MAXIMUM_RESPONSE_BYTES:
            raise ReconcileFailure(stage)
        if not payload and allow_empty:
            return None
        try:
            return json.loads(payload.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ReconcileFailure(stage) from exc


class RollbackMarkerStore:
    """原子保存并校验唯一非秘密 Keycloak client 属性的旧值。"""

    def __init__(self, path: Path, enforce_root_ownership: bool = True) -> None:
        self.path = path
        self.enforce_root_ownership = enforce_root_ownership

    def save_once(self, present: bool, value: str | None) -> None:
        """首次变更前写入 0600 标记，已有标记不得被后续发布覆盖。"""

        if self.path.exists():
            self.load()
            return
        payload = {
            "version": 1,
            "realm": REALM,
            "client_id": CLIENT_ID,
            "attribute": POST_LOGOUT_ATTRIBUTE,
            "previous_present": present,
            "previous_value": value if present else None,
        }
        encoded = (json.dumps(payload, ensure_ascii=True, sort_keys=True) + "\n").encode(
            "utf-8"
        )
        temporary = self.path.with_name(f".{self.path.name}.{os.getpid()}.tmp")
        descriptor = -1
        try:
            descriptor = os.open(
                temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
            )
            os.write(descriptor, encoded)
            os.fsync(descriptor)
            os.close(descriptor)
            descriptor = -1
            os.replace(temporary, self.path)
            os.chmod(self.path, 0o600)
        except OSError as exc:
            if descriptor >= 0:
                os.close(descriptor)
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass
            raise ReconcileFailure("rollback_marker_write") from exc

    def load(self) -> dict[str, Any]:
        """读取固定 schema 标记并拒绝权限、目标或旧值类型漂移。"""

        try:
            file_stat = self.path.stat()
            if self.enforce_root_ownership and (
                stat.S_IMODE(file_stat.st_mode) != 0o600
                or file_stat.st_uid != 0
                or file_stat.st_gid != 0
            ):
                raise ReconcileFailure("rollback_marker_permissions")
            payload = json.loads(self.path.read_text(encoding="utf-8"))
        except ReconcileFailure:
            raise
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ReconcileFailure("rollback_marker_read") from exc
        expected_keys = {
            "version",
            "realm",
            "client_id",
            "attribute",
            "previous_present",
            "previous_value",
        }
        if not isinstance(payload, dict) or set(payload) != expected_keys:
            raise ReconcileFailure("rollback_marker_read")
        if (
            payload["version"] != 1
            or payload["realm"] != REALM
            or payload["client_id"] != CLIENT_ID
            or payload["attribute"] != POST_LOGOUT_ATTRIBUTE
            or not isinstance(payload["previous_present"], bool)
            or (
                payload["previous_present"]
                and not isinstance(payload["previous_value"], str)
            )
            or (
                not payload["previous_present"]
                and payload["previous_value"] is not None
            )
        ):
            raise ReconcileFailure("rollback_marker_read")
        return payload


def read_runtime_password(path: Path) -> str:
    """从 0600 root:root env 文件读取唯一管理密码且不执行文件内容。"""

    try:
        file_stat = path.stat()
        if stat.S_IMODE(file_stat.st_mode) != 0o600 or file_stat.st_uid != 0 or file_stat.st_gid != 0:
            raise ReconcileFailure("runtime_permissions")
        matches = []
        for line in path.read_text(encoding="utf-8").splitlines():
            if line.startswith("MAIL_SUITE_KEYCLOAK_ADMIN_PASSWORD="):
                matches.append(line.split("=", 1)[1])
    except ReconcileFailure:
        raise
    except (OSError, UnicodeDecodeError) as exc:
        raise ReconcileFailure("runtime_secret") from exc
    if len(matches) != 1 or not matches[0]:
        raise ReconcileFailure("runtime_secret")
    return matches[0]


def discover_keycloak_base_url(deployment_root: Path) -> str:
    """从固定 Compose 项目发现唯一 Keycloak 容器及其私有 bridge IPv4。"""

    try:
        resolved_root = deployment_root.resolve(strict=True)
    except OSError as exc:
        raise ReconcileFailure("deployment_root") from exc
    if RELEASES_ROOT not in resolved_root.parents:
        raise ReconcileFailure("deployment_root")
    compose_file = resolved_root / "deploy" / "server" / "compose.yaml"
    if not compose_file.is_file():
        raise ReconcileFailure("deployment_root")
    compose_command = [
        "docker",
        "compose",
        "--project-directory",
        str(resolved_root),
        "--env-file",
        str(RUNTIME_ENV),
        "-f",
        str(compose_file),
        "ps",
        "-q",
        "keycloak",
    ]
    container_output = _run_command(compose_command, "container_lookup")
    container_ids = [line.strip() for line in container_output.splitlines() if line.strip()]
    if len(container_ids) != 1:
        raise ReconcileFailure("container_lookup")
    inspect_output = _run_command(
        ["docker", "inspect", container_ids[0]], "container_inspect"
    )
    try:
        inspected = json.loads(inspect_output)
        networks = inspected[0]["NetworkSettings"]["Networks"]
        addresses = {
            details.get("IPAddress", "")
            for details in networks.values()
            if isinstance(details, dict) and details.get("IPAddress")
        }
    except (json.JSONDecodeError, KeyError, IndexError, TypeError) as exc:
        raise ReconcileFailure("container_inspect") from exc
    if len(addresses) != 1:
        raise ReconcileFailure("container_inspect")
    address = next(iter(addresses))
    try:
        parsed_address = ipaddress.ip_address(address)
    except ValueError as exc:
        raise ReconcileFailure("container_inspect") from exc
    if parsed_address.version != 4 or not parsed_address.is_private:
        raise ReconcileFailure("container_inspect")
    return f"http://{address}:8080"


def _run_command(arguments: list[str], stage: str) -> str:
    """执行固定参数命令并将 stdout/stderr 隔离在进程内。"""

    try:
        result = subprocess.run(
            arguments,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=20,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise ReconcileFailure(stage) from exc
    if result.returncode != 0:
        raise ReconcileFailure(stage)
    return result.stdout


def reconcile_client(
    admin: Any,
    marker_store: RollbackMarkerStore,
    action: str,
) -> bool:
    """应用、检查或回滚唯一 client 属性，并在写入后重新读取确认。"""

    internal_id = admin.find_unique_client()
    representation = admin.get_client(internal_id)
    if representation.get("clientId") != CLIENT_ID:
        raise ReconcileFailure("client_read")
    raw_attributes = representation.get("attributes", {})
    if not isinstance(raw_attributes, dict):
        raise ReconcileFailure("client_read")
    attributes = dict(raw_attributes)
    current_present = POST_LOGOUT_ATTRIBUTE in attributes
    current_value = attributes.get(POST_LOGOUT_ATTRIBUTE)
    if current_present and not isinstance(current_value, str):
        raise ReconcileFailure("client_read")

    if action == "check":
        if current_value != POST_LOGOUT_REDIRECT_URI:
            raise ReconcileFailure("client_check")
        return False

    updated = copy.deepcopy(representation)
    updated_attributes = dict(attributes)
    if action == "apply":
        if current_value == POST_LOGOUT_REDIRECT_URI:
            return False
        marker_store.save_once(current_present, current_value)
        updated_attributes[POST_LOGOUT_ATTRIBUTE] = POST_LOGOUT_REDIRECT_URI
        expected_present = True
        expected_value: str | None = POST_LOGOUT_REDIRECT_URI
    elif action == "rollback":
        marker = marker_store.load()
        if current_value != POST_LOGOUT_REDIRECT_URI:
            raise ReconcileFailure("rollback_drift")
        expected_present = marker["previous_present"]
        expected_value = marker["previous_value"]
        if expected_present:
            updated_attributes[POST_LOGOUT_ATTRIBUTE] = expected_value
        else:
            updated_attributes.pop(POST_LOGOUT_ATTRIBUTE, None)
    else:
        raise ReconcileFailure("action")

    updated["attributes"] = updated_attributes
    admin.update_client(internal_id, updated)
    verified = admin.get_client(internal_id)
    verified_attributes = verified.get("attributes", {})
    if not isinstance(verified_attributes, dict):
        raise ReconcileFailure("client_verify")
    if (POST_LOGOUT_ATTRIBUTE in verified_attributes) != expected_present:
        raise ReconcileFailure("client_verify")
    if expected_present and verified_attributes.get(POST_LOGOUT_ATTRIBUTE) != expected_value:
        raise ReconcileFailure("client_verify")
    return True


def parse_arguments() -> argparse.Namespace:
    """只接受发布目录和三个互斥的固定属性操作。"""

    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--deployment-root", required=True, type=Path)
    actions = parser.add_mutually_exclusive_group(required=True)
    actions.add_argument("--apply", action="store_true")
    actions.add_argument("--check", action="store_true")
    actions.add_argument("--rollback", action="store_true")
    return parser.parse_args()


def main() -> int:
    """验证运行边界后执行 reconcile，公开输出只包含动作和稳定阶段。"""

    arguments = parse_arguments()
    if not hasattr(os, "geteuid") or os.geteuid() != 0:
        print("Keycloak client reconcile 失败：execution_identity", file=sys.stderr)
        return 1
    action = "apply" if arguments.apply else "check" if arguments.check else "rollback"
    try:
        password = read_runtime_password(RUNTIME_ENV)
        base_url = discover_keycloak_base_url(arguments.deployment_root)
        admin = KeycloakAdminClient(base_url, password)
        admin.authenticate()
        changed = reconcile_client(
            admin,
            RollbackMarkerStore(ROLLBACK_MARKER),
            action,
        )
    except ReconcileFailure as exc:
        print(f"Keycloak client reconcile 失败：{exc.stage}", file=sys.stderr)
        return 1
    except Exception:
        print("Keycloak client reconcile 失败：internal", file=sys.stderr)
        return 1
    state = "changed" if changed else "unchanged"
    print(f"Keycloak client post-logout 属性{action}通过：{state}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
