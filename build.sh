#!/bin/bash
# 构建 Mister 下载器 fnOS 应用（x86_64）
# 用法：UPSTREAM_DIR=/path/to/MisterDownloadManager-0.5.2-linux-x86_64 ./build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
VERSION="$(awk -F'=' '/^version/{gsub(/ /,"",$2); print $2}' fpk/src/manifest)"
OUT="$ROOT/mister-downloader_${VERSION}_x86.fpk"
UPSTREAM_DIR="${UPSTREAM_DIR:-}"

log() { echo "[build] $*"; }
die() { echo "[build][ERROR] $*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "未找到 go，请安装 Go 1.24+"
PYTHON="${PYTHON:-}"
if [ -z "$PYTHON" ]; then
  for c in python3 python; do
    if "$c" -c "import sys" >/dev/null 2>&1; then PYTHON="$c"; break; fi
  done
fi
[ -n "$PYTHON" ] || die "未找到可用的 python"
command -v fnpack >/dev/null 2>&1 || die "未找到 fnpack 1.2.x"

if [ -z "$UPSTREAM_DIR" ]; then
  die "请设置 UPSTREAM_DIR 指向已解压的上游 Linux x86_64 发行目录（含 misterdownloadmanager 与 misterdownloadmanager-cli）"
fi
for f in misterdownloadmanager misterdownloadmanager-cli; do
  [ -f "$UPSTREAM_DIR/$f" ] || die "上游文件缺失：$UPSTREAM_DIR/$f"
done

log "构建 Linux x86_64 适配层"
(cd app && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o "$ROOT/fpk/src/app/mdm-fnos-server" .)

log "复制上游引擎"
cp "$UPSTREAM_DIR/misterdownloadmanager" "$ROOT/fpk/src/app/"
cp "$UPSTREAM_DIR/misterdownloadmanager-cli" "$ROOT/fpk/src/app/"
chmod 0755 "$ROOT/fpk/src/app/mdm-fnos-server" \
           "$ROOT/fpk/src/app/misterdownloadmanager" \
           "$ROOT/fpk/src/app/misterdownloadmanager-cli"

log "fnpack 打包"
(cd fpk && fnpack build -d src)

log "修正权限与 checksum"
"$PYTHON" tools/patch-fpk.py "fpk/mister-downloader.fpk" "$OUT"

log "生成 SHA256SUMS.txt"
cd "$ROOT"
sha256sum "$(basename "$OUT")" \
  fpk/src/app/licenses/upstream-source-0.5.2.tar.gz \
  fpk/src/app/licenses/LICENSE > SHA256SUMS.txt

log "完成：$OUT"
