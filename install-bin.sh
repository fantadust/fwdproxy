#!/usr/bin/env bash
# 第一步：把新二进制就位。不影响正在运行的服务——mv 只改目录项，
# 运行中的进程继续使用旧 inode，所以这一步随时可做，无需停机。
# 就位后执行 ./restart.sh 才真正生效。
#
# 用法：sudo ./install-bin.sh [新二进制路径]     默认 ./fwdproxy-linux-amd64
set -euo pipefail

APP_DIR=/data/apps/fwdproxy
RUN_USER=fwdproxy
NEW_BIN="${1:-./fwdproxy-linux-amd64}"

[ -f "$NEW_BIN" ] || { echo "找不到新二进制：$NEW_BIN"; exit 1; }
[ -d "$APP_DIR" ] || { echo "目录不存在：$APP_DIR"; exit 1; }

# 必须是 ELF 可执行文件，挡住"传错文件"（比如传了 .conf 或 macOS 版）
if [ "$(od -An -c -N4 "$NEW_BIN" | tr -d ' \n')" != "177ELF" ]; then
  echo "$NEW_BIN 不是 Linux ELF 可执行文件，拒绝安装"; exit 1
fi

# 架构必须和本机匹配，否则重启后才会暴露 Exec format error
# ELF header 偏移 18 的 e_machine：62=x86-64，183=aarch64
MACH=$(od -An -tu1 -j18 -N1 "$NEW_BIN" | tr -d ' \n')
case "$(uname -m)" in
  x86_64)  WANT=62;  ARCH_NAME=x86-64;;
  aarch64|arm64) WANT=183; ARCH_NAME=aarch64;;
  *) WANT=""; ARCH_NAME="$(uname -m)";;
esac
if [ -n "$WANT" ] && [ "$MACH" != "$WANT" ]; then
  echo "架构不匹配：本机是 $ARCH_NAME，但 $NEW_BIN 的 ELF e_machine=$MACH"
  echo "  x86-64 机器用 fwdproxy-linux-amd64，ARM 用 fwdproxy-linux-arm64"
  exit 1
fi

sha() { command -v sha256sum >/dev/null && sha256sum "$1" | cut -c1-12 || echo "?"; }
[ -f "$APP_DIR/fwdproxy" ] && echo "当前在用：$(sha "$APP_DIR/fwdproxy")  $(stat -c '%y' "$APP_DIR/fwdproxy" 2>/dev/null)"
echo "即将安装：$(sha "$NEW_BIN")  $NEW_BIN"

# mv 而非 cp：对运行中的二进制不能覆盖写（ETXTBSY），改名则允许
if [ -f "$APP_DIR/fwdproxy" ]; then
  mv -f "$APP_DIR/fwdproxy" "$APP_DIR/fwdproxy.bak"
  echo "旧版本已备份到 $APP_DIR/fwdproxy.bak"
fi
mv -f "$NEW_BIN" "$APP_DIR/fwdproxy"
chmod +x "$APP_DIR/fwdproxy"
chown "$RUN_USER:$RUN_USER" "$APP_DIR/fwdproxy" 2>/dev/null || \
  echo "提示：chown 到 $RUN_USER 失败，确认服务账号名是否一致"

echo "✓ 新二进制已就位。服务仍在运行旧版本，执行 sudo ./restart.sh 生效。"
