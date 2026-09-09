# 本地 Keycloak 认证 Harness

该目录只用于本地 OIDC 与浏览器认证集成。生产部署继续保持 `MAIL_SUITE_AUTH_MODE=disabled`，本套
编排不会修改 `deploy/server/compose.yaml`、DNS、防火墙或测试 ECS。

## 前置条件

- Docker Compose v2。
- PowerShell 7 或具备相同 .NET 加密 API 的环境。
- 一个已经核对来源的 Keycloak OCI 引用，格式必须为
  `repository:version@sha256:<64 位小写十六进制>`。

本地基线已于 2026-09-02 从 registry manifest 核对 Keycloak `26.7.3` 的 `linux/amd64` 摘要：
`sha256:88943b6ad06d6293a239f0dfca5acec64218c9b3ab327bf9c936acf408a6ae3b`。准备脚本仍要求显式传入
完整不可变引用，避免以后把浮动 tag 或其他架构镜像误当成已验收基线。

## 准备

在仓库根目录运行：

```powershell
powershell -ExecutionPolicy Bypass -File deploy/identity/prepare-local.ps1 `
  -KeycloakImage "quay.io/keycloak/keycloak:26.7.3@sha256:88943b6ad06d6293a239f0dfca5acec64218c9b3ab327bf9c936acf408a6ae3b"
powershell -ExecutionPolicy Bypass -File deploy/identity/verify-local.ps1
```

准备脚本只在 `.tmp/identity/` 生成 Web 与 Keycloak 各自独立的短期自签 TLS、随机数据库密码、
管理员密码、测试用户密码、管理员 TOTP、confidential client secret、隔离 realm 和
`identity-bootstrap` manifest。它不会启动容器、连接数据库或输出任何秘密；目录已有上一轮材料时会
拒绝覆盖。

## 启停

```powershell
powershell -ExecutionPolicy Bypass -File deploy/identity/start-local.ps1
powershell -ExecutionPolicy Bypass -File deploy/identity/stop-local.ps1
powershell -ExecutionPolicy Bypass -File deploy/identity/stop-local.ps1 -RemoveVolumes
```

启动顺序固定为数据库与 Keycloak、显式 migration、显式测试身份 apply、OIDC API 与 HTTPS Web。
成功后入口为 `https://mail.127-0-0-1.sslip.io:18444`，Keycloak issuer 为
`https://idp.127-0-0-1.sslip.io:18443/realms/mail-suite-local`。两个 hostname 在宿主机都解析到
`127.0.0.1`，但浏览器 Cookie 因 host-only 约束保持隔离。API 容器通过 Compose network alias 直接访问
相同 issuer hostname 和端口，不绕行宿主回环。证书和私钥先由一次性初始化容器复制到按服务隔离的
named volume；API 只挂载 Keycloak CA，不挂载任何私钥。浏览器只为本轮两张短期证书建立临时信任，
不要把证书或私钥导入长期系统信任库。

`-RemoveVolumes` 只移除当前 `runtime.env` 所属 Compose 项目的隔离数据卷，不删除 `.tmp` 私密材料。
确认环境停止后可人工删除精确目录 `.tmp/identity`，再生成下一轮互不复用的 realm、subject 和秘密。

## 验收边界

`verify-local.ps1` 默认只验证镜像引用固定、Compose 解析、独立短期证书、真实 TOTP/AMR realm 结构、
realm 与 manifest 一致性，以及 `identity-bootstrap --check`。`-Running` 额外用证书指纹固定方式读取
宿主 discovery、固定 end-session endpoint 和匿名 session，从后端容器网络读取同一 issuer，并检查
Keycloak/Web 只能读取自己的私钥挂载。临时 realm 为 client 登记精确主站根路径作为 post-logout URI。

固定 Keycloak 镜像尚未在当前工作区启动，mailbox/administrator 的真实 Code + PKCE、管理员 TOTP
挑战、三类拒绝身份、AMR claim、前台退出确认/回跳、双向换号以及 IdP 停止恢复仍必须在 Docker engine
恢复后运行，未执行前不得声称互操作、MFA 或账号切换已完成。
