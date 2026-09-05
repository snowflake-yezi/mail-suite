"""验证 AliDNS DNS-01 hook 的签名传输和精确清理边界。"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


REPOSITORY_ROOT = Path(__file__).resolve().parents[3]
HOOK_PATH = REPOSITORY_ROOT / "deploy" / "server" / "alidns-dns-hook.py"
SPEC = importlib.util.spec_from_file_location("alidns_dns_hook", HOOK_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("无法加载 AliDNS DNS hook")
HOOK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOOK)


class FakeResponse:
    """模拟 urllib 返回的 AliDNS JSON 响应。"""

    def __init__(self, payload: dict[str, object]) -> None:
        self._payload = payload

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def read(self) -> bytes:
        """返回 UTF-8 JSON 响应体。"""

        return json.dumps(self._payload).encode("utf-8")


class RecordingClient:
    """记录清理流程调用的 AliDNS action 与参数。"""

    def __init__(self, record: dict[str, object]) -> None:
        self.record = record
        self.calls: list[tuple[str, dict[str, str]]] = []

    def request(self, action: str, **parameters: str) -> dict[str, object]:
        """返回预置记录，并记录删除调用。"""

        self.calls.append((action, parameters))
        if action == "DescribeDomainRecordInfo":
            return self.record
        if action == "DeleteDomainRecord":
            return {"RecordId": parameters["RecordId"]}
        raise AssertionError(f"unexpected action: {action}")


class AliDnsHookTest(unittest.TestCase):
    """覆盖不会触碰真实 DNS 的自动续期安全测试。"""

    def test_uses_signed_post_without_putting_secret_in_body(self) -> None:
        """AccessKey Secret 只用于签名，API 请求使用 HTTPS POST。"""

        requests: list[object] = []

        def open_request(request: object, timeout: int) -> FakeResponse:
            requests.append(request)
            self.assertEqual(timeout, 15)
            return FakeResponse({"RecordId": "12345"})

        client = HOOK.AliDnsClient(
            "A" * 20,
            "secret-value-never-sent",
            urlopen=open_request,
        )
        response = client.request(
            "AddDomainRecord",
            DomainName="snowye.fun",
            RR="_acme-challenge.mail.test",
            Type="TXT",
            Value="validation-value",
        )

        self.assertEqual(response["RecordId"], "12345")
        self.assertEqual(len(requests), 1)
        request = requests[0]
        self.assertEqual(request.full_url, "https://alidns.aliyuncs.com/")
        self.assertEqual(request.method, "POST")
        body = request.data.decode("ascii")
        self.assertIn("Action=AddDomainRecord", body)
        self.assertIn("Signature=", body)
        self.assertNotIn("secret-value-never-sent", body)

    def test_parses_split_txt_values(self) -> None:
        """长 TXT 的多个引号片段必须组合为一个精确值。"""

        values = HOOK.parse_txt_values('"first" "second"\n"other"\n')

        self.assertEqual(values, {"firstsecond", "other"})

    def test_deletes_only_record_id_that_still_matches_state(self) -> None:
        """cleanup 复核完整记录后只删除状态文件中的 RecordId。"""

        domain = "mail.test.snowye.fun"
        validation = "expected-validation"
        state = {
            "version": 1,
            "domain": domain,
            "root_domain": "snowye.fun",
            "rr": "_acme-challenge.mail.test",
            "type": "TXT",
            "record_id": "12345",
            "validation_sha256": HOOK.challenge_digest(validation),
        }
        record = {
            "DomainName": "snowye.fun",
            "RR": "_acme-challenge.mail.test",
            "Type": "TXT",
            "Value": validation,
        }
        client = RecordingClient(record)
        with tempfile.TemporaryDirectory() as directory:
            temporary_state = Path(directory) / f"{domain}.json"
            temporary_state.write_text("{}", encoding="utf-8")
            with (
                mock.patch.object(HOOK, "STATE_ROOT", Path(directory)),
                mock.patch.object(HOOK, "read_json_object", return_value=state),
                mock.patch.object(HOOK, "publish_tombstone") as publish,
            ):
                HOOK.remove_owned_record(client, domain, validation)

        self.assertEqual(
            client.calls,
            [
                ("DescribeDomainRecordInfo", {"RecordId": "12345"}),
                ("DeleteDomainRecord", {"RecordId": "12345"}),
            ],
        )
        publish.assert_called_once_with(state)

    def test_rejects_record_id_that_no_longer_matches_state(self) -> None:
        """RecordId 被改写为其他内容时必须保留记录并失败关闭。"""

        domain = "mail.test.snowye.fun"
        validation = "expected-validation"
        state = {
            "version": 1,
            "domain": domain,
            "root_domain": "snowye.fun",
            "rr": "_acme-challenge.mail.test",
            "type": "TXT",
            "record_id": "12345",
            "validation_sha256": HOOK.challenge_digest(validation),
        }
        client = RecordingClient(
            {
                "DomainName": "snowye.fun",
                "RR": "mail",
                "Type": "A",
                "Value": "192.0.2.1",
            }
        )
        with tempfile.TemporaryDirectory() as directory:
            temporary_state = Path(directory) / f"{domain}.json"
            temporary_state.write_text("{}", encoding="utf-8")
            with (
                mock.patch.object(HOOK, "STATE_ROOT", Path(directory)),
                mock.patch.object(HOOK, "read_json_object", return_value=state),
                self.assertRaisesRegex(HOOK.HookError, "拒绝删除"),
            ):
                HOOK.remove_owned_record(client, domain, validation)

        self.assertEqual(
            client.calls,
            [("DescribeDomainRecordInfo", {"RecordId": "12345"})],
        )

    def test_rejects_unapproved_certbot_domain(self) -> None:
        """Certbot 环境不能把 AccessKey 用于白名单外域名。"""

        with (
            mock.patch.dict(
                HOOK.os.environ,
                {
                    "CERTBOT_DOMAIN": "other.snowye.fun",
                    "CERTBOT_VALIDATION": "validation-value",
                },
                clear=True,
            ),
            self.assertRaisesRegex(HOOK.HookError, "未批准域名"),
        ):
            HOOK.require_challenge()

    def test_rejects_non_string_credentials(self) -> None:
        """凭据文件不能把数值等其他 JSON 类型隐式转换为 AccessKey。"""

        with (
            mock.patch.object(
                HOOK,
                "read_json_object",
                return_value={
                    "access_key_id": 1234567890123456,
                    "access_key_secret": "S" * 32,
                },
            ),
            self.assertRaisesRegex(HOOK.HookError, "字段类型无效"),
        ):
            HOOK.load_credentials()


if __name__ == "__main__":
    unittest.main()
