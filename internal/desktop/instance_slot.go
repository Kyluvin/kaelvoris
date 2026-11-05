package desktop

// 实例槽位分配：Kaelvoris 桌面端支持多开（Wails 无单例锁），多个实例共用
// ~/.kaelvoris/remote.json 的同一节点令牌连接服务中心。若都按基础节点 ID 注册，
// 服务端会出现同节点反复顶替（后连者覆盖先连者）。本模块用「锁文件 + PID 存活
// 检测」为每个进程分配独立槽位：
//   - 锁目录 ~/.kaelvoris/instances/slot-N.lock，内容为本进程 PID；
//   - 启动时认领最低可用槽位：锁不存在，或锁内 PID 已消亡（崩溃残留）即可回收认领；
//   - 进程正常退出经 Wails OnShutdown（App.shutdown）释放；崩溃残留由后续实例回收。
// 远程连接器按槽位派生节点身份（deriveNodeIdentity）：槽位 1 沿用基础 ID/名称
// （兼容既有注册），槽位 N≥2 派生 "ID-N" / "名称 (N)"，服务端呈现为多个节点，
// 移动端可指定节点派发任务。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kaelvoris/internal/tools"
)

// maxInstanceSlots 槽位扫描上限，防止异常情况下无限占用。
const maxInstanceSlots = 16

// instanceSlotDir 返回槽位锁目录（~/.kaelvoris/instances），失败返回空串。
func instanceSlotDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kaelvoris", "instances")
}

// claimInstanceSlot 认领最低可用实例槽位，返回槽位号与释放函数（幂等，可安全多次调用）。
// 槽位锁为 slot-N.lock 文件，内容为本进程 PID；持有进程消亡后锁可被后续实例回收复用。
func claimInstanceSlot() (int, func(), error) {
	dir := instanceSlotDir()
	if dir == "" {
		return 0, nil, errors.New("无法定位用户主目录，无法分配实例槽位")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, nil, fmt.Errorf("创建槽位目录失败: %w", err)
	}
	pid := os.Getpid()
	pidStr := strconv.Itoa(pid)
	for slot := 1; slot <= maxInstanceSlots; slot++ {
		path := filepath.Join(dir, fmt.Sprintf("slot-%d.lock", slot))
		if data, err := os.ReadFile(path); err == nil {
			stalePID := parseSlotPID(string(data))
			if stalePID == pid || !pidAlive(stalePID) {
				// 锁残留自本 PID 的历史进程（PID 已复用）或持有进程已消亡 → 回收重认领。
				if stalePID > 0 && stalePID != pid {
					slog.Info("回收失效实例槽位", "slot", slot, "stalePid", stalePID)
				}
				_ = os.Remove(path)
			} else {
				continue // 槽位被存活实例占用
			}
		}
		// O_EXCL 独占创建：多实例并发认领同一槽位时仅一个成功，其余继续扫描下一槽位。
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		if _, err := f.WriteString(pidStr + "\n"); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			continue
		}
		_ = f.Close()
		release := func() {
			// 仅当锁内容仍是本进程 PID 时删除，避免误删后继实例已重新认领的锁。
			if data, err := os.ReadFile(path); err == nil && parseSlotPID(string(data)) == pid {
				_ = os.Remove(path)
			}
		}
		return slot, release, nil
	}
	return 0, nil, fmt.Errorf("实例槽位已满（上限 %d）", maxInstanceSlots)
}

// parseSlotPID 解析槽位锁文件中的 PID；内容非法返回 0（视为可回收）。
func parseSlotPID(s string) int {
	pid, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// deriveNodeIdentity 按槽位派生远程节点身份。
// 多开命名规约（2026-10 用户要求）：同机多窗口在手机端以「名称-N」区分展示，
// 槽位 1 也带 -1 后缀；ID 侧槽位 1 保持基础形态（兼容历史会话），槽位 N≥2
// 派生为 "ID-N"，使同令牌多实例在服务端呈现为多个节点。
func deriveNodeIdentity(baseID, baseName string, slot int) (string, string) {
	if baseID == "" {
		return baseID, baseName // 未配置远程时无身份可派生
	}
	if slot <= 1 {
		if baseName == "" {
			return baseID, "1" // 空基础名：回退纯序号
		}
		return baseID, baseName + "-1"
	}
	if baseName == "" {
		return fmt.Sprintf("%s-%d", baseID, slot), fmt.Sprintf("%d", slot)
	}
	return fmt.Sprintf("%s-%d", baseID, slot), fmt.Sprintf("%s-%d", baseName, slot)
}

// acquireSlot 为本进程认领实例槽位（幂等），认领结果供远程连接器派生节点身份。
func (a *App) acquireSlot() {
	a.remoteMu.Lock()
	if a.slotRelease != nil {
		a.remoteMu.Unlock()
		return
	}
	a.remoteMu.Unlock()

	slot, release, err := claimInstanceSlot()
	if err != nil {
		slog.Warn("实例槽位分配失败，降级为基础节点身份", "err", err)
		return
	}

	a.remoteMu.Lock()
	if a.slotRelease != nil { // shutdown 已并发释放，让出刚认领的槽位
		a.remoteMu.Unlock()
		release()
		return
	}
	a.instanceSlot = slot
	a.slotRelease = release
	a.remoteMu.Unlock()
	slog.Info("实例槽位已分配", "slot", slot, "pid", os.Getpid())
}

// releaseSlot 释放本进程实例槽位（幂等）。
func (a *App) releaseSlot() {
	a.remoteMu.Lock()
	release := a.slotRelease
	slot := a.instanceSlot
	a.slotRelease = nil
	a.instanceSlot = 0
	a.remoteMu.Unlock()
	if release != nil {
		release()
		slog.Info("实例槽位已释放", "slot", slot)
	}
}

// Shutdown 应用退出钩子（Wails OnShutdown）：断开远程连接器（服务端节点下线）并释放实例槽位；
// 停止自主迭代自动循环（中断的开发条目下次启动恢复续跑，见 roadmapRecoverOrphans）。
// （导出：根包 main.go 经 OnShutdown 引用；internal/desktop 迁包后需跨包可见）
func (a *App) Shutdown(ctx context.Context) {
	if a.roadmapStopCh != nil {
		select {
		case <-a.roadmapStopCh:
		default:
			close(a.roadmapStopCh)
		}
	}
	a.stopRemoteClient()
	a.stopWebhookServer()
	shutdownEmbeddedBrowser() // 内嵌浏览器窗口随应用退出销毁（X 关闭时已为空，幂等）
	tools.CloseBrowser()
	a.releaseSlot()
}
