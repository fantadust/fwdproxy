#!/usr/bin/env bash
# 第二步：重启服务使新二进制生效。
# 默认等待当前请求执行完成（端口上无 ESTABLISHED 连接）才重启，
# 避免掐断正在进行的请求——生图类请求可长达几百秒。
# 启动失败会自动回滚到 fwdproxy.bak。
#
# 用法：sudo ./restart.sh [--timeout N] [--force]
#   --timeout N   最长等待秒数，默认 900；0 表示一直等
#   --force       不等待，立即重启（会中断正在进行的请求）
set -euo pipefail

APP_DIR=/data/apps/fwdproxy
SERVICE=fwdproxy
CONF="$APP_DIR/fwdproxy.conf"
TIMEOUT=900
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --force) FORCE=1; shift;;
    --timeout) TIMEOUT="${2:?--timeout 需要秒数}"; shift 2;;
    *) echo "未知参数：$1"; exit 2;;
  esac
done

# 监听端口从配置解析，取不到则回退 8443
PORT=$(sed -n 's/^[[:space:]]*addr[[:space:]]*=.*:\([0-9]\{1,5\}\).*/\1/p' "$CONF" 2>/dev/null | tail -1)
PORT="${PORT:-8443}"

if command -v ss >/dev/null; then
  conns() { ss -Htn state established "sport = :$PORT" 2>/dev/null | wc -l | tr -d ' '; }
else
  echo "缺少 ss 命令（iproute2），无法检测活跃连接。"
  [ "$FORCE" -eq 1 ] || { echo "  确认可以停机请加 --force"; exit 1; }
  conns() { echo 0; }
fi

if [ "$FORCE" -eq 1 ]; then
  N=$(conns); [ "$N" -gt 0 ] && echo "警告：--force 将中断端口 $PORT 上的 $N 个活跃连接"
else
  START=$(date +%s)
  while :; do
    N=$(conns)
    [ "$N" -eq 0 ] && break
    ELAPSED=$(( $(date +%s) - START ))
    if [ "$TIMEOUT" -gt 0 ] && [ "$ELAPSED" -ge "$TIMEOUT" ]; then
      echo "等待 ${ELAPSED}s 后端口 $PORT 上仍有 $N 个活跃连接，放弃重启。"
      echo "  可加长 --timeout，或用 --force 强制重启。当前连接："
      ss -Htn state established "sport = :$PORT"
      exit 1
    fi
    echo "端口 $PORT 上有 $N 个请求进行中，等待完成…（已等 ${ELAPSED}s）"
    sleep 2
  done
  echo "端口 $PORT 无活跃连接，开始重启。"
fi

systemctl restart "$SERVICE"
sleep 1

if systemctl is-active --quiet "$SERVICE"; then
  echo "✓ 重启成功，$SERVICE 运行中"
  systemctl status "$SERVICE" --no-pager | head -4
  echo "确认无误后可删除 $APP_DIR/fwdproxy.bak"
else
  echo "✗ 新版本启动失败，自动回滚…"
  if [ -f "$APP_DIR/fwdproxy.bak" ]; then
    mv -f "$APP_DIR/fwdproxy.bak" "$APP_DIR/fwdproxy"
    systemctl start "$SERVICE" && echo "已回滚到旧版本并启动"
  fi
  echo "排查：journalctl -u $SERVICE -n 50"
  exit 1
fi
