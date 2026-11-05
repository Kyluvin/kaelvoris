package version

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manifest 升级源清单（update.json）——部署目录或任意 http(s)/file 地址提供。
//
// 示例（D:\service\kaelvoris\update.json）：
//
//	{
//	  "version": "0.7.0",
//	  "url": "kaelvoris-desktop.exe",      // 相对清单所在目录或绝对 URL/路径
//	  "notes": ["新增 XXX", "修复 YYY"],
//	  "date": "2025-01-01",
//	  "sha256": "…hex…",                  // 可选：产物完整性校验
//	  "mandatory": false
//	}
type Manifest struct {
	Version       string   `json:"version"`                 // 最新版本号
	URL           string   `json:"url"`                     // 新版可执行文件下载地址（空 = 清单目录下 kaelvoris-desktop.exe）
	Notes         []string `json:"notes,omitempty"`         // 新版新增功能说明（提示与面板展示）
	Date          string   `json:"date,omitempty"`          // 发布日期
	Sha256        string   `json:"sha256,omitempty"`        // 可选完整性校验（hex；空 = 跳过）
	MinUpgradable string   `json:"minUpgradable,omitempty"` // 低于此版本不允许直接升级（提示重新下载安装包）
	Mandatory     bool     `json:"mandatory,omitempty"`     // 强制升级标记（面板红色提示）
	// SanityMinBytes 下载产物最小字节数门禁（发布侧可设；0 = 默认阈值 8MB）。
	// 防半截下载/坏包/HTML 错误页冒充安装包（发布事故门禁，2026-10 复盘）。
	SanityMinBytes int64 `json:"sanityMinBytes,omitempty"`
}

// CheckResult 一次检查更新的结果。
type CheckResult struct {
	Current       string   `json:"current"`         // 当前版本
	Latest        string   `json:"latest"`          // 清单版本
	Update        bool     `json:"update"`          // 是否有更新
	Notes         []string `json:"notes,omitempty"` // 新版新增功能
	Date          string   `json:"date,omitempty"`
	Source        string   `json:"source,omitempty"`   // 升级源
	AssetURL      string   `json:"assetUrl,omitempty"` // 解析后的下载地址
	Mandatory     bool     `json:"mandatory,omitempty"`
	MinUpgradable string   `json:"minUpgradable,omitempty"`
	Sha256        string   `json:"sha256,omitempty"` // 清单声明的产物哈希（升级校验用）
}

// fetchTimeout 拉取清单/安装包的超时。
const fetchTimeout = 30 * time.Second

// sanityMinBytesDefault 下载产物体积门禁默认值：桌面端 exe 正常约 38MB，
// 低于 8MB 必然是半截下载/坏包/文本错误页，直接拒绝替换（发布事故门禁）。
const sanityMinBytesDefault = 8 << 20

// peSignature Windows PE 文件 DOS 头标记（"MZ"）。
const peSignature = "MZ"

// ValidateExecutable 产物完整性门禁：体积下限 + PE 头 + 可选 SHA256。
// 发布/自升级链路在替换任何文件前必须先过此检查——坏包绝不允许落地
// （2026-10 发布事故：坏安装包未经校验直接替换线上文件导致客户端无法运行）。
//
//	path        待校验文件路径
//	minBytes    体积门禁（<=0 用默认 8MB）
//	wantSha256  期望哈希（hex，大小写不敏感；空 = 跳过哈希校验）
func ValidateExecutable(path string, minBytes int64, wantSha256 string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("产物文件不存在: %w", err)
	}
	if minBytes <= 0 {
		minBytes = sanityMinBytesDefault
	}
	if info.Size() < minBytes {
		return fmt.Errorf("产物体积异常: %d 字节（低于门禁 %d），疑似半截下载或坏包", info.Size(), minBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开产物失败: %w", err)
	}
	defer f.Close()
	// PE 头校验 + 全量流式哈希（一次读盘完成，38MB 级无感知）。
	h := sha256.New()
	hdr := make([]byte, len(peSignature))
	if _, err := io.ReadFull(io.TeeReader(f, h), hdr); err != nil {
		return fmt.Errorf("读取产物头失败: %w", err)
	}
	if string(hdr) != peSignature {
		return fmt.Errorf("产物不是有效的 Windows PE 文件（缺少 MZ 头），拒绝替换")
	}
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("读取产物失败: %w", err)
	}
	if wantSha256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != strings.ToLower(strings.TrimSpace(wantSha256)) {
			return fmt.Errorf("产物 SHA256 不匹配（下载损坏或升级源被污染）")
		}
	}
	return nil
}

// Sha256File 计算文件 SHA256（hex 小写；发布脚本与升级面板复用）。
func Sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FetchManifest 拉取并解析升级源清单。
// src 支持 http(s):// 与本地/UNC 路径（file 语义直接读盘，部署同盘场景零依赖）。
func FetchManifest(src string) (*Manifest, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, errors.New("升级源为空")
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: fetchTimeout}
		resp, err := client.Get(src)
		if err != nil {
			return nil, fmt.Errorf("拉取清单失败: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("拉取清单失败: HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("读取清单失败: %w", err)
		}
		return parseManifest(b)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("读取清单失败: %w", err)
	}
	return parseManifest(b)
}

// parseManifest 解析清单 JSON（拒绝明显非法输入）。
func parseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := UnmarshalCompat(b, &m); err != nil {
		return nil, fmt.Errorf("清单 JSON 解析失败: %w", err)
	}
	if strings.TrimSpace(m.Version) == "" {
		return nil, errors.New("清单缺少 version 字段")
	}
	return &m, nil
}

// ResolveAssetURL 把清单中的下载地址解析为可拉取 URL：
//   - 绝对 http(s) URL 原样返回；
//   - 相对路径按清单来源（清单 URL 或清单文件路径所在目录）拼接；
//   - 空 = 清单目录下 kaelvoris-desktop.exe。
func ResolveAssetURL(manifestURL, asset string) string {
	asset = strings.TrimSpace(asset)
	// 基准目录：清单 URL（http）去掉文件名；本地清单取所在目录。
	var base string
	if strings.HasPrefix(manifestURL, "http://") || strings.HasPrefix(manifestURL, "https://") {
		base = manifestURL[:strings.LastIndex(manifestURL, "/")+1]
	} else {
		base = filepath.ToSlash(filepath.Dir(manifestURL)) + "/"
	}
	if asset == "" {
		asset = "kaelvoris-desktop.exe"
	}
	if strings.HasPrefix(asset, "http://") || strings.HasPrefix(asset, "https://") {
		return asset
	}
	if filepath.IsAbs(asset) || strings.Contains(asset, "://") {
		return asset // 本地绝对路径 / UNC
	}
	return base + filepath.ToSlash(asset)
}

// Check 检查更新：拉取清单并与当前版本比较。
// 有更新时返回 CheckResult.Update=true（含新版功能说明与下载地址）。
func Check(src string) (*CheckResult, error) {
	m, err := FetchManifest(src)
	if err != nil {
		return nil, err
	}
	r := &CheckResult{
		Current:       Number,
		Latest:        m.Version,
		Source:        src,
		Notes:         m.Notes,
		Date:          m.Date,
		Mandatory:     m.Mandatory,
		MinUpgradable: m.MinUpgradable,
		Sha256:        m.Sha256,
		AssetURL:      ResolveAssetURL(src, m.URL),
	}
	r.Update = IsNewer(Number, m.Version)
	if r.Update && m.MinUpgradable != "" && IsNewer(m.MinUpgradable, Number) {
		// 当前版本过旧：不允许在线升级（数据/配置兼容性），提示整包安装。
		r.Update = false
		r.Notes = append([]string{"当前版本过低，请下载完整安装包升级"}, m.Notes...)
	}
	return r, nil
}

// Download 下载新版可执行文件到 destDir（临时目录），返回文件路径。
// 大小上限 512MB（防误配升级源拉到超大文件）。
// 下载完成后立即做产物校验（体积门禁 + PE 头 + 可选 SHA256），
// 校验失败删除残留并报错——坏包绝不允许进入暂存/替换链路（发布事故门禁）。
func Download(url, destDir, wantSha256 string) (string, error) {
	var dst string
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		p, err := downloadHTTP(url, destDir)
		if err != nil {
			return "", err
		}
		dst = p
	} else {
		// 本地 / UNC：直接复制（部署源同盘场景最常见）。
		if _, err := os.Stat(url); err != nil {
			return "", fmt.Errorf("新版文件不存在: %w", err)
		}
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return "", err
		}
		dst = filepath.Join(destDir, "update.exe")
		in, err := os.Open(url)
		if err != nil {
			return "", err
		}
		out, err := os.Create(dst)
		if err != nil {
			in.Close()
			return "", err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			_ = os.Remove(dst)
			return "", fmt.Errorf("复制新版文件失败: %w", err)
		}
		in.Close()
		out.Close()
	}
	// —— 产物完整性门禁：体积 + PE 头（+ 可选哈希，比对清单 sha256 字段）——
	if err := ValidateExecutable(dst, 0, wantSha256); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

func downloadHTTP(url, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(destDir, "update.exe")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("下载中断: %w", err)
	}
	if n == 0 {
		_ = os.Remove(dst)
		return "", errors.New("下载内容为空")
	}
	slog.Info("新版已下载", "url", url, "bytes", n, "dst", dst)
	return dst, nil
}
