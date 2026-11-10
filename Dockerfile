FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY VERSION go.mod main.go receiver.go verify.go retention.go auth.go layout.go split_verify.go paths.go sources.go manual.go pairing.go run_control.go telegram.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bz-rsync .

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates openssh-server rsync python3 tzdata tini util-linux \
    && rm -rf /var/lib/apt/lists/* /etc/ssh/ssh_host_* \
    && test -x /usr/bin/rrsync \
    && useradd --uid 10001 --create-home --shell /bin/sh bzsender \
    && passwd -d bzsender
COPY --from=build --chmod=0755 /bz-rsync /usr/local/bin/bz-rsync
COPY deploy/sshd_config /etc/ssh/bz-rsync.conf
COPY deploy/entrypoint.sh /usr/local/bin/bz-entrypoint
COPY --chmod=0644 design/bz-rsync-preview.html /app/index.html
COPY --chmod=0644 sender/install.sh sender/bz-rsync-send.py sender/direct_sources.py sender/pairing.py sender/config.example.json sender/README.md sender/bz-rsync-manual.service sender/bz-rsync-manual.timer /tmp/bz-sender-bundle/sender/
RUN <<'SH'
set -eu
mkdir -p /app/downloads
chmod 0755 /app /app/downloads
cd /tmp/bz-sender-bundle
python3 - <<'PY'
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

with ZipFile("/app/downloads/sender.zip", "w", ZIP_DEFLATED, strict_timestamps=False) as archive:
    for file in sorted(Path("sender").iterdir()):
        archive.write(file, file.as_posix())
PY
chmod 0644 /app/downloads/sender.zip
cd /
rm -rf /tmp/bz-sender-bundle
SH
EXPOSE 8080 2222
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/bz-rsync", "healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/bin/sh", "/usr/local/bin/bz-entrypoint"]
