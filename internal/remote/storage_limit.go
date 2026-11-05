package remote

// storage_limit.go 服务端消息存储总量上限（P-Storage）：
//
// 服务端全部消息持久化产物只增不减（公网小盘服务器同机混部 mysql/pgsql/nginx，
// 磁盘曾 96% 满引发锁内 IO 死锁事故，见 hub.go 异步持久化注释），需硬上限兜底。
// 统计口径（全部为消息链路持久化产物）：
//
//	tasks.json           任务/会话记录（内存态无上限，落盘截 1000 条）
//	sessions.json        会话镜像消息（单会话 800 条上限，会话数无上限）
//	session_todos.json   会话 Todo 清单镜像
//	agent_msg_ids.json   上行去重表（JSONL 只追加，纯无界）
//	images/              聊天/绘画生成的图片（单图最大 32MB，只增不减）
//
// 超过上限（SetStoreLimit 配置，默认 512MB，main.go -store-limit 可调）后，
// 按「日期从旧到新」逐轮清理：
//   1. images/     最旧一天的生成图片整天删除（文件名时间戳与 ModTime 同为日期锚点，
//                  整天删避免同一任务/会话的图片被拆散）；
//   2. 会话记录    最旧一天的全部「终态」会话整体删除（复用 DeleteSession 全链路：
//                  任务记录 + 事件历史 + 会话镜像 + Todo 镜像，与移动端手动删除
//                  行为完全一致；运行中/待执行会话绝不强删，同终态保护语义）；
//   3. 去重表      仍超限时压缩 agent_msg_ids.json（保留最新一半 msgID；
//                  LRU 容量 4096 远超补发窗口，截断最旧一半安全）。
//
// 渐进收敛：每轮只清一个「最旧日期单位」，落盘重统计后复查；单次调用最多
// storePruneMaxRounds 轮（防一次性大规模删除断链服务），未清完的由下一轮
// 巡检（每小时，startStorePruner）继续。清理后广播 tasks/sessions 让在线端收敛。

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 默认与巡检参数。
const (
	// defaultStoreLimitBytes 消息存储总量默认上限 512MB。服务器磁盘 39G 且同机
	// 混部 mysql/pgsql/nginx/node 等服务（巡检实测），512MB 可容纳数月的会话
	// 记录 + 生成图片，同时为宿主机留足余量。
	defaultStoreLimitBytes = int64(512) << 20

	// storePruneInterval 存储巡检周期（首轮启动后 1 分钟，见 startStorePruner）。
	storePruneInterval = time.Hour

	// storePruneMaxRounds 单次 PruneStorage 最大清理轮数（每轮一个最旧日期单位）。
	// 超出则留给下轮巡检，防历史积压一次性删除断链服务。
	storePruneMaxRounds = 16

	// msgIDCompactMinLines 去重表压缩最小行数（低于此不压缩，防高频空转重写）。
	msgIDCompactMinLines = 256
)

// SetStoreLimit 配置消息存储总量上限（字节）。<=0 = 关闭自动清理。
// NewHub 默认启用 defaultStoreLimitBytes；宿主（main.go -store-limit）在创建后覆盖。
func (h *Hub) SetStoreLimit(maxBytes int64) { h.storeLimitBytes.Store(maxBytes) }

// storeUsage 消息存储各产物字节占用（磁盘实况）。
// 说明：统计取磁盘文件大小（落盘值），内存中未落盘的增量不计——清理每轮
// FlushPersistence 后重统计，渐进收敛对精度无要求。
type storeUsage struct {
	Tasks    int64 `json:"tasks"`    // tasks.json
	Sessions int64 `json:"sessions"` // sessions.json + session_todos.json
	MsgIDs   int64 `json:"msgIds"`   // agent_msg_ids.json（上行去重表）
	Images   int64 `json:"images"`   // images/ 目录
}

// Total 存储总量（字节）。
func (u storeUsage) Total() int64 { return u.Tasks + u.Sessions + u.MsgIDs + u.Images }

// imagesDir 服务端生成图片目录（与 main.go SetImageDir 装配路径一致）。
func (h *Hub) imagesDir() string { return filepath.Join(h.dir, "images") }

// storeUsage 统计消息存储占用（纯磁盘读，无锁；文件级粒度足够巡检判断）。
func (h *Hub) storeUsage() storeUsage {
	var u storeUsage
	u.Tasks = fileSizeOf(h.tasksFile())
	u.Sessions = fileSizeOf(filepath.Join(h.dir, "sessions.json")) +
		fileSizeOf(filepath.Join(h.dir, "session_todos.json"))
	u.MsgIDs = fileSizeOf(h.msgIDsFile())
	u.Images = dirSizeOf(h.imagesDir())
	return u
}

// PruneStorage 消息存储超限清理（P-Storage 入口）：总量超上限时按日期从旧到新
// 逐轮清理，直到回到上限内或无可删。返回释放的字节数。上限未启用（<=0）即返回 0。
// 触发点：启动首轮巡检 / 每小时巡检 / 图片上传成功后（异步，见 images.go）。
// 清理删除了会话时广播 tasks/sessions，全部在线端立即收敛（会话卡消失）。
func (h *Hub) PruneStorage() int64 {
	if h.dir == "" {
		return 0 // 纯内存模式：无持久化产物
	}
	limit := h.storeLimitBytes.Load()
	if limit <= 0 {
		return 0
	}
	// 互斥防并发清理（定时巡检与图片上传触发可能同时进入；清理动作幂等，
	// 加锁仅为避免重复统计/重复删除的浪费与日志噪音）。
	h.storePruneMu.Lock()
	defer h.storePruneMu.Unlock()

	var freed int64
	sessionsRemoved := false
	for round := 0; round < storePruneMaxRounds; round++ {
		before := h.storeUsage()
		if before.Total() <= limit {
			break
		}
		removedSessions := false
		if !h.pruneStorageStep(&removedSessions) {
			// 无可清理项（无图片/无可删终态会话/去重表不足阈值）：
			// 剩余占用全部为活动会话或不可删数据，保守不动，告警留观。
			slog.Warn("消息存储超限但无可清理项（活动会话不可强删）",
				"totalMB", before.Total()>>20, "limitMB", limit>>20)
			break
		}
		sessionsRemoved = sessionsRemoved || removedSessions
		// 落盘后重统计：会话删除走异步持久化，flush 让 tasks/sessions 文件真实缩小，
		// 下一轮统计与收敛判定才准确。
		h.FlushPersistence()
		after := h.storeUsage()
		if d := before.Total() - after.Total(); d > 0 {
			freed += d
		}
		if after.Total() <= limit {
			break
		}
	}
	if freed > 0 {
		after := h.storeUsage()
		slog.Info("消息存储超限清理完成（按日期从旧到新）",
			"freedMB", freed>>20, "remainMB", after.Total()>>20, "limitMB", limit>>20)
	}
	if sessionsRemoved {
		h.Broadcast(TypeTasks, h.ListTasks(100))
		h.Broadcast("sessions", map[string]interface{}{"sessions": h.ListSessionSummaries()})
	}
	return freed
}

// pruneStorageStep 执行一轮「最旧日期优先」清理；返回是否有清理动作。
// removedSessions 指示本轮是否删除了会话（调用方据以广播列表刷新）。
// 清理优先级即释放效率：生成图片（体积大头）→ 终态会话 → 去重表压缩。
func (h *Hub) pruneStorageStep(removedSessions *bool) bool {
	if h.pruneImagesOldestDay() {
		return true
	}
	if h.pruneOldestSessionDay() {
		*removedSessions = true
		return true
	}
	return h.compactMsgIDStore()
}

// pruneImagesOldestDay 删除最旧一天的生成图片（整天删除）。返回是否有删除。
func (h *Hub) pruneImagesOldestDay() bool {
	dir := h.imagesDir()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		return false
	}
	// ModTime 日期分组（文件名前缀同为服务端生成的时间戳，口径一致；
	// 用 ModTime 兜底兼容非命名规则的文件）。
	byDay := map[string][]string{}
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		day := info.ModTime().Format("2006-01-02")
		byDay[day] = append(byDay[day], filepath.Join(dir, ent.Name()))
	}
	if len(byDay) == 0 {
		return false
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	oldest := days[0]
	removed := 0
	for _, p := range byDay[oldest] {
		if err := os.Remove(p); err == nil {
			removed++
			h.DropFileMeta(filepath.Base(p)) // 联动清理归属：防残留记录被误作兼容窗口锚点
		}
	}
	if removed == 0 {
		return false
	}
	slog.Info("存储清理：删除最旧日期的生成图片", "day", oldest, "count", removed)
	return true
}

// pruneOldestSessionDay 删除最旧日期的终态会话（存储超限时的会话清理，
// 与 maxFinishedSessions 的会话数上限互补）：会话按「最近活跃日期」（其最新
// 任务 CreatedAt 所在天）升序分组，删首个存在可删会话的日期分组的全部可删
// 会话；无活动会话的日期为空时跳过继续下一天（日期升序逐日清理）。
// 返回是否有删除；removedSessions 置位供调用方广播。
func (h *Hub) pruneOldestSessionDay() bool {
	h.mu.RLock()
	list := h.taskList() // CreatedAt 倒序
	h.mu.RUnlock()
	// 会话 → 最近创建时间 + 是否活动（与 pruneFinishedSessions 同口径）。
	latest := map[string]int64{}
	active := map[string]bool{}
	for _, t := range list {
		if _, ok := latest[t.SessionID]; !ok {
			latest[t.SessionID] = t.CreatedAt
		}
		if t.Status == TaskStatusPending || t.Status == TaskStatusRunning {
			active[t.SessionID] = true
		}
	}
	type daySess struct {
		id string
		at int64
	}
	byDay := map[string][]daySess{}
	for sid, at := range latest {
		if active[sid] {
			continue
		}
		day := time.UnixMilli(at).Format("2006-01-02")
		byDay[day] = append(byDay[day], daySess{id: sid, at: at})
	}
	if len(byDay) == 0 {
		return false
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, day := range days {
		removed := 0
		for _, ds := range byDay[day] {
			if _, err := h.DeleteSession(ds.id); err != nil {
				continue // 竞态（删除前一刻复活）/部分失败：跳过
			}
			removed++
		}
		if removed > 0 {
			slog.Info("存储清理：删除最旧日期的会话记录", "day", day, "count", removed)
			return true
		}
	}
	return false
}

// compactMsgIDStore 压缩去重表（agent_msg_ids.json）：JSONL 只追加无界增长，
// 为唯一无日期结构的纯无界文件，超限时重写保留最新一半（行序即时间序）。
// 内存 LRU（seenRing）不回滚：淘汰节奏独立，截断只影响最旧 ID 的补发去重窗口
// （容量 4096 远超在途/补发量级，且受益载荷多数覆盖式幂等，见 msgIDCapacity）。
// 行数不足 msgIDCompactMinLines 不动（避免高频空转压缩小文件）。
func (h *Hub) compactMsgIDStore() bool {
	path := h.msgIDsFile()
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < msgIDCompactMinLines {
		return false
	}
	kept := lines[len(lines)/2:]
	seen := map[string]struct{}{}
	var buf strings.Builder
	for _, line := range kept {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	atomicWriteFile(path, []byte(buf.String()), 0o644)
	slog.Info("存储清理：压缩 msgID 去重表（保留最新一半）", "before", len(lines), "after", len(kept))
	return true
}

// fileSizeOf 文件字节数（不存在/不可读返回 0，不致命）。
func fileSizeOf(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// dirSizeOf 目录内全部普通文件字节总和（不递归；images/ 为平铺结构）。
func dirSizeOf(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		if info, err := ent.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// startStorePruner 启动存储定时巡检（NewHub 内调用一次；Stop 收口）。
// 首轮延迟 1 分钟：等宿主（main.go）完成 SetStoreLimit 装配——main.go 在
// NewHub 之后才以 -store-limit 覆盖默认上限，立即首轮会读到装配前的值。
func (h *Hub) startStorePruner() {
	stop := make(chan struct{})
	h.storePrunerStop = stop
	go func() {
		tick := time.NewTimer(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.PruneStorage()
				tick.Reset(storePruneInterval)
			}
		}
	}()
}
