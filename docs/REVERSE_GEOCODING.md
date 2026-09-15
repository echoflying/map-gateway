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

## 成功响应

```json
{
  "location": { "lon": 103.8343, "lat": 30.0508 },
  "formattedAddress": "四川省眉山市东坡区苏祠街道雕像国际广场地下停车场",
  "administrative": {
    "country": { "name": "中国", "code": "" },
    "province": { "name": "四川省", "code": "156510000" },
    "city": { "name": "眉山市", "code": "156511400" },
    "county": { "name": "东坡区", "code": "156511402" },
    "town": { "name": "苏祠街道", "code": "156511402003" },
    "village": { "name": "", "code": "" }
  },
  "municipality": false,
  "source": "tianditu"
}
```

响应固定包含全部层级对象。天地图没有稳定的结构化村/社区字段，因此当前 `village.name` 和 `village.code` 通常为空；不得从附近 POI 或格式化地址猜测行政村。某一级缺失时保留该对象并返回空字符串，不把下级值错误填入上级。

## 直辖市

天地图对北京、上海、天津、重庆等直辖市通常返回省级名称，但 `city` 和 `city_code` 为空，区县直接出现在 `county`。map-gateway 保留上游的真实层级，并返回 `municipality: true`：

```json
{
  "province": { "name": "北京市", "code": "156110000" },
  "city": { "name": "", "code": "" },
  "county": { "name": "东城区", "code": "156110101" },
  "town": { "name": "东华门街道", "code": "156110101001" }
}
```

调用者展示地址时可以跳过空的 `city`；数据处理时不要复制 `province` 填充 `city`，否则会丢失直辖市与普通省市结构的区别。

## 状态码

| 状态码 | 含义 |
|---|---|
| `200` | 查询成功；部分层级仍可能为空 |
| `400` | 参数缺失、重复、越界，或包含 `lon`、`lat` 以外参数 |
| `404` | 天地图没有返回可用结果 |
| `405` | 非 GET 请求 |
| `429` | 超过按客户端 IP 计算的频率限制 |
| `502` | 天地图超时、HTTP 异常或响应无效 |
| `503` | 服务端没有配置 `TIANDITU_KEY` |

响应不包含天地图 Key，也不包含上游 URL。调用方只能通过 map-gateway 使用该能力。
