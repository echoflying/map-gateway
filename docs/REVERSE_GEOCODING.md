# 逆地理编码接口

map-gateway 将坐标转成天地图提供的完整地址与行政区划。统一公网入口：

```http
GET https://x.zaitu.cn/map-gateway/geocode/reverse?lon=103.8343&lat=30.0508
```

## 请求

只接受两个查询参数：

| 参数 | 含义 | 范围 |
|---|---|---|
| `lon` | 经度 | `-180` 至 `180` |
| `lat` | 纬度 | `-90` 至 `90` |

没有 `level` 参数。一次上游调用会返回所有可得层级，由调用者选择使用到省、市、区县或乡镇/街道。

本接口是下游系统持久化行政区划的唯一权威入口。天地图负责坐标定位，map-gateway 使用随版本发布且经过校验的四级行政区划目录规范化名称、编码和父子层级；调用方不需要、也不应再用自己的行政区划表二次验证或替换返回值。

## 成功响应

```json
{
  "schemaVersion": "1.0",
  "authority": {
    "provider": "map-gateway",
    "codeSystem": "china-national-geonames-level4+tianditu-prefix"
  },
  "location": { "lon": 103.8343, "lat": 30.0508 },
  "formattedAddress": "四川省眉山市东坡区苏祠街道雕像国际广场地下停车场",
  "administrative": {
    "country": { "level": "country", "available": true, "name": "中国", "code": "156" },
    "province": { "level": "province", "available": true, "name": "四川省", "code": "156510000" },
    "city": { "level": "city", "available": true, "name": "眉山市", "code": "156511400" },
    "county": { "level": "county", "available": true, "name": "东坡区", "code": "156511402" },
    "town": { "level": "town", "available": true, "name": "苏祠街道", "code": "156511402003" },
    "village": { "level": "village", "available": false, "name": "", "code": "" }
  },
  "resolvedLevel": "town",
  "municipality": false,
  "source": "tianditu"
}
```

响应固定包含全部层级对象，每个对象都明确给出：

- `level`：固定层级标识；
- `available`：该次查询是否获得可信结果；
- `name`、`code`：`available=true` 时均为非空字符串，`available=false` 时均为空字符串；
- `resolvedLevel`：本次最深可信层级。

编码必须作为不透明字符串持久化，不能转成数字，也不能自行截断。当前编码是在国家地名信息库四级编码前增加中国数字代码 `156`：省/市/区县为 9 位，乡镇/街道为 12 位。map-gateway 会校验编码存在、层级正确、父子前缀一致；不满足时返回错误，不会把未经确认的数据包装成 `200`。

当前目录覆盖中国大陆及台湾，包含 32 个省级、354 个市级、3,209 个区县级、39,187 个乡镇/街道级记录，共 42,782 条有效记录；不包含香港、澳门。源数据库快照时间为 2026-09-01，发布文件 SHA-256 为 `b7e7570618b24bfc542ada6d64c6550e8453626d640cc4236092bbea35048267`。目录覆盖范围外的坐标不会返回可持久化的成功结果。

目录只到乡镇/街道，不含行政村。`village` 因此固定按统一缺失值返回：`available=false`、`name=""`、`code=""`；不得从附近 POI 或格式化地址猜测行政村。

## 直辖市

天地图对北京、上海、天津、重庆等直辖市通常返回省级名称，但 `city` 和 `city_code` 为空，区县直接出现在 `county`。map-gateway 保留上游的真实层级，并返回 `municipality: true`：

```json
{
  "province": { "level": "province", "available": true, "name": "北京市", "code": "156110000" },
  "city": { "level": "city", "available": false, "name": "", "code": "" },
  "county": { "level": "county", "available": true, "name": "东城区", "code": "156110101" },
  "town": { "level": "town", "available": true, "name": "东华门街道", "code": "156110101001" }
}
```

调用者展示地址时可以跳过空的 `city`；数据处理时不要复制 `province` 填充 `city`，否则会丢失直辖市与普通省市结构的区别。

## 状态码

| 状态码 | 含义 |
|---|---|
| `200` | 查询成功；每一级通过 `available` 明确表示是否可用，至少省和区县已通过目录校验 |
| `400` | 参数缺失、重复、越界，或包含 `lon`、`lat` 以外参数 |
| `404` | 天地图没有返回可用结果 |
| `405` | 非 GET 请求 |
| `429` | 超过按客户端 IP 计算的频率限制 |
| `502` | 天地图超时、HTTP 异常，或上游行政编码无法通过权威目录校验 |
| `503` | 服务端没有配置 `TIANDITU_KEY`，或权威行政区划目录缺失/校验和不符 |

响应不包含天地图 Key，也不包含上游 URL。调用方只能通过 map-gateway 使用该能力。
