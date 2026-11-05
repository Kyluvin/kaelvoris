#Requires -Version 5.1
# ============================================================
# office 插件安装脚本：把插件（清单 + 二进制 + 附属文件）安装到
#   ~/.kaelvoris/plugins/office/
# 安装后 Kaelvoris 桌面端启动/新任务时自动发现（buildMCPTools 连接 mcp_office_* 工具）。
#
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\office-plugin\install.ps1
# ============================================================
$ErrorActionPreference = 'Stop'

$PluginDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$HomeDir   = $env:USERPROFILE
$Target    = Join-Path $HomeDir '.kaelvoris\plugins\office'
$BinDir    = Join-Path $PluginDir 'bin'
$Exe       = Join-Path $BinDir 'goffice.exe'

if (-not (Test-Path $Exe)) {
    Write-Host "未找到产物 $Exe，请先运行 build.ps1" -ForegroundColor Red
    exit 1
}

# 1) 清空旧安装（幂等）。
if (Test-Path $Target) {
    Remove-Item -Recurse -Force $Target
}
New-Item -ItemType Directory -Path $Target -Force | Out-Null

# 2) 复制清单 + 二进制（附带 README 等）。
Copy-Item (Join-Path $PluginDir 'plugin.json') -Destination $Target -Force
Copy-Item $Exe -Destination $Target -Force
if (Test-Path (Join-Path $PluginDir 'README.md')) {
    Copy-Item (Join-Path $PluginDir 'README.md') -Destination $Target -Force
}

Write-Host "office 插件已安装到 $Target" -ForegroundColor Green
Write-Host "重启 Kaelvoris 桌面端或开启新任务后，office_* 工具自动可用（mcp_office_*）。" -ForegroundColor Cyan
