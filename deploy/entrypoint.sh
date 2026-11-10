#!/bin/sh
set -eu
umask 077

uid=${BZ_STORAGE_UID:-10001}
gid=${BZ_STORAGE_GID:-10001}
if [ "$gid" != "$(id -g bzsender)" ]; then
    groupmod --gid "$gid" bzsender
fi
if [ "$uid" != "$(id -u bzsender)" ]; then
    usermod --uid "$uid" --gid bzsender bzsender
fi
install -d -m 0755 /run/sshd
install -d -m 0700 -o bzsender -g bzsender /run/bz-rsync /state
runuser -u bzsender -- test -w /backups || {
    echo "The dedicated NAS backup directory must be writable by UID $uid." >&2
    exit 1
}
if [ ! -s /state/ssh_host_ed25519_key ]; then
    ssh-keygen -q -t ed25519 -N '' -f /state/ssh_host_ed25519_key
fi
if [ ! -s /state/ssh_host_ed25519_key.pub ]; then
    ssh-keygen -y -f /state/ssh_host_ed25519_key > /state/ssh_host_ed25519_key.pub
fi
chmod 0644 /state/ssh_host_ed25519_key.pub
/usr/sbin/sshd -t -f /etc/ssh/bz-rsync.conf
/usr/sbin/sshd -D -e -f /etc/ssh/bz-rsync.conf &
export BZ_EXPECT_SSH=1
exec runuser -u bzsender -- /usr/local/bin/bz-rsync
