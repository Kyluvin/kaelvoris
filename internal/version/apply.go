package version

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kaelvoris/internal/sysproc"
)

// 标记文件名：升级意图落盘（当前目录读写；启动时由 App 检查并提示）。
// Windows 下运行中的 exe 无法被覆盖，故升级 = 下载新 exe → 同盘暂存 →
// 写标记 → 退出进程；批处理在新进程启动前完成替换。
const (
	pendingMarker = "update-pending.json"
	backupDirName = "_backup"
	// failedMarker 升级失败回滚记录（脚本回滚后落盘，下次启动提示一次并清除）。
	failedMarker = "update-failed.txt"
	// backupKeep 自升级备份保留份数（超出后按修改时间删最旧，防目录无限膨胀）。
	backupKeep = 10
)

// PendingUpgrade 升级标记（记录目标版本与暂存文件，供重启后的替换脚本使用）。
type PendingUpgrade struct {
	Target    string `json:"target"` // 目标版本
	Staged    string `json:"staged"` // 暂存的新版 exe（同盘绝对路径）
	Notes     string `json:"notes,omitempty"`
	StartedAt string `json:"startedAt"`
}

// ExePath 当前可执行文件绝对路径。
func ExePath() (string, error) {
	return os.Executable()
}

// ExeDir 当前可执行文件目录（标记 / 备份 / 暂存的默认位置）。
func ExeDir() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// MarkerPath 升级标记文件路径（exe 同目录）。
func MarkerPath() (string, error) {
	dir, err := ExeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingMarker), nil
}

// FailedMarkerPath 升级失败记录文件路径（exe 同目录）。
func FailedMarkerPath() (string, error) {
	dir, err := ExeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, failedMarker), nil
}

// ReadFailedNotice 读取升级失败回滚记录（不存在返回空串）。
// 内容为脚本回滚时写入的一行时间戳说明。
func ReadFailedNotice() string {
	path, err := FailedMarkerPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearFailedNotice 清除升级失败回滚记录（提示一次后调用）。
func ClearFailedNotice() {
	if path, err := FailedMarkerPath(); err == nil {
		_ = os.Remove(path)
	}
}

// WritePending 落盘升级标记（更新程序退出后由替换脚本消费）。
func WritePending(p PendingUpgrade) error {
	path, err := MarkerPath()
	if err != nil {
		return err
	}
	if p.StartedAt == "" {
		p.StartedAt = time.Now().Format(time.RFC3339)
	}
	b, err := MarshalIndentCompat(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadPending 读取升级标记（不存在返回 nil）。
func ReadPending() *PendingUpgrade {
	path, err := MarkerPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p PendingUpgrade
	if err := UnmarshalCompat(b, &p); err != nil {
		return nil
	}
	return &p
}

// ClearPending 清除升级标记（替换完成后调用）。
func ClearPending() {
	if path, err := MarkerPath(); err == nil {
		_ = os.Remove(path)
	}
}

// StageUpgrade 升级第 1 步：把已下载的新版 exe 移到与当前 exe 同目录的暂存名
// （同盘 rename 原子；跨盘先复制）。返回暂存路径。
func StageUpgrade(downloaded string) (string, error) {
	if strings.TrimSpace(downloaded) == "" {
		return "", errors.New("未提供新版文件")
	}
	cur, err := ExePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(cur)
	staged := filepath.Join(dir, "update-staged.exe")
	// 清理残留暂存（上次升级中断）。
	_ = os.Remove(staged)
	if sameVolume(downloaded, dir) {
		if err := os.Rename(downloaded, staged); err != nil {
			return "", fmt.Errorf("暂存新版失败: %w", err)
		}
	} else {
		if err := copyFile(downloaded, staged); err != nil {
			return "", fmt.Errorf("复制新版失败: %w", err)
		}
		_ = os.Remove(downloaded)
	}
	return staged, nil
}

// PruneBackups 备份保留策略：_backup 目录只保留最近 keep 份（按修改时间），
// 其余删除（best effort，失败不阻断升级）。
func PruneBackups(dir string, keep int) {
	if keep <= 0 {
		keep = backupKeep
	}
	bkDir := filepath.Join(dir, backupDirName)
	entries, err := os.ReadDir(bkDir)
	if err != nil {
		return // 目录不存在 = 无备份可清理
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(bkDir, e.Name()), info.ModTime()})
	}
	// 新的在前。
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].mod.After(items[j-1].mod); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	for i := keep; i < len(items); i++ {
		_ = os.Remove(items[i].path)
	}
}

// ApplyAndRestart 升级第 2 步：写标记 → 启动替换脚本 → 退出当前进程。
// 替换脚本（Windows 批处理）流程（2026-10 发布事故加固版）：
//
//	等待发起进程退出 → 强停全部旧实例（exe 文件锁释放，多开场景必须）→
//	旧 exe 移入 _backup（时间戳文件名，不覆盖历史备份）→ 重试替换 →
//	启动新版 → 探活等待 → 存活则清标记自删；
//	启动失败（秒退/崩溃）→ 删除新 exe → 从备份还原旧版并重启 → 写失败标记。
//
// 本函数在启动脚本成功后返回（调用方随后退出进程）。
func ApplyAndRestart(p PendingUpgrade) error {
	if p.Staged == "" {
		return errors.New("升级标记缺少暂存文件")
	}
	if err := WritePending(p); err != nil {
		return fmt.Errorf("写入升级标记失败: %w", err)
	}
	cur, err := ExePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(cur)
	// 备份保留策略：超过上限删最旧（best effort）。
	PruneBackups(dir, backupKeep)
	// 替换脚本：不依赖 PowerShell，纯 cmd（deploy 同款流程的最小闭环）。
	script := filepath.Join(dir, "update.bat")
	exeName := filepath.Base(cur)
	bat := "@echo off\r\n" +
		"rem kaelvoris 自升级：等待退出→强停旧实例→备份→替换→启动探活→失败回滚→自删\r\n" +
		"set \"CUR=" + cur + "\"\r\n" +
		"set \"NEW=" + p.Staged + "\"\r\n" +
		"set \"MARKER=" + markerBatPath(dir) + "\"\r\n" +
		"set \"BKDIR=" + filepath.Join(dir, backupDirName) + "\"\r\n" +
		"set \"FAIL=" + filepath.Join(dir, failedMarker) + "\"\r\n" +
		"set \"EXE=" + exeName + "\"\r\n" +
		":waitself\r\n" +
		"tasklist /FI \"PID eq " + itoa(os.Getpid()) + "\" | find \"" + itoa(os.Getpid()) + "\" >nul\r\n" +
		"if not errorlevel 1 (\r\n" +
		"  ping -n 2 127.0.0.1 >nul\r\n" +
		"  goto waitself\r\n" +
		")\r\n" +
		"rem 多开实例一并结束，否则 exe 文件被锁无法替换\r\n" +
		"taskkill /F /IM \"%EXE%\" >nul 2>&1\r\n" +
		"ping -n 2 127.0.0.1 >nul\r\n" +
		"if not exist \"%BKDIR%\" mkdir \"%BKDIR%\"\r\n" +
		"set \"BK=%BKDIR%\\kaelvoris-desktop_%RANDOM%.exe\"\r\n" +
		"set /a BKTRIES=0\r\n" +
		"set /a MVTRIES=0\r\n" +
		"set /a RSTRIES=0\r\n" +
		":bk\r\n" +
		"move /y \"%CUR%\" \"%BK%\" >nul 2>&1\r\n" +
		"if not errorlevel 1 goto mv\r\n" +
		"set /a BKTRIES=BKTRIES+1\r\n" +
		"if %BKTRIES% geq 10 goto fail_in_place\r\n" +
		"ping -n 2 127.0.0.1 >nul\r\n" +
		"goto bk\r\n" +
		":mv\r\n" +
		"set /a MVTRIES=0\r\n" +
		":mv2\r\n" +
		"move /y \"%NEW%\" \"%CUR%\" >nul 2>&1\r\n" +
		"if not errorlevel 1 goto launch\r\n" +
		"set /a MVTRIES=MVTRIES+1\r\n" +
		"if %MVTRIES% geq 10 goto restore\r\n" +
		"ping -n 2 127.0.0.1 >nul\r\n" +
		"goto mv2\r\n" +
		":launch\r\n" +
		"start \"\" \"%CUR%\"\r\n" +
		"ping -n 7 127.0.0.1 >nul\r\n" +
		"tasklist /FI \"IMAGENAME eq %EXE%\" | find /I \"%EXE%\" >nul\r\n" +
		"if errorlevel 1 goto rollback\r\n" +
		"del /f /q \"%MARKER%\" >nul 2>&1\r\n" +
		"del /f /q \"%~f0\"\r\n" +
		"exit /b 0\r\n" +
		":rollback\r\n" +
		"rem 新版启动失败（秒退/崩溃）：删新包，从备份还原旧版并重启\r\n" +
		"del /f /q \"%CUR%\" >nul 2>&1\r\n" +
		"set /a RSTRIES=0\r\n" +
		":restore\r\n" +
		"move /y \"%BK%\" \"%CUR%\" >nul 2>&1\r\n" +
		"if not errorlevel 1 goto start_old\r\n" +
		"set /a RSTRIES=RSTRIES+1\r\n" +
		"if %RSTRIES% geq 10 goto restore_copy\r\n" +
		"ping -n 2 127.0.0.1 >nul\r\n" +
		"goto restore\r\n" +
		":restore_copy\r\n" +
		"rem move 连续失败（文件被 AV 等占用）降级 copy 兑底\r\n" +
		"copy /y \"%BK%\" \"%CUR%\" >nul 2>&1\r\n" +
		"if not exist \"%CUR%\" goto fail_write\r\n" +
		":start_old\r\n" +
		"start \"\" \"%CUR%\"\r\n" +
		"goto fail_write\r\n" +
		":fail_in_place\r\n" +
		"rem 旧版备份失败：旧 exe 未动，恢复启动旧版即可\r\n" +
		"start \"\" \"%CUR%\"\r\n" +
		"goto fail_write\r\n" +
		":fail_write\r\n" +
		"echo %date% %time% 自升级失败，已自动回滚到旧版 > \"%FAIL%\"\r\n" +
		"exit /b 1\r\n"
	if err := os.WriteFile(script, []byte(bat), 0o644); err != nil {
		return fmt.Errorf("写入升级脚本失败: %w", err)
	}
	slog.Info("执行自升级", "target", p.Target, "staged", p.Staged, "script", script)
	cmd := exec.Command("cmd", "/c", script)
	sysproc.Hide(cmd) // GUI 进程下隐藏升级批处理黑框（start 启动的新 exe 为 GUI 子系统不受影响）。
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动升级脚本失败: %w", err)
	}
	// 脚本已独立运行；主进程退出（批处理等待本 PID 消失后替换）。
	return nil
}

// markerBatPath 升级标记的批处理引用路径（直接字符串拼接避免转义问题）。
func markerBatPath(dir string) string {
	return filepath.Join(dir, pendingMarker)
}

// sameVolume 粗判两路径是否同盘（盘符一致；UNC 视为异盘走复制）。
func sameVolume(a, dirB string) bool {
	va := filepath.VolumeName(a)
	vb := filepath.VolumeName(filepath.Join(dirB, "x"))
	return va != "" && strings.EqualFold(va, vb)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return nil
}

// itoa 极简 int → string（避免为脚本拼一个 fmt 大依赖——这里 fmt 已被引入，
// 保留小工具语义清晰即可）。
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
