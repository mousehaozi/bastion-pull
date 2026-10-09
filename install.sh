#!/usr/bin/env bash
# bastion-pull installer / bastion-pull 安装脚本
#
#   curl -fsSL https://raw.githubusercontent.com/mousehaozi/bastion-pull/main/install.sh | sudo bash
#
# Options / 参数:
#   --version <tag>    install a specific release tag (default: latest)   安装指定版本（默认最新）
#   --file <tar.gz>    install from a local archive, no network needed    使用本地压缩包安装（离线）
#   --uninstall        remove binary and service, keep config             卸载程序和服务，保留配置
#   -h, --help
# Environment / 环境变量:
#   GH_PROXY=https://ghfast.top/   prefix for GitHub download URLs (mirror) / GitHub 下载加速前缀
set -euo pipefail

REPO="${BP_REPO:-mousehaozi/bastion-pull}"
BIN_DIR="${BP_BIN_DIR:-/usr/local/bin}"
CONF_DIR="${BP_CONF_DIR:-/etc/bastion-pull}"
UNIT_DIR="${BP_UNIT_DIR:-/etc/systemd/system}"
GH_PROXY="${GH_PROXY:-}"
SERVICE="bastion-pull"

VERSION="" ; LOCAL_FILE="" ; UNINSTALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="${2:?--version needs a value}"; shift 2 ;;
    --file)      LOCAL_FILE="${2:?--file needs a path}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)   sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!! \033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx \033[0m %s\n' "$*" >&2; exit 1; }

[ "${BP_SKIP_ROOT_CHECK:-0}" = 1 ] || [ "$(id -u)" -eq 0 ] || die "please run as root (use sudo) / 请使用 root 或 sudo 运行"
HAVE_SYSTEMD=0
if [ "${BP_SKIP_SYSTEMD:-0}" != 1 ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then HAVE_SYSTEMD=1; fi

# ---------- uninstall ----------
if [ "$UNINSTALL" -eq 1 ]; then
  if [ "$HAVE_SYSTEMD" -eq 1 ]; then
    systemctl disable --now "$SERVICE" 2>/dev/null || true
    rm -f "$UNIT_DIR/$SERVICE.service"
    systemctl daemon-reload
  fi
  rm -f "$BIN_DIR/bastion-pull"
  info "uninstalled. Config kept in $CONF_DIR / 已卸载，配置保留在 $CONF_DIR"
  exit 0
fi

# ---------- detect architecture ----------
case "$(uname -m)" in
  x86_64|amd64)   ARCH=amd64 ;;
  aarch64|arm64)  ARCH=arm64 ;;
  armv7l|armv7)   ARCH=armv7 ;;
  i386|i486|i586|i686) ARCH=386 ;;
  riscv64)        ARCH=riscv64 ;;
  loongarch64)    ARCH=loong64 ;;
  mips64|mips64el) ARCH=mips64le ;;
  *) die "unsupported architecture: $(uname -m) / 不支持的架构" ;;
esac
[ "$(uname -s)" = Linux ] || die "Linux only / 仅支持 Linux"
info "architecture: linux/$ARCH"

for c in tar sha256sum; do command -v "$c" >/dev/null 2>&1 || die "missing command: $c"; done
fetch() { # fetch <url> <dest>
  if command -v curl >/dev/null 2>&1; then curl -fL --retry 5 --retry-all-errors --connect-timeout 15 -C - -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then wget -c -t 5 -O "$2" "$1"
  else die "need curl or wget"; fi
}

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# ---------- obtain archive ----------
if [ -n "$LOCAL_FILE" ]; then
  [ -f "$LOCAL_FILE" ] || die "file not found: $LOCAL_FILE"
  ARCHIVE="$LOCAL_FILE"
  SUMS="$(dirname "$LOCAL_FILE")/checksums.txt"
  if [ -f "$SUMS" ] && grep -q " $(basename "$ARCHIVE")\$" "$SUMS"; then
    (cd "$(dirname "$ARCHIVE")" && grep " $(basename "$ARCHIVE")\$" checksums.txt | sha256sum -c -) || die "checksum mismatch"
  else
    warn "no checksums.txt next to the archive, skipping verification"
  fi
else
  if [ -z "$VERSION" ]; then
    info "looking up latest release"
    fetch "${GH_PROXY}https://api.github.com/repos/$REPO/releases/latest" "$TMP/latest.json" >/dev/null 2>&1 \
      || die "cannot reach GitHub. Download the archive manually and use --file, or set GH_PROXY. / 无法连接 GitHub，可手动下载后用 --file 安装，或设置 GH_PROXY"
    VERSION="$(grep -m1 '"tag_name"' "$TMP/latest.json" | sed -E 's/.*"tag_name"[^"]*"([^"]+)".*/\1/')"
    [ -n "$VERSION" ] || die "cannot determine latest version"
  fi
  info "version: $VERSION"
  NAME="bastion-pull_${VERSION}_linux_${ARCH}.tar.gz"
  BASE="${GH_PROXY}https://github.com/$REPO/releases/download/$VERSION"
  info "downloading $NAME"
  fetch "$BASE/$NAME" "$TMP/$NAME" || die "download failed / 下载失败"
  fetch "$BASE/checksums.txt" "$TMP/checksums.txt" || die "cannot download checksums.txt"
  (cd "$TMP" && grep " $NAME\$" checksums.txt | sha256sum -c -) || die "checksum mismatch / 校验失败"
  ARCHIVE="$TMP/$NAME"
fi

mkdir -p "$TMP/x"
tar -xzf "$ARCHIVE" -C "$TMP/x"
BIN_SRC="$(find "$TMP/x" -type f -name bastion-pull | head -n1)"
[ -n "$BIN_SRC" ] || die "archive does not contain the bastion-pull binary"
EXAMPLE="$(find "$TMP/x" -type f -name config.example.yaml | head -n1)"

# ---------- install binary ----------
WAS_ACTIVE=0
if [ "$HAVE_SYSTEMD" -eq 1 ] && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then WAS_ACTIVE=1; fi
mkdir -p "$BIN_DIR"
install -m 755 "$BIN_SRC" "$BIN_DIR/bastion-pull.new"
mv -f "$BIN_DIR/bastion-pull.new" "$BIN_DIR/bastion-pull"
info "installed $BIN_DIR/bastion-pull"

# ---------- config ----------
mkdir -p "$CONF_DIR"
CONF="$CONF_DIR/config.yaml"
NEW_CONFIG=0
if [ -f "$CONF" ]; then
  info "config exists, left untouched: $CONF"
else
  [ -n "$EXAMPLE" ] || die "archive does not contain config.example.yaml"
  install -m 600 "$EXAMPLE" "$CONF"
  NEW_CONFIG=1
  info "created default config: $CONF"
fi
chmod 600 "$CONF"

# ---------- systemd ----------
if [ "$HAVE_SYSTEMD" -eq 1 ]; then
  cat > "$UNIT_DIR/$SERVICE.service" <<UNIT
[Unit]
Description=bastion-pull release watcher
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN_DIR/bastion-pull -c $CONF
Restart=always
RestartSec=10
# If the server needs a proxy to reach GitHub / 如需代理访问 GitHub，取消注释：
#Environment=HTTPS_PROXY=http://127.0.0.1:7890

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable "$SERVICE" >/dev/null 2>&1
  info "service registered and enabled at boot / 已注册 systemd 服务并设为开机自启"

  if [ "$WAS_ACTIVE" -eq 1 ]; then
    systemctl restart "$SERVICE"
    info "service restarted with the new version / 服务已重启"
  elif [ "$NEW_CONFIG" -eq 0 ] && ! grep -q 'github_pat_xxxx' "$CONF"; then
    systemctl start "$SERVICE"
    info "service started / 服务已启动"
  fi
else
  warn "systemd not available: service not registered. Run manually: bastion-pull -c $CONF"
fi

# ---------- summary ----------
cat <<MSG

=====================================================================
 bastion-pull installed. / 安装完成
=====================================================================
 Command / 命令  : bastion-pull   ($BIN_DIR/bastion-pull)
 Config  / 配置  : $CONF
MSG
if grep -q 'github_pat_xxxx' "$CONF"; then
cat <<MSG

 NEXT STEP / 下一步 (required / 必须):
   1. Edit the config: token, repo, assets, restart command
      编辑配置文件：填写 token、仓库、资产路径和重启命令
        sudo vi $CONF
   2. Test once in the foreground / 前台试跑一次:
        sudo bastion-pull -c $CONF -once
   3. Start the service / 启动服务:
        sudo systemctl start $SERVICE

 The service is enabled: it auto-starts on boot and restarts on failure.
 服务已设置开机自启，异常退出会自动重启。
MSG
else
cat <<MSG

 Logs / 日志    : journalctl -u $SERVICE -f
 Status / 状态  : systemctl status $SERVICE
MSG
fi
echo
