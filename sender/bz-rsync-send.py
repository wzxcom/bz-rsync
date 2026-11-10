#!/usr/bin/env python3
import argparse
import hashlib
import ipaddress
import json
import os
import re
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import threading
import time
from collections import deque
from datetime import date
from pathlib import Path


DEFAULT_CONFIG = Path("/opt/bz-rsync-sender/config.json")
DAY = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}")
RUN_ID = re.compile(r"[0-9a-f]{32}")
SHA256 = re.compile(r"[0-9a-f]{64}")
MANIFEST_ENTRY = re.compile(
    rb"([0-9a-fA-F]{64}) [ *](site|database)/([A-Za-z0-9_][A-Za-z0-9._-]*)"
)
PROGRESS = re.compile(
    r"^\s*([0-9,]+)\s+([0-9]{1,3})%\s+([0-9.]+[A-Za-z]+/s)\s+(\S+)"
)
SUBPROCESS_ENV = {
    **{key: value for key, value in os.environ.items() if not key.startswith("RSYNC_")},
    "LC_ALL": "C",
}


def parse_day(value):
    if not DAY.fullmatch(value):
        raise ValueError("批次日期须使用 YYYY-MM-DD")
    return date.fromisoformat(value).isoformat()


def safe_ssh_path(value, *, private=False):
    if not isinstance(value, str) or not re.fullmatch(r"/[A-Za-z0-9_./-]+", value):
        raise ValueError("SSH 密钥和 known_hosts 须使用不含空格的绝对路径")
    path = Path(value)
    info = path.lstat()
    forbidden = 0o077 if private else 0o022
    if path.resolve() != path or not stat.S_ISREG(info.st_mode) or info.st_mode & forbidden:
        raise ValueError(f"SSH 文件类型或权限不安全：{path}")
    return str(path)


class Receiver:
    def __init__(self, config):
        self.host = config["host"]
        user = config.get("user", "bzsender")
        port = config.get("port", 2222)
        try:
            self.ipv6 = ipaddress.ip_address(self.host).version == 6 and "%" not in self.host
        except ValueError:
            self.ipv6 = False
        if not self.ipv6 and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]{0,252}", self.host):
            raise ValueError("NAS 主机名或 IP 地址无效")
        if not re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", user) or type(port) is not int or not 1 <= port <= 65535:
            raise ValueError("SSH 用户或端口无效")
        self.user = user
        key = safe_ssh_path(config["identity_file"], private=True)
        known = safe_ssh_path(config["known_hosts_file"])
        ssh = shutil.which("ssh")
        if not ssh:
            raise ValueError("请先安装 OpenSSH 客户端")
        self.ssh = [
            ssh, "-F", "/dev/null", "-T", "-p", str(port), "-i", key,
            "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
            "-o", "IdentityAgent=none", "-o", "PreferredAuthentications=publickey",
            "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
            "-o", f"UserKnownHostsFile={known}", "-o", "GlobalKnownHostsFile=/dev/null",
            "-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=15",
            "-o", "ServerAliveCountMax=3", "-o", "ClearAllForwardings=yes",
        ]

    def command(self, name, data=None, *, json_output=True, timeout=15):
        result = subprocess.run(
            [*self.ssh, "--", f"{self.user}@{self.host}", name],
            input=json.dumps(data, ensure_ascii=False).encode("utf-8") if data else b"",
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout,
            env=SUBPROCESS_ENV, check=False,
        )
        if result.returncode:
            detail = (result.stderr or result.stdout).decode("utf-8", errors="replace").strip()
            raise RuntimeError(f"{name} 失败 ({result.returncode})：{detail[-600:]}")
        if json_output:
            response = json.loads(result.stdout)
            if not isinstance(response, dict):
                raise ValueError(f"{name} 返回了无效响应")
            return response
        return result.stdout.decode("utf-8", errors="replace").strip()

    def destination(self, run_id):
        host = f"[{self.host}]" if self.ipv6 else self.host
        return f"{self.user}@{host}:{run_id}/"


def batch_info(root, requested):
    if not root.is_absolute() or root.resolve(strict=True) != root or not root.is_dir():
        raise ValueError("待发目录须是存在的绝对路径，且不能使用符号链接")
    if requested:
        batch = root / parse_day(requested)
    else:
        candidates = []
        for item in root.iterdir():
            if not DAY.fullmatch(item.name) or not stat.S_ISDIR(item.lstat().st_mode):
                continue
            try:
                parse_day(item.name)
            except ValueError:
                continue
            candidates.append(item)
        if not candidates:
            raise ValueError(f"待发目录中没有日期批次：{root}")
        batch = max(candidates)
    if not stat.S_ISDIR(batch.lstat().st_mode):
        raise ValueError(f"批次不是常规目录：{batch}")
    if {item.name for item in batch.iterdir()} != {"site", "database", "SHA256SUMS"}:
        raise ValueError(f"{batch.name} 必须包含 site、database、SHA256SUMS")
    manifest = batch / "SHA256SUMS"
    manifest_stat = manifest.lstat()
    if not stat.S_ISREG(manifest_stat.st_mode):
        raise ValueError("SHA256SUMS 必须是普通文件")
    with manifest.open("rb") as handle:
        content = handle.read(1024 * 1024 + 1)
    if len(content) != manifest_stat.st_size or not 0 < len(content) <= 1024 * 1024:
        raise ValueError("SHA256SUMS 为空、过大或在读取时发生变化")
    names = {"site": set(), "database": set()}
    canonical_lines = []
    for entry in content.splitlines():
        match = MANIFEST_ENTRY.fullmatch(entry)
        if not match:
            raise ValueError("SHA256SUMS 存在无效记录")
        digest, raw_kind, raw_filename = match.groups()
        kind, filename = raw_kind.decode("ascii"), raw_filename.decode("ascii")
        if filename in names[kind]:
            raise ValueError("SHA256SUMS 存在重复记录")
        names[kind].add(filename)
        canonical_lines.append(digest.lower() + b"  " + raw_kind + b"/" + raw_filename + b"\n")
    total = len(content)
    for kind, expected in names.items():
        folder = batch / kind
        if not stat.S_ISDIR(folder.lstat().st_mode):
            raise ValueError(f"{kind} 必须是普通目录")
        files = list(folder.iterdir())
        if not expected or {item.name for item in files} != expected:
            raise ValueError(f"{kind} 文件与 SHA256SUMS 不符")
        for archive in files:
            info = archive.lstat()
            if not stat.S_ISREG(info.st_mode) or not info.st_size:
                raise ValueError(f"归档为空或不是普通文件：{archive.name}")
            total += info.st_size
    content_digest = hashlib.sha256(b"".join(sorted(canonical_lines))).hexdigest()
    return batch, total, hashlib.sha256(content).hexdigest(), content_digest


class ProgressReporter:
    def __init__(self, receiver, run_id):
        self.receiver = receiver
        self.run_id = run_id
        self.pending = None
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.publish, daemon=True)
        self.thread.start()

    def update(self, payload):
        self.pending = payload

    def publish(self):
        sent = None
        last = 0
        warned = False
        while not self.stopped.wait(2):
            current = self.pending
            if current is None or (current == sent and time.monotonic() - last < 12):
                continue
            try:
                self.receiver.command(
                    "bz-progress", {"run_id": self.run_id, **current}, json_output=False,
                )
                sent, last, warned = current, time.monotonic(), False
            except (RuntimeError, OSError, subprocess.TimeoutExpired) as error:
                if not warned:
                    print(f"暂时无法更新 NAS 进度：{error}", file=sys.stderr, flush=True)
                    warned = True

    def close(self):
        self.stopped.set()
        self.thread.join()


def output_lines(stream):
    pending = b""
    while block := stream.read(4096):
        lines = re.split(rb"[\r\n]", pending + block)
        pending = lines.pop()[-4096:]
        for line in lines:
            if line.strip():
                yield line.decode("utf-8", errors="replace").strip()
    if pending.strip():
        yield pending.decode("utf-8", errors="replace").strip()


def progress_payload(line):
    match = PROGRESS.match(line)
    if match is None:
        return None
    sent, percent, speed, eta = match.groups()
    return {
        "bytes": int(sent.replace(",", "")),
        "percent": min(int(percent), 99),
        "speed": speed,
        "eta": eta,
    }


def interrupt_transfer(process):
    if process.poll() is not None:
        return
    process.send_signal(signal.SIGINT)
    try:
        process.wait(timeout=10)
        return
    except subprocess.TimeoutExpired:
        process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def watch_run_control(receiver, run_id, process, stopped, actions):
    warned = False
    while not stopped.wait(3):
        try:
            response = receiver.command("bz-run-control", {"run_id": run_id}, timeout=8)
            if response.get("ok") is not True:
                raise ValueError("NAS 返回了无效的同步控制状态")
            warned = False
        except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired):
            if not warned:
                print("暂时无法读取 NAS 同步控制状态", file=sys.stderr, flush=True)
                warned = True
            continue
        action = response.get("action")
        if action in ("pause", "cancel"):
            actions.append(action)
            interrupt_transfer(process)
            return


def transfer(receiver, batch, run_id, bandwidth, lock_fd):
    rsync = shutil.which("rsync")
    if not rsync:
        raise ValueError("请先安装 rsync")
    remote_shell = receiver.ssh
    if receiver.ipv6:
        remote_shell = [sys.executable, str(Path(__file__).resolve()), "_ssh_ipv6", *remote_shell]
    command = [
        rsync, "-rt", "--checksum", "--no-links", "--no-inc-recursive",
        "--inplace",
        "--info=progress2", "--outbuf=N", "--timeout=120",
        "-e", shlex.join(remote_shell),
    ]
    if bandwidth:
        command.append(f"--bwlimit={bandwidth}")
    command.extend(["--", f"{batch}/", receiver.destination(run_id)])
    diagnostics = deque(maxlen=5)
    process = subprocess.Popen(
        command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        env=SUBPROCESS_ENV, bufsize=0, pass_fds=(lock_fd,),
    )
    progress = ProgressReporter(receiver, run_id)
    stopped = threading.Event()
    actions = []
    control = threading.Thread(
        target=watch_run_control, args=(receiver, run_id, process, stopped, actions), daemon=True,
    )
    control.start()
    try:
        with process.stdout:
            for line in output_lines(process.stdout):
                payload = progress_payload(line)
                if payload is not None:
                    progress.update(payload)
                else:
                    diagnostics.append(line)
                    print(line, flush=True)
        code = process.wait()
    finally:
        stopped.set()
        control.join()
        interrupt_transfer(process)
        progress.close()
    if actions:
        raise RuntimeError("同步已暂停" if actions[0] == "pause" else "同步已取消")
    if code:
        detail = " | ".join(diagnostics)
        raise RuntimeError(f"rsync 退出码 {code}：{detail[-700:]}")


def run_batch(receiver, config, batch_details, lock_fd, *, request_id=None, scheduled=False):
    bandwidth = config.get("bandwidth_limit_kib", 0)
    if type(bandwidth) is not int or not 0 <= bandwidth <= 2**31 - 1:
        raise ValueError("bandwidth_limit_kib 须为非负整数")
    batch, total, manifest_digest, content_digest = batch_details
    if not shutil.which("rsync"):
        raise ValueError("请先安装 rsync")
    print(f"准备发送 {batch.name}，{total:,} 字节", flush=True)
    start = {
        "day": batch.name, "total_bytes": total,
        "manifest_sha256": manifest_digest, "content_sha256": content_digest,
        "stage_mode": "inplace",
    }
    if request_id is not None:
        start["request_id"] = request_id
    if scheduled:
        start["scheduled"] = True
    started = receiver.command("bz-start", start, timeout=None)
    if started.get("already_done") is True:
        print(f"{batch.name} 已校验入库", flush=True)
        return
    run_id = started.get("run_id")
    if not isinstance(run_id, str) or not RUN_ID.fullmatch(run_id):
        raise ValueError("NAS 返回的运行编号无效")
    try:
        transfer(receiver, batch, run_id, bandwidth, lock_fd)
    except (Exception, KeyboardInterrupt) as error:
        try:
            receiver.command(
                "bz-fail", {"run_id": run_id, "error": str(error)[:800] or "发送端停止传输"},
                json_output=False,
            )
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as report_error:
            print(f"暂时无法上报传输失败：{report_error}", file=sys.stderr, flush=True)
        raise
    print("传输完成，等待 NAS 执行 SHA-256 校验", flush=True)
    receiver.command("bz-finish", {"run_id": run_id}, json_output=False, timeout=None)
    print(f"{batch.name} 已通过校验并入库", flush=True)


def handle_stop(signum, frame):
    raise KeyboardInterrupt


def prepare_batch(config, arguments, config_dir, namespace=None):
    from direct_sources import direct_day, snapshot, source_namespace

    requested = parse_day(arguments.day) if arguments.day else None
    script = arguments.stage_command
    if script and not script.is_absolute():
        raise ValueError("--stage-command 须使用宝塔脚本的绝对路径")
    if config.get("source_mode", "outbox") == "direct":
        if not (arguments.backup_completed or script):
            raise ValueError("直读模式须在宝塔备份成功后用 run --backup-completed 执行")
        day = direct_day(requested)
        namespace = namespace or source_namespace(config, "local")
        return batch_info(snapshot(config, day, config_dir, namespace), day.isoformat())

    if script:
        info = script.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in {0, os.geteuid()} or info.st_mode & 0o022:
            raise ValueError("--stage-command 须为由发送账号或 root 持有的受保护脚本")
        requested = direct_day(requested).isoformat()
        result = subprocess.run(["/bin/bash", str(script), "--date", requested], check=False)
        if result.returncode:
            raise RuntimeError(f"宝塔批次整理失败（退出码 {result.returncode}），详情见宝塔任务日志")
    source = Path(config.get("source_root", "/var/lib/bz-rsync/outbox"))
    return batch_info(source, requested)


def report_config(receiver, revision, error=None):
    message = "" if error is None else (str(error) or "宝塔来源准备已中断")[:500]
    try:
        receiver.command("bz-config-result", {
            "revision": revision,
            "ok": error is None,
            "error": message,
        }, json_output=False)
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as report_error:
        print(f"暂时无法向 NAS 报告来源设置状态：{report_error}", file=sys.stderr, flush=True)


def read_remote_config(receiver):
    published = receiver.command("bz-config")
    revision = published.get("revision")
    if type(revision) is not int or revision < 0:
        raise ValueError("NAS 返回的备份来源设置版本无效")
    return published, revision


def check_sources(receiver, config, requested):
    from direct_sources import SOURCE_TYPES, apply_remote_sources, direct_day, match_archives, source_rules, trusted_path

    published, revision = read_remote_config(receiver)
    effective, _ = apply_remote_sources(config, published)
    mode = effective.get("source_mode", "outbox")
    output = {"revision": revision, "source_mode": mode}
    if mode == "direct":
        day = direct_day(parse_day(requested) if requested else None)
        for _, key, suffix in SOURCE_TYPES:
            source_rules(effective, day, key, suffix)
            output[key] = effective[key]
        if requested:
            output["archives"] = [
                {"type": archive.kind, "path": str(archive.path), "bytes": archive.metadata.st_size}
                for archive in match_archives(effective, day)
            ]
    else:
        root = trusted_path(Path(effective.get("source_root", "/var/lib/bz-rsync/outbox")))
        output["source_root"] = str(root)
        if requested:
            batch, total, _, _ = batch_info(root, requested)
            output.update(day=batch.name, total_bytes=total)
    print(json.dumps(output, ensure_ascii=False, indent=2))


def configured_batch(receiver, config, arguments, config_dir):
    enabled = config.get("remote_sources", False)
    if type(enabled) is not bool:
        raise ValueError("remote_sources 须为 true 或 false")
    if not (enabled or arguments.stage_command):
        return config, prepare_batch(config, arguments, config_dir)

    from direct_sources import apply_remote_sources

    published, revision = read_remote_config(receiver)
    try:
        effective, namespace = apply_remote_sources(config, published)
        details = prepare_batch(effective, arguments, config_dir, namespace)
    except (Exception, KeyboardInterrupt) as error:
        report_config(receiver, revision, error)
        raise
    report_config(receiver, revision)
    return effective, details


def clear_confirmed_remote_snapshot(config, config_dir, batch):
    if config.get("source_mode") != "direct" or not batch.parent.name.startswith("nas-"):
        return
    from direct_sources import source_namespace

    root = Path(config.get("direct_stage_root", config_dir / "direct-batches"))
    if batch != root / source_namespace(config, "nas") / batch.name:
        return
    try:
        shutil.rmtree(batch)
    except OSError as error:
        print(f"NAS 已确认入库，宝塔暂存副本清理失败：{error}", file=sys.stderr, flush=True)


def send_configured_batch(receiver, config, arguments, config_dir, lock_fd, *, request_id=None, expected_manifest=None):
    effective, details = configured_batch(receiver, config, arguments, config_dir)
    if expected_manifest is not None and details[2] != expected_manifest:
        raise ValueError("该日期的备份清单已变更，无法继续原同步")
    run_batch(receiver, effective, details, lock_fd, request_id=request_id, scheduled=arguments.scheduled)
    clear_confirmed_remote_snapshot(effective, config_dir, details[0])


def poll_manual(receiver, config, arguments, config_dir, lock_fd):
    claim = receiver.command("bz-manual-claim")
    if claim.get("ok") is not True:
        raise RuntimeError("NAS 未返回有效的手动同步状态")
    request = claim.get("manual_sync")
    if request is None:
        return
    request_id = request.get("id") if isinstance(request, dict) else None
    if not isinstance(request_id, str) or not RUN_ID.fullmatch(request_id):
        raise ValueError("NAS 返回的手动同步编号无效")

    try:
        day = request.get("day")
        if not isinstance(day, str):
            raise ValueError("NAS 返回的手动同步日期无效")
        arguments.day = parse_day(day)
        expected_manifest = request.get("manifest_sha256")
        if expected_manifest == "":
            expected_manifest = None
        if expected_manifest is not None and (
            not isinstance(expected_manifest, str) or not SHA256.fullmatch(expected_manifest)
        ):
            raise ValueError("NAS 返回的续传清单摘要无效")
        send_configured_batch(
            receiver, config, arguments, config_dir, lock_fd,
            request_id=request_id, expected_manifest=expected_manifest,
        )
    except (Exception, KeyboardInterrupt) as error:
        try:
            receiver.command("bz-manual-result", {
                "request_id": request_id,
                "ok": False,
                "error": (str(error) or "手动同步已中断")[:500],
            }, json_output=False)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as report_error:
            print(f"暂时无法向 NAS 报告手动同步失败：{report_error}", file=sys.stderr, flush=True)
        raise
    receiver.command("bz-manual-result", {
        "request_id": request_id, "ok": True, "error": "",
    }, json_output=False)


def main():
    parser = argparse.ArgumentParser(description="bz-rsync 宝塔发送端")
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    actions = parser.add_subparsers(dest="action", required=True)
    actions.add_parser("test-connection")
    actions.add_parser("pair", help="用 NAS 生成的一次性配对码建立连接")
    actions.add_parser("configure-sources", help="交互设置备份来源")
    actions.add_parser("check-sources", help="读取 NAS 的有效来源设置并在宝塔检查目录").add_argument("day", nargs="?")
    actions.add_parser("preview").add_argument("day", nargs="?")
    run = actions.add_parser("run")
    run.add_argument("day", nargs="?")
    run.add_argument("--backup-completed", action="store_true", help="确认宝塔直读源的备份任务已成功完成")
    run.add_argument("--stage-command", type=Path, help="宝塔备份完成后调用；outbox 模式先执行指定整理脚本")
    run.add_argument("--scheduled", action="store_true", help="记录定时任务对已入库批次的复核结果")
    poll = actions.add_parser("poll-manual", help="领取并执行 NAS 控制台发起的手动同步")
    poll.add_argument("--stage-command", type=Path, help="outbox 模式可用的整理脚本")
    poll.add_argument("--backup-completed", action="store_true", help="直读手动同步时选择已完成的归档")
    poll.set_defaults(scheduled=False)
    arguments = parser.parse_args()
    signal.signal(signal.SIGTERM, handle_stop)
    os.umask(0o077)
    config_path = arguments.config.resolve(strict=True)
    original = config_path.read_bytes()
    config = json.loads(original)
    if not isinstance(config, dict):
        raise ValueError("发送端配置应为 JSON 对象")
    source_mode = config.get("source_mode", "outbox")
    if source_mode not in {"outbox", "direct"}:
        raise ValueError("source_mode 须为 outbox 或 direct")
    if arguments.action == "configure-sources":
        from direct_sources import configure_sources

        configure_sources(config_path, config, original)
        return
    if arguments.action == "preview":
        if source_mode != "direct":
            raise ValueError("preview 仅用于 direct 源模式")
        from direct_sources import direct_day, match_archives

        day = direct_day(parse_day(arguments.day) if arguments.day else None)
        for archive in match_archives(config, day):
            print(f"{archive.kind}\t{archive.metadata.st_size}\t{archive.path}")
        return
    if arguments.action == "test-connection":
        print(json.dumps(Receiver(config).command("bz-ping"), ensure_ascii=False))
        return
    if arguments.action == "check-sources":
        check_sources(Receiver(config), config, arguments.day)
        return
    import fcntl

    config_dir = config_path.parent
    lock = config_dir / ".send.lock"
    fd = os.open(lock, os.O_CREAT | os.O_WRONLY | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    with os.fdopen(fd, "wb") as handle:
        if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
            raise ValueError("发送锁必须是普通文件")
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            if arguments.action == "poll-manual":
                return
            raise ValueError("已有发送任务正在运行") from None
        if arguments.action == "pair":
            from pairing import pair_sender

            pair_sender(config_path, config, original, Receiver)
        elif arguments.action == "poll-manual":
            receiver = Receiver(config)
            poll_manual(receiver, config, arguments, config_dir, handle.fileno())
        else:
            receiver = Receiver(config)
            send_configured_batch(receiver, config, arguments, config_dir, handle.fileno())


if __name__ == "__main__":
    if len(sys.argv) > 2 and sys.argv[1] == "_ssh_ipv6":
        ssh, *args = sys.argv[2:]
        # Some rsync builds append -4 even for a bracketed IPv6 literal.
        os.execv(ssh, [ssh, "-6", *(arg for arg in args if arg != "-4")])
    try:
        main()
    except (KeyError, TypeError, ValueError, OSError, RuntimeError, subprocess.TimeoutExpired) as error:
        print(f"bz-rsync：{error}", file=sys.stderr, flush=True)
        sys.exit(1)
    except EOFError:
        print("bz-rsync：设置输入已结束", file=sys.stderr, flush=True)
        sys.exit(1)
    except KeyboardInterrupt:
        print("bz-rsync：发送端任务已中断", file=sys.stderr, flush=True)
        sys.exit(130)
