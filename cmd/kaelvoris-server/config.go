// kaelvoris-server 启动配置集中校验（规划板 rm_94698f59e404）：
// 在 config 加载完成后统一遍历必填环境变量/配置项及格式，缺失或非法时
// 一次性汇总输出缺失清单并 exit non-zero，避免运行期半初始化状态。
package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
)

// configFatal 打印配置错误清单后以非零码退出（启动快速失败，不进入运行期）。
func configFatal(errs []string) {
	fmt.Fprintln(os.Stderr, "配置校验失败，共 "+strconv.Itoa(len(errs))+" 项：")
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  - "+e)
	}
	os.Exit(1)
}

// validateStartup 集中校验启动配置（环境变量 + flag 配置项及格式）。
// 必须在任何 os.UserHomeDir/文件操作之前调用：data 为空时默认数据目录依赖
// 主目录环境变量，Linux systemd 默认环境无 HOME 会直接 panic（deployment.md §9.6）。
func validateStartup(data, addr, apiUser, apiPass string, noAPIAuth bool, nodeGrace, storeLimit int) []string {
	var errs []string

	// 主目录环境变量（仅 -data 为空时需要；os.UserHomeDir 依赖：Windows 依次
	// USERPROFILE/HOME（HOMEDRIVE+HOMEPATH 组合不生效，实测 go1.27.1），Unix 仅 HOME）。
	if data == "" && !homeEnvOK() {
		if runtime.GOOS == "windows" {
			errs = append(errs, "主目录环境变量 USERPROFILE/HOME 均缺失：无法定位默认数据目录（请设置环境变量或传 -data）")
		} else {
			errs = append(errs, "环境变量 HOME 缺失：os.UserHomeDir 将失败（systemd 请在 unit 中设置 Environment=HOME=/root，或传 -data）")
		}
	}

	// -addr 格式：host:port，端口需为 0-65535 数字（0 = 随机端口）。
	if _, port, err := net.SplitHostPort(addr); err != nil {
		errs = append(errs, fmt.Sprintf("-addr 格式非法 %q（示例 :8900 / 0.0.0.0:8900）: %v", addr, err))
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		errs = append(errs, fmt.Sprintf("-addr 端口非法 %q（需为 0-65535 数字）", port))
	}

	// REST Basic 凭据必须成对出现：只提供其一 → 认证静默落到空密码（半初始化）。
	if !noAPIAuth {
		if apiUser != "" && apiPass == "" {
			errs = append(errs, "提供了 -api-user 但缺少 -api-pass：REST Basic 认证将静默使用空密码（请成对提供或 -no-api-auth）")
		}
		if apiUser == "" && apiPass != "" {
			errs = append(errs, "提供了 -api-pass 但缺少 -api-user：REST Basic 认证无法生效")
		}
	}

	// 数值配置项不得为负。
	if nodeGrace < 0 {
		errs = append(errs, fmt.Sprintf("-node-grace 非法（%d）：节点失联宽限期不能为负", nodeGrace))
	}
	if storeLimit < 0 {
		errs = append(errs, fmt.Sprintf("-store-limit 非法（%d）：存储上限不能为负（0 = 关闭自动清理）", storeLimit))
	}

	return errs
}

// homeEnvOK 判断主目录环境变量是否齐备（实测对齐 os.UserHomeDir 的取值顺序：
// Windows 仅依次认 USERPROFILE/HOME（HOMEDRIVE+HOMEPATH 组合不生效，实测 go1.27.1），
// Unix 仅认 HOME）。
func homeEnvOK() bool {
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE") != "" || os.Getenv("HOME") != ""
	}
	return os.Getenv("HOME") != ""
}
