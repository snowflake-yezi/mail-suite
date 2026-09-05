#!/usr/bin/env python3
"""为三个固定测试域名提供 AliDNS DNS-01 challenge 生命周期。"""

from __future__ import annotations

import base64
import concurrent.futures
import datetime
import getpass
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request
import uuid


API_ENDPOINT = "https://alidns.aliyuncs.com/"
API_VERSION = "2015-01-09"
ROOT_DOMAIN = "snowye.fun"
ACME_ROOT = Path("/opt/mail-suite/shared/acme-alidns")
CREDENTIALS_PATH = ACME_ROOT / "credentials.json"
STATE_ROOT = ACME_ROOT / "state"
TOMBSTONE_ROOT = ACME_ROOT / "tombstones"
PUBLIC_RESOLVERS = ("223.5.5.5", "8.8.8.8", "1.1.1.1")
ALLOWED_RECORDS = {
    "mail.test.snowye.fun": "_acme-challenge.mail.test",
    "idp.test.snowye.fun": "_acme-challenge.idp.test",
    "mx1.test.snowye.fun": "_acme-challenge.mx1.test",
}
NOT_FOUND_CODES = {
    "InvalidDomainRecordId.NotFound",
    "InvalidRecordId.NotFound",
}


class HookError(RuntimeError):
    """表示可安全展示且不含 AccessKey 或 challenge 的 hook 错误。"""


class AliDnsApiError(HookError):
    """保存 AliDNS 错误码，但不传播可能包含敏感参数的请求信息。"""

    def __init__(self, action: str, code: str) -> None:
        super().__init__(f"AliDNS {action} 失败：{code}")
        self.code = code


def percent_encode(value: str) -> str:
    """按 Aliyun RPC 签名规则执行 RFC 3986 编码。"""

    return urllib.parse.quote(value, safe="-_.~")


def challenge_digest(value: str) -> str:
    """生成只用于状态比对、不可反推出 challenge 的摘要。"""

    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def require_root() -> None:
    """限制凭据、状态与 DNS 变更只能由目标机 root 执行。"""

    if not hasattr(os, "geteuid") or os.geteuid() != 0:
        raise HookError("请使用 root 执行 AliDNS ACME 操作")


def require_restricted_file(path: Path) -> None:
    """要求秘密或状态文件保持 root:root 0600。"""

    try:
        metadata = path.stat()
    except FileNotFoundError as error:
        raise HookError(f"缺少受限文件：{path}") from error
    if (
        stat.S_IMODE(metadata.st_mode) != 0o600
        or metadata.st_uid != 0
        or metadata.st_gid != 0
    ):
        raise HookError(f"受限文件必须为 0600 root:root：{path}")


def ensure_private_directory(path: Path) -> None:
    """创建或核对 root 专用状态目录。"""

    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chown(path, 0, 0)
    os.chmod(path, 0o700)


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    """以 0600 root:root 原子写入 JSON，避免留下部分状态。"""

    ensure_private_directory(path.parent)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary_path = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(payload, output, ensure_ascii=True, sort_keys=True)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.chown(temporary_path, 0, 0)
        os.replace(temporary_path, path)
        os.chmod(path, 0o600)
    finally:
        try:
            temporary_path.unlink()
        except FileNotFoundError:
            pass


def read_json_object(path: Path) -> dict[str, Any]:
    """从受限文件读取单个 JSON 对象并拒绝其他顶层类型。"""

    require_restricted_file(path)
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise HookError(f"受限 JSON 文件无效：{path}") from error
    if not isinstance(payload, dict):
        raise HookError(f"受限 JSON 文件必须包含对象：{path}")
    return payload


def validate_credential_value(name: str, value: str) -> str:
    """拒绝空白、换行和异常长度的 AccessKey 字段。"""

    if not 16 <= len(value) <= 128 or re.search(r"\s", value):
        raise HookError(f"{name} 格式无效")
    return value


def load_credentials() -> tuple[str, str]:
    """读取唯一允许的凭据文件，且不把值写入输出。"""

    payload = read_json_object(CREDENTIALS_PATH)
    if set(payload) != {"access_key_id", "access_key_secret"}:
        raise HookError("AliDNS 凭据文件字段无效")
    if not isinstance(payload["access_key_id"], str) or not isinstance(
        payload["access_key_secret"], str
    ):
        raise HookError("AliDNS 凭据文件字段类型无效")
    access_key_id = validate_credential_value(
        "AccessKey ID", payload["access_key_id"]
    )
    access_key_secret = validate_credential_value(
        "AccessKey Secret", payload["access_key_secret"]
    )
    return access_key_id, access_key_secret


class AliDnsClient:
    """使用固定 HTTPS endpoint 调用 AliDNS 2015-01-09 RPC API。"""

    def __init__(
        self,
        access_key_id: str,
        access_key_secret: str,
        urlopen: Callable[..., Any] = urllib.request.urlopen,
    ) -> None:
        """保存内存凭据和可替换传输函数，凭据不会进入命令参数。"""

        self._access_key_id = access_key_id
        self._access_key_secret = access_key_secret
        self._urlopen = urlopen

    def request(self, action: str, **action_parameters: str) -> dict[str, Any]:
        """签名并发送一次 HTTPS POST，返回 AliDNS JSON 对象。"""

        parameters = {
            "AccessKeyId": self._access_key_id,
            "Action": action,
            "Format": "JSON",
            "SignatureMethod": "HMAC-SHA1",
            "SignatureNonce": str(uuid.uuid4()),
            "SignatureVersion": "1.0",
            "Timestamp": datetime.datetime.now(datetime.timezone.utc).strftime(
                "%Y-%m-%dT%H:%M:%SZ"
            ),
            "Version": API_VERSION,
            **action_parameters,
        }
        canonical_query = "&".join(
            f"{percent_encode(key)}={percent_encode(str(parameters[key]))}"
            for key in sorted(parameters)
        )
        string_to_sign = f"POST&%2F&{percent_encode(canonical_query)}"
        signature = base64.b64encode(
            hmac.new(
                f"{self._access_key_secret}&".encode("utf-8"),
                string_to_sign.encode("utf-8"),
                hashlib.sha1,
            ).digest()
        ).decode("ascii")
        body = (
            canonical_query + f"&Signature={percent_encode(signature)}"
        ).encode("ascii")
        request = urllib.request.Request(
            API_ENDPOINT,
            data=body,
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            method="POST",
        )
        try:
            with self._urlopen(request, timeout=15) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            code = "HTTP_ERROR"
            try:
                error_payload = json.loads(error.read().decode("utf-8"))
                if isinstance(error_payload, dict):
                    code = str(error_payload.get("Code", code))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                pass
            raise AliDnsApiError(action, code) from error
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            raise HookError(f"AliDNS {action} 网络请求失败") from error
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise HookError(f"AliDNS {action} 返回无效 JSON") from error
        if not isinstance(payload, dict):
            raise HookError(f"AliDNS {action} 返回类型无效")
        if "Code" in payload:
            raise AliDnsApiError(action, str(payload["Code"]))
        return payload


def client_from_credentials() -> AliDnsClient:
    """从受限文件构造 AliDNS 客户端。"""

    access_key_id, access_key_secret = load_credentials()
    return AliDnsClient(access_key_id, access_key_secret)


def configure_credentials() -> None:
    """从真实 TTY 隐藏读取并原子保存独立 RAM AccessKey。"""

    if not sys.stdin.isatty() or not sys.stderr.isatty():
        raise HookError("凭据只能在目标机交互式 TTY 中录入")
    access_key_id = validate_credential_value(
        "AccessKey ID", getpass.getpass("AliDNS AccessKey ID（隐藏输入）：")
    )
    access_key_secret = validate_credential_value(
        "AccessKey Secret", getpass.getpass("AliDNS AccessKey Secret（隐藏输入）：")
    )
    write_json_atomic(
        CREDENTIALS_PATH,
        {
            "access_key_id": access_key_id,
            "access_key_secret": access_key_secret,
        },
    )
    print("AliDNS 凭据已保存为 0600 root:root；未回显任何值")


def require_challenge() -> tuple[str, str, str]:
    """校验 Certbot 环境并返回批准的域名、RR 和 challenge。"""

    domain = os.environ.get("CERTBOT_DOMAIN", "")
    validation = os.environ.get("CERTBOT_VALIDATION", "")
    try:
        rr = ALLOWED_RECORDS[domain]
    except KeyError as error:
        raise HookError("拒绝未批准域名的 DNS-01 challenge") from error
    if not validation or len(validation) > 512 or re.search(r"\s", validation):
        raise HookError("DNS-01 challenge 值无效")
    return domain, rr, validation


def state_path(domain: str) -> Path:
    """返回固定域名的唯一状态文件路径。"""

    return STATE_ROOT / f"{domain}.json"


def tombstone_path(domain: str) -> Path:
    """返回固定域名的最近一次清理证明路径。"""

    return TOMBSTONE_ROOT / f"{domain}.json"


def validate_state(payload: dict[str, Any], domain: str) -> None:
    """验证状态只能描述脚本批准且创建过的单条 TXT。"""

    expected_keys = {
        "version",
        "domain",
        "root_domain",
        "rr",
        "type",
        "record_id",
        "validation_sha256",
    }
    if set(payload) != expected_keys:
        raise HookError("AliDNS challenge 状态字段无效")
    if (
        payload["version"] != 1
        or payload["domain"] != domain
        or payload["root_domain"] != ROOT_DOMAIN
        or payload["rr"] != ALLOWED_RECORDS[domain]
        or payload["type"] != "TXT"
        or not re.fullmatch(r"[0-9]+", str(payload["record_id"]))
        or not re.fullmatch(r"[0-9a-f]{64}", str(payload["validation_sha256"]))
    ):
        raise HookError("AliDNS challenge 状态内容无效")


def record_matches_state(record: dict[str, Any], state: dict[str, Any]) -> bool:
    """确认 RecordId 当前仍指向本 hook 创建的完整 TXT 内容。"""

    value = record.get("Value")
    return (
        record.get("DomainName") == state["root_domain"]
        and record.get("RR") == state["rr"]
        and record.get("Type") == state["type"]
        and isinstance(value, str)
        and hmac.compare_digest(challenge_digest(value), state["validation_sha256"])
    )


def publish_tombstone(state: dict[str, Any]) -> None:
    """保存不含明文 challenge 的公共解析清理检查材料。"""

    write_json_atomic(
        tombstone_path(str(state["domain"])),
        {
            "version": 1,
            "domain": state["domain"],
            "fqdn": f"_acme-challenge.{state['domain']}",
            "validation_sha256": state["validation_sha256"],
            "deleted_at": datetime.datetime.now(datetime.timezone.utc).strftime(
                "%Y-%m-%dT%H:%M:%SZ"
            ),
        },
    )


def remove_owned_record(
    client: AliDnsClient, domain: str, expected_validation: str | None
) -> None:
    """核对完整记录后只删除状态文件持有的 RecordId。"""

    path = state_path(domain)
    if not path.exists():
        return
    state_payload = read_json_object(path)
    validate_state(state_payload, domain)
    if expected_validation is not None and not hmac.compare_digest(
        challenge_digest(expected_validation), state_payload["validation_sha256"]
    ):
        raise HookError("cleanup challenge 与已持有状态不匹配")
    try:
        record = client.request(
            "DescribeDomainRecordInfo", RecordId=str(state_payload["record_id"])
        )
    except AliDnsApiError as error:
        if error.code not in NOT_FOUND_CODES:
            raise
    else:
        if not record_matches_state(record, state_payload):
            raise HookError("RecordId 当前内容与 hook 持有状态不匹配，拒绝删除")
        client.request("DeleteDomainRecord", RecordId=str(state_payload["record_id"]))
    publish_tombstone(state_payload)
    path.unlink()


def parse_txt_values(output: str) -> set[str]:
    """解析 dig 的一行或分段引号 TXT 输出。"""

    values: set[str] = set()
    for line in output.splitlines():
        try:
            chunks = shlex.split(line, posix=True)
        except ValueError:
            continue
        if chunks:
            values.add("".join(chunks))
    return values


def query_resolver(fqdn: str, resolver: str) -> tuple[bool, set[str]]:
    """查询单个公共解析器并区分空答案与网络失败。"""

    try:
        completed = subprocess.run(
            [
                "dig",
                "+time=5",
                "+tries=1",
                "+short",
                "TXT",
                fqdn,
                f"@{resolver}",
            ],
            check=False,
            capture_output=True,
            text=True,
            timeout=8,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False, set()
    if completed.returncode != 0:
        return False, set()
    return True, parse_txt_values(completed.stdout)


def query_all_resolvers(fqdn: str) -> list[tuple[bool, set[str]]]:
    """并发查询三家公共解析器，缩短单轮失败等待。"""

    with concurrent.futures.ThreadPoolExecutor(
        max_workers=len(PUBLIC_RESOLVERS)
    ) as executor:
        futures = [executor.submit(query_resolver, fqdn, item) for item in PUBLIC_RESOLVERS]
        return [future.result() for future in futures]


def wait_for_public_value(domain: str, validation: str) -> None:
    """要求三家公共解析器连续三轮返回精确 challenge。"""

    fqdn = f"_acme-challenge.{domain}"
    stable_checks = 0
    for _attempt in range(120):
        results = query_all_resolvers(fqdn)
        if all(ok and validation in values for ok, values in results):
            stable_checks += 1
            if stable_checks >= 3:
                return
        else:
            stable_checks = 0
        time.sleep(5)
    raise HookError("DNS-01 challenge 在 10 分钟内未完成三解析器稳定传播")


def authenticate_challenge() -> None:
    """创建单条 TXT、持有 RecordId，并等待公共 DNS 稳定传播。"""

    domain, rr, validation = require_challenge()
    client = client_from_credentials()
    remove_owned_record(client, domain, None)
    response = client.request(
        "AddDomainRecord",
        DomainName=ROOT_DOMAIN,
        RR=rr,
        Type="TXT",
        Value=validation,
        TTL="600",
    )
    record_id = str(response.get("RecordId", ""))
    if not re.fullmatch(r"[0-9]+", record_id):
        raise HookError("AliDNS AddDomainRecord 未返回有效 RecordId")
    state_payload = {
        "version": 1,
        "domain": domain,
        "root_domain": ROOT_DOMAIN,
        "rr": rr,
        "type": "TXT",
        "record_id": record_id,
        "validation_sha256": challenge_digest(validation),
    }
    try:
        write_json_atomic(state_path(domain), state_payload)
    except Exception:
        client.request("DeleteDomainRecord", RecordId=record_id)
        raise
    try:
        wait_for_public_value(domain, validation)
    except Exception:
        remove_owned_record(client, domain, validation)
        raise
    print(f"AliDNS DNS-01 challenge 已完成三解析器稳定传播：{domain}")


def cleanup_challenge() -> None:
    """清理当前 Certbot challenge 精确持有的 RecordId。"""

    domain, _rr, validation = require_challenge()
    client = client_from_credentials()
    remove_owned_record(client, domain, validation)
    print(f"AliDNS DNS-01 challenge 已按持有 RecordId 清理：{domain}")


def validate_tombstone(payload: dict[str, Any], domain: str) -> None:
    """验证清理检查材料不包含明文 challenge 且属于固定域名。"""

    if (
        set(payload)
        != {"version", "domain", "fqdn", "validation_sha256", "deleted_at"}
        or payload["version"] != 1
        or payload["domain"] != domain
        or payload["fqdn"] != f"_acme-challenge.{domain}"
        or not re.fullmatch(r"[0-9a-f]{64}", str(payload["validation_sha256"]))
    ):
        raise HookError("AliDNS cleanup 检查材料无效")


def wait_for_public_cleanup() -> None:
    """并行等待三条 challenge 从三家公共递归解析器缓存消失。"""

    tombstones: list[dict[str, Any]] = []
    for domain in ALLOWED_RECORDS:
        payload = read_json_object(tombstone_path(domain))
        validate_tombstone(payload, domain)
        tombstones.append(payload)

    stable_checks = 0
    for _attempt in range(90):
        all_clean = True
        for payload in tombstones:
            for ok, values in query_all_resolvers(str(payload["fqdn"])):
                if not ok or any(
                    hmac.compare_digest(challenge_digest(value), payload["validation_sha256"])
                    for value in values
                ):
                    all_clean = False
                    break
            if not all_clean:
                break
        if all_clean:
            stable_checks += 1
            if stable_checks >= 3:
                print("三条 DNS-01 challenge 已从三家公共解析器清理")
                return
        else:
            stable_checks = 0
        time.sleep(10)
    raise HookError("DNS-01 challenge 在 15 分钟内未从三家公共解析器清理")


def check_credentials() -> None:
    """用只读接口确认 AccessKey 可访问固定解析域。"""

    client_from_credentials().request(
        "DescribeDomainRecords",
        DomainName=ROOT_DOMAIN,
        PageNumber="1",
        PageSize="1",
    )
    print("AliDNS RAM 凭据只读连通性验证通过")


def main() -> int:
    """分派交互配置、auth、cleanup 和清理传播检查。"""

    if len(sys.argv) != 2 or sys.argv[1] not in {
        "configure",
        "check",
        "auth",
        "cleanup",
        "wait-cleanup",
    }:
        print(
            "用法：alidns-dns-hook.py configure|check|auth|cleanup|wait-cleanup",
            file=sys.stderr,
        )
        return 2
    try:
        require_root()
        command = sys.argv[1]
        if command == "configure":
            configure_credentials()
        elif command == "check":
            check_credentials()
        elif command == "auth":
            authenticate_challenge()
        elif command == "cleanup":
            cleanup_challenge()
        else:
            wait_for_public_cleanup()
    except HookError as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
