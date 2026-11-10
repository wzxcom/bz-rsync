#!/bin/sh
set -eu
umask 077

target=/opt/bz-rsync-sender
source_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
if [ "$#" -ne 0 ] && { [ "$#" -ne 1 ] || [ "$1" != "--pair" ]; }; then
    printf '%s\n' '用法：sh sender/install.sh [--pair]' >&2
    exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
    printf '%s\n' '请在宝塔服务器终端以 root 用户执行安装。' >&2
    exit 1
fi
for program in python3 rsync ssh ssh-keygen install stat; do
    if ! command -v "$program" >/dev/null 2>&1; then
        printf '缺少依赖 %s，请通过系统软件包管理器安装。\n' "$program" >&2
        exit 1
    fi
done
python3 -c 'import sys; sys.exit(sys.version_info < (3, 9))' || {
    printf '%s\n' '发送端需要 Python 3.9 或以上。' >&2
    exit 1
}
if [ -L "$target" ] || { [ -e "$target" ] && [ ! -d "$target" ]; }; then
    printf '安装目录不安全：%s\n' "$target" >&2
    exit 1
fi
if [ -d "$target" ] && [ "$(stat -c %u -- "$target")" -ne 0 ]; then
    printf '安装目录必须属于 root：%s\n' "$target" >&2
    exit 1
fi
install -d -m 0700 -- "$target"
for name in bz-rsync-send.py direct_sources.py pairing.py config.json id_ed25519 id_ed25519.pub known_hosts; do
    if [ -L "$target/$name" ] || { [ -e "$target/$name" ] && [ ! -f "$target/$name" ]; }; then
        printf '安装文件类型不安全：%s/%s\n' "$target" "$name" >&2
        exit 1
    fi
done
install -m 0700 -- "$source_dir/bz-rsync-send.py" "$target/bz-rsync-send.py"
install -m 0600 -- "$source_dir/direct_sources.py" "$target/direct_sources.py"
install -m 0600 -- "$source_dir/pairing.py" "$target/pairing.py"
if [ ! -f "$target/config.json" ]; then
    install -m 0600 -- "$source_dir/config.example.json" "$target/config.json"
fi
if [ ! -f "$target/id_ed25519" ]; then
    if [ -e "$target/id_ed25519.pub" ]; then
        printf '%s\n' '检测到旧公钥但缺少对应私钥，请先检查现有密钥。' >&2
        exit 1
    fi
    ssh-keygen -q -t ed25519 -N '' -C bz-rsync-sender -f "$target/id_ed25519"
fi
if [ ! -f "$target/known_hosts" ]; then
    : > "$target/known_hosts"
fi
printf '已安装到 %s\n' "$target"
printf '发送端公钥：%s/id_ed25519.pub\n' "$target"
printf '连接配置：%s/config.json\n' "$target"
if [ "${1-}" = "--pair" ]; then
    exec python3 "$target/bz-rsync-send.py" pair
fi
