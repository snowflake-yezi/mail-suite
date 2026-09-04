# 单节点测试服务器部署

该目录只部署当前已实现的 PostgreSQL、API/worker 健康基础和 Web 未连接预览。API 在此编排中
显式使用 `MAIL_SUITE_AUTH_MODE=disabled`，不注册认证路由；部署不包含 IdP、TLS 或 mail-core，
因此不能作为 SMTP 收件、OIDC 端到端登录、邮件查看或导出的验收证据。

## 固定环境

- Ubuntu Server 24.04.4 LTS x86_64
- Docker `29.1.3-0ubuntu3~24.04.2`
- Docker Compose v2 `2.40.3+ds1-0ubuntu1~24.04.1`
- Docker Buildx `0.30.1-0ubuntu1~24.04.1`
- PostgreSQL `17.11-alpine`，镜像使用 manifest digest 固定
- 预览入口 `127.0.0.1:18080`，不直接暴露公网
- PostgreSQL 不发布宿主机端口，只允许受控脚本通过 SSH 隧道访问容器内网地址

## 首次主机引导

在解压后的仓库根目录以 root 执行：

```bash
MAIL_SUITE_REGISTRY_MIRROR=https://你的阿里云专属地址 bash deploy/server/bootstrap-host.sh
```

脚本会应用当前 Ubuntu 更新、安装固定版本 Docker/Compose、配置有界 Docker 日志、创建
`/opt/mail-suite`，配置运行时注入的阿里云官方 registry mirror，并启用仅放行 OpenSSH 的 UFW 基线。脚本不会修改 SSH 端口或认证方式，
也不会开放 `25`、`80`、`443`。

## 部署与验证

ECS 无法直接访问 Docker Hub 时，先在受控 Windows 开发机的仓库根目录构建离线镜像包：

```powershell
powershell -ExecutionPolicy Bypass -File deploy/server/build-offline-images.ps1
```

将脚本输出的 SHA-256 与服务器收到的文件逐字核对，再在服务器执行 `docker load`。随后从服务器的
发布目录执行：

离线包将固定 digest 的 PostgreSQL 镜像标记为 `mail-suite-postgres:17.11-alpine-18cfe3ef`；该别名
只用于避免 `docker save/load` 丢失 RepoDigest 引用，不改变镜像内容。

```bash
MAIL_SUITE_SKIP_BUILD=1 bash deploy/server/deploy.sh .
bash deploy/server/verify.sh
```

首次部署会在 `/opt/mail-suite/shared/runtime.env` 生成权限为 `0600` 的数据库密码。不要运行会
展开 Compose 环境变量的命令，也不要把该文件复制回仓库。数据库 migration 由部署脚本显式执行，
API、worker 或 PostgreSQL 启动过程不会隐式修改 schema。

部署还会幂等创建 `mail_suite_reader`，只授予当前及未来 public 表的 `SELECT`。独立随机密码保存在
`/opt/mail-suite/shared/postgres-readonly.env`，权限为 `0600 root:root`；脚本和验证输出不会回显密码。

部署制品包含 `identity-bootstrap` 工具，但部署脚本不会自动创建测试主体。只有固定版本 Keycloak
已经生成稳定 issuer 和 subject 后，才把已核验且不含密码/token 的 manifest 放到
`/opt/mail-suite/shared/test-identity.json`，权限设为 `0600 root:root`，并从当前发布执行：

```bash
release_dir="$(readlink -f /opt/mail-suite/current)"
docker compose \
  --project-directory "${release_dir}" \
  --env-file /opt/mail-suite/shared/runtime.env \
  -f "${release_dir}/deploy/server/compose.yaml" \
  --profile tools run --rm \
  -v /opt/mail-suite/shared/test-identity.json:/run/test-identity.json:ro \
  identity-bootstrap --check --manifest /run/test-identity.json
```

将 `--check` 改为 `--apply` 才会创建数据；将其改为 `--remove` 会显式回收 fixture。当前 Keycloak
尚未部署，因此服务器上不得执行 `--apply`。该工具只加入现有 internal backend 网络，不需要也不得
开放云安全组、UFW 或宿主机 PostgreSQL 端口。

通过本地 SSH 隧道预览：

```powershell
ssh -i key/mail-suite.pem -L 18080:127.0.0.1:18080 root@服务器地址
```

浏览器访问 `http://127.0.0.1:18080`。当前页面必须继续显示未登录预览或未连接状态。

## 通过 SSH 隧道查看 PostgreSQL

不要在云安全组或 UFW 放行 `5432` 或 `15432`。在仓库根目录打开 PowerShell，保持以下命令运行；
脚本会校验私钥和 known-hosts 文件，动态读取当前 PostgreSQL 容器内网地址，并只监听本机回环端口：

```powershell
powershell -ExecutionPolicy Bypass -File deploy/server/open-readonly-database-tunnel.ps1 -Server 服务器公网IP
```

另开一个 PowerShell，只在首次配置数据库客户端时读取密码；不要把输出写入命令、文档或截图：

```powershell
ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=.tmp/ssh_known_hosts -i key/mail-suite.pem root@服务器公网IP "sed -n 's/^MAIL_SUITE_POSTGRES_READONLY_PASSWORD=//p' /opt/mail-suite/shared/postgres-readonly.env"
```

DBeaver、DataGrip 或其他 PostgreSQL 客户端填写：

| 配置项   | 值                          |
| -------- | --------------------------- |
| Host     | `127.0.0.1`                 |
| Port     | `15432`                     |
| Database | `mail_suite`                |
| Username | `mail_suite_reader`         |
| Password | 上一步读取的只读密码        |
| SSL      | 关闭；公网链路已由 SSH 加密 |

若本机 `15432` 已被占用，可把脚本参数改为 `-LocalPort 25432`，并让数据库客户端连接本地
`25432`。使用完成后在隧道 PowerShell 中按 `Ctrl+C` 正常断开，本地数据库入口随即失效；服务器始终
不监听 `5432/15432`。

## 回滚

先保留诊断和数据库备份，再在当前发布目录执行：

```bash
docker compose --env-file /opt/mail-suite/shared/runtime.env -f deploy/server/compose.yaml down
```

该命令保留 PostgreSQL volume。只有明确放弃测试数据后才可额外删除 volume；不要清空整套 UFW
规则。只读访问回滚时先关闭本地 SSH 隧道，再删除 `mail_suite_reader` 和受限凭据文件；不得删除
数据库 volume 或改变 backend internal 网络。若需撤销防火墙基线，先确认云控制台的备用管理通道，
再逐项删除本轮新增规则。
