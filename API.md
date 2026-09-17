# API v1

Base URL: `https://<域名>/api/v1`。所有业务请求须带 `Authorization: Bearer fsg_...`。不接受 URL 参数传 Token。

成功：`{"data": ...}`。失败：`{"requestId":"req_...","error":{"code":"...","message":"..."}}`。每个响应含 `X-Request-ID`。日期均为 UTC ISO8601，飞书日期字段本身使用毫秒时间戳。

## 业务接口

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/tables` | 已认证；仅返回当前应用有权限的表 |
| GET | `/tables/{table}/fields` | `{table}:read` |
| POST | `/tables/{table}/records/search` | `{table}:read` |
| GET | `/tables/{table}/records/{recordId}` | `{table}:read` |
| POST | `/tables/{table}/records` | `tasks:write` 或 `bugs:create` |
| PATCH | `/tables/{table}/records/{recordId}` | 按表及字段判断 |

`table` 为 `requirements` / `tasks` / `bugs`。记录 ID 使用飞书实际 `rec...` 值。

### 搜索

```json
{
  "limit": 20,
  "filter": {
    "conjunction": "and",
    "conditions": [{"field_name":"BUG状态","operator":"is","value":["未解决"]}]
  },
  "fieldNames": ["Bug 描述", "BUG状态"]
}
```

搜索整个数据表，不隐含视图筛选。`data.items` 是飞书记录数组，`data.has_more` 为 true 时使用 `data.page_token` 作为下一次的 `pageToken`。limit 默认20，最大100。更新前使用 get 获取完整记录及版本，不能从字段被裁剪的搜索结果推断版本。

### 创建

必须提供请求头 `Idempotency-Key: <8至128位唯一键>`，允许字母、数字、点、下划线与横线。

```json
{"fields":{"任务名称":"完善登录异常处理","备注":"按已确认的需求处理"}}
```

只填写真实存在且可写的字段。需求不能创建。人员字段使用当前飞书应用对应的 open_id，关联字段使用目标记录 ID；不自动上传附件。

### 更新

先 GET 记录，读取 `data.revision`。再 PATCH：

```json
{
  "expectedRevision":"<64位revision>",
  "fields":{"BUG状态":"待验收"}
}
```

BUG 非状态字段需要 `bugs:edit`，并填写：

```json
{
  "expectedRevision":"<64位revision>",
  "fields":{"备注":"补充经确认的复现步骤"},
  "reason":"根据负责人的明确要求补充复现步骤"
}
```

状态更新权限不自动允许其他字段。`bugs:edit` 也可更新状态。需求仅允许业务上的状态更新，但当前需求/任务状态均为 API 不可写的流程字段，返回422。

创建返回201，更新返回200。写入结果包含 `verified`、`record`、`submittedFields`、`mismatchedFields`。`record` 仅含 `record_id` 和 `revision`，完整回读仅用于服务内部核验；写响应与幂等重放都不返回未提交字段，读取完整记录需独立 read 权限。`verified=false` 不能当作回读核验成功。

## 错误语义

| HTTP | 常见错误 | 调用方处理 |
|---|---|---|
| 400 | INVALID_JSON / REVISION_REQUIRED / IDEMPOTENCY_KEY_REQUIRED | 修正参数 |
| 401 | INVALID_TOKEN | 提供有效 API Token |
| 403 | TOKEN_DISABLED / TOKEN_EXPIRED / SCOPE_DENIED / POLICY_DENIED | 检查应用权限与状态 |
| 409 | REVISION_CONFLICT | 重新读取，依据当前值决定修改 |
| 409 | IDEMPOTENCY_CONFLICT | 不要把旧 Key 用于不同请求 |
| 409 | IDEMPOTENCY_PENDING | 请求仍执行或结果待核实，先查记录与日志 |
| 422 | FIELD_READ_ONLY / INVALID_FIELD / INVALID_OPTION | 读取字段定义后修正 |
| 429 | RATE_LIMITED / ENTRY_RATE_LIMITED | 应用或入口预算耗尽，等待下一分钟，遵循 Retry-After |
| 502 | UPSTREAM_ERROR | 检查飞书权限/可用性 |
| 502 | WRITE_UNCERTAIN | 飞书写入可能已成功，禁止盲目重试创建 |
| 502 | VERIFICATION_FAILED | 已写入但回读失败，根据 recordId 核实 |
| 503 | BUSY / IDEMPOTENCY_SAVE_FAILED | 繁忙可用相同 Key 重试；保存失败先核实结果 |

幂等重放保留原响应中的 requestId，当前 HTTP 响应头 X-Request-ID 标识这次重放调用，两者可能不同。Token撤销/过期或权限减少后不会因幂等记录而绕过校验。

## 管理接口

这些接口不在 Pangolin 白名单内，不接受外部应用 Token 作为管理员凭据。

`GET /admin-api/session` 获取 `{csrfToken, authMode, adminName, publicOrigin}`。所有管理 POST/PATCH 需携带 `Origin: <PUBLIC_ORIGIN>` 和 `X-CSRF-Token`，浏览器同源调用会自动提供 Origin。

- `GET /admin-api/overview`：今日请求、成功率、7天趋势、最近操作。
- `GET /admin-api/apps`：应用列表，仅含 Token 前缀。
- `POST /admin-api/apps`：`{name,description,scopes,expiresAt}`，返回 `{data:{app,token}}`，token仅此一次展示。
- `PATCH /admin-api/apps/{id}`：可修改 `{name,description,scopes,expiresAt,enabled}`。
- `POST /admin-api/apps/{id}/rotate`：空对象请求体，生成新 Token，旧值立即失效。
- `GET /admin-api/resources`：全局表范围。
- `GET /admin-api/logs?limit=50&offset=0&result=all&q=...`：分页审计，limit最多500；result可选all/success/failure。
- `GET /admin-api/settings`：服务配置摘要，不含任何密钥。
- `POST /admin-api/connection-test`：空对象，真实只读查询三个表字段。
- `POST /admin-api/debug`：`{appId,action,table,recordId?,fields?,expectedRevision?,filter?,fieldNames?,limit?,pageToken?,reason?,idempotencyKey?}`。使用指定应用当前权限发起真实业务调用，返回 `{data:{status,body}}`。外层200表示调试动作被接收，实际业务状态看 `data.status`。

应用权限枚举和 Pangolin 部署说明见 README。
