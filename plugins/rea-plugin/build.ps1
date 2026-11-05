#Requires -Version 5.1
# ============================================================
# rea 插件构建脚本：独立 go.mod（零主工程依赖）→ plugins/rea-plugin/bin/rea-bridge.exe
#
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\rea-plugin\build.ps1
# ============================================================
$ErrorActionPreference = 'Stop'

$PluginDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir    = Join-Path $PluginDir 'bin'
$GoExe = 'D:\code\tools\go\bin\go.exe'
if (-not (Test-Path $GoExe)) {
    # 动态探测：PATH 中的 go，或常见安装位置（避免硬编码机器路径）。
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $GoExe = $cmd.Source }
    else {
        foreach ($cand in @('D:\code\setup\go\go\bin\go.exe', "$env:ProgramFiles\Go\bin\go.exe")) {
            if (Test-Path $cand) { $GoExe = $cand; break }
        }
    }
    if (-not (Test-Path $GoExe)) {
        Write-Host "未找到 Go 工具链" -ForegroundColor Red
        exit 1
    }
}

Push-Location $PluginDir
try {
    & $GoExe build -o (Join-Path $BinDir 'rea-bridge.exe') .
    if ($LASTEXITCODE -ne 0) {
        Write-Host "构建失败" -ForegroundColor Red
        exit 1
    }
    Write-Host "构建成功: $(Join-Path $BinDir 'rea-bridge.exe')" -ForegroundColor Green
} finally {
    Pop-Location
}
