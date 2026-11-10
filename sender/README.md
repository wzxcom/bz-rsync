# 宝塔发送端

发送端安装在宝塔所在的 Linux 主机，将已完成的网站和数据库备份发送给 NAS。需要 Python 3.9+、rsync 3.1+、OpenSSH 客户端和 `Asia/Shanghai` 时区数据。以下日期按北京时间处理。

## 安装和配对

从 NAS 的「连接设置」下载发送端安装包。解压后把完整的 `sender/` 上传到宝塔主机。在 NAS 的「一次性配对码」填写宝塔能访问的 NAS SSH 主机名或 IP，生成配对码。远程宝塔可使用 NAS 的 VPN 地址，端口取 NAS 的 `BZ_SSH_PORT`，默认 `2222`。

在宝塔终端以 root 从 `sender/` 的上级目录执行安装。出现提示后粘贴配对码并回车，输入会隐藏。配对码有效期为十分钟，成功使用后失效。发送端验证主机密钥，并以长期密钥验证连接后保存配置。

```sh
sh sender/install.sh --pair
python3 /opt/bz-rsync-sender/bz-rsync-send.py test-connection
```

已安装的发送端可以运行 `python3 /opt/bz-rsync-sender/bz-rsync-send.py pair` 重新配对。

### 手动配对

1. 在宝塔以 root 运行 `sh sender/install.sh`。安装目录为 `/opt/bz-rsync-sender`。将 `/opt/bz-rsync-sender/id_ed25519.pub` 的整行公钥保存到 NAS「连接设置」的发送端配对区域。
2. 在宝塔编辑 `/opt/bz-rsync-sender/config.json` 的 `host` 和数字 `port`。接收端的 SSH 账号为 `bzsender`。
3. 在 NAS 的 Compose 项目目录核对接收端主机公钥及其指纹，再录入宝塔的 `/opt/bz-rsync-sender/known_hosts`。使用 `2222` 端口时，格式为 `[NAS主机名或IP]:2222 ssh-ed25519 公钥内容`。地址和端口应与 `config.json` 一致。

   ```sh
   docker compose exec -T bz-rsync cat /state/ssh_host_ed25519_key.pub
   docker compose exec -T bz-rsync ssh-keygen -lf /state/ssh_host_ed25519_key.pub
   ```

4. 运行上文的 `test-connection` 确认连接。宝塔发送端私钥保存在本机安装目录。

## 选择备份来源

在宝塔可运行 `configure-sources` 交互选择模式，或编辑 `/opt/bz-rsync-sender/config.json`。配对会启用 `remote_sources`。此时，NAS「同步总览 → 设置同步路径 → 宝塔备份来源」发布的来源会在下一次发送前应用。NAS 尚未发布来源时，发送端使用本地设置。需要由本机管理来源时，将 `remote_sources` 设为 `false`，并使用不带 `--stage-command` 的发送命令。

```sh
python3 /opt/bz-rsync-sender/bz-rsync-send.py configure-sources
python3 /opt/bz-rsync-sender/bz-rsync-send.py check-sources
```

`check-sources` 会读取 NAS 设置并检查宝塔端有效来源，也可追加日期检查对应的归档。NAS 发布的目录默认限于 `/www/backup/site` 和 `/www/backup/database`。其他备份目录由宝塔管理员在本地配置 `remote_website_roots` 或 `remote_database_roots`，值为允许读取的绝对目录列表。

### 直读网站和数据库归档

选择 `direct` 模式。在 NAS 来源设置或宝塔本地配置中分别填写网站和数据库目录。下面是本地 `config.json` 的来源字段示例，保留文件中现有的连接字段。

```json
{
  "source_mode": "direct",
  "website_sources": [
    {"directory": "/www/backup/site/example.com", "pattern": "*{date_compact}*.tar.gz"}
  ],
  "database_sources": [
    {"directory": "/www/backup/database/mysql/crontab_backup/example_db", "pattern": "*{date}*.sql.gz"}
  ],
  "quiet_seconds": 60,
  "direct_stage_root": "/opt/bz-rsync-sender/direct-batches"
}
```

匹配式针对目录直属文件名。`{date}` 展开为 `YYYY-MM-DD`，`{date_compact}` 展开为 `YYYYMMDD`。每条规则须匹配至少一个归档，同一类别的文件名应唯一。网站使用 `.tar.gz`，数据库使用 `.sql.gz`。

宝塔的两类备份均成功并超过静默时间后，核对文件再发送。将示例中的日期换成实际备份日期。下列 `preview` 适用于本地直读配置。使用 NAS 发布的来源时，改用 `check-sources 2026-01-15` 检查对应日期。

```sh
python3 /opt/bz-rsync-sender/bz-rsync-send.py preview 2026-01-15
python3 /opt/bz-rsync-sender/bz-rsync-send.py run --backup-completed 2026-01-15
```

发送命令省略日期时取北京时间当天。发送端检查归档在复制期间的稳定性和 gzip 完整性，再生成带清单的私有副本。请为 `direct_stage_root` 留出归档副本所需空间。由 NAS 来源设置发起的直读副本在入库确认后自动清理，失败副本用于重试。本机直读副本可在确认入库且发送端空闲后，按日期清理发送端生成的副本目录。

### 发送预先整理的 outbox 批次

选择 `outbox` 模式，将 `source_root` 设为待发根目录，默认 `/var/lib/bz-rsync/outbox`。先在独立工作目录整理网站、数据库和清单，再将完整日期目录移到同一文件系统上的待发目录。下面是待发目录示例。

```text
outbox/
└── 2026-01-15/
    ├── site/
    │   └── website.tar.gz
    ├── database/
    │   └── database.sql.gz
    └── SHA256SUMS
```

在工作目录中的日期目录内，可用 `sha256sum site/*.tar.gz database/*.sql.gz > SHA256SUMS` 生成示例清单。每条记录使用 `site/文件名` 或 `database/文件名` 的相对路径。

```sh
python3 /opt/bz-rsync-sender/bz-rsync-send.py run 2026-01-15
```

省略日期会选择最新的日期目录。若已有自己的整理脚本，可传入 `run --stage-command /usr/local/sbin/prepare-bz-rsync-outbox`。发送端先读取 NAS 来源设置，在 outbox 模式下以 `--date YYYY-MM-DD` 调用该脚本。脚本成功退出后，发送端读取 `source_root` 下的批次。使用宝塔实际安装的、由执行账号或 root 持有的受保护脚本路径。

## 计划任务和网页手动同步

在宝塔「计划任务」添加 Shell 脚本任务，安排在网站和数据库备份均成功之后。直读模式使用下面的命令。outbox 模式可在整理成功后调用 `run --scheduled` 并传入已经整理的日期。

```sh
/usr/bin/python3 /opt/bz-rsync-sender/bz-rsync-send.py run --backup-completed --scheduled
```

`--scheduled` 将已入库批次的成功复核记录为定时结果。宝塔任务日志记录执行结果，NAS 界面显示同步与校验状态。

要从 NAS「同步总览 → 手动同步」发起任务，在使用 systemd 的宝塔主机安装并启动发送端定时器。随包服务使用 `poll-manual --backup-completed`，适用于直读已完成归档或发送已经整理好的 outbox。若要在 outbox 模式即时整理，可把服务的 `ExecStart` 参数改为 `poll-manual --stage-command` 并填入自己的脚本路径。在宝塔终端以 root 从 `sender/` 的上级目录运行下列命令。

```sh
install -m 0644 sender/bz-rsync-manual.service sender/bz-rsync-manual.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now bz-rsync-manual.timer
```

发送端空闲时约每 30 秒领取一次请求，手动与计划任务共用发送锁。传输中断后，可以重试同一日期。接收端使用暂存数据继续传输，并在入库前校验清单和归档。需要限速时，在 `config.json` 设置 `bandwidth_limit_kib`，单位为 KiB/s，`0` 表示不限速。

[宝塔计划任务文档](https://docs.bt.cn/user-guide/crontab/task/)
