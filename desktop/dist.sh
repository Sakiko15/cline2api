#!/usr/bin/env sh
# 打包发布 zip：把桌面版 exe + 使用说明打包成单个 zip。
# 网盘分发 zip 时，浏览器拦截率显著低于裸 exe（zip 不是直接执行的 PE 文件）。
# 用法：sh desktop/dist.sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT="$ROOT/desktop/build/windows-amd64"
EXE="$OUT/cline-proxy-desktop.exe"

if [ ! -f "$EXE" ]; then
  echo "未找到 $EXE，请先运行 desktop/build.sh" >&2
  exit 1
fi

DIST="$ROOT/desktop/dist"
rm -rf "$DIST"
mkdir -p "$DIST/ccline2api"

cp "$EXE" "$DIST/ccline2api/"
cat > "$DIST/ccline2api/使用说明.txt" <<'EOF'
Cline2API 桌面版（cline-proxy-desktop.exe）

1. 双击 exe 启动，自动打开管理界面 http://127.0.0.1:3457/admin/
2. 首次使用需在管理界面添加 Cline 账号（OAuth 登录）并生成 API Key
3. 客户端（Cline CLI 等）将 API 地址指向本机 3457 端口即可

若 Windows SmartScreen 提示"Windows 已保护你的电脑"：
  - 这是未购买代码签名证书的正常提示，点击"更多信息" → "仍要运行" 即可
EOF

cd "$DIST"
# 用 Windows 自带的 Compress-Archive 打包（Git Bash 无 zip 命令）
powershell -NoProfile -Command "Compress-Archive -Path 'ccline2api' -DestinationPath 'ccline2api-windows-amd64.zip' -Force"
rm -rf "ccline2api"

echo "已生成: $DIST/ccline2api-windows-amd64.zip"
