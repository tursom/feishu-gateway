# API

业务前缀 `/api/v1`，请求头 `Authorization: Bearer <Token>`。

## 业务接口

| 方法 | 路径 | 请求 |
|---|---|---|
| GET | `/tables` | 返回当前 Token 可访问的表，`{"data":[...]}` |
| GET | `/tables/{table}/fields` | 飞书原生 `page_size` / `page_token` 查询参数 |
| POST | `/tables/{table}/records/search` | 飞书原生筛选请求及查询参数 |
| GET | `/tables/{table}/records/{id}` | 直接读取单条记录 |
| POST | `/tables/{table}/records` | `{"fields":{...}}` |
| PATCH / PUT | `/tables/{table}/records/{id}` | `{"fields":{...}}`，映射为飞书 PUT |

`table` 为 `requirements`、`tasks`、`bugs`。字段/记录接口的 HTTP 状态和 JSON 正文来自飞书。页大小、下一页游标、过滤条件使用飞书原生格式，不做自动翻页。

查询未解决的 BUG：

```http
POST /api/v1/tables/bugs/records/search?page_size=20
Authorization: Bearer <Token>
Content-Type: application/json
```

```json
{
  "filter": {
    "conjunction": "and",
    "conditions": [{"field_name":"BUG状态","operator":"is","value":["未解决"]}]
  },
  "field_names": ["Bug 描述","BUG状态"]
}
```

典型飞书响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {"items": [], "has_more": false}
}
```

继续翻页时，将飞书返回的 `data.page_token` 放入下一次 URL 的 `page_token`。

更新 BUG 状态：

```json
{"fields":{"BUG状态":"待验收"}}
```

不要求 `expectedRevision`、幂等键或额外修改原因。更新非状态字段需要 `bugs:edit` 权限。没有删除接口。

## 响应与失败

- 上游响应保留真实 HTTP 状态和原生业务 `code` / `msg` / `data`。例如飞书可能用 HTTP200 返回非0业务code，调用方需要检查业务code。
- `X-Upstream-Stage` 为 `auth`、`resolve` 或 `operation`，表示返回的响应来自认证、知识库解析还是对应业务操作。认证/解析被拒绝时，不会继续发起业务操作。
- 有上游追踪 ID 时放入 `X-Feishu-Request-ID`。
- 对仅写 Token，写响应去除未提交的记录字段，`X-Response-Fields: submitted-only` 表示发生了权限过滤；业务状态和错误信息不因过滤而改变。
- 确实没有取得完整 HTTP 响应时，网关返回502及 `{"error":{"message":"...","stage":"operation","responseReceived":false}}`。如果已收到响应头但读取正文失败，`responseReceived` 为 true，并附 `upstreamHttpStatus`。这不代表写入成功或失败。
- 网关自身的 Token、权限或请求格式错误使用相应4xx及 `{"error":{"message":"..."}}`，不伪装成飞书响应。

服务不缓存写入结果、不去重、不重试、不写后回读。即使重复携带同一个 `Idempotency-Key`，也是两次独立请求。重试、重复记录控制和结果核实由调用方负责。

## 管理接口

管理访问由 Pangolin 和源站网络隔离保护。下列接口不加入 Pangolin 白名单。

管理 POST/PATCH 使用 `Content-Type: application/json` 和 `X-Requested-With: FeishuGateway`；这个头用于防止跨站表单请求，不是认证密钥。浏览器同源管理页面自动添加。不需要额外代理密钥、IP白名单或随机 CSRF Token。

管理响应格式 `{ "data": ... }`，错误 `{ "error": { "message": ... } }`。

| 接口 | 说明 |
|---|---|
| `GET /admin-api/overview` | 应用总数、累计日志数量、最近操作 |
| `GET /admin-api/settings` | `{appId,secretConfigured}`，不返回 Secret |
| `POST /admin-api/settings` | `{appId,appSecret?}`；同 App ID 留空保留，首次/更换 ID 必填 |
| `GET /admin-api/apps` | 应用列表，无 Token 明文 |
| `POST /admin-api/apps` | `{name,description,scopes,expiresAt}` → `{app,token}` |
| `PATCH /admin-api/apps/{id}` | 修改名称、用途、权限、有效期或 enabled |
| `POST /admin-api/apps/{id}/rotate` | 空对象 → `{app,token}`，旧 Token 立即失效 |
| `GET /admin-api/tables` | 三张表及维护范围 |
| `GET /admin-api/logs` | 最近100条操作日志 |
| `POST /admin-api/debug` | 使用指定应用权限发起一次真实操作 |

调试请求：

```json
{
  "appId": "<调用方应用ID>",
  "table": "bugs",
  "action": "search",
  "pageSize": 20,
  "payload": {}
}
```

`action` 可选 fields/search/get/create/update；get/update 传 `recordId`，写入传 `payload:{"fields":{...}}`。返回 `{data:{httpStatus,stage,requestId,body}}`，`body` 为上游原始JSON，非JSON正文按字符串展示。外层HTTP200表示调试接口返回了结果，不代表飞书业务操作成功。
