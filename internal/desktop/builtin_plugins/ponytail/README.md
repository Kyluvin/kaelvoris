# Ponytail 插件（懒人资深工程师）

> 提炼自开源项目 [DietrichGebert/ponytail](https://github.com/DietrichGebert/ponytail)（MIT）。
> 治 agent 话痨 / 过度工程：把「懒人资深工程师」放进 agent。

本插件以标准 MCP 插件形态接入 Kaelvoris，模型可主动调用
`mcp_ponytail_review` / `mcp_ponytail_audit` / `mcp_ponytail_lint` /
`mcp_ponytail_debt` 四个工具自检。

## 安装

```powershell
# 构建（独立 go.mod，纯标准库）
powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\ponytail-plugin\build.ps1

# 安装到 ~/.kaelvoris/plugins/ponytail/
powershell -NoProfile -ExecutionPolicy Bypass -File D:\code\gocoder\plugins\ponytail-plugin\install.ps1
```

重启桌面端或开启新任务后，工具自动注册为 `mcp_ponytail_*`。

## 工具集

| 工具 | 说明 |
|---|---|
| `ponytail_review` | 对一次改动做 YAGNI 阶梯审查（精确输出协议：6 标签 + net: 计分） |
| `ponytail_audit` | 对文件/目录做过度工程审计（仓库级扫描语义 + 清单） |
| `ponytail_lint` | 对单段代码做输出克制检查（能否更短/解释>代码） |
| `ponytail_debt` | **真实扫描**工作区 `ponytail:` 注释生成债务台账（含 no-trigger 腐化标记） |

## 附带技能（6 个，随插件安装到 ~/.kaelvoris/plugins/ponytail/skills/）

| 技能 | 说明 |
|---|---|
| `ponytail` | 懒人模式本体（支持 lite/full/ultra 强度） |
| `ponytail-review` | 只查过度工程的代码 review（精确标签协议） |
| `ponytail-audit` | 仓库级过度工程审计（按砍掉收益排序） |
| `ponytail-debt` | 收集 `ponytail:` 简写注释成债务台账 |
| `ponytail-gain` | 实测影响计分板（基准均值） |
| `ponytail-help` | 全部命令/技能速查卡 |

## 与系统提示词 / 内置技能的关系

- 系统提示词 `LAZY SENIOR DEV MODE` 段：常驻守则（无条件注入），治「习惯性话痨」。
- 内置技能 `ponytail / ponytail-review / ponytail-audit / ponytail-debt / ponytail-gain / ponytail-help`（`internal/skills/builtin/`）：随发行版内置，按需加载。
- 本插件：可调用工具（`ponytail_debt` 为真实文件扫描），模型每次改代码后可主动跑一遍审查。
