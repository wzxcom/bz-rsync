import fnmatch
import gzip
import hashlib
import json
import os
import re
import stat
import tempfile
import time
import zlib
from dataclasses import dataclass
from datetime import date, datetime
from pathlib import Path
from zoneinfo import ZoneInfo


SOURCE_TYPES = (
    ("site", "website_sources", ".tar.gz"),
    ("database", "database_sources", ".sql.gz"),
)
NAME = re.compile(r"[A-Za-z0-9_][A-Za-z0-9._-]*")
GLOB = re.compile(r"[A-Za-z0-9_.?*\[\]-]+")
BLOCK = 4 * 1024 * 1024


@dataclass(frozen=True)
class Archive:
    kind: str
    path: Path
    metadata: os.stat_result


def direct_day(requested):
    today = datetime.now(ZoneInfo("Asia/Shanghai")).date()
    day = date.fromisoformat(requested) if requested else today
    if day > today:
        raise ValueError("备份日期不能晚于北京时间的今天")
    return day


def trusted_path(path):
    if not path.is_absolute() or path.resolve(strict=True) != path or not path.is_dir():
        raise ValueError(f"须填写存在且无符号链接的绝对目录：{path}")
    return path


def rule_pattern(template, day, key, suffix):
    if not isinstance(template, str) or ("{date}" not in template and "{date_compact}" not in template):
        raise ValueError(f"{key} 匹配规则须包含 {{date}} 或 {{date_compact}}")
    pattern = template.replace("{date}", day.isoformat()).replace("{date_compact}", day.strftime("%Y%m%d"))
    if not GLOB.fullmatch(pattern) or not pattern.endswith(suffix):
        raise ValueError(f"{key} 匹配规则须为以 {suffix} 结尾的文件名通配式")
    return pattern


def source_rules(config, day, key, suffix):
    rules = config.get(key)
    if not isinstance(rules, list) or not rules:
        raise ValueError(f"{key} 至少需要一个备份目录")
    return [
        (trusted_path(Path(rule["directory"])), rule_pattern(rule["pattern"], day, key, suffix))
        for rule in rules
    ]


def source_namespace(config, prefix):
    rules = {}
    for _, key, _ in SOURCE_TYPES:
        entries = config.get(key)
        if not isinstance(entries, list) or not entries:
            raise ValueError(f"{key} 至少需要一个备份目录")
        rules[key] = sorted((entry["directory"], entry["pattern"]) for entry in entries)
    content = json.dumps(rules, sort_keys=True, ensure_ascii=False).encode("utf-8")
    return prefix + "-" + hashlib.sha256(content).hexdigest()[:24]


def apply_remote_sources(local, published):
    if published.get("ok") is not True:
        raise ValueError("NAS 未返回有效的备份来源设置")
    if published["revision"] == 0:
        if published.get("source_mode", "outbox") != "outbox":
            raise ValueError("NAS 默认备份来源设置无效")
        return local, None
    mode = published.get("source_mode")
    if mode not in {"outbox", "direct"}:
        raise ValueError("NAS 发布的备份来源模式无效")
    config = {**local, "source_mode": mode}
    if mode == "outbox":
        return config, None

    rules = {}
    for kind, key, suffix in SOURCE_TYPES:
        rules[key] = published.get(key)
        root_name = "remote_website_roots" if kind == "site" else "remote_database_roots"
        default_root = "/www/backup/site" if kind == "site" else "/www/backup/database"
        roots = local.get(root_name, [default_root])
        if not isinstance(roots, list) or not roots:
            raise ValueError(f"{root_name} 须包含允许 NAS 选择的目录")
        allowed = [trusted_path(Path(root)) for root in roots]
        for folder, _ in source_rules(rules, date(2000, 1, 1), key, suffix):
            if not any(folder.is_relative_to(root) for root in allowed):
                raise ValueError(f"宝塔未授权 NAS 选择此备份目录：{folder}")
    # Isolate a changed set of sources from today's immutable copy.
    effective = {**config, **rules}
    return effective, source_namespace(effective, "nas")


def match_archives(config, day):
    quiet = config.get("quiet_seconds", 60)
    if type(quiet) is not int or quiet < 30:
        raise ValueError("quiet_seconds 须为至少 30 的整数")
    cutoff = time.time_ns() - quiet * 1_000_000_000
    chosen = []
    for kind, key, suffix in SOURCE_TYPES:
        names = set()
        for folder, pattern in source_rules(config, day, key, suffix):
            matches = sorted(item for item in folder.iterdir() if fnmatch.fnmatchcase(item.name, pattern))
            if not matches:
                raise ValueError(f"找不到 {day.isoformat()} 的备份：{folder}/{pattern}")
            for path in matches:
                if not NAME.fullmatch(path.name) or path.name in names:
                    raise ValueError(f"备份文件名不受支持或在同一类别重复：{path.name}")
                metadata = path.lstat()
                if not stat.S_ISREG(metadata.st_mode) or not metadata.st_size:
                    raise ValueError(f"备份为空或不是普通文件：{path}")
                if max(metadata.st_mtime_ns, metadata.st_ctime_ns) > cutoff:
                    raise ValueError(f"备份最近仍有写入，请在备份完成 {quiet} 秒后重试：{path}")
                names.add(path.name)
                chosen.append(Archive(kind, path, metadata))
    return chosen


def prompt_rules(current, label, default_pattern):
    if isinstance(current, list):
        for rule in current:
            if isinstance(rule, dict):
                print(f"当前{label}目录 {rule.get('directory', '')}，匹配 {rule.get('pattern', '')}")
    rules = []
    folder = input(f"{label}备份目录（回车保留已有设置）> ").strip()
    while folder:
        folder = str(trusted_path(Path(folder)))
        pattern = input(f"文件名匹配式 [{default_pattern}] > ").strip() or default_pattern
        rules.append({"directory": folder, "pattern": pattern})
        folder = input(f"下一个{label}目录（回车结束）> ").strip()
    return rules or current


def configure_sources(config_path, config, original):
    mode = config.get("source_mode", "outbox")
    print("1 使用已整理的日期批次")
    print("2 选择宝塔网站和数据库的备份目录")
    selected = input(f"备份来源 [{1 if mode == 'outbox' else 2}] > ").strip()
    if selected:
        try:
            mode = {"1": "outbox", "2": "direct"}[selected]
        except KeyError:
            raise ValueError("请输入 1 或 2") from None
    updated = {**config, "source_mode": mode}
    if mode == "outbox":
        default = config.get("source_root", "/var/lib/bz-rsync/outbox")
        root = input(f"已整理批次目录 [{default}] > ").strip() or default
        updated["source_root"] = str(trusted_path(Path(root)))
    else:
        for kind, key, suffix in SOURCE_TYPES:
            label, token = ("网站", "{date_compact}") if kind == "site" else ("数据库", "{date}")
            updated[key] = prompt_rules(config.get(key), label, f"*{token}*{suffix}")
            # Use a representative date to validate patterns without scanning for current backups.
            source_rules(updated, date(2000, 1, 1), key, suffix)
    if updated == config:
        print("备份来源设置已是当前值")
        return
    if config_path.read_bytes() != original:
        raise ValueError("配置文件在设置期间发生变更，请重新运行")
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", newline="\n", dir=config_path.parent, delete=False,
        ) as output:
            temporary = Path(output.name)
            json.dump(updated, output, ensure_ascii=False, indent=2)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(config_path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    print(f"备份来源设置已保存到 {config_path}")


def identity(info):
    return info.st_dev, info.st_ino, info.st_mode, info.st_size, info.st_mtime_ns, info.st_ctime_ns


def copy_archive(archive, destination):
    expected = identity(archive.metadata)
    digest = hashlib.sha256()
    source_fd = os.open(archive.path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(source_fd, "rb") as source, destination.open("xb") as output:
        if identity(os.fstat(source.fileno())) != expected:
            raise ValueError(f"备份已变更：{archive.path}")
        while block := source.read(BLOCK):
            digest.update(block)
            output.write(block)
        if identity(os.fstat(source.fileno())) != expected:
            raise ValueError(f"复制时备份发生变更：{archive.path}")
        output.flush()
        os.fsync(output.fileno())
    try:
        with gzip.open(destination, "rb") as compressed:
            if not compressed.read(1):
                raise ValueError(f"备份解压后为空：{archive.path}")
            while compressed.read(BLOCK):
                pass
    except (gzip.BadGzipFile, EOFError, zlib.error) as error:
        raise ValueError(f"备份压缩校验失败：{archive.path}") from error
    return digest.hexdigest()


def private_stage_root(root):
    root.mkdir(mode=0o700, exist_ok=True)
    info = trusted_path(root).stat()
    if info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise ValueError("直读批次目录须归发送账号所有，权限为 0700")
    return root


def snapshot(config, day, config_dir, namespace=None):
    root = Path(config.get("direct_stage_root", config_dir / "direct-batches"))
    parent = trusted_path(root.parent)
    parent_info = parent.stat()
    if root == root.parent or parent_info.st_uid not in {0, os.geteuid()} or parent_info.st_mode & 0o022:
        raise ValueError("直读批次目录须位于可信且非公共可写的目录下")
    root = private_stage_root(root)
    if namespace is not None:
        root = private_stage_root(root / namespace)
    ready = root / day.isoformat()
    if ready.is_symlink():
        raise ValueError(f"直读批次路径不能是符号链接：{ready}")
    if ready.exists():
        return root

    chosen = match_archives(config, day)
    with tempfile.TemporaryDirectory(prefix=".prepare-", dir=root) as temporary:
        batch = Path(temporary) / "batch"
        batch.mkdir(mode=0o700)
        for kind, _, _ in SOURCE_TYPES:
            (batch / kind).mkdir(mode=0o700)
        with (batch / "SHA256SUMS").open("x", encoding="ascii", newline="\n") as manifest:
            for archive in sorted(chosen, key=lambda item: (item.kind, item.path.name)):
                relative = f"{archive.kind}/{archive.path.name}"
                digest = copy_archive(archive, batch / relative)
                manifest.write(f"{digest}  {relative}\n")
            manifest.flush()
            os.fsync(manifest.fileno())
        for archive in chosen:
            if identity(archive.path.lstat()) != identity(archive.metadata):
                raise ValueError(f"备份在批次整理期间发生变更：{archive.path}")
        batch.rename(ready)
    return root
