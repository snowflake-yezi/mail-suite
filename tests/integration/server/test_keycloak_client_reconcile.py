"""验证 Keycloak client reconcile 的幂等、回滚和秘密传输边界。"""

from __future__ import annotations

import copy
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest


REPOSITORY_ROOT = Path(__file__).resolve().parents[3]
SCRIPT_PATH = REPOSITORY_ROOT / "deploy" / "server" / "reconcile-keycloak-client.py"
SPEC = importlib.util.spec_from_file_location("keycloak_client_reconcile", SCRIPT_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("无法加载 Keycloak client reconcile")
RECONCILE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RECONCILE)


class RecordingAdmin:
    """以内存 representation 模拟唯一 Keycloak client 并记录 PUT。"""

    def __init__(self, representation: dict[str, object]) -> None:
        self.representation = copy.deepcopy(representation)
        self.updates: list[dict[str, object]] = []

    def find_unique_client(self) -> str:
        """返回固定内部标识。"""

        return "internal-client-id"

    def get_client(self, internal_id: str) -> dict[str, object]:
        """返回隔离副本，模拟 Admin REST 重新读取。"""

        if internal_id != "internal-client-id":
            raise AssertionError("unexpected client")
        return copy.deepcopy(self.representation)

    def update_client(
        self, internal_id: str, representation: dict[str, object]
    ) -> None:
        """记录完整 representation 并更新内存状态。"""

        if internal_id != "internal-client-id":
            raise AssertionError("unexpected client")
        self.updates.append(copy.deepcopy(representation))
        self.representation = copy.deepcopy(representation)


class FakeHTTPResponse:
    """提供 context manager 和有界读取接口的假 HTTP 响应。"""

    def __init__(self, payload: object) -> None:
        self.payload = json.dumps(payload).encode("utf-8")

    def __enter__(self) -> "FakeHTTPResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def read(self, limit: int) -> bytes:
        """按调用方上限返回假响应。"""

        return self.payload[:limit]


class RecordingOpener:
    """记录 token 请求，证明密码只出现在 POST body。"""

    def __init__(self, payload: object | None = None) -> None:
        self.requests: list[object] = []
        self.payload = (
            {"access_token": "short-lived-test-token"}
            if payload is None
            else payload
        )

    def open(self, http_request: object, timeout: int) -> FakeHTTPResponse:
        """保存请求并返回固定短期 token。"""

        self.requests.append((http_request, timeout))
        return FakeHTTPResponse(self.payload)


class KeycloakClientReconcileTest(unittest.TestCase):
    """覆盖不会连接 Docker 或真实 Keycloak 的本地 reconcile 回归。"""

    def marker_store(self, directory: str) -> object:
        """创建不要求本机 root owner 的临时标记存储。"""

        return RECONCILE.RollbackMarkerStore(
            Path(directory) / "rollback.json", enforce_root_ownership=False
        )

    def test_apply_preserves_fields_and_is_idempotent(self) -> None:
        """首次只改目标属性，第二次不 PUT 且不覆盖回滚标记。"""

        admin = RecordingAdmin(
            {
                "id": "internal-client-id",
                "clientId": RECONCILE.CLIENT_ID,
                "redirectUris": ["https://mail.test.snowye.fun/api/v1/auth/callback"],
                "attributes": {"existing": "preserved"},
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            marker = self.marker_store(directory)
            self.assertTrue(RECONCILE.reconcile_client(admin, marker, "apply"))
            self.assertFalse(RECONCILE.reconcile_client(admin, marker, "apply"))
            saved_marker = marker.load()
            if os.name != "nt":
                self.assertEqual(stat.S_IMODE(marker.path.stat().st_mode), 0o600)

        self.assertEqual(len(admin.updates), 1)
        self.assertEqual(admin.updates[0]["attributes"]["existing"], "preserved")
        self.assertEqual(
            admin.updates[0]["attributes"][RECONCILE.POST_LOGOUT_ATTRIBUTE],
            RECONCILE.POST_LOGOUT_REDIRECT_URI,
        )
        self.assertFalse(saved_marker["previous_present"])
        self.assertIsNone(saved_marker["previous_value"])

    def test_rollback_restores_absent_attribute(self) -> None:
        """严格回滚删除原本不存在的属性并保留其他字段。"""

        admin = RecordingAdmin(
            {
                "id": "internal-client-id",
                "clientId": RECONCILE.CLIENT_ID,
                "attributes": {"existing": "preserved"},
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            marker = self.marker_store(directory)
            RECONCILE.reconcile_client(admin, marker, "apply")
            self.assertTrue(RECONCILE.reconcile_client(admin, marker, "rollback"))

        self.assertNotIn(
            RECONCILE.POST_LOGOUT_ATTRIBUTE, admin.representation["attributes"]
        )
        self.assertEqual(admin.representation["attributes"]["existing"], "preserved")

    def test_rollback_restores_previous_attribute_value(self) -> None:
        """严格回滚恢复首次 reconcile 前的非秘密旧值。"""

        previous = "https://previous.test.invalid/"
        admin = RecordingAdmin(
            {
                "id": "internal-client-id",
                "clientId": RECONCILE.CLIENT_ID,
                "attributes": {RECONCILE.POST_LOGOUT_ATTRIBUTE: previous},
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            marker = self.marker_store(directory)
            RECONCILE.reconcile_client(admin, marker, "apply")
            RECONCILE.reconcile_client(admin, marker, "rollback")

        self.assertEqual(
            admin.representation["attributes"][RECONCILE.POST_LOGOUT_ATTRIBUTE],
            previous,
        )

    def test_check_rejects_drift(self) -> None:
        """目标属性缺失时检查必须失败且不写 client。"""

        admin = RecordingAdmin(
            {"id": "internal-client-id", "clientId": RECONCILE.CLIENT_ID}
        )
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RECONCILE.ReconcileFailure) as raised:
                RECONCILE.reconcile_client(admin, self.marker_store(directory), "check")
        self.assertEqual(raised.exception.stage, "client_check")
        self.assertEqual(admin.updates, [])

    def test_admin_password_is_only_in_token_request_body(self) -> None:
        """管理密码不得进入 URL、header 或命令参数。"""

        opener = RecordingOpener()
        password = "test-password-not-for-logs"
        client = RECONCILE.KeycloakAdminClient(
            "http://172.18.0.2:8080", password, opener=opener
        )
        client.authenticate()

        self.assertEqual(client.access_token, "short-lived-test-token")
        self.assertEqual(len(opener.requests), 1)
        http_request, timeout = opener.requests[0]
        self.assertEqual(timeout, 10)
        self.assertNotIn(password, http_request.full_url)
        self.assertNotIn(password, str(http_request.headers))
        self.assertIn(f"password={password}", http_request.data.decode("ascii"))

    def test_admin_client_requires_exactly_one_matching_client(self) -> None:
        """Admin REST 返回零个或多个 client 时均拒绝继续。"""

        for payload in ([], [{"id": "one"}, {"id": "two"}]):
            with self.subTest(count=len(payload)):
                client = RECONCILE.KeycloakAdminClient(
                    "http://172.18.0.2:8080",
                    "test-password",
                    opener=RecordingOpener(payload),
                )
                client.access_token = "short-lived-test-token"
                with self.assertRaises(RECONCILE.ReconcileFailure) as raised:
                    client.find_unique_client()
                self.assertEqual(raised.exception.stage, "client_lookup")


if __name__ == "__main__":
    unittest.main()
