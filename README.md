# 飞书管理网关

Go 单程序，内嵌管理页面，SQLite 保存飞书凭据、调用方 Token 和操作日志。管理访问交给现有的 Pangolin 与防火墙；对外业务 API 使用 Bearer Token。

## 部署

```sh
docker compose pull
docker compose up -d
```

默认端口8787，数据目录 `./data`。无需 `.env`；需要改端口、数据路径或镜像版本时参考 `.env.example`。Compose 只有镜像、端口映射和一个本地目录挂载，使用默认用户及默认网络。

在 Pangolin 中保护管理页面和 `/admin-api/*`，只将 `/api/v1/*` 加入登录白名单。源站由现有防火墙隔离。应用不再要求可信 IP、代理密钥、固定公网地址或专用身份头。后台没有额外登录系统；管理员写请求使用普通自定义请求头阻止跨站表单提交。

首次打开后台：

1. 在「服务设置」填写飞书 App ID 和 App Secret。
2. 在「应用与 Token」创建调用方、选择权限，保存生成的 Token。
3. 在「API 调试」中选择应用和数据表，发送读取请求验证接入。

Secret 不回显，同一 App ID 下留空表示保留；更换 App ID 时必须填写新 Secret。配置保存在数据目录的 SQLite 中，更新镜像后仍然保留。Token 明文只在创建或轮换时显示，数据库保存其哈希值。

## 服务边界

- 每次业务调用至多向飞书发送一次对应操作。首次认证、令牌到期或解析知识库时会有必要的认证/解析请求。
- 返回飞书实际 HTTP 状态、业务 `code`、`msg` 和 `data`；HTTP200 不代表业务 code 一定成功。
- 不做请求去重、幂等缓存、自动重试、写后查询或结果比对。
- 更新不要求 revision，不获取写锁，不在更新前读取记录。
- 是否重试、如何防重复、是否查询确认，由调用方决定。网络错误只说明本次未取得完整响应，不推断业务结果。
- 保留三个表的操作范围。写权限不隐含读取其他字段；仅写 Token 的写响应只包含本次提交的字段，并通过响应头说明过滤。

飞书的需求状态和任务状态为流程字段，当前公开 API 不支持写入。网关展示这个平台限制，实际提交时直接返回飞书拒绝结果，不自行模拟状态流转。

## 表和权限

| 表别名 | 权限 | 行为 |
|---|---|---|
| `requirements` | `requirements:read` | 读取需求 |
| `requirements` | `requirements:status` | 仅能提交需求状态修改，不允许创建需求 |
| `tasks` | `tasks:read` / `tasks:write` | 读取 / 创建与更新任务 |
| `bugs` | `bugs:read` / `bugs:create` | 读取 / 创建 BUG |
| `bugs` | `bugs:status` | 仅更新 BUG状态 |
| `bugs` | `bugs:edit` | 更新 BUG 其他字段及状态 |

不提供删除记录或修改表结构的接口。人员、关联、字段选项等数据格式由飞书 API 校验。需要完整记录响应时，请同时授予对应 read 权限。

## API

详细接口见 [API.md](API.md)。读取示例：

```sh
curl https://你的域名/api/v1/tables \
  -H "Authorization: Bearer $GATEWAY_TOKEN"
```

创建任务：

```sh
curl https://你的域名/api/v1/tables/tasks/records \
  -H "Authorization: Bearer $GATEWAY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"fields":{"任务名称":"完善登录异常处理"}}'
```

无须提交幂等键，也不会自动去重。数据表、字段和记录接口采用飞书原生响应格式。后台调试台发送真实请求，界面直接展示响应，不执行额外核验流程。

## 升级旧版本

应用、Token 与日志继续使用原有 SQLite 表，现有 Token 可继续使用。首次启动新版本时，如果数据库还没有飞书设置，会自动导入同一数据目录中的旧 `feishu-credentials.json`。不会删除或覆盖旧凭据文件。

旧幂等缓存表和锁目录不再参与运行，也不会自动删除。旧环境变量 `AUTH_MODE`、`PUBLIC_ORIGIN`、`TRUSTED_PROXY_IPS`、`PANGOLIN_PROXY_SECRET`、`FEISHU_LOCK_DIR` 不再使用。

**业务 API 已简化为原生响应**：不再返回 `verified`、合成 revision 等字段，不要求 `expectedRevision` 或 `Idempotency-Key`。原来依赖网关缓存响应、网关比对结果或额外响应包装的调用方需要按新契约调整。

SQLite 包含飞书 Secret，请保护数据目录及备份。日志只记录应用、动作、HTTP状态、业务code和上游追踪ID，不保存请求正文或密钥；后台展示最近100条。

## 本地开发

Go 1.25+，运行时无需 Node.js：

```sh
make test
make check
make build
./bin/feishu-gateway
```

默认监听 `:8787`，数据库 `./data/gateway.sqlite`。原生运行可设置 `LISTEN_ADDR` 和 `DATABASE_PATH`。

## 镜像发布

GitHub Actions 运行 Go 测试后构建 amd64 / arm64 镜像并推送到 `ghcr.io/tursom/feishu-gateway`。默认分支更新 `latest`；`v*` 标签发布同名镜像；PR 只验证不推送。可手动触发，认证使用仓库的 `GITHUB_TOKEN`。
