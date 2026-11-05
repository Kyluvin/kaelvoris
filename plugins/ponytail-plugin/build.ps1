#Requires -Version 5.1
# ============================================================
# ponytail 插件构建脚本：独立 go.mod（零主工程依赖）→ plugins/ponytail-plugin/bin/ponytail.exe
#
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\ponytail-plugin\build.ps1
# ============================================================
$ErrorActionPreference = 'Stop'

$PluginDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir    = Join-Path $PluginDir 'bin'
	$GoExe     = 'D:\code\setup\go\go\bin\go.exe'

if (-not (Test-Path $GoExe)) {
    Write-Host "未找到 Go 工具链: $GoExe" -ForegroundColor Red
    # 尝试 PATH 中的 go。
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) {
        $GoExe = $cmd.Source
        Write-Host "使用 PATH 中的 go: $GoExe" -ForegroundColor Yellow
    } else {
        exit 1
    }
}

Push-Location $PluginDir
try {
    & $GoExe build -o (Join-Path $BinDir 'ponytail.exe') .
    if ($LASTEXITCODE -ne 0) {
        Write-Host "构建失败" -ForegroundColor Red
        exit 1
    }
    Write-Host "构建成功: $(Join-Path $BinDir 'ponytail.exe')" -ForegroundColor Green
} finally {
    Pop-Location
}
