# map-gateway

天地图 / 地形 / 卫星瓦片代理服务（Go 单二进制，带磁盘缓存与限流）。

浏览器不直连任何外部地图服务，一律经本网关转发——统一缓存、控制凭据、便于故障排查。

## 功能特性

| 路由 | 上游 | KEY |
|------|------|:---:|
| `/tianditu/{vec\|cva\|img\|cia}/{z}/{x}/{y}.png` | 天地图 WMTS | 需要 `TIANDITU_KEY`（服务端持有） |
| `/tile/terrain/{z}/{x}/{y}.png` | AWS Terrarium 高程 | 免 KEY |
| `/tile/sat/{z}/{x}/{y}.jpg` | ArcGIS World Imagery | 免 KEY |
| `/cfg/maps` | 下发天地图客户端 KEY 与直连模板（前端 Fallback 用） | — |
| `/health` `/healthz` | 存活探针 | — |
| `/admin` | 管理后台（统计 / 缓存明细） | 密码登录 |

- 磁盘缓存：容量上限（默认 200MB）+ 旧文件优先淘汰（LRU，保留 80% 水位）
- 按 IP 频控（默认 600 次/分钟），返回 `429`
- 路径严格正则校验（z≤2 位、x/y≤10 位数字），拒绝一切非瓦片路径
- 上游超时 / 超大瓦片防护，异常返回 `502`
- 所有凭据仅从环境变量读取，**不写日志、不经 HTTP 暴露**（唯一例外：`/cfg/maps` 返回天地图客户端 KEY——天地图 tk 本就是浏览器端 key，供直连容灾）

## 快速开始

```bash
# 1. 配置（占位符 → 实际值）
cp .env.example .env && $EDITOR .env

# 2. 构建
go build -o map-gateway .

# 3. 运行
./map-gateway                 # 默认监听 127.0.0.1:8082
```

配置项见 [.env.example](.env.example)（`MAP_LISTEN_ADDR` / `MAP_CACHE_DIR` / `MAP_CACHE_MAX_MB` / `MAP_TILE_RATE` / `MAP_UPSTREAM_TIMEOUT` / `MAP_ADMIN_PASS` / `TIANDITU_KEY` 等）。

## 部署（systemd + nginx）

参见 [deploy/](deploy/)（`map-gateway.service` 等）。核心步骤：

```bash
sudo cp deploy/map-gateway.service /etc/systemd/system/
sudo systemctl enable --now map-gateway

# nginx：把 /tile/、/tianditu/、/cfg/maps 转发到 127.0.0.1:8082
# 参考 deploy/add-map-gateway-location.sh
```

## Fallback 直连容灾

当前端检测到网关故障（连续 3 次瓦片加载失败）时，浏览器自动直连上游地图 API 保证业务延续；网关恢复后自动切回代理。完整说明见 [docs/FALLBACK_MANUAL.md](docs/FALLBACK_MANUAL.md)。

```bash
curl -s https://<host>/cfg/maps   # 应有 tiandituKey 与三个上游模板
```

## 测试

```bash
go test ./...
```

## 目录结构

```
main.go                 代理与缓存实现
docs/FALLBACK_MANUAL.md Fallback 直连容灾手册
docs/MIGRATION_REPORT.md 迁移说明
deploy/                 systemd 服务文件与脚本
.env.example            环境变量模板（占位符）
```

## 安全说明

- `.env` 已 git-ignore，禁止提交真实凭据；仅提交占位符模板
- 部署时务必用 `MAP_ADMIN_PASS` 覆盖默认管理密码
- 天地图 KEY 属客户端 KEY（浏览器直连本应携带），建议在天地图控制台配置 Referer 域名白名单防止盗用