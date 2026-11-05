// Package tools 敏感文件写保护（对齐 Zoo-Code RooProtectedController 简版）：
// 命中保护清单的路径禁止静默写入/改写——由引擎在执行前强制走人工审批，
// 即使自动审批开启（防凭据与环境配置被静默篡改）。
package tools

import (
	"path/filepath"
	"regexp"
	"strings"
)

// protectedRes 敏感路径匹配（工作区相对路径，统一 '/' 分隔后匹配）。
// 覆盖：环境变量文件、私钥/证书/密钥库、SSH 身份文件、凭据类文件、
// .git 内部对象、.rooignore（保护清单本体）。
var protectedRes = []*regexp.Regexp{
	// 环境变量文件：.env / .env.local / .env.production ...
	regexp.MustCompile(`(?i)(^|/)\.env(\.[^/]*)?$`),
	// 私钥/证书/密钥库（*.key/*.pem/*.p12/...）
	regexp.MustCompile(`(?i)\.(pem|key|p12|pfx|jks|keystore|ppk)$`),
	// SSH 私钥（id_rsa / id_ed25519 等，无扩展名形态）
	regexp.MustCompile(`(?i)(^|/)id_(rsa|dsa|ecdsa|ed25519)$`),
	// 凭据类文件
	regexp.MustCompile(`(?i)(^|/)\.netrc$`),
	regexp.MustCompile(`(?i)(^|/)\.aws/credentials$`),
	// git 内部对象（改写钩子/引用会破坏仓库一致性）
	regexp.MustCompile(`(^|/)\.git/`),
	// ignore 系统自身
	regexp.MustCompile(`(^|/)\.rooignore$`),
}

// IsProtectedRelPath 判定工作区相对路径是否属于敏感文件（分隔符无关）。
func IsProtectedRelPath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(strings.ReplaceAll(rel, "\\", "/")))
	for _, re := range protectedRes {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}
