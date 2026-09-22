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
| `/geocode/reverse?lon={经度}&lat={纬度}` | 天地图逆地理编码，返回全部可得行政层级 | 需要 `TIANDITU_KEY`（服务端持有） |
| `/search/administrative?keyword=&specify=` | 天地图行政区候选及其供应商标注中心点 | 需要 `TIANDITU_KEY`（服务端持有） |
| `/search/nearby?lon=&lat=&radius_m=&keyword=` | 天地图周边 POI 候选 | 需要 `TIANDITU_KEY`（服务端持有） |
| `/resolve/candidates?lon=&lat=&radius_m=&keyword=` | 反查、行政区中心和周边 POI 的通用聚合结果 | 需要 `TIANDITU_KEY`（服务端持有） |
| `/help` | 面向 Agent 的机器可读接口契约与调用约束 | — |
| `/health` `/healthz` | 存活探针 | — |
| `/admin` | 管理后台（统计 / 缓存明细） | 密码登录 |

- 磁盘缓存：容量上限（默认 200MB）+ 旧文件优先淘汰（LRU，保留 80% 水位）；瓦片和天地图查询共用该容量
- 按 IP 频控（默认 600 次/分钟），返回 `429`
- 路径严格正则校验（z≤2 位、x/y≤10 位数字），拒绝一切非瓦片路径
- 上游超时 / 超大瓦片防护，异常返回 `502`
- 逆地理编码结果由内置四级行政区划目录校验并规范化，可作为下游持久化的权威值
- 所有凭据仅从环境变量读取，**不写日志、不经 HTTP 暴露**（唯一例外：`/cfg/maps` 返回天地图客户端 KEY——天地图 tk 本就是浏览器端 key，供直连容灾）
- 公共数据接口支持跨域 `GET` / `OPTIONS`（`Access-Control-Allow-Origin: *`）；`/admin` 不开放 CORS

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

统一公网入口为 `https://x.zaitu.cn/map-gateway`。不再使用其他历史域名。服务内部仍监听 `127.0.0.1:8082`。

| 路由 | 用途 |
|------|------|
| `/health` `/healthz` | 存活探针（返回 `ok`） |
| `/help` | Agent 优先读取的 JSON 接口目录：参数、缓存、响应字段与调用约束 |
| `/cfg/maps` | 获取天地图客户端 KEY 与三个直连上游模板（Fallback 用） |
| `/geocode/reverse?lon={经度}&lat={纬度}` | 坐标反查完整行政区划；不接受层级参数 |
| `/search/administrative?keyword={名称}&specify={行政区代码}` | 返回 queryType=12 的行政区候选；明确区分 `tianditu_area_center` 与同名 POI 中心 |
| `/search/nearby?lon={经度}&lat={纬度}&radius_m={米}&keyword={关键词}` | 返回 queryType=3 的附近 POI 候选；半径 1–10,000 米 |
| `/resolve/candidates?lon=&lat=&radius_m=&keyword=` | 通用聚合接口：逆地理、行政区候选、附近 POI 候选 |
| `/tianditu/{vec\|cva\|img\|cia}/{z}/{x}/{y}.png` | 天地图底图瓦片 |
| `/tile/terrain/{z}/{x}/{y}.png` | AWS Terrarium 地形高程瓦片 |
| `/tile/sat/{z}/{x}/{y}.jpg` | ArcGIS 卫星影像瓦片 |

### 示例（相对路径，同源即可用）

```bash
# Agent 接入前先读取此 JSON 契约；不要直连任何地图上游。
curl -s https://x.zaitu.cn/map-gateway/help

# 存活探针
curl -s https://x.zaitu.cn/map-gateway/health

# 天地图瓦片（layer: vec/cva/img/cia，z/x/y 为瓦片坐标）
curl -s -o vec.png https://x.zaitu.cn/map-gateway/tianditu/vec/{z}/{x}/{y}.png

# 地形高程瓦片
curl -s -o terr.png https://x.zaitu.cn/map-gateway/tile/terrain/{z}/{x}/{y}.png

# 卫星瓦片
curl -s -o sat.jpg https://x.zaitu.cn/map-gateway/tile/sat/{z}/{x}/{y}.jpg

# Fallback 配置（返回 tiandituKey + 上游模板）
curl -s https://x.zaitu.cn/map-gateway/cfg/maps

# 逆地理编码：一次返回全部可得层级
curl -s 'https://x.zaitu.cn/map-gateway/geocode/reverse?lon=103.8343&lat=30.0508'

# 行政区候选中心。specify 使用天地图行政区代码。
curl -s 'https://x.zaitu.cn/map-gateway/search/administrative?keyword=%E4%B8%9C%E5%9D%A1%E5%8C%BA&specify=156511402'

# 100 米附近的“公园”候选。keyword 必填，网关不杜撰 POI 名称。
curl -s 'https://x.zaitu.cn/map-gateway/search/nearby?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD'
```

逆地理编码的完整契约、直辖市规则和降级语义见 [docs/REVERSE_GEOCODING.md](docs/REVERSE_GEOCODING.md)。
通用行政区中心、周边 POI、缓存和聚合接口见 [docs/SEARCH_CANDIDATES.md](docs/SEARCH_CANDIDATES.md)。

### 调用约束

- **仅 GET**：其他方法返回 `405`（`Allow: GET`）
- **路径严格校验**：z ≤ 2 位数字、x/y ≤ 10 位数字；**带任何查询串一律 `404`**（避免 cache-buster/注入到达缓存与上游）
- **天地图需 KEY**：`TIANDITU_KEY` 未配置时 `/tianditu/*` 返回 `503`；`/tile/*`（terrain/sat）免 KEY 始终可用

## Fallback 直连容灾

当前端检测到网关故障（连续 3 次瓦片加载失败）时，浏览器自动直连上游地图 API 保证业务延续；网关恢复后自动切回代理。完整说明见 [docs/FALLBACK_MANUAL.md](docs/FALLBACK_MANUAL.md)。

```bash
curl -s https://x.zaitu.cn/map-gateway/cfg/maps   # 应有 tiandituKey 与三个上游模板
```

## 测试

```bash
go test ./...
```

## 目录结构

```
main.go                 代理与缓存实现
docs/FALLBACK_MANUAL.md Fallback 直连容灾手册
docs/REVERSE_GEOCODING.md 逆地理编码接口契约
docs/SEARCH_CANDIDATES.md 通用行政区中心与周边 POI 契约
data/admin-divisions.tsv 中国省/市/区县/乡镇街道四级权威目录
deploy/                 systemd 服务文件与脚本
.env.example            环境变量模板（占位符）
```

## 安全说明

- `.env` 已 git-ignore，禁止提交真实凭据；仅提交占位符模板
- 部署时务必用 `MAP_ADMIN_PASS` 覆盖默认管理密码
- 天地图 KEY 属客户端 KEY（浏览器直连本应携带），建议在天地图控制台配置 Referer 域名白名单防止盗用
