#Requires -Version 5.1
# ============================================================
# ponytail 插件安装脚本：把插件（清单 + 二进制 + 附属文件）安装到
#   ~/.kaelvoris/plugins/ponytail/
# 安装后 Kaelvoris 桌面端启动/新任务时自动发现（buildMCPTools 连接 mcp_ponytail_* 工具）。
#
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\ponytail-plugin\install.ps1
# ============================================================
$ErrorActionPreference = 'Stop'

$PluginDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$HomeDir   = $env:USERPROFILE
$Target    = Join-Path $HomeDir '.kaelvoris\plugins\ponytail'
$BinDir    = Join-Path $PluginDir 'bin'
$Exe       = Join-Path $BinDir 'ponytail.exe'

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
# 附带技能（按需加载完整版 SKILL.md）。
$SkillSrc = Join-Path $PluginDir 'skills'
if (Test-Path $SkillSrc) {
    Copy-Item -Recurse -Force $SkillSrc -Destination (Join-Path $Target 'skills')
}

Write-Host "ponytail 插件已安装到 $Target" -ForegroundColor Green
Write-Host "重启 Kaelvoris 桌面端或开启新任务后，ponytail_* 工具自动可用（mcp_ponytail_*）。" -ForegroundColor Cyan
