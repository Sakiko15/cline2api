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

### Windows 资源信息（图标 / 版本 / manifest）

Windows exe 的图标、文件版本信息（属性页"详细信息"）和应用 manifest（DPI 感知、Win10/11 兼容声明）
都来自 PE 资源文件 `resource_windows_amd64.syso`（项目根目录，go build 自动链接）。

修改图标或版本号后需重新生成：

```bash
cd desktop/icon-gen
go run .            # 生成 desktop/icon-gen/resource_windows_amd64.syso
cp resource_windows_amd64.syso ../../resource_windows_amd64.syso
```

版本号在 `desktop/icon-gen/main.go` 顶部的 `appVersion` 等常量中修改。生成的 `.syso` 应提交到仓库，
CI 构建依赖它（见根目录 `.gitignore` 中的 `!resource_windows_amd64.syso`）。

### 发布 zip（网盘分发推荐）

裸 exe 上传网盘后 Chrome/Edge 容易拦截，打包成 zip 可显著降低拦截率：

```bash
./desktop/build.sh && ./desktop/dist.sh
# 产物：desktop/dist/ccline2api-windows-amd64.zip（exe + 使用说明）
```

### 自签名（可选，免费防篡改）

自签名证书**无法**消除 SmartScreen 拦截（浏览器不信任自签根），但能让文件属性显示签名者、
防止被篡改，且部分安全软件对"有签名的文件"更友好：

```powershell
# 管理员 PowerShell
powershell -ExecutionPolicy Bypass -File desktop/sign-self.ps1
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
- Windows 图标、版本信息、应用 manifest 通过 `.syso` PE 资源嵌入，由 `desktop/icon-gen/` 生成（图标 + 版本信息 + manifest 在同一资源文件内）。
- `desktop_main.go` 位于项目根目录（`package main`），需调用同包内 `startProxy` 等未导出函数。
