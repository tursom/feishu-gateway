# 飞书 API 管理台

独立 Go 服务，内嵌原生 HTML/CSS/JavaScript 页面，SQLite 持久化。运行时不依赖 Node.js，不需要前端构建服务。

## 启动

Go 1.25+：

```sh
make build
make test
make check
# 本地开发，强制只监听回环地址：
make run
```

访问 `http://127.0.0.1:8787`。如果浏览器不在服务器上，可使用 SSH 转发：`ssh -L 8787:127.0.0.1:8787 user@server`，再用本机浏览器打开相同地址。`AUTH_MODE=local` 不提供用户登录，只供本机开发，不允许绑定公网地址。

默认复用 `~/.config/feishu/credentials.json`，格式为 `{"app_id":"...","app_secret":"..."}`。也可设置 `FEISHU_APP_ID` / `FEISHU_APP_SECRET`，或通过 `FEISHU_CREDENTIALS_FILE` 指定路径。不要把凭据提交到仓库。没有凭据时，应用、Token 和审计管理仍可运行，飞书操作会明确报配置错误。

首次启动没有预置 Token 或演示记录。在后台创建应用，选择权限，复制仅显示一次的 Token。刷新页面后配置仍保存在 SQLite，无法重新获取完整 Token，只能轮换。轮换立即使旧 Token 失效，禁用和到期也立即影响新请求；已发出的飞书请求可能继续完成。

## 路径与鉴权

| 路径 | 认证方式 |
|---|---|
| `/`、`/assets/*` | Pangolin 保护，服务校验可信入口 |
| `/admin-api/*` | Pangolin 保护，服务校验可信入口；管理写请求另校验 Origin 与 CSRF Token |
| `/api/v1/*` | Pangolin 精确路径白名单；服务独立校验 Bearer Token 和应用权限 |
| `/healthz` | 与管理入口相同的访问校验，只返回存活状态 |

管理端不提供独立账号密码。调用方的 API Token 无权登录后台、创建其他 Token 或访问管理 API；Pangolin 转发的身份也不能替代业务 API Token。

### Pangolin 部署契约

1. 建立一个要求 Pangolin 登录的资源，将该资源的访问权只授予后台管理员。
2. 只对 **`/api/v1/*`** 设置绕过 Pangolin 登录的路径规则。不要放行 `/admin-api/*`、根路径或整个 `/api/*`。如规则区分 exact/prefix/glob，按实际版本匹配 `/api/v1/` 下所有子路径，并实际测试未登录访问被正确拒绝。
3. 设置生产环境 `AUTH_MODE=pangolin`、`PUBLIC_ORIGIN=https://实际域名`，并使用 `openssl rand -hex 32` 生成 `PANGOLIN_PROXY_SECRET`。
4. 可信反向代理必须 **覆盖** `X-Gateway-Secret` 为此入口密钥，保留实际域名的 `Host`，不允许客户端提供的同名头透传。具体配置入口取决于 Pangolin/Traefik 版本；若当前版本不能覆盖上游请求头，可在受保护的私网入口加入反向代理实现覆盖，但该代理不得能被绕过 Pangolin 直接访问。该密钥不能发送给浏览器或 API 调用方。
5. 将服务配置的 `TRUSTED_PROXY_IPS` 设为连接 Go 服务的真实对端 IP（支持逗号分隔的精确 IPv4/IPv6），不使用 `X-Forwarded-For` 判断可信来源。生产管理员请求同时要求对端 IP 和入口密钥匹配。
6. 源站不开放公网端口；仅允许 Pangolin 代理/隧道访问。可选的 `PANGOLIN_IDENTITY_HEADER` 只有在可信入口明确覆盖真实身份值时才能启用。默认不假设某个 Pangolin 身份头存在，审计显示“Pangolin 管理员”。若需逐人审计，必须接入已经核实的身份头。
7. 验收：匿名浏览器不能访问后台或 `/admin-api/session`；业务 API 不带 Token 返回401；仅有 API Token 访问管理接口仍被拒绝；合法 Token 只能执行对应权限。

Pangolin 的公网登录策略由 Pangolin 执行，入口密钥用于防止直接绕过源站，不能代替 Pangolin 配置。应用启动时缺少生产入口密钥或 HTTPS 公网地址会拒绝启动。

### 环境配置

参考 `.env.example`。Go 二进制不自动解析 `.env`；可用 systemd 的 `EnvironmentFile=` 注入，或者仅对自己创建并信任的配置运行：

```sh
set -a
. ./.env
set +a
./bin/feishu-gateway
```

`DATABASE_PATH` 默认 `./data/gateway.sqlite`。数据目录应位于本地持久磁盘。数据库文件权限0600；WAL运行期间备份请使用 SQLite 在线备份功能，或停止服务后备份整个数据目录。需保护备份，其中包含业务响应和审计数据。幂等记录当前不自动清理，以防旧请求重复创建。

原生部署默认使用 `~/.cache/pi-feishu-locks`，与现有 pi 插件在本机使用同一套表级写锁；可用 `FEISHU_LOCK_DIR` 指定。残留锁只在确认无进行中的写操作后手动移除。此锁及 SQLite 适用于单机实例，不是多主集群方案。

### Docker

`Dockerfile` 生成非 root 运行镜像，Compose 将8787端口仅绑定宿主机回环地址。复制 `.env.example` 为 `.env` 并填入实际配置。Compose 固定私网网段 `172.30.87.0/24`；宿主机代理通过端口映射访问时通常以网关 `172.30.87.1` 出现，仍需检查实际对端后设置 `TRUSTED_PROXY_IPS`。若此网段已占用，请同时调整网段与可信 IP。

`FEISHU_SECRET_SOURCE` 指向只读挂载的凭据文件。Compose 文件型 secret 实质为挂载，镜像使用 UID10001，源文件必须允许 UID10001 读取；建议在仓库外准备专用、权限0400且所有者为10001的副本，父目录不向其他用户开放。不要为了挂载把原凭据改成全局可读。

```sh
docker compose build
docker compose up -d
```

Compose 使用独立的持久卷 `/locks`；如同时运行本机直连飞书插件，应改成双方可访问的共享锁目录，或统一通过网关写入，避免两套独立锁。Pangolin/Newt 与源站的具体网络连接需按你的现有部署填写；此项目不会改动正在运行的 Pangolin 服务。

### GitHub Actions 镜像发布

工作流位于 `.github/workflows/image.yml`，镜像发布至 GitHub Container Registry：

```sh
docker pull ghcr.io/tursom/feishu-gateway:latest
```

- 推送 `master` / `main`：测试通过后发布对应分支标签及 `sha-<完整提交 SHA>`；仅仓库默认分支更新 `latest`。
- 推送 `v*` 标签：发布同名镜像标签，例如 `v1.0.0`，同时提供提交 SHA 标签；版本发布不会覆盖 `latest`。
- 在 GitHub Actions 页面手动运行 **Build and publish image**：发布所选分支或标签的镜像；选择非默认分支不会覆盖 `latest`。
- PR：运行测试并构建双架构镜像，不登录 GHCR、不推送镜像。

支持 `linux/amd64` 和 `linux/arm64`。发布前运行 `go test -race ./...` 和 `go vet ./...`；构建使用缓存，并生成来源证明和 SBOM。镜像摘要及标签显示在工作流运行摘要中。

认证使用 GitHub 自动提供的 `GITHUB_TOKEN`，发布 job 申请 `packages: write`，无需额外配置 PAT 或飞书凭据。镜像不会内置运行配置、数据库或密钥。GHCR 包默认可见性由 GitHub 决定；如需匿名拉取，在包设置中将可见性设为 Public。已有同名包时，需确认该仓库具有包的 Actions 写权限。

使用发布镜像部署时，将 Compose 中的 `build: .` 替换为 `image: ghcr.io/tursom/feishu-gateway:v1.0.0`（换成实际版本），保留原有环境变量、端口、数据卷和 secret 配置，再执行 `docker compose pull && docker compose up -d`。

## 数据表范围

| 表别名 | 可用范围 |
|---|---|
| `requirements` | 读取；业务上最多允许更新需求状态，但该状态是 type24 流程字段，当前 API 不能写入 |
| `tasks` | 创建、更新所有 API 可写字段；任务状态同样是流程字段，不能写入 |
| `bugs` | 创建，常规更新 BUG状态；其他字段需 `bugs:edit` 和明确的 `reason` |

不提供删除记录、修改表结构或权限接口。不存在的字段、只读字段和选项会在写入前检查；系统计算字段不可写。

权限可选：`requirements:read`、`requirements:status`（受上述平台限制）、`tasks:read`、`tasks:write`、`bugs:read`、`bugs:create`、`bugs:status`、`bugs:edit`。写权限不隐含读取权限；调用方通常也需要相应 read 权限来获取字段和记录版本。

## API 调用

接口明细见 [API.md](API.md)。示例使用环境中的 `GATEWAY_TOKEN`，不得写入源码：

```sh
curl -H "Authorization: Bearer $GATEWAY_TOKEN" \
  https://实际域名/api/v1/tables
```

默认每个应用每分钟60次，认证后的失败请求也计入预算，计数存储在 SQLite，跨进程重启有效。业务 API 入口另设每实例每分钟600次、最多64个并发请求的上限，覆盖未认证和拒绝请求；超过入口预算的请求不再逐条写入审计。最多16个并发飞书请求；超过返回503。请求体最大256KB。服务不开放跨域 CORS；外部客户端应在服务器端持有 Token。

更新前 `GET` 单条记录，提交返回的 `revision` 作为 `expectedRevision`；只提交修改字段。版本不符返回409。飞书没有提供此处的原子条件更新，无法彻底排除网页或其他客户端在检查之后的并发修改；写后会再次读取核验。完整回读仅供服务内部核验，写响应及其幂等重放只返回记录 ID、版本、提交字段和核验结果，不返回未提交的完整记录。读取记录仍需独立 read 权限。

创建必须传 `Idempotency-Key`，更新可选。同一应用相同 Key 与相同请求会返回已保存的响应；不同请求使用相同 Key 返回409。处理中或进程崩溃遗留的 pending 请求返回待核实，禁止自动重新发起。飞书写入超时不会自动重试，结果不明会明确返回。调用方也不要更换 Key 重复创建。

后台调试台会发起真实请求，使用所选应用当前权限。写入前有显式确认，且与外部 API 共用权限、限流、幂等和审计逻辑。不会自动生成真实测试任务或 BUG。

## 测试与运维

```sh
go test -race ./...
go vet ./...
make build
```

测试使用临时数据库和模拟飞书上游，覆盖管理入口与 API Token 隔离、CSRF、Token生命周期、权限限制、幂等重放/并发/未知结果、分页和限流。启动日志只包含监听地址与认证模式；不输出 Token、飞书密钥、请求正文或上游原始错误。审计记录应用、动作、字段名、结果与原因，原因截断为1024字节，字段名合计最多2048字节。审计保留最近30天且最多约50,000条（每128次写入清理，短时可超过上限127条），不会清理幂等记录。创建/轮换 Token 的明文仅通过当次 HTTPS 响应交付。

真实公网联调需要最终域名、Pangolin 入口头覆盖方式和可信对端 IP。真实写入需选择明确的业务记录或专用测试表完成验收。
