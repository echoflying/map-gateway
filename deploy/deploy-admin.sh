#!/bin/bash
# map-gateway 后台统计版一键部署（需要 root）
#   sudo bash ~/projects/map-gateway/deploy/deploy-admin.sh
#
# 1) 备份现网二进制
# 2) 用 mv 原子替换新二进制（避免 Text file busy）
# 3) 重启服务加载新版
# 4) 验证健康 + admin 登录页 + 统计 API
set -euo pipefail

SRC=/home/piboy/projects/map-gateway
BIN=$SRC/map-gateway
NEW=/tmp/map-gateway.new.deploy
ENV=$SRC/.env

[ -x "$NEW" ] || { echo "缺少新二进制 $NEW"; exit 1; }

echo "===== 1/4 备份现网二进制 ====="
[ -f "$BIN" ] && cp -v "$BIN" "$SRC/map-gateway.bak.$(date +%Y%m%d-%H%M%S)"

echo "===== 2/4 原子替换二进制 ====="
mv -v "$NEW" "$BIN"
chmod +x "$BIN"

echo "===== 3/4 重启服务 ====="
systemctl daemon-reload
systemctl restart map-gateway
sleep 1
systemctl is-active map-gateway
curl -sf http://127.0.0.1:8082/health && echo "  → health ok"

echo "===== 4/4 验证 admin ====="
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8082/admin)
echo "  /admin 登录页 → HTTP $code"
# 登录并取统计
JAR=/tmp/mgw-deploy-cookie.txt
curl -s -c "$JAR" -d 'user=admin&pass=boygo' -o /dev/null http://127.0.0.1:8082/admin/login
curl -s -b "$JAR" http://127.0.0.1:8082/admin/api/stats | python3 -c "
import json,sys
d=json.load(sys.stdin)
s=d['stats']
print('  stats: 版本=%s 缓存=%.1fMB 总请求=%d 命中=%d 未命中=%d 上游OK=%d 上游错=%d' % (
    d['version'], d['cacheSizeMB'], s['totalRequests'], s['cacheHits'], s['cacheMisses'],
    s['upstreamOK'], s['upstreamErrors']))
print('  缓存分层:', [(c['label'], c['count']) for c in d['cacheBreakdown']])
" 2>/dev/null || echo "  stats 接口异常（未登录或出错）"

echo "===== 部署完成 ====="