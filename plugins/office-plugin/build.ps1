#Requires -Version 5.1
# ============================================================
# office 插件构建脚本：独立 go.mod（零主工程依赖）→ plugins/office-plugin/bin/goffice.exe
#
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\office-plugin\build.ps1
# ============================================================
$ErrorActionPreference = 'Stop'

$PluginDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir    = Join-Path $PluginDir 'bin'
$GoExe     = 'D:\code\tools\go\bin\go.exe'

if (-not (Test-Path $GoExe)) {
    Write-Host "未找到 Go 工具链: $GoExe" -ForegroundColor Red
    exit 1
}

# 独立 go.mod（不碰主工程依赖）；拷贝源码自有 go.mod。
Push-Location $PluginDir
try {
    & $GoExe env GOMOD
    # 先 vendoring-free 构建（download 依赖到模块缓存）。
    & $GoExe build -o (Join-Path $BinDir 'goffice.exe') .
    if ($LASTEXITCODE -ne 0) {
        Write-Host "构建失败" -ForegroundColor Red
        exit 1
    }
    Write-Host "构建成功: $(Join-Path $BinDir 'goffice.exe')" -ForegroundColor Green
} finally {
    Pop-Location
}
