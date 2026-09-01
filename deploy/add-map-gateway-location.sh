#!/bin/bash
# 为 map-gateway 后台加 /map-gateway/ 前缀公网入口（piboy.ccwu.cc）
#   sudo bash ~/projects/map-gateway/deploy/add-map-gateway-location.sh
set -euo pipefail

NGX=/etc/nginx/sites-available/piboy

[ -f "$NGX" ] || { echo "缺少 $NGX"; exit 1; }

echo "===== 1/3 备份 + 插入 /map-gateway/ location ====="
if grep -q 'location ^~ /map-gateway/' "$NGX"; then
  echo "  → /map-gateway/ location 已存在，跳过"
else
  cp "$NGX" "$NGX.bak-$(date +%Y%m%d-%H%M%S)"
  python3 - "$NGX" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
block = '''
    # ===== map-gateway 后台统计（8082，带密码登录，前缀 rewrite） =====
    location = /map-gateway/admin {
        return 302 /map-gateway/admin/;
    }
    location ^~ /map-gateway/ {
        rewrite ^/map-gateway/(.*)$ /$1 break;
        proxy_pass http://127.0.0.1:8082;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_http_version 1.1;
        proxy_read_timeout 60s;
    }
'''
anchor = '    location / {\n        return 404;'
if anchor in s:
    s = s.replace(anchor, block + '\n' + anchor, 1)
    open(p, 'w').write(s)
    print('  → 已插入 /map-gateway/ location（在兜底 404 前）')
else:
    print('  !! 未找到兜底 location / 锚点，未修改', file=sys.stderr)
    sys.exit(1)
PY
fi

echo "===== 2/3 nginx 校验 + 重载 ====="
if nginx -t; then
  systemctl reload nginx
  echo "  → nginx 已重载"
else
  echo "  !! nginx -t 失败，回滚"
  cp "$NGX.bak-"* "$NGX" 2>/dev/null || true
  exit 1
fi

echo "===== 3/3 验证 ====="
for p in /map-gateway/admin /map-gateway/health; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: piboy.ccwu.cc' "http://127.0.0.1$p")
  echo "  piboy.ccwu.cc$p → HTTP $code"
done
curl -s -H 'Host: piboy.ccwu.cc' http://127.0.0.1/map-gateway/admin | grep -o '<title>[^<]*</title>' || true

echo "===== 完成 ====="