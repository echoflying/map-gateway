# map-gateway Fallback（直连容灾）使用手册

> 目标：当 map-gateway 代理不可用时，前端浏览器自动直连上游地图 API（天地图 / AWS Terrarium / ArcGIS），保证业务不中断；未配置 KEY 时在用户侧明确提示。

---

## 1. 背景与设计

### 1.1 两套运行模式

| 模式 | 条件 | 数据路径 | KEY 位置 |
|------|------|---------|---------|
| **代理模式**（默认） | map-gateway 正常 | 浏览器 → `/tianditu/*`、`/tile/*` → map-gateway → 上游 | 服务端（.env），浏览器不可见 |
| **直连模式**（Fallback） | 网关故障（超时/502/宕机） | 浏览器 → 上游直连 | 天地图 key 由浏览器携带（客户端 key） |

### 1.2 为什么需要 `/cfg/maps`

代理模式下天地图 KEY（`TIANDITU_KEY`）只在服务端拼接，浏览器不需要知道。
但**直连模式必须由浏览器自己带 `tk=` 请求天地图**，所以需要在网关正常时，向前端下发天地图客户端 KEY 供缓存使用。

> 安全说明：天地图 tk 是**客户端 KEY**（浏览器直连本就携带，官方用法），下发前端不属于服务端密钥泄露。Terrarium / ArcGIS 为免 KEY 源，无需下发。

---

## 2. Fallback 判定与切换逻辑（前端）

### 2.1 判定规则（防误判）

个别瓦片 404 不代表网关故障，因此采用**连续失败计数**：

- 任意瓦片加载失败 → `MAPFALLBACK.fail()`，`failCount++`
- `failCount >= 3` → 判定网关故障（`down = true`），触发直连切换 + 用户提示
- 切换后不再重复提示（`errShown` 去重）

### 2.2 各资源直连行为

| 资源 | 直连地址 | 需要 KEY | 网关故障时 |
|------|---------|:---:|-----------|
| 2D 底图（vec/cva/img/cia） | `https://t0.tianditu.gov.cn/{layer}_w/wmts?...&tk=KEY` | ✅ | 有 KEY 自动切换；无 KEY → 用户侧提示「未配置天地图 KEY，2D 底图无法直连」 |
| 3D 地形 | `https://s3.amazonaws.com/elevation-tiles-prod/terrarium/...` | ❌ | 自动直连，永不中断 |
| 3D 卫星 | `https://server.arcgisonline.com/.../World_Imagery/MapServer/tile/...` | ❌ | 自动直连，永不中断 |

### 2.3 恢复探测

- 首次判定故障后启动 `setInterval(30s)` 探测 `GET /health`
- 探测成功 → `down = false`、`failCount = 0`，重新走代理，提示「地图网关已恢复」

### 2.4 用户提示文案

| 场景 | 提示 |
|------|------|
| 判定网关故障且切直连 | `⚠️ 地图网关不可用，已切换直连地图服务（未中断）` |
| 网关故障且无天地图 KEY | `⚠️ 地图网关不可用，且未配置天地图 KEY，2D 底图无法直连（建议服务端配置 TIANDITU_KEY）` |
| 网关恢复 | `✅ 地图网关已恢复，重新走代理` |

---

## 3. `/cfg/maps` 端点

### 3.1 请求

```
GET /cfg/maps
```

- 无需鉴权（任一浏览器可直接拉取）
- 响应 `Cache-Control: no-store`
- 仅 GET；其他方法返回 `405 Method Not Allowed`

### 3.2 响应示例

```json
{
  "tiandituKey": "<请在服务端 .env 配置的 TIANDITU_KEY 自动回填，例如 a1b2c3d4...>",
  "upstreams": {
    "sat":     "https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/%s/%s/%s",
    "terrain": "https://s3.amazonaws.com/elevation-tiles-prod/terrarium/%s/%s/%s.png",
    "tianditu": "https://t%d.tianditu.gov.cn/%s_w/wmts?SERVICE=WMTS&REQUEST=GetTile&VERSION=1.0.0&LAYER=%s&STYLE=default&TILEMATRIXSET=w&FORMAT=tiles&TILEMATRIX=%s&TILEROW=%s&TILECOL=%s&tk=%s"
  }
}
```

| 字段 | 说明 |
|------|------|
| `tiandituKey` | 天地图客户端 KEY；未配置 `TIANDITU_KEY` 时为空字符串 |
| `upstreams.*` | 上游直连模板（`%s` 占位）；前端通常自行构造直连 URL，此字段仅作参考/镜像覆盖 |

### 3.3 nginx 路由要求

`/cfg/maps` 必须转发到 map-gateway（默认 127.0.0.1:8082）：

```nginx
location = /cfg/maps {
    proxy_pass http://127.0.0.1:8082;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
    proxy_http_version 1.1;
    proxy_read_timeout 60s;
}
```

> 注意：`location =` 精确匹配优先级高于 `^~ /tile/`、`^~ /tianditu/`，不影响瓦片路由。

---

## 4. 前端接入（实现说明）

### 4.1 全局状态（terrain3d.js 顶部，先于 app.js 执行）

```js
window.MAPFALLBACK = {
  tiandituKey: '',   // 从 /cfg/maps 获取（优先 localStorake 缓存）
  down: false,       // 网关是否判定故障
  failCount: 0,
  init(),            // 先读 localStorage 缓存 key，再尝试 fetch /cfg/maps 刷新（双路径，见 4.4）
  fail(),            // 瓦片失败上报，≥3 判定故障 + 启动恢复探测
  probe(),           // 探测 /health，恢复后 down=false
  direct(kind,z,x,y,layer) // 生成直连 URL；key 缺失时 tianditu 返回 null
};
```

### 4.4 引导问题（bootstrap）：Gateway 全挂时怎么拿到 KEY？

Fallback 配置源（`/cfg/maps`）本身由 Gateway 提供——若首次访问时 Gateway 就已宕机，`/cfg/maps` 也取不到，天地图无法直连。

**解法：localStorage 持久化**。`init()` 走双路径：

1. **先读本地缓存**：`localStorage['kmlguru_mapfallback']`（首次成功获取 key 后落盘）——即使 Gateway 不可用、页面刷新，也能用缓存 key 直连天地图；
2. **再尝试刷新**：fetch `/cfg/maps` 成功则更新缓存（key 可能过期/轮换）。

```js
// localStorage 缓存结构
{ "tiandituKey": "...", "upstreams": { "sat": "...", "terrain": "...", "tianditu": "..." } }
```

> 注意：首次访问就遇到 Gateway 全挂（本地也无缓存）时，天地图确实无法直连（这是无解的：key 只能从服务端来），此时仅 Terrarium/ArcGIS（免 key）可直连，2D 底图提示用户；待 Gateway 恢复一次后，key 会持久化，后续故障均可用缓存直连。

### 4.2 2D 底图（Leaflet tileLayer）

```js
lyr.on('tileerror', () => {
  const down = MAPFALLBACK.fail();
  if (!down) return;                    // 未达阈值，继续代理
  if (!MAPFALLBACK.tiandituKey) { /* 提示用户，不切换 */ return; }
  lyr.setUrl('https://t0.tianditu.gov.cn/{layer}_w/wmts?SERVICE=WMTS&...&TILEMATRIX={z}&TILEROW={y}&TILECOL={x}&tk=KEY');
});
```

### 4.3 3D 地形/卫星

代理失败后 `MAPFALLBACK.fail()`，若 down 则用 `MAPFALLBACK.direct(kind,z,x,y)` 直连重试一次（terrain/sat 免 key 必成功）。

---

## 5. 部署步骤

### 5.1 服务端（一次性的）

```bash
# 1. 构建新二进制（含 /cfg/maps）
cd ~/projects/map-gateway && go build -o map-gateway.new .

# 2. 停止服务 → 替换二进制 → 启动（运行中的二进制不可直接覆盖）
sudo systemctl stop map-gateway
sudo cp map-gateway.new map-gateway && sudo chmod +x map-gateway
sudo systemctl start map-gateway

# 3. nginx 配置插入 location = /cfg/maps（见 3.3）后 reload
sudo nginx -t && sudo systemctl reload nginx

# 4. 验证
curl -s https://x.zaitu.cn/map-gateway/cfg/maps
```

一键脚本：`~/projects/sysadmin/deploy-map-fallback.sh`（会自动完成以上步骤）。

### 5.2 前端（随 kmlguru 构建发布）

前端 fallback 逻辑已内置（`terrain3d.js` + `app.js`），重新构建部署 kmlguru.html 即可，无需额外配置。

---

## 6. 验证 Fallback 是否工作

```js
// 浏览器控制台手动模拟
// 1) 确认 key 已缓存
window.MAPFALLBACK.tiandituKey.length   // 应为 32

// 2) 模拟三次失败触发故障
window.MAPFALLBACK.fail(); window.MAPFALLBACK.fail(); window.MAPFALLBACK.fail();
window.MAPFALLBACK.down                  // true

// 3) 直连 URL 生成
window.MAPFALLBACK.direct('tianditu', 10, 1234, 567, 'vec')
// https://t0.tianditu.gov.cn/vec_w/wmts?...&tk=xxx

// 4) 恢复探测（手动触发；自动每 30s）
window.MAPFALLBACK.probe()
```

---

## 7. 故障排查

| 现象 | 可能原因 | 处理 |
|------|---------|------|
| `/cfg/maps` 404 | nginx 未配 location，或未 reload | 检查 `nginx -T \| grep cfg/maps` |
| `/cfg/maps` 空 key | `.env` 未配 `TIANDITU_KEY` | 配置后重启 map-gateway |
| 瓦片一直走代理、不切直连 | 失败计数 < 3，或图层 `.on` 未绑定 | DevTools 控制台观察 `MAPFALLBACK.failCount` |
| 直连后 2D 白屏 | 天地图 KEY 域名白名单限制 | 天地图控制台把当前域名加入 referer 白名单 |
| 恢复后仍直连 | 探测 `/health` 被墙/404 | 确认 nginx 有 `/health` 转发（或直连 map-gateway） |

---

## 8. 安全提示

- 天地图 tk 属**客户端 KEY**：浏览器（F12 Network）可见属正常，建议在天地图控制台配置 **Referer 域名白名单**（仅允许业务域名），防止被他人盗用配额。
- Terrarium / ArcGIS 免 KEY，不涉及泄露。
- 本服务不会把任何服务端密钥（AWS、管理密码等）经 HTTP 暴露，仅下发天地图客户端 KEY。
