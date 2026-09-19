# 通用行政区中心与周边 POI

统一公网入口为 `https://x.zaitu.cn/map-gateway`。本页描述的是
map-gateway 的通用地理候选能力；它不包含 Trip Craft 或任何调用方的
业务字段、排名策略或持久化逻辑。

## 设计边界

- 所有天地图调用仅由 Gateway 发起，调用方不会获得天地图 Key 或上游 URL。
- POI 查询必须带 `keyword`。天地图 `queryType=3` 本身要求这个参数；对于
  河滩、山头或没有可靠供应商 POI 的坐标，返回空 `candidates`，绝不虚构名称。
- 天地图未提供置信度，因此响应中没有 `confidence` 字段。
- 行政区中心来自天地图 `queryType=12` 的 `area.lonlat`。它是供应商提供的
  区域定位中心，标识为 `center_type: "tianditu_area_center"`，**不是**政府驻地，
  不应据此推断行政机关位置。
- 天地图是否返回 `area` 取决于其索引；请求成功但 `candidates` 为空是正常的
  可表达结果，不会把同次搜索中的同名 POI 冒充成行政区中心。

## 行政区候选中心

```text
GET /search/administrative?keyword={名称}&specify={天地图行政区代码}[&limit=1..50]
```

`keyword` 和 `specify` 必填；`specify` 透传为天地图限定区域，例如
`156511402`。该接口使用天地图搜索 `queryType=12`。`limit` 默认 20，最大 50。

```json
{
  "schema_version": "1.0",
  "provider": "tianditu",
  "candidates": [
    {
      "name": "东坡区",
      "provider": "tianditu",
      "source": "tianditu.search.v2.area",
      "center": {
        "location": {"lon": 103.83, "lat": 30.05},
        "center_type": "tianditu_area_center"
      },
      "raw": {
        "lonlat": "103.83,30.05",
        "bound": "103.7,29.9,104.0,30.2",
        "adminCode": "156511402",
        "level": "3"
      }
    }
  ],
  "cache": {"state": "miss", "fetched_at": "…", "expires_at": "…", "version": "1"}
}
```

`raw` 原样保留天地图的 `lonlat`、`bound`、`adminCode` 和 `level`；其中代码和
层级即使供应商以 JSON 数字给出，也会安全转成字符串以避免精度和格式歧义。
若 `lonlat` 不可解析，候选仍可返回原始值，但没有 `center`。

## 附近 POI 候选

```text
GET /search/nearby?lon={经度}&lat={纬度}&radius_m={1..10000}&keyword={关键词}[&data_types={天地图分类}][&limit=1..50]
```

接口使用天地图搜索 `queryType=3`。`radius_m` 的单位是米，100 米查询是受支持
的普通场景。只返回具备供应商名称、坐标和可解析距离的记录；`distance_m` 始终
是数字，供应商以 `km` 返回的距离会换算为米。

```json
{
  "schema_version": "1.0",
  "provider": "tianditu",
  "location": {"lon": 103.8343, "lat": 30.0508},
  "query_radius_m": 100,
  "keyword": "公园",
  "candidates": [
    {
      "name": "东坡湖公园",
      "location": {"lon": 103.8349, "lat": 30.0509},
      "distance_m": 80,
      "type_code": "110101",
      "type_name": "公园",
      "provider": "tianditu",
      "source": "天地图",
      "hotPointID": "…",
      "source_id": "…",
      "province": {"name": "四川省", "code": "156510000"},
      "city": {"name": "眉山市", "code": "156511400"},
      "county": {"name": "东坡区", "code": "156511402"}
    }
  ],
  "cache": {"state": "miss", "fetched_at": "…", "expires_at": "…", "version": "1"}
}
```

`source`、`hotPointID`、`source_id`、分类和省市县字段仅在供应商给出时返回；
Gateway 不补写、推测或伪造这些字段。

## 聚合候选

```text
GET /resolve/candidates?lon={经度}&lat={纬度}&radius_m={1..10000}&keyword={关键词}[&data_types={天地图分类}][&limit=1..50]
```

它一次返回兼容的 `reverse_geocode`、按反查到的最深行政区请求的
`administrative_candidates`，以及 `poi_candidates`。调用方自行决定使用县、乡镇、
行政区中心或某个 POI；Gateway 不提供业务判断。若供应商未返回行政区 `area`，
`administrative_candidates` 是空数组；这不影响反查结果或 POI 候选。

## 缓存与失败语义

查询结果保存在 Gateway 的 `MAP_CACHE_DIR/queries/`，与瓦片共享容量上限和淘汰
策略。调用方不需要也不应维护一份 Trip Craft 专用的地图缓存。

| 类别 | 缓存键 | 新鲜期 |
|---|---|---|
| 逆地理编码 | provider + 经纬度五位小数网格 | 30 天 |
| 行政区中心 | provider + `specify`（adminCode）+ `keyword` + `limit` | 7 天 |
| 附近 POI | provider + 经纬度五位小数网格 + 半径 + keyword/category（`data_types`）+ limit | 10 分钟 |
| 聚合候选 | provider + 经纬度五位小数网格 + 半径 + keyword/category（`data_types`）+ limit | 10 分钟 |

每个响应都有 `cache`：`state` 为 `miss`（刚从上游刷新）、`hit`（未访问上游）或
`stale`（缓存已过期、刷新失败后安全返回旧结果），并带 `fetched_at`、`expires_at`
和 `version`。HTTP 头 `X-Map-Gateway-Cache` 给出同一个状态。`stale` 响应必须由
调用方按自身时效性要求决定是否采用。

## 直辖市

逆地理编码的直辖市规则不变：北京、上海、天津、重庆的 `city` 可用性为 `false`，
`municipality: true`，区县仍位于 `county`，不能把空 `city` 当成失败。行政区中心和
POI 接口保留天地图原始的省、市、县字段；调用方需要统一展示时，应结合聚合结果中
的 `reverse_geocode.municipality` 判断，不要按名称猜测。

## 部署验收

部署后执行：

```bash
curl -s 'https://x.zaitu.cn/map-gateway/search/nearby?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD'
curl -si 'https://x.zaitu.cn/map-gateway/search/nearby?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD' | grep X-Map-Gateway-Cache
curl -s 'https://x.zaitu.cn/map-gateway/search/administrative?keyword=%E4%B8%9C%E5%9D%A1%E5%8C%BA&specify=156511402'
curl -s 'https://x.zaitu.cn/map-gateway/resolve/candidates?lon=103.8343&lat=30.0508&radius_m=100&keyword=%E5%85%AC%E5%9B%AD'
```

对相同请求重复执行，第二次应为 `X-Map-Gateway-Cache: hit`，且服务日志中没有对应
的新增上游查询；临时故障时，如已有缓存，则会看到 `stale`。
