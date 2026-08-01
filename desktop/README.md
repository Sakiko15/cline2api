# Cline Go Proxy Desktop

单文件跨平台桌面版（Wails v2）：代理服务、管理后台和系统 WebView 窗口都在同一个可执行文件中。

## 支持平台

| 平台 | WebView | 前置依赖 |
|------|---------|----------|
| Windows x64 | WebView2 | Win10/11 自带，无需额外安装 |
| macOS (Intel/Apple Silicon) | 系统 WebKit | Xcode Command Line Tools (`xcode-select --install`) |
| Linux x64 | WebKit2GTK | GTK3 + WebKit2GTK 开发包 |

## 给最终用户

1. 解压发布包，双击可执行文件。
2. 首次使用时，在窗口内的管理后台完成账号登录。
3. 客户端使用本机地址：
   - Base URL：`http://127.0.0.1:3457/v1`
   - API Key：在管理后台生成
   - Model：如 `cline-free/glm-5.2`
4. 关闭桌面窗口会终止整个程序和本地代理。

程序默认仅监听 `127.0.0.1`，不会将管理后台开放到局域网或公网。

## 构建各平台版本

Wails 依赖各平台原生 WebView 的 C 绑定，**不能交叉编译**，需在每个目标系统上分别构建：

### Windows（当前机器即可）

```bash
./desktop/build.sh
# 产物：desktop/build/windows-amd64/cline-proxy-desktop.exe
```

### macOS

```bash
xcode-select --install   # 首次需要
./desktop/build.sh
# 产物：desktop/build/darwin-arm64/cline-proxy-desktop（Apple Silicon）
#       desktop/build/darwin-amd64/cline-proxy-desktop（Intel）
```

正式分发还需签名和 notarize：

```bash
codesign --deep --force --sign "Developer ID Application: <你的名字>" cline-proxy-desktop
xcrun notarytool submit cline-proxy-desktop --apple-id <你的AppleID> --wait
```

### Linux

```bash
# Debian/Ubuntu
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
# Fedora
sudo dnf install gtk3-devel webkit2gtk4.1-devel
# Arch
sudo pacman -S gtk3 webkit2gtk

./desktop/build.sh
# 产物：desktop/build/linux-amd64/cline-proxy-desktop
```

## 调试构建

构建带控制台输出的版本（不隐藏命令行窗口）：

```bash
go build -tags "desktop production" -o cline-proxy-desktop-debug.exe .
```

自检模式（启动代理、验证 /health、退出，不弹窗口）：

```bash
./cline-proxy-desktop.exe -selfcheck
```

## 技术说明

- 构建必须包含 `-tags "desktop production"`。直接 `go build` 会进入 Wails 的保护分支报错。
- 普通 `go build .` 仍构建原来的命令行代理；`desktop` build tag 只影响桌面版入口。
- 账号、API Key 和请求日志仍由原有代理逻辑保存到 exe 所在目录。不要将 `.cline-accounts.json` 等凭据打进发布包。
- `CLINE_PROXY_PORT` 环境变量或 `-port` 参数可覆盖默认端口 `3457`。
- Windows 图标通过 `.syso` PE 资源嵌入，由 `desktop/icon-gen/` 生成。
- `desktop_main.go` 位于项目根目录（`package main`），需调用同包内 `startProxy` 等未导出函数。
