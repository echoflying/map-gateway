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

## 调用方式

所有路由均为 GET、相对路径（经部署主机 nginx 或直接访问监听地址），**不绑定具体域名**。前端浏览器与部署主机同源时直接使用相对路径即可。

| 路由 | 用途 |
|------|------|
| `/health` `/healthz` | 存活探针（返回 `ok`） |
| `/cfg/maps` | 获取天地图客户端 KEY 与三个直连上游模板（Fallback 用） |
| `/tianditu/{vec\|cva\|img\|cia}/{z}/{x}/{y}.png` | 天地图底图瓦片 |
| `/tile/terrain/{z}/{x}/{y}.png` | AWS Terrarium 地形高程瓦片 |
| `/tile/sat/{z}/{x}/{y}.jpg` | ArcGIS 卫星影像瓦片 |

### 示例（相对路径，同源即可用）

```bash
# 存活探针
curl -s /health

# 天地图瓦片（layer: vec/cva/img/cia，z/x/y 为瓦片坐标）
curl -s -o vec.png /tianditu/vec/{z}/{x}/{y}.png

# 地形高程瓦片
curl -s -o terr.png /tile/terrain/{z}/{x}/{y}.png

# 卫星瓦片
curl -s -o sat.jpg /tile/sat/{z}/{x}/{y}.jpg

# Fallback 配置（返回 tiandituKey + 上游模板）
curl -s /cfg/maps
```

### 跨主机访问

部署主机对外可达时，用 `<host>` 占位前缀（替换为你的域名/IP）：

```bash
curl -s https://<host>/health
curl -s https://<host>/tianditu/vec/10/1234/567.png
curl -s https://<host>/cfg/maps
```

> 若服务仅监听回环地址（默认 `127.0.0.1:8082`），跨主机必须经 nginx 等反代转发（见上文部署）。

### 调用约束

- **仅 GET**：其他方法返回 `405`（`Allow: GET`）
- **路径严格校验**：z ≤ 2 位数字、x/y ≤ 10 位数字；**带任何查询串一律 `404`**（避免 cache-buster/注入到达缓存与上游）
- **天地图需 KEY**：`TIANDITU_KEY` 未配置时 `/tianditu/*` 返回 `503`；`/tile/*`（terrain/sat）免 KEY 始终可用

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
deploy/                 systemd 服务文件与脚本
.env.example            环境变量模板（占位符）
```

## 安全说明

- `.env` 已 git-ignore，禁止提交真实凭据；仅提交占位符模板
- 部署时务必用 `MAP_ADMIN_PASS` 覆盖默认管理密码
- 天地图 KEY 属客户端 KEY（浏览器直连本应携带），建议在天地图控制台配置 Referer 域名白名单防止盗用