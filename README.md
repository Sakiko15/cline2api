<div align="center">

# Cline2API

Cline API 反向代理 · 多账号轮询 · 双协议兼容 · 桌面端

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20macOS%20%7C%20Linux-blue)](#构建)

</div>

---

## 简介

Cline2API 是 Cline API 的反向代理服务，支持多账号轮询、OpenAI 和 Anthropic Messages API 双协议、API Key 鉴权，内置中文管理后台。提供跨平台桌面端单文件应用（Windows / macOS / Linux），双击即用。

**开发语言**：Go（后端 + 代理 + 桌面壳），HTML/CSS/JS（管理后台前端，内嵌于二进制）。

## 功能

- **双协议兼容**：同时支持 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic Messages API）
- **多账号轮询**：自动在多个 Cline 账号间切换负载（`round_robin` / `fill` / `random` 策略）
- **中文管理后台**：浏览器访问 `/admin/` 管理账号、API Key、模型配置、请求头、代理设置
- **API Key 鉴权**：保护代理端点，支持生成/删除多个 API Key
- **System Prompt 覆盖**：项目目录下放 `override.md` 则自动替换系统提示词
- **账号导入/导出**：支持 OAuth 登录、手动 Token、批量文件导入，以及跨设备导出
- **请求日志**：记录每次请求的 token 用量、耗时、TPS 等指标
- **桌面端**：单文件跨平台桌面应用（Wails v2），关闭窗口即停止服务

## 快速开始

### 方式一：桌面端（推荐，分享给他人）

从 [Releases](https://github.com/luawei1/cline2api/releases) 下载对应平台的可执行文件，双击运行即可。

| 平台 | 文件 | 说明 |
|------|------|------|
| Windows x64 | `cline-proxy-desktop.exe` | Win10/11 自带 WebView2 |
| macOS Apple Silicon | `cline-proxy-desktop-darwin-arm64` | 需 Xcode CLT |
| macOS Intel | `cline-proxy-desktop-darwin-amd64` | 需 Xcode CLT |
| Linux x64 | `cline-proxy-desktop-linux-amd64` | 需 GTK3 + WebKit2GTK |

### 方式二：命令行

```bash
go build -o cline-proxy .
./cline-proxy              # 默认端口 3457
./cline-proxy -port 8080   # 指定端口
```

启动后访问 http://127.0.0.1:3457/admin/ 进入管理后台。

### 方式三：Docker

```bash
docker compose up -d      # 构建并启动
docker compose logs -f     # 查看日志
docker compose down        # 停止
```

## 使用指南

### 1. 添加 Cline 账号

在管理后台 **账号管理 → 导入账号**：

- **OAuth 浏览器登录**：点击按钮启动设备授权流程，在系统浏览器中完成登录（支持已登录 Cline 的浏览器）
- **手动输入 Token**：输入已有账号的 refreshToken
- **批量文件导入**：上传 JSON 文件或粘贴文本（每行一个 token，或 JSON 数组 `[{refreshToken, email}]`）

### 2. 配置客户端

```
Base URL: http://127.0.0.1:3457/v1
API Key:  <在管理后台生成的 Key>
Model:    cline-free/glm-5.2
```

兼容 OpenAI 和 Anthropic 两种 API 格式。

### 3. 账号导出/导入（跨设备迁移）

- **导出**：账号管理页面点击「导出」按钮，下载 `cline-accounts-export.json`
- **导入**：在另一台设备上用「从文件导入」上传该文件
- 导出格式与批量导入格式完全兼容

### 4. System Prompt 覆盖

在 exe 同目录下创建 `override.md`，内容将替换所有客户端请求的系统提示词。

## 构建

### 桌面端（单文件跨平台）

Wails 依赖各平台原生 WebView，需在目标系统上构建：

```bash
# Windows（当前机器）
./desktop/build.sh

# macOS
xcode-select --install
./desktop/build.sh

# Linux
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
./desktop/build.sh
```

### CI 自动构建

推送 `v*` 标签触发 GitHub Actions 三平台自动构建并发布 Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```

## 数据文件

程序按以下顺序查找数据文件（找到即使用）：

1. 可执行文件所在目录
2. 当前工作目录
3. 用户主目录 `~/.cline2api/`

| 文件 | 说明 |
|------|------|
| `.cline-accounts.json` | 账号池和 API Key |
| `.cline-request-logs.json` | 请求日志 |
| `override.md` | System Prompt 覆盖（可选）|

> ⚠️ 账号文件含明文 refreshToken，属于敏感凭据，不要放入发布包或提交到 Git。

## 可用模型

| 模型 ID | 说明 |
|---------|------|
| `cline-free/glm-5.2` | ✅ 免费，不消耗额度 |
| `cline-pass/glm-5.2` | ❌ 需要 cline-pass 订阅 |
| `cline-pass/deepseek-v4-flash` | ❌ 需要 cline-pass 订阅 |
| `cline-pass/qwen3.7-max` | ❌ 需要 cline-pass 订阅 |

可在后台 **设置 → 默认模型** 中修改。

## 项目结构

```
├── main.go              CLI 入口（go build .）
├── desktop_main.go      桌面端入口（go build -tags desktop）
├── proxy.go             HTTP 服务、API 路由、协议转换、SSE
├── admin.go             管理后台 REST API
├── admin_html.go        管理后台前端（内嵌）
├── auth.go              WorkOS OAuth + Token 刷新
├── pool.go              账号池管理、多位置数据查找
├── request_logs.go      请求日志
├── desktop/             桌面端构建脚本、文档、图标生成器
├── Dockerfile           Docker 构建
├── docker-compose.yml   Docker Compose
└── .github/workflows/   CI 三平台自动构建
```

## 技术栈

- [Go 1.25](https://go.dev) — 后端、代理、桌面壳
- [Wails v2](https://wails.io) — 跨平台桌面 WebView（单文件，非 Electron）
- [WebView2](https://developer.microsoft.com/microsoft-edge/webview2/) / [WebKit](https://webkit.org) / [WebKitGTK](https://webkitgtk.org) — 各平台原生 WebView

## 许可证

[MIT License](LICENSE) © 2026 [luawei1](https://github.com/luawei1)
