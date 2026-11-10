# bz-rsync

bz-rsync 把宝塔面板生成的网站和数据库备份通过 rsync 和 SSH 发送到 NAS。接收端运行在 Docker 中，适用于支持 Docker Compose 的群晖、飞牛 fnOS 和其他 Linux NAS。

中文控制台可查看传输进度、校验结果与历史记录，管理发送端配对、手动同步、备份保留和 Telegram 通知。NAS 在 SHA-256 校验通过后发布完整批次。

[界面预览](design/bz-rsync-preview.html)

## 1. 用 Docker Compose 部署 NAS 接收端

在 Linux NAS 上安装 Docker 和支持 `docker compose` 命令的 Compose。`v1.0.2` 的镜像地址是 `wzxsoft/bz-rsync:v1.0.2`，面向 Linux `amd64` 和 `arm64`。

将下面两个文件放到 NAS 的同一个项目目录。用 `.env.example` 创建 `.env`，预先建立独立备份目录，并给指定的 UID、GID 写入权限。下面以 `/srv/bz-rsync/backups` 为例，请换成 NAS 的实际共享目录路径。[Compose 文件](compose.yaml) · [环境变量示例](.env.example)

```sh
cp .env.example .env
sudo install -d -m 0750 -o 10001 -g 10001 /srv/bz-rsync/backups
```

| .env 设置 | 说明 |
| --- | --- |
| `BZ_IMAGE` | 正式版使用 `wzxsoft/bz-rsync:v1.0.2`。从源码构建时可填写本地镜像标签。 |
| `BZ_BACKUP_PATH` | NAS 已存在的专用备份目录，例如 `/srv/bz-rsync/backups`。 |
| `BZ_STORAGE_UID`、`BZ_STORAGE_GID` | 接收端使用的文件账号，示例为 `10001`、`10001`。按目录实际写入权限调整。 |
| `BZ_WEB_BIND`、`BZ_WEB_PORT` | Web 管理入口。示例使用 `0.0.0.0` 和 `8080`，请在受信任的局域网或 VPN 内访问。也可填写具体的 NAS 网络地址或 `127.0.0.1`。 |
| `BZ_SSH_BIND`、`BZ_SSH_PORT` | 宝塔发送端使用的接收入口。示例为 `127.0.0.1` 和 `2222`。宝塔在另一台主机上时，直接配对需要绑定到宝塔可达的 NAS 局域网或 VPN 地址。 |
| `BZ_PAIR_HOST` | 可选，配对页预填的主机名或 IP，填写宝塔能访问的地址。 |
| `BZ_LAYOUT` | 模板选用 `split`，例如 `site/20260115/` 和 `database/20260115/`。选 `dated` 时，网站、数据库和清单均在 `2026-01-15/` 下。 |

保存 `.env` 后运行。

```sh
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail 50 bz-rsync
```

浏览器打开 `http://NAS地址:8080`。首次登录只填密码 `password`，没有用户名。在「连接设置」修改初始密码。如果 Web 端口绑定在 `127.0.0.1`，可从电脑用 NAS 系统 SSH 转发，再打开 `http://127.0.0.1:18080`。

```sh
ssh -N -L 18080:127.0.0.1:8080 NAS_SSH_USER@NAS_HOST
```

Compose 的 `bzstate` 数据卷保存登录设置、接收端主机密钥、配对、历史和通知设置。归档保存在 `BZ_BACKUP_PATH` 指定的目录。升级时保留项目配置、数据卷和归档目录。

## 2. 安装并配对宝塔发送端

1. 在 NAS「连接设置」下载宝塔发送端安装包。解压后把完整的 `sender/` 目录上传到宝塔 Linux 主机。需要 Python 3.9+、rsync 3.1+、OpenSSH 客户端和时区数据。
2. 在「一次性配对码」填写宝塔可达的 NAS SSH 主机名或 IP，生成配对码。配对使用 `BZ_SSH_PORT`。同一局域网可填 NAS 局域网地址，不同网络可用双方可达的 VPN 地址。
3. 在宝塔终端以 root 进入 `sender/` 的上级目录，运行以下命令。提示出现后粘贴配对码并回车，配对码十分钟内有效。连接验证成功后会保存设置。

```sh
sh sender/install.sh --pair
python3 /opt/bz-rsync-sender/bz-rsync-send.py test-connection
```

已安装发送端可运行 `python3 /opt/bz-rsync-sender/bz-rsync-send.py pair` 重新配对。手动配对和 SSH 主机公钥核对见[发送端说明](sender/README.md#安装和配对)。

## 3. 设置来源并同步

- **直读宝塔归档**。在「同步总览 → 设置同步路径 → 宝塔备份来源」选择直接读取，填写网站和数据库备份目录以及文件名匹配式。也可在宝塔运行 `configure-sources` 设置本地来源。常用目录、日期变量和允许读取范围见[发送端来源设置](sender/README.md#选择备份来源)。
- **已整理批次**。选择 outbox 模式，并将 `source_root` 指向自己的待发目录。每个 `YYYY-MM-DD/` 下须有 `site/`、`database/` 和匹配的 `SHA256SUMS`。发送端可读取预先整理好的批次，也支持调用自己的整理脚本。

先在宝塔完成一份网站和数据库备份。直读模式确认归档已停止写入后运行。

```sh
python3 /opt/bz-rsync-sender/bz-rsync-send.py run --backup-completed
```

outbox 模式可运行 `python3 /opt/bz-rsync-sender/bz-rsync-send.py run 2026-01-15`，将日期换成已有批次。直读模式省略日期时选择北京时间当天，outbox 模式省略日期时选择最新日期目录。

通过[发送端计划任务说明](sender/README.md#计划任务和网页手动同步)配置定时发送。安装发送端的 systemd 定时器后，可在 NAS「同步总览 → 手动同步」提交指定日期。网页请求与宝塔计划任务共用发送锁。

传输中的任务可暂停、继续或取消。暂停保留已接收的暂存数据，继续时核对原批次清单。取消后，接收端在传输退出后清理该批次的暂存数据。相同日期的已入库批次会重新校验，内容一致时直接确认，内容不同时保留现有归档并报告冲突。

## 4. 保留和通知

「连接设置 → NAS 备份保留」的 `0` 关闭自动清理，也是初始值。可设置 `1` 至 `365` 天，按北京时间（`Asia/Shanghai`）计算，当天计为第 1 天。清理范围为已由 bz-rsync 校验入库、登记且仍与清单相符的归档。最近 60 次传输历史独立保存。

在「连接设置」填写 Telegram Bot Token 和接收会话的数字 Chat ID，启用并保存后可发送测试消息。群组和频道也可接收通知，公开频道可填写 `@用户名`。备份完成通知包含批次日期、结果、任务耗时和批次总大小。成功归档时，还会分别列出网站和数据库归档大小，三项大小均以 GB 显示。定时发送使用 `--scheduled`，已入库的相同批次复核通过后也可记录并发送通知。

「允许在 Telegram 手动同步」默认关闭。要启用，先开启通知，填写与 Bot 私聊的正数 Chat ID，再勾选此项并保存。为此功能使用专用 Bot，确保同一 Bot 没有其他轮询服务或 Webhook。已开始对话的私聊会收到常驻「手动同步」按钮，并可从输入框旁的 Telegram 菜单选择命令。首次使用 Bot 时点 Telegram 的「开始」即可建立私聊。命令和按钮都将北京时间当天的批次提交到与网页手动同步共用的队列。[Telegram Bot 菜单](https://core.telegram.org/bots/features#menu-button)

手动请求复核已入库批次成功后，Telegram 推送会列出批次总大小、网站归档大小、数据库归档大小和 NAS 的实际复核耗时。通过网页或 Telegram 提交的手动请求都使用此通知。若发送端在传输前失败，也会发送手动同步失败通知。

## 从源码构建

具备 Docker 构建环境时，也可在源码目录构建镜像并用同一份 Compose 配置启动。

```sh
docker build -t bz-rsync:source .
BZ_IMAGE=bz-rsync:source docker compose up -d --pull never
```

[Docker Compose 文档](https://docs.docker.com/compose/) · [rsync 官方手册](https://rsync.samba.org/ftp/rsync/rsync.1.html)
