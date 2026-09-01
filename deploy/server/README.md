# 单节点测试服务器部署

该目录只部署当前已实现的 PostgreSQL、API/worker 健康基础和 Web 未连接预览。它不包含 mail-core，
因此不能作为 SMTP 收件、OIDC 登录、邮件查看或导出的验收证据。

## 固定环境

- Ubuntu Server 24.04.4 LTS x86_64
- Docker `29.1.3-0ubuntu3~24.04.2`
- Docker Compose v2 `2.40.3+ds1-0ubuntu1~24.04.1`
- Docker Buildx `0.30.1-0ubuntu1~24.04.1`
- PostgreSQL `17.11-alpine`，镜像使用 manifest digest 固定
- 预览入口 `127.0.0.1:18080`，不直接暴露公网

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

通过本地 SSH 隧道预览：

```powershell
ssh -i key/mail-suite.pem -L 18080:127.0.0.1:18080 root@服务器地址
```

浏览器访问 `http://127.0.0.1:18080`。当前页面必须继续显示未登录预览或未连接状态。

## 回滚

先保留诊断和数据库备份，再在当前发布目录执行：

```bash
docker compose --env-file /opt/mail-suite/shared/runtime.env -f deploy/server/compose.yaml down
```

该命令保留 PostgreSQL volume。只有明确放弃测试数据后才可额外删除 volume；不要清空整套 UFW
规则。若需撤销防火墙基线，先确认云控制台的备用管理通道，再逐项删除本轮新增规则。
