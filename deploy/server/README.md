# 单节点测试服务器部署

该目录用于在 Ubuntu 24.04 测试 ECS 上部署完整基础服务集：PostgreSQL、Keycloak、Stalwart、API、
真实 operation worker 和 Web/Nginx。公网只允许 `25/80/443`，PostgreSQL、Keycloak、API、worker
以及 Stalwart 的 Admin/JMAP/recovery 端口均保持在 Docker 内网。

这套编排具备 OIDC、mail-core 和 worker 的运行条件，但不替代业务验收。邮件列表、搜索、详情、附件、
原文、管理员正文短时授权和审计 API 尚未全部实现时，不能将基础服务健康等同于完整在线闭环通过。

## 固定环境

- Ubuntu Server `24.04.4 LTS` x86_64
- Docker `29.1.3-0ubuntu3~24.04.2`
- Docker Compose `2.40.3+ds1-0ubuntu1~24.04.1`
- Docker Buildx `0.30.1-0ubuntu1~24.04.1`
- Certbot `2.9.0-1`
- jq `1.7.1-3ubuntu0.24.04.2`
- Python `3.12.3-0ubuntu2.1`
- bind9-dnsutils `1:9.18.39-0ubuntu0.24.04.7`
- PostgreSQL `17.11-alpine`、Keycloak `26.7.3`、Stalwart `0.16.19` 和 Stalwart CLI `1.0.12`
  均使用固定 digest 对应的本地别名
- 资源上限：PostgreSQL 384 MiB、Keycloak 512 MiB、Stalwart 320 MiB、API 128 MiB、worker
  128 MiB、Web 96 MiB

## 部署前置

开始申请证书前，以下三个 A 记录必须唯一指向目标 ECS，未提供 IPv6 服务时不要创建 AAAA：

```text
mail.test.snowye.fun
idp.test.snowye.fun
mx1.test.snowye.fun
```

云安全组保留现有 SSH 管理入口，并允许后续验收使用的 `25/80/443`。主机 UFW 仍由部署脚本控制：
首次 ACME challenge 只临时放行 `80`，内部验证通过后才持久放行 `25/80/443`。在内部 SMTP、持久化、
JMAP 读取和未知收件人拒绝均通过前，不要切换 MX。

ACME 临时 Web 保持只读根文件系统，仅通过非持久化 tmpfs 生成 Nginx 配置；脚本先完成本机 challenge
路径探针，再提交证书订单。临时容器未就绪时不会消耗外部域名验证请求。

若公网 HTTP 被云厂商备案网关明确拦截，可对缺失的单个 hostname 使用人工 DNS-01 回退。以受限的
后台服务运行 `request-manual-dns-certificate.sh` 后，从
`/opt/mail-suite/shared/acme-manual/<hostname>.challenge` 读取 `record` 和 `value`，在 DNS 控制台创建
该 TXT；脚本只在阿里、Google 和 Cloudflare 公共解析器连续两分钟返回精确值后继续签发。签发完成后
删除 TXT 和 challenge 文件。此方式不保存 DNS API 凭据，只用于首次签发或应急恢复，不能作为长期
自动续期方案。当前 ECS 仅用于受控测试和验收，不面向全网发布，因此备案暂不作为前置；公网 Web 与
STARTTLS 的云链路限制仍须单独记录，不能据此宣称公网验收通过。

三条既有 lineage 使用 AliDNS 自动 DNS-01 续期。先在阿里云 RAM 创建独立用户，将
`deploy/server/config/alidns-acme-policy.json` 的自定义策略绑定给该用户，再创建专用 AccessKey。该策略
中的 `<ALIYUN_ACCOUNT_ID>` 必须先替换为当前阿里云账号 ID，不得保留占位符或改成通配符。该策略
只允许对 `snowye.fun` 域执行 `DescribeDomainRecords`、`DescribeDomainRecordInfo`、`AddDomainRecord`
和 `DeleteDomainRecord`；AliDNS 不支持继续按 RR/TXT 细分 RAM 权限，hook 会在应用层只允许三个固定的
`_acme-challenge` TXT。

把发布目录传给配置脚本，并确保当前 SSH 会话分配了 TTY：

```bash
bash deploy/server/configure-alidns-certificate-renewal.sh .
```

脚本以隐藏输入读取 AccessKey，保存到目标机 `0600 root:root` 文件，然后通过 Certbot `reconfigure`
逐条执行 staging 续期。auth hook 会在阿里、Google、Cloudflare 三家公共解析器连续返回精确 TXT 后
放行；cleanup 只删除本次状态持有且内容仍匹配的 RecordId。由于 AliDNS 默认 TTL 为 600 秒，最后的
公共缓存清理确认首次可能等待约 10 分钟，最长 15 分钟。三条都成功、TXT 均清理后才写成功标记并启用
`certbot.timer`。轮换 AccessKey 时重新执行：

```bash
bash deploy/server/configure-alidns-certificate-renewal.sh . --rotate-credentials
```

## 首次主机引导

仅在未完成运行时初始化的 Ubuntu 24.04 主机上，从解压后的发布目录以 root 执行：

```bash
MAIL_SUITE_REGISTRY_MIRROR=https://你的阿里云专属地址 bash deploy/server/bootstrap-host.sh
```

脚本安装固定版本运行时、保留有界 Docker 日志、创建 `/opt/mail-suite`，并启用只放行 OpenSSH 的
UFW 基线。它不会修改 SSH 端口或认证方式，也不会提前开放业务端口。

## 构建离线镜像包

ECS 无法直接访问固定 OCI registry 时，在受控 Windows 开发机的仓库根目录执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File deploy/server/build-offline-images.ps1
```

脚本构建四个后端制品和 Web 镜像，拉取固定外部镜像，输出
`.tmp/mail-suite-images-20260905-rc6.tar` 及其 SHA-256。上传发布目录和镜像包后，必须在服务器重新计算
SHA-256，再执行 `docker load --input <已核对的镜像包>`。不得覆盖旧 release、旧镜像或现有 volume。

## 部署

每次发布使用新的 `/opt/mail-suite/releases/<release-id>`。确认镜像已加载且三个 A 记录生效后，从该
发布目录执行：

```bash
export MAIL_SUITE_SKIP_BUILD=1
export MAIL_SUITE_IMAGE_TAG=test-20260905-rc7
export MAIL_SUITE_EXPECTED_PUBLIC_IPV4=<目标-ECS-IPv4>
export MAIL_SUITE_ACME_EMAIL=<ACME-联系邮箱>
bash deploy/server/deploy.sh .
```

首次部署会在 `/opt/mail-suite/shared` 生成 `0600 root:root` 的运行配置、Keycloak realm、受控身份
manifest 和 worker 专用 `0400` secret。Stalwart recovery 身份只用于首次 Bootstrap，永久管理员创建
后会从 `runtime.env` 删除并通过重建容器清除。脚本不会回显密码、TOTP secret 或派生密钥。

部署顺序固定为运行材料、证书、migration、数据库角色、Stalwart Bootstrap、Keycloak、测试身份、
长期服务和内部验证。公网 Nginx 在容器内额外监听 `443`，供 backend 网络按正式 issuer 完成严格 TLS
OIDC discovery；部署脚本会在启动 API 前先验证该路径。只有全部内部验证通过后，才原子更新
`/opt/mail-suite/current` 并放行公网端口。
Web 健康检查也通过 backend 别名访问 `https://mail.test.snowye.fun/health/ready`，不会跳过证书校验或
误入默认 `421` 虚拟主机。
身份 fixture 会为活动测试邮箱创建唯一的 `mailbox.provision` operation/outbox；部署会等待 worker 通过
Stalwart adapter 将其推进到 `succeeded`。停用身份不创建 mail-core 账号，不能用容器 healthy 代替该
业务收敛证据。
目标 2 GB ECS 上 Keycloak 首次增强、建表和 realm 导入约需 4 分钟；容器健康检查读取管理端真实
`/health/ready`，不会把端口已监听但仍返回 `503` 的初始化阶段误判为可用。

## 验证

部署脚本已执行不依赖公网防火墙的内部验证。切换后再执行完整主机验证：

```bash
bash /opt/mail-suite/current/deploy/server/verify.sh
```

验证覆盖容器健康和资源限制、secret 权限、recovery 清理、数据库隔离、发布端口、OIDC issuer、IdP
管理路径拒绝、Stalwart 严格 TLS、SMTP STARTTLS、UFW 和非回环 listener。验证脚本不会展开
Compose 配置或回显运行 secret。

还必须从 ECS 之外的网络独立验证：

- `mail.test.snowye.fun` 和 `idp.test.snowye.fun` 的公共 TLS 链、SAN 与 HTTP 行为；
- TCP `25`、SMTP EHLO/STARTTLS、合法收件人 RCPT/DATA、未知收件人和开放中继拒绝；
- 真实测试邮件在普通重启后仍可通过 JMAP/API 读取；
- 五类受控身份的 Code + PKCE、TOTP、拒绝路径、退出、IdP 停止与恢复；
- 桌面、移动端和 320px 视口下的真实邮件列表、搜索、详情、附件、原文、越权和审计。

未实现的业务 API 必须保持未连接或明确失败状态，不得用 fixture 或 Stalwart 管理页面替代验收证据。

## 证书续期与回滚

Certbot timer 续期成功后调用固定 deploy hook，重新验证三条独立 lineage、复制只读快照，并热加载
Nginx 与 Stalwart。AliDNS auth/cleanup hook 安装在 `/opt/mail-suite/shared/acme-hooks`，不绑定历史
release；最近 staging 成功标记只保存时间、lineage 和 hook 摘要，不保存 AccessKey 或 challenge。
查看 timer 和最近执行结果：

```bash
systemctl status certbot.timer
journalctl -u certbot.service --since today
bash /opt/mail-suite/current/deploy/server/verify.sh
```

若续期后的外部验证失败，可切回上一份已归档证书快照：

```bash
bash /opt/mail-suite/current/deploy/server/install-public-certificates.sh \
  /opt/mail-suite/current --rollback
```

回滚会再次校验 hostname、有效期、证书与私钥配对及三条私钥互异，然后热加载运行服务。不得直接
编辑 `/opt/mail-suite/shared/tls/current` 或删除归档。

## PostgreSQL 只读隧道

不要在云安全组或 UFW 放行 `5432/15432`。在开发机仓库根目录启动动态 SSH 隧道：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File deploy/server/open-readonly-database-tunnel.ps1 -Server <ECS-公网-IP>
```

客户端连接 `127.0.0.1:15432`、数据库 `mail_suite`、用户 `mail_suite_reader`。密码只在首次配置客户端
时从 `/opt/mail-suite/shared/postgres-readonly.env` 受控读取，不写入命令、文档、截图或仓库。使用完
按 `Ctrl+C` 关闭隧道，服务器始终不监听数据库公网端口。

## 发布回滚

回滚前先保留脱敏诊断和数据库、Stalwart 配置/数据备份；若已切换 MX，先恢复原 DNS 并等待 TTL。
停止新公网入口，逐项撤销本轮新增的 `25/80/443` UFW 规则，再使用保留的旧 release 和准确旧 image
tag 重建旧 Compose 服务，最后把 `/opt/mail-suite/current` 指回旧 release。

回滚不得执行 `docker compose down --volumes`，不得删除 PostgreSQL 或 Stalwart volume，不得复用或
回显新 secret，也不得修改现有 MailHub 邮件数据。旧控制面使用 `disabled` 认证时，应同时确保新
OIDC Cookie 不再有可访问入口。
