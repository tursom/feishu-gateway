# 飞书网关管理前端

原生 JavaScript，无依赖、无构建步骤。视觉沿用 `feishu-tools/prototype/index.html` 的侧栏、卡片和青绿色配色，已替换全部模拟数据与模拟操作。界面为中文，并提供窄屏导航和原生对话框。

后端需同源提供以下静态映射：

| 请求路径 | 文件 |
| --- | --- |
| 管理页面入口（如 `/`） | `web/index.html` |
| `/assets/app.js` | `web/app.js` |
| `/assets/style.css` | `web/style.css` |

所有脚本与样式均外置，无内联事件、脚本或样式；兼容 `script-src 'self'; style-src 'self'; connect-src 'self'`。页面采用 hash 导航，不需要六个页面各自的后端路由。管理页面应由后端认证保护。

## 接口与行为

- 启动先调用 `GET /admin-api/session`；CSRF Token 仅保存在当前页面内存。所有管理 POST/PATCH 请求都携带 `X-CSRF-Token`，使用同源 Cookie，禁用请求缓存。
- 概览使用 `/overview` 返回的统计和趋势。`successRate` 按百分数（例如 `99.2`）展示，`null` 显示 `—`；无数据时不补造统计。趋势用逐日读取/写入明细表展示。
- 应用列表、创建、编辑、启停、轮换均调用 `/apps` 系列接口。完整 Token 仅在创建或轮换响应后展示，不写入存储、URL 或日志；关闭对话框清除 DOM。剪贴板失败会提示手动复制。
- 资源和设置分别读取 `/resources`、`/settings`。连接测试只在手动点击后调用 `/connection-test`，保留实际响应。
- 日志按 `limit=50` 和 `offset` 分页，支持 `result`、`q` 查询；详情显示请求 ID、字段名、原因和耗时。
- 调试统一调用 `POST /admin-api/debug`，由后端根据所选应用当前状态、有效期、scope 和资源策略执行权限检查。UI 不伪造飞书结果，也不将 HTTP 请求结束等同于已核验成功。
- 调试提供 search/get/fields/create/update。创建和更新先展示具体请求进行确认；create 必须输入或生成 `idempotencyKey`，发送后保留，便于同一次创建重试复用。新一次独立创建需显式生成新键。
- update 必须有记录 ID 和 `expectedRevision`。可点击读取按钮，以 `action:get` 获取真实记录后从 `body.data.revision`（兼容 `body.revision`）填入版本。切换应用、表、操作或记录 ID 会清除旧版本。
- `fields` 与 `filter` 是 JSON 对象编辑器，不预填假记录或假字段值。BUG 非状态字段修改可填写 `reason`，最终规则由服务端校验。
- 网络错误、非 JSON 响应和统一错误结构会显示错误信息；提供的 `requestId` 会一并展示。写请求不自动重试。失败或核验未知时应读取记录/核对日志后再操作。

支持当前主流浏览器，需要 Fetch、原生 dialog、Web Crypto（生成 UUID 需要 HTTPS 或 localhost）。权限选项沿用约定的需求只读、任务读写、BUG 分级权限；已有未知 scope 在编辑时原样保留，不悄悄移除。

## 验证记录与联调清单

已完成 `node --check web/app.js`；使用 Node VM 和模拟网络响应检查 GET/PATCH 方法、CSRF、同源凭据、HTML 转义、JSON 对象校验、幂等键请求体和统一错误传播。静态检查确认 HTML/JS 无内联脚本、样式或事件处理器。测试没有启动 HTTP 服务，也没有访问真实飞书。

本次环境无浏览器自动化依赖，尚未完成真实浏览器布局和后端端到端联调。主会话完成后端后建议检查：登录失效提示；空数据页面；应用创建/编辑/轮换及一次性 Token；日志筛选分页；窄屏导航、键盘对话框；调试读取字段与记录、版本回填、权限拒绝。真实写入验收需由获授权的操作者针对指定记录手动确认，不属于本次验证范围。
