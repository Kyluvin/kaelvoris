package desktop

// channel_assign.go 四渠道（lark / wecom / wecom_ws / im）共用收口辅助
// （doc/09-设计方案/岗位分配与双区权限设计方案.md Phase 2-①，§4.1 渠道同一收口）：
//
//	新会话创建处（各渠道 run*Task 建会话的同一位置）→ ResolveAssignment →
//	会话快照 EmployeeID + PositionID + AssignRule → taskRun.employeeID →
//	buildRunEngine 员工内核。下游链路零改动，只动会话创建入口（§4.1）。
//
// 并发约定（§2.1 / §10）：round_robin 游标回写与面板 SaveChannels 保存的并发
// 以「进程内互斥（channelsFileMutex）+ tmp+rename 原子替换」序列化；渠道
// goroutine 与保存入口竞争同一 channels.json，丢失更新窗口收敛为「锁内读改
// 写一次完成」，无独立游标存储（§2.1 ponytail：游标落盘即可，重启丢内存态可接受）。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"kaelvoris/internal/employee"
	"kaelvoris/internal/session"
	"kaelvoris/pkg/message"
)

// channelsFileMutex 渠道配置文件互斥（进程内；游标回写 vs SaveChannels 保存
// 竞争同一 channels.json，锁住读-改-写全程防丢更新。跨进程由 os.Rename 原子
// 替换兜底，最坏丢一次游标推进，轮转公平性不受结构性破坏——§2.1 容忍语义）。
var channelsFileMutex sync.Mutex

// readChannelsFile / writeChannelsFile channels.json 整表读写（原子替换形态
// 对齐 employee.Store.saveLocked 的 tmp+rename；调用方须持 channelsFileMutex）。
func readChannelsFile() ([]ChannelConfig, error) {
	b, err := os.ReadFile(channelsConfigPath())
	if err != nil {
		return nil, err
	}
	var out []ChannelConfig
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeChannelsFile(list []ChannelConfig) error {
	if err := os.MkdirAll(filepath.Dir(channelsConfigPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := channelsConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, channelsConfigPath())
}

// channelAssignResult 收口解析结果（四渠道建会话共用）。
type channelAssignResult struct {
	employeeID string // 解析出的员工（空 = 裸跑，走普通会话内核）
	positionID string // 岗位快照（随会话固化，§6.1）
	assignRule string // 分配规则快照（空 = random，§2.1）
	degraded   bool   // 降级发生（§2-②，须留痕）
	notice     string // 会话可见提示（降级留痕；空 = 无）
	errMsg     string // 解析报错原文（ok=false 时非空；触发方回执用，§2 池空语义）
	poolSize   int    // 池规模（分支②③选取时记录；降级留痕文案与测试断言用）
}

// resolveChannelAssignment 渠道建会话收口唯一入口（§4.1）：
//
//  1. 从员工 Store 取快照（调用方传入，锁在 Store 内部 List()）；
//  2. ResolveAssignment 四分支解析（固定/降级/岗位池/裸跑）；
//  3. 裸跑（分支④，绑定全空）返回零值结果，调用方保持存量行为——启用渠道
//     在 SaveChannels 侧已被拒（§4.4），运行侧不重复拦截；
//  4. round_robin 游标回写 ChannelConfig.LastAssignedIndex 并原子落盘（§2.1）。
//
// 返回值 ok=false 仅在解析报错（绑定员工不存在/池空等）时出现，调用方把
// 错误回给触发方并终止本轮任务（§2 池空语义：不静默降级）。
func (ch *LarkChannel) resolveChannelAssignment() (channelAssignResult, bool) {
	return resolveAssignmentForChannel(ch.app, &ch.cfg, "飞书")
}

// resolveChannelAssignment WeComChannel 收口（应用消息 + 智能机器人 WS 共用；
// wecom_ws 路径最终进入 runWecomTask，无需独立解析器）。
func (ch *WeComChannel) resolveChannelAssignment() (channelAssignResult, bool) {
	return resolveAssignmentForChannel(ch.app, &ch.cfg, "企业微信")
}

// resolveChannelAssignment IMChannel 收口（公共号/群消息/中继路径共用）。
func (ch *IMChannel) resolveChannelAssignment() (channelAssignResult, bool) {
	return resolveAssignmentForChannel(ch.app, &ch.cfg, "IM")
}

// resolveAssignmentForChannel 渠道无关的解析实现（四渠道结构体字段同名，
// app + cfg 泛化为入参；kind 仅用于日志与提示文案）。四渠道 cfg 均为
// ChannelConfig 值拷贝（startXxxChannel 传入），游标回写需整表定位：按
// Type+Name 匹配（与 ListChannelStatus 渠道定位同粒度），避免按值拷贝
// 直接写回的别名问题。
func resolveAssignmentForChannel(a *App, cfg *ChannelConfig, channelKind string) (channelAssignResult, bool) {
	var res channelAssignResult

	// 分支④：绑定全空 = 裸跑（存量语义；保存侧已拦启用渠道，运行侧不拦截）。
	empID := cfg.EmployeeID
	posID := cfg.PositionID
	if empID == "" && posID == "" {
		return res, true
	}

	if a == nil || a.empStore == nil {
		// 无员工存储（极端降级：测试/未初始化）——无法解析，保持裸跑不阻断。
		slog.Warn("岗位分配解析跳过（员工存储不可用）", "channel", channelKind, "name", cfg.Name)
		return res, true
	}

	employees := a.empStore.List() // Store 内部持锁快照
	binding := employee.Assignment{
		EmployeeID: empID,
		PositionID: posID,
		AssignRule: cfg.AssignRule,
		LastIndex:  cfg.LastAssignedIndex,
		Load:       a.employeeLoad, // least_loaded 运行计数（策略层按需调用）
	}
	resolved, nextIndex, err := employee.ResolveAssignment(employees, binding)
	if err != nil {
		res.errMsg = err.Error()
		slog.Warn("渠道岗位分配解析失败", "channel", channelKind, "name", cfg.Name, "err", err)
		return res, false
	}

	// 游标回写：仅 round_robin 且发生池分配（分支②③）时推进；固定员工（分支①）
	// 与 random 恒返回 0，无需落盘。按 §2.1 进程内互斥 + tmp+rename 原子落盘。
	if cfg.AssignRule == employee.AssignRoundRobin && nextIndex > 0 {
		writeChannelCursor(cfg, nextIndex)
	}
	_ = nextIndex // least_loaded/random 等策略恒 0：保持调用点语义不变

	res.employeeID = resolved.EmployeeID
	res.degraded = resolved.Degraded
	res.poolSize = resolved.PoolSize
	// 岗位快照：解析员工当前岗位；绑定岗位存在时优先绑定值（§3 快照语义：
	// 保存时已回填，此处兜底空值场景）。规则快照随会话固化（空 = random）。
	res.positionID = posID
	if res.positionID == "" {
		res.positionID = resolved.Employee.PositionID
	}
	res.assignRule = cfg.AssignRule
	if res.degraded {
		// §10 降级留痕：会话可见提示（say text 落历史）+ 日志。
		res.notice = fmt.Sprintf("⚠️ 绑定的数字员工不可运行，已按分配规则（%s）从岗位池降级选取「%s」（池规模 %d）。",
			ruleLabel(cfg.AssignRule), resolved.Employee.Name, resolved.PoolSize)
		slog.Warn("渠道员工降级选取", "channel", channelKind, "name", cfg.Name,
			"from", empID, "to", resolved.EmployeeID, "pool", resolved.PoolSize)
	}
	return res, true
}

// writeChannelCursor round_robin 游标回写（§2.1）：channels.json 整表读出 →
// 定位本渠道 → 更新 LastAssignedIndex → 原子落盘（tmp+rename，对齐 Store 原子写）。
// 进程内互斥防与 SaveChannels 并发丢更新；文件级失败仅记日志（游标丢失可接受，
// 不阻断本次任务执行）。
func writeChannelCursor(cfg *ChannelConfig, nextIndex int) {
	channelsFileMutex.Lock()
	defer channelsFileMutex.Unlock()
	list, err := readChannelsFile()
	if err != nil {
		slog.Warn("轮转游标回写失败（读取）", "name", cfg.Name, "err", err)
		return
	}
	matched := false
	for i := range list {
		if channelSameIdentity(&list[i], cfg) {
			list[i].LastAssignedIndex = nextIndex
			matched = true
			break
		}
	}
	if !matched {
		// 配置已被面板并发改写（渠道指纹未命中）：放弃本次回写，游标下次触发
		// 从新配置读取（丢一次推进可接受，§2.1 ponytail 约定）。
		slog.Warn("轮转游标回写未匹配渠道（配置可能已变更）", "name", cfg.Name, "type", cfg.Type)
		return
	}
	if err := writeChannelsFile(list); err != nil {
		slog.Warn("轮转游标回写失败（落盘）", "name", cfg.Name, "err", err)
	}
}

// channelSameIdentity 渠道身份判定（Type+Name；渠道名在 SaveChannels 校验中
// 必填且唯一性由前端列表编辑保障，同名渠道共享游标属可接受边界——与
// ListChannelStatus 按 Name 匹配的既有语义一致）。
func channelSameIdentity(a, b *ChannelConfig) bool {
	return a.Type == b.Type && a.Name == b.Name
}

// employeeLoad 员工运行中任务计数（least_loaded 分配策略注入点，channel_assign.go）：
// 复用 taskRun 表（runs 的 key = 会话 ID），run.employeeID 即该会话绑定的员工；
// 仅统计真正在跑的会话（registry.HasRunning 排除已入队未启动者）。计数不需
// 精确快照语义（策略选取容忍微小竞态），锁内拷贝 ID 列表即可。
func (a *App) employeeLoad(employeeID string) int {
	a.runsMu.Lock()
	ids := make([]string, 0, len(a.runs))
	empBySession := make(map[string]string, len(a.runs))
	for id, run := range a.runs {
		ids = append(ids, id)
		empBySession[id] = run.employeeID
	}
	a.runsMu.Unlock()
	n := 0
	for _, id := range ids {
		if empBySession[id] != employeeID {
			continue
		}
		if a.registry.HasRunning(id) {
			n++
		}
	}
	return n
}

// ruleLabel 分配规则展示名（降级留痕文案用；空 = random 默认，§2.1）。
func ruleLabel(rule string) string {
	switch rule {
	case employee.AssignRoundRobin:
		return "轮转 round_robin"
	case employee.AssignLeastLoaded:
		return "负载最低 least_loaded"
	default:
		return "随机 random"
	}
}

// emitAssignNotice 降级留痕的会话可见提示（§10）：以 say text 定型消息写入
// 会话历史（AppendMerged 持久化 + 前端实时推送），不入引擎上下文（仅留痕，
// 不改变任务输入）；渠道侧另行把 notice 回给触发方（回执可见）。
func emitAssignNotice(a *App, sess *session.Session, run *taskRun, sessionID string, notice string) {
	if notice == "" || sess == nil {
		return
	}
	m := message.NewSay(message.SayText, notice)
	run.histMu.Lock()
	run.hist = append(run.hist, m)
	run.histMu.Unlock()
	sess.AppendMerged(m)
	a.store.Save(sess)
	// 前端实时推送（对齐 makeSayBridge 的定型事件通道）。
	uiEmit(a.ctx, "cline:message", map[string]any{"sessionId": sessionID, "message": m})
}

// applyChannelAssignSnapshot 四渠道建会话快照写入（§4.1/§6.1）：
//
//	sess.SetEmployeeID(res.employeeID)
//	sess.SetPositionSnapshot(res.positionID, res.assignRule)
//
// 固化进会话（生命周期内不重选，§2）；旧会话（复用）不重写快照（固化语义：
// 首次绑定固定生效，换绑 = 修改资源项配置 + 删除旧会话，§2 池空与固化语义）。
func applyChannelAssignSnapshot(sess *session.Session, res channelAssignResult) {
	if res.employeeID != "" {
		sess.SetEmployeeID(res.employeeID)
	}
	sess.SetPositionSnapshot(res.positionID, res.assignRule)
}
