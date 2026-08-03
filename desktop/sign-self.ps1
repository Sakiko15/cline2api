# 自签名脚本：给 exe 添加数字签名（免费方案）。
#
# 说明（重要）：
#   - 自签名证书不是 CA 签发的，Windows SmartScreen / Chrome / Edge 不会因此放行下载，
#     它解决不了"下载被标记不安全"的问题。
#   - 但它仍然有价值：文件属性显示签名者、防篡改（修改后签名失效）、
#     部分安全软件对"有签名的文件"比对"完全无签名"更友好。
#   - 想彻底消除浏览器拦截，只能购买 OV/EV 代码签名证书（付费），或用本证书导入
#     到用户机器的"受信任的根证书颁发机构"（不适合分发给陌生人）。
#
# 用法：
#   1. 先运行 desktop/build.sh 构建 exe
#   2. 管理员身份运行 PowerShell，执行：
#        powershell -ExecutionPolicy Bypass -File desktop/sign-self.ps1
#   3. 脚本会生成证书并签名 desktop/build/windows-amd64/cline-proxy-desktop.exe

$ErrorActionPreference = "Stop"

$exe = Join-Path $PSScriptRoot "build\windows-amd64\cline-proxy-desktop.exe"
if (-not (Test-Path $exe)) {
    Write-Host "未找到 $exe ，请先运行 desktop/build.sh" -ForegroundColor Red
    exit 1
}

$certName = "Cline2API Self-Signed"

# 检查签名工具（Windows SDK 自带，或随 VS 安装）
$signtool = Get-Command signtool.exe -ErrorAction SilentlyContinue
if (-not $signtool) {
    Write-Host "未找到 signtool.exe，请安装 Windows SDK（含 Desktop apps with C++ 组件）。" -ForegroundColor Red
    exit 1
}

# 1. 创建（或复用）自签名代码签名证书
$cert = Get-ChildItem Cert:\CurrentUser\My | Where-Object { $_.Subject -like "*$certName*" } | Select-Object -First 1
if (-not $cert) {
    Write-Host "生成自签名证书: $certName" -ForegroundColor Yellow
    $cert = New-SelfSignedCertificate -Type CodeSigningCert `
        -Subject "CN=$certName" `
        -CertStoreLocation Cert:\CurrentUser\My `
        -KeyUsage DigitalSignature `
        -NotAfter (Get-Date).AddYears(3)
}

# 2. 签名（时间戳服务器可保证证书过期后签名仍有效）
Write-Host "签名: $exe" -ForegroundColor Yellow
& $signtool.Source sign /fd SHA256 /f "Cert:\CurrentUser\My\$($cert.Thumbprint)" `
    /tr "http://timestamp.digicert.com" /td SHA256 `
    "$exe"

if ($LASTEXITCODE -eq 0) {
    Write-Host "签名完成。验证：" -ForegroundColor Green
    & $signtool.Source verify /pa /v "$exe"
} else {
    Write-Host "签名失败，退出码: $LASTEXITCODE" -ForegroundColor Red
    exit $LASTEXITCODE
}
