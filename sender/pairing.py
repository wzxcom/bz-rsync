import base64
import binascii
import getpass
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import warnings
from pathlib import Path


MAX_CODE_LENGTH = 16_384
ED25519_PREFIX = b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"
PRIVATE_KEY_HEADER = re.compile(r"-----BEGIN (OPENSSH |RSA |EC )?PRIVATE KEY-----")


def ed25519_key(value):
    fields = value.split() if isinstance(value, str) else []
    if len(fields) != 2 or fields[0] != "ssh-ed25519":
        raise ValueError("配对只支持 Ed25519 SSH 公钥")
    try:
        raw = base64.b64decode(fields[1], validate=True)
    except (ValueError, binascii.Error):
        raise ValueError("配对公钥格式无效") from None
    if len(raw) != len(ED25519_PREFIX) + 32 or not raw.startswith(ED25519_PREFIX):
        raise ValueError("配对公钥格式无效")
    return f"ssh-ed25519 {base64.b64encode(raw).decode('ascii')}"


def parse_pair_code(raw):
    code = raw.strip()
    if len(code) > MAX_CODE_LENGTH or not re.fullmatch(r"BZ1-[A-Za-z0-9_-]+={0,2}", code):
        raise ValueError("配对码格式无效")
    body = code[4:]
    try:
        decoded = base64.b64decode(body + "=" * (-len(body) % 4), altchars=b"-_", validate=True)
        payload = json.loads(decoded)
    except (ValueError, binascii.Error, RecursionError):
        raise ValueError("配对码格式无效") from None
    if not isinstance(payload, dict) or type(payload.get("v")) is not int or payload["v"] != 1:
        raise ValueError("配对码版本无效")
    host, port = payload.get("host"), payload.get("port")
    if not isinstance(host, str) or not re.fullmatch(r"[A-Za-z0-9:.][A-Za-z0-9.:-]{0,252}", host):
        raise ValueError("配对码中的 NAS 主机名或 IP 地址无效")
    if type(port) is not int or not 1 <= port <= 65535:
        raise ValueError("配对码中的 SSH 端口无效")
    token = payload.get("token")
    if not isinstance(token, str) or not re.fullmatch(r"[A-Za-z0-9_-]{16,512}", token):
        raise ValueError("配对码的授权信息无效")
    key = payload.get("bootstrap_key")
    lines = key.strip().splitlines() if isinstance(key, str) else []
    if len(lines) < 3 or not PRIVATE_KEY_HEADER.fullmatch(lines[0]):
        raise ValueError("配对码的临时密钥无效")
    if lines[-1] != lines[0].replace("BEGIN", "END") or any(not re.fullmatch(r"[A-Za-z0-9+/=]+", line) for line in lines[1:-1]):
        raise ValueError("配对码的临时密钥无效")
    return host, port, ed25519_key(payload.get("host_key")), "\n".join(lines) + "\n", token


def read_pair_code():
    if not sys.stdin.isatty():
        return sys.stdin.readline(MAX_CODE_LENGTH + 2)
    with warnings.catch_warnings():
        warnings.simplefilter("error", getpass.GetPassWarning)
        try:
            return getpass.getpass("请粘贴 NAS 配对码（输入隐藏）> ")
        except getpass.GetPassWarning:
            raise RuntimeError("无法安全隐藏输入，请在宝塔终端重新运行配对") from None


def public_key_for(private_key):
    keygen = shutil.which("ssh-keygen")
    if not keygen:
        raise ValueError("请先安装 OpenSSH 客户端")
    result = subprocess.run(
        [keygen, "-y", "-P", "", "-f", private_key],
        input=b"", capture_output=True, timeout=15, check=False,
    )
    if result.returncode:
        raise ValueError("无法读取宝塔发送端的 Ed25519 私钥")
    fields = result.stdout.decode("ascii", errors="replace").split()
    return ed25519_key(" ".join(fields[:2]))


def write_private_file(path, content):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as handle:
        handle.write(content)
        handle.flush()
        os.fsync(handle.fileno())


def pair_sender(config_path, config, original, receiver_factory):
    folder = config_path.parent
    info = folder.stat()
    if info.st_uid != os.geteuid() or info.st_mode & 0o022:
        raise ValueError("配对配置目录须归发送账号所有，且不能由其他用户写入")
    host, port, host_key, bootstrap_key, token = parse_pair_code(read_pair_code())
    address = host if port == 22 else f"[{host}]:{port}"
    pin = f"{address} {host_key}\n".encode("ascii")
    final_pin = folder / ("known_hosts.bz-" + hashlib.sha256(pin).hexdigest()[:24])
    candidate = {
        **config, "host": host, "port": port, "user": "bzsender",
        "known_hosts_file": str(final_pin), "remote_sources": True,
    }

    with tempfile.TemporaryDirectory(prefix=".pair-", dir=folder) as scratch:
        temporary = Path(scratch)
        staged_pin, staged_config, bootstrap = (temporary / name for name in ("known_hosts", "config.json", "bootstrap"))
        write_private_file(staged_pin, pin)
        write_private_file(staged_config, (json.dumps(candidate, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
        write_private_file(bootstrap, bootstrap_key.encode("ascii"))
        temporary_config = {**candidate, "known_hosts_file": str(staged_pin)}
        regular_receiver = receiver_factory(temporary_config)
        sender_public = public_key_for(temporary_config["identity_file"])
        bootstrap_receiver = receiver_factory({**temporary_config, "identity_file": str(bootstrap)})
        if config_path.read_bytes() != original:
            raise ValueError("发送端配置已变更，请重新开始配对")
        try:
            enrolled = bootstrap_receiver.command("bz-enroll", {"token": token, "public_key": sender_public})
            if enrolled.get("ok") is not True:
                raise RuntimeError
        except (ValueError, OSError, RuntimeError, subprocess.TimeoutExpired):
            raise RuntimeError("NAS 配对失败，请检查连接或在 NAS 生成新配对码") from None
        finally:
            bootstrap.unlink()
        try:
            if regular_receiver.command("bz-ping").get("ok") is not True:
                raise RuntimeError
        except (ValueError, OSError, RuntimeError, subprocess.TimeoutExpired):
            raise RuntimeError("NAS 已接收发送端公钥，但连接核验失败，请重新生成配对码") from None
        if config_path.read_bytes() != original:
            raise ValueError("发送端配置已变更，请重新开始配对")
        staged_pin.replace(final_pin)
        staged_config.replace(config_path)
    print("配对成功，已验证宝塔到 NAS 的 SSH 连接")
