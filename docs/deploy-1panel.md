# 1Panel 部署手册（海外 VPS · 远程镜像）

本手册描述在海外 VPS 上，通过 1Panel 的 **容器 → 编排** 功能，以拉取 GHCR 预构建镜像的方式部署 cline2api。全程无需在 VPS 上构建。

> 中国大陆网络环境请先看文末[附录](#12-附录中国大陆网络备用方案)。

---

## 1. 前提条件

- 一台海外 VPS，已安装 1Panel（自带或已接管 Docker）
- 1Panel 面板 → 容器 → 概览，确认 Docker 运行正常
- 镜像已由 CI 发布（见下一节）

## 2. 确认镜像可拉取（首次部署必做）

镜像由 GitHub Actions 在推送 `v*` 标签时自动发布到：
`ghcr.io/sakiko15/cline2api`（多架构：linux/amd64、linux/arm64）

> **注意：首次 CI 推送后，GHCR 包默认是 Private（私有）**，匿名 `docker pull` 会报 `denied`。二选一：
>
> - **推荐**：GitHub 仓库页 → Packages → `cline2api` → Package settings → **Change visibility → Public**
> - 或在 VPS 上登录后拉取：`docker login ghcr.io`（用户名 = GitHub 用户名，密码 = 带 `read:packages` 权限的 PAT）

网页验证包存在：`https://github.com/Sakiko15?tab=packages`

## 3. 准备数据目录与文件（关键步骤，不可跳过）

在 VPS 上执行：

```bash
mkdir -p /opt/cline2api && cd /opt/cline2api
touch .cline-accounts.json .cline-config.json .cline-zen.json
```

**为什么必须先创建空文件**：Docker 挂载时，如果宿主机上被挂载的文件路径不存在，Docker 会自动创建一个**同名目录**来挂载。容器内程序往"目录"写文件会**静默失败**（只在日志留一行错误），表现为：添加的账号、修改的配置、设置的管理密码在容器重建后全部丢失。

也可以用 1Panel 的 文件 管理 → 定位到 `/opt/cline2api` → 新建这 3 个空文件。

如需 System Prompt 覆盖功能，再执行 `touch override.md` 并写入内容（可选）。

## 4. 创建编排

1Panel → **容器 → 编排 → 创建编排**：

- **名称**：`cline2api`
- **来源**：编辑（使用 Web 编辑器定义服务）
- **勾选**：强制拉取镜像（首次创建建议勾上）
- 粘贴以下内容（与仓库 `docker-compose.yml` 的差别仅在于数据卷使用绝对路径）：

```yaml
services:
  cline-proxy:
    image: ghcr.io/sakiko15/cline2api:latest   # 固定版本示例: ghcr.io/sakiko15/cline2api:v1.3.0
    pull_policy: always
    container_name: cline-proxy
    restart: unless-stopped
    ports:
      - "3457:3457"                # 公网直连（需放行防火墙/安全组）
      # - "127.0.0.1:3457:3457"    # 仅本机 + 反向代理（更安全，二选一）
    environment:
      # 可选：固定管理密码（不设则首启自动生成随机密码并打印到容器日志，见第 6 节）
      - CLINE_ADMIN_PASSWORD=change-me-first
    volumes:
      - /opt/cline2api/.cline-accounts.json:/app/.cline-accounts.json
      - /opt/cline2api/.cline-config.json:/app/.cline-config.json
      - /opt/cline2api/.cline-zen.json:/app/.cline-zen.json
      # 可选：System Prompt 覆盖（需先创建文件）
      # - /opt/cline2api/override.md:/app/override.md:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3457/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

说明：

- 不要写 `version:` 字段（Compose v2 已废弃，只会产生警告）
- 1Panel 创建编排时会**自动预拉取镜像**，然后 `docker compose up -d`；创建过程可在任务日志中看到
- `.cline-request-logs.json`（请求日志）**故意不挂载**：程序以 tmp+rename 原子写该文件，单文件 bind mount 会导致 rename 静默失败；日志保存在容器内，重建编排后清空（属预期行为）

## 5. 首次启动验证

- 容器列表中 `cline-proxy` 状态应为运行中，健康状态约 10–40 秒内变绿
- 查看容器日志，应出现类似：`Loaded 0 active accounts from pool`、`Cline Go Proxy vX.Y.Z ... listening on 0.0.0.0:3457`
- VPS 上执行：

```bash
curl http://127.0.0.1:3457/health
# 期望: {"status":"ok","version":"vX.Y.Z","activeAccounts":0}
```

## 6. 获取管理后台密码（必须先做）

**fail-closed 机制**：未设置管理密码时，管理 API **只允许本机（127.0.0.1）访问**；从公网直接访问 `/admin/api/*` 会返回 403（静态页面 `/admin/` 仍可打开用于展示）。首次启动的密码获取路径：

- **方式 A（默认，零配置）**：首次启动未设密码时，程序**自动生成随机初始密码并打印到容器日志**（一行 `admin panel initial password: xxxx-xxxx-xxxx-xxxx`）。在 1Panel 容器日志里找到它，直接用 `http://<VPS_IP>:3457/admin/` 以此密码登录。该密码只在生成时显示一次，**登录后请立即在后台修改**
- **方式 B（固定密码）**：第 4 节编排 YAML 中配置 `CLINE_ADMIN_PASSWORD` 环境变量 → 容器首次启动即自动设置该密码，以此密码登录（已设密码的实例忽略该变量）
- **方式 C（备用）**：SSH 隧道进入本机设置：`ssh -L 3457:127.0.0.1:3457 <VPS>`，然后在本地浏览器打开 `http://127.0.0.1:3457/admin/`

已设密码后，公网访问恢复正常（输入密码登录）。**忘记密码**：删除宿主机 `/opt/cline2api/.cline-accounts.json` 中的 `AdminPasswordHash` 字段后重启容器，下次启动重新走初始密码生成。

更安全的替代方案（可选）：

- 安全组只对自己的 IP 放行 3457
- 端口改为仅本机监听（`127.0.0.1:3457:3457`），再用 1Panel **网站 → 反向代理** 指向 `http://127.0.0.1:3457` 并套 HTTPS

## 7. 添加 Cline 账号

管理后台 → 账号管理 → 导入账号：

- **OAuth 浏览器登录**（推荐）：走设备授权流程
- 或手动粘贴 refreshToken / 批量导入

## 8. 持久化验证

- 1Panel 容器列表中**重启** `cline-proxy`
- 再次 `curl http://127.0.0.1:3457/health`，`activeAccounts` 应仍为账号数
- 宿主机确认数据落盘：`cat /opt/cline2api/.cline-accounts.json`（含账号信息即正常）

## 9. 升级流程

- **方式 A（推荐，可锁定版本）**：编辑编排，把 `image:` 的 tag 改成新版本号（如 `:v1.4.0`）→ 保存并勾选强制拉取 → 重建
- **方式 B（跟随 latest）**：直接 **强制拉取镜像 + 重建**（compose 中已有 `pull_policy: always`，1Panel 的强制拉取会保证拿到新镜像）

升级后用 `curl http://127.0.0.1:3457/health` 确认 `version` 字段已更新。

## 10. 回滚

把 `image:` 的 tag 改回上一个版本号 → 强制拉取 + 重建。所有数据文件都在宿主机 `/opt/cline2api/`，回滚不影响数据。

## 11. 安全清单

- [ ] 管理后台已设置密码
- [ ] 云安全组仅放行必要来源 IP（或改用 127.0.0.1 + 反向代理 + HTTPS）
- [ ] `.cline-accounts.json` 含**明文 refreshToken**，已将其目录权限收紧（`chmod 600 /opt/cline2api/.cline-*`），且不外传、不入库
- [ ] 定期升级镜像获取修复
- [ ] （可选）启用管理后台的请求日志定期清理，避免容器内日志文件无限增长

## 12. 附录：中国大陆网络备用方案

海外 VPS 不需要本节。若在大陆机器上拉取 `ghcr.io` 超时，把编排中镜像前缀改为镜像代理即可：

```yaml
image: ghcr.m.daocloud.io/sakiko15/cline2api:latest
```

（注意：Docker daemon.json 的 `registry-mirrors` 只对 Docker Hub 生效，对 ghcr.io 无效，必须改写镜像前缀；第三方镜像源可用性波动较大，以部署时实测为准。）