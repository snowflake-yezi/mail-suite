# mail-suite

`mail-suite` 当前已在工程脚手架上开始实现控制面业务。本仓库提供 Go 控制面进程、React 管理
工作区、PostgreSQL 开发依赖，以及邮箱开通意图、operation 和 outbox 的原子持久化闭环；尚未
开放业务 HTTP 路由、执行真实 mail-core 副作用或完成邮箱投递、邮件读取和高可用能力。

当前业务契约见
[邮箱开通意图与 Operation Ledger 需求](docs/requirements/2026-08-31-mailbox-provisioning-intent.md)，
工程基线见 [工程脚手架需求](docs/requirements/2026-08-28-project-scaffolding.md)，技术边界见
[技术栈与模块边界](docs/mail-suite-technology-stack.md)。

## 固定工具版本

| 工具       | 版本      |
| ---------- | --------- |
| Go         | `1.26.6`  |
| Node.js    | `24.15.0` |
| Corepack   | `0.34.6`  |
| pnpm       | `10.33.3` |
| Docker CLI | `29.4.0`  |

Node 版本记录在 `.node-version`，pnpm 版本记录在 `src/web/package.json`，Go 版本记录在
`go.mod`。Web 只允许使用 `src/web/pnpm-lock.yaml`，不得生成 npm 或 Yarn 锁文件。

## 安装依赖

在仓库根目录执行：

```powershell
$env:GOCACHE = "$PWD\.cache\go-build"
$env:GOTMPDIR = "$PWD\.tmp"
corepack pnpm --dir src/web install --frozen-lockfile --store-dir ../../.pnpm-store
go mod download
```

## 启动本地依赖

Compose 只启动 PostgreSQL 17。以下命令为当前终端生成一次性本地密码，不会写入仓库：

```powershell
$randomBytes = New-Object byte[] 24
$randomNumberGenerator = [Security.Cryptography.RandomNumberGenerator]::Create()
$randomNumberGenerator.GetBytes($randomBytes)
$randomNumberGenerator.Dispose()
$env:MAIL_SUITE_DEV_POSTGRES_PASSWORD = [Convert]::ToBase64String($randomBytes)
docker compose -f deploy/compose/compose.yaml up -d --wait
$databasePassword = [Uri]::EscapeDataString($env:MAIL_SUITE_DEV_POSTGRES_PASSWORD)
$env:MAIL_SUITE_DATABASE_URL = "postgres://mail_suite:${databasePassword}@127.0.0.1:5432/mail_suite?sslmode=disable"
```

停止本地数据库：

```powershell
docker compose -f deploy/compose/compose.yaml down
```

## 启动进程

三个后端入口相互独立。`api` 和 `worker` 当前仍只提供健康端点，不会执行 DDL 或暴露未认证业务
路由。数据库 schema 只能由显式 `migrator` 命令变更：

```powershell
go run ./src/backend/cmd/migrator --up
go run ./src/backend/cmd/migrator --status
go run ./src/backend/cmd/api
go run ./src/backend/cmd/worker
go run ./src/backend/cmd/migrator --check-config
corepack pnpm --dir src/web dev
```

`migrator --down` 只回滚最后一个 migration，会删除对应业务表；仅在明确需要回滚且数据已备份时
手工执行。应用进程不会自动调用该命令。

默认地址：API 为 `127.0.0.1:8080`，worker 为 `127.0.0.1:8081`，Web 为
`http://127.0.0.1:5173`。可以通过 `MAIL_SUITE_HTTP_ADDRESS`、
`MAIL_SUITE_SHUTDOWN_TIMEOUT`、`MAIL_SUITE_PROBE_TIMEOUT` 和
`MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT` 覆盖非敏感运行参数。

## 完整验证

Go 检查：

```powershell
$unformatted = gofmt -l ./src/backend ./tests
if ($unformatted) { $unformatted; throw "Go 文件未格式化" }
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
$generatedChanges = git status --short --untracked-files=all -- src/backend/internal/data/postgres/generated
if ($generatedChanges) { $generatedChanges; throw "sqlc 生成代码与权威 SQL 不一致" }
go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run ./...
go test ./...
go test -race ./...
go build -o .tmp/bin/api ./src/backend/cmd/api
go build -o .tmp/bin/worker ./src/backend/cmd/worker
go build -o .tmp/bin/migrator ./src/backend/cmd/migrator
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
```

Web 与契约检查：

```powershell
corepack pnpm --dir src/web install --frozen-lockfile --store-dir ../../.pnpm-store
corepack pnpm --dir src/web run format:check
corepack pnpm --dir src/web run lint
corepack pnpm --dir src/web run typecheck
corepack pnpm --dir src/web test
corepack pnpm --dir src/web run contract
corepack pnpm --dir src/web run build
corepack pnpm --dir src/web run e2e
corepack pnpm --dir src/web licenses list --prod
corepack pnpm --dir src/web audit --prod --audit-level high
git diff --check
```

真实 PostgreSQL 集成测试需要额外设置 `MAIL_SUITE_TEST_DATABASE_URL`；未设置时该测试明确跳过。
CI 使用固定 PostgreSQL 镜像运行 readiness、migration、事务幂等和租户隔离用例。sqlc 的权威
输入位于 `src/backend/migrations/` 和 `src/backend/internal/data/postgres/query/`，生成代码位于
独立的 `src/backend/internal/data/postgres/generated/`，不得手工修改。

## OCI 构建

后端使用同一构建定义生成三个独立制品，Web 使用独立 Nginx 制品。基础镜像同时固定 tag 和
manifest digest；源码 revision 和版本通过 OCI label 与 Go 构建变量写入。

```powershell
$goProxy = go env GOPROXY
docker build -f src/backend/Dockerfile --build-arg SERVICE=api --build-arg GOPROXY=$goProxy --build-arg VERSION=dev --build-arg REVISION=(git rev-parse HEAD) -t mail-suite-api:dev .
docker build -f src/backend/Dockerfile --build-arg SERVICE=worker --build-arg GOPROXY=$goProxy --build-arg VERSION=dev --build-arg REVISION=(git rev-parse HEAD) -t mail-suite-worker:dev .
docker build -f src/backend/Dockerfile --build-arg SERVICE=migrator --build-arg GOPROXY=$goProxy --build-arg VERSION=dev --build-arg REVISION=(git rev-parse HEAD) -t mail-suite-migrator:dev .
docker build -f src/web/Dockerfile --build-arg VERSION=dev --build-arg REVISION=(git rev-parse HEAD) -t mail-suite-web:dev .
```

Web 容器监听 `8080`，并通过 `MAIL_SUITE_API_UPSTREAM` 指定同源 `/health/` 请求的 API 上游。

## 单节点测试服务器部署

Ubuntu 24.04 测试主机的固定运行时版本、最小防火墙、资源限制、部署、验证和回滚命令见
[单节点测试服务器部署](deploy/server/README.md)。当前服务器编排只提供数据库、内部健康服务和本机
预览入口，不包含 mail-core，也不表示 SMTP 收件、OIDC 或邮件管理能力已经完成。
