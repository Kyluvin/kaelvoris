// ws_tasks.go —— 工作区面板「执行」链路 + 工作内容总结回写（用户需求 2026-12）。
//
// 执行语义（WSRunProject）：
//   1. 面板操作目录（Config.WSPanelDir）绑定为会话工作区；
//   2. 关联会话：扫描会话库找「工作区 = 该目录」的最近会话 → 复用（跳转语义）；
//      无则新建（时间戳 ID）；新会话读取该工作区一级目录 README.md（Markdown）
//      作为工作区说明随任务注入；
//   3. 以 SendMessageWithEmployee 同路启动任务（异步，事件经 cline:message 路由，
//      前端切到 chat 视图聚焦该会话即可看到执行流）；
//   4. 会话 hook（runTask OnTaskFinished / acpRunTask 同源路径）：任务结束后把
//      工作内容总结回写工作区根 README.md 的「项目工作日志」区块（追加式，最多
//      保留 10 条），仅对「工作区面板发起」的会话生效（wsLogSessions 登记）。
//
// 约束：回写失败静默（日志），不阻塞任务收尾；README 不存在时自动创建。
package desktop

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kaelvoris/internal/session"
)

// wsProjectLogBegin/End 回写区块标记（README.md 内的 HTML 注释包裹，渲染不可见）。
const (
	wsProjectLogBegin   = "<!-- WS_PROJECT_LOG:BEGIN -->"
	wsProjectLogEnd     = "<!-- WS_PROJECT_LOG:END -->"
	wsProjectLogTitle   = "## 项目工作日志"
	wsProjectLogMaxRows = 10 // 日志区块最多保留条数（新的在前，旧的滚动丢弃）
)

// wsLogSessions 工作区面板发起的会话登记表（sessionID → 工作区根）。
// 命中即任务结束后回写 README.md 工作日志。内存态：重启后未完成任务的
// 登记丢失 = 不再回写（可接受：未完成任务本就建议续跑重新执行）。
var (
	wsLogMu       sync.Mutex
	wsLogSessions = map[string]string{}
)

// wsRegisterLog 会话登记（面板「执行」入口调用）。
func wsRegisterLog(sessionID, workspace string) {
	wsLogMu.Lock()
	wsLogSessions[sessionID] = workspace
	wsLogMu.Unlock()
}

// wsWorkspaceOf 会话登记查询（hook 收尾时判定是否回写 + 取工作区）。
func wsWorkspaceOf(sessionID string) string {
	wsLogMu.Lock()
	defer wsLogMu.Unlock()
	return wsLogSessions[sessionID]
}

// wsSessionWorkspace 存量会话的工作区查询（复用会话判定）：
// 优先会话记录的 Workspace 字段；会话 ID 前缀 cron_/roadmap_ 等自动会话不参与复用。
func (a *App) wsSessionWorkspace(wsRoot string) string {
	wsRoot = strings.TrimRight(strings.TrimSpace(wsRoot), "\\/")
	if wsRoot == "" {
		return ""
	}
	list := a.store.List()
	// 按 UpdatedAt 倒序（List 已排序，这里再保证一次不依赖实现细节）。
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	for _, s := range list {
		if strings.HasPrefix(s.ID, "cron_") || strings.HasPrefix(s.ID, roadmapDevelopSessionIDPrefix) {
			continue // 自动会话不劫持执行入口
		}
		if s.ParentID != "" {
			continue // 子会话跟随父会话
		}
		sw := strings.TrimRight(strings.TrimSpace(s.Workspace), "\\/")
		if sw != "" && strings.EqualFold(sw, wsRoot) {
			return s.ID
		}
	}
	return ""
}

// wsReadReadme 读取工作区一级目录 README.md（大小写不敏感；不存在返回空串）。
// 用户需求：每个一级工作区有 README.md（Markdown），新建会话时读取并把
// 里面的工作区设置注入会话任务。
func wsReadReadme(wsRoot string) string {
	for _, name := range []string{"README.md", "readme.md", "Readme.md", "README.MD"} {
		b, err := os.ReadFile(filepath.Join(wsRoot, name))
		if err == nil {
			const max = 4000
			s := string(b)
			if r := []rune(s); len(r) > max {
				s = string(r[:max]) + "\n…（README 过长已截断）"
			}
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// WSSessionFor 面板「执行」的会话关联查询（前端执行前展示目标会话信息）。
// 返回 {sessionId, isNew, readmeFound}。
func (a *App) WSSessionFor() map[string]any {
	root := a.wsPanelDir()
	if root == "" {
		return map[string]any{"sessionId": "", "isNew": false, "readmeFound": false}
	}
	sid := a.wsSessionWorkspace(root)
	readme := wsReadReadme(root) != ""
	return map[string]any{"sessionId": sid, "isNew": sid == "", "readmeFound": readme}
}

// WSRunProject 面板「执行」入口：把面板目录绑定为会话工作区并启动任务。
//   - 复用该工作区最近会话（前端跳转到该会话即看到执行流）或新建会话；
//   - 新会话读 README.md 注入 [workspace-readme] 段（工作区设置随任务生效）；
//   - 登记 wsLogSessions：任务结束 hook 回写 README.md 工作日志。
//
// 复用 a.sendMessageOpts（排队/续接/员工路由全兼容），事件 sessionId 路由
// 前端现有渲染管线零改动。返回会话 ID 供前端聚焦。
func (a *App) WSRunProject(task string) (string, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return "", fmt.Errorf("任务内容不能为空")
	}
	root := a.wsPanelDir()
	if root == "" {
		return "", fmt.Errorf("请先在上方选择工作区文件夹")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("工作区目录不存在: %s", root)
	}

	// 关联会话：已有 → 复用；无 → 新建（时间戳 ID，runTask 落会话记录时写入工作区）。
	sid := a.wsSessionWorkspace(root)
	isNew := sid == ""
	if isNew {
		sid = fmt.Sprintf("%d", time.Now().UnixMilli())
	} else if a.IsSessionRunning(sid) {
		// 复用会话运行中：消息入队语义与 sendMessageOpts 一致（自动排队续发）。
	}

	// 新会话：README.md 工作区说明随任务注入（工作区设置进会话上下文）。
	prompt := task
	if readme := wsReadReadme(root); readme != "" {
		prompt = task + "\n\n[workspace-readme] 以下是本工作区根目录 README.md 内容（项目说明，执行时请遵循其中的约定）：\n\n" + readme
	}

	// 登记回写 hook（仅面板发起的会话）。
	wsRegisterLog(sid, root)

	// 把面板目录同步为会话工作区：新会话由 runTask 创建时写 Workspace=当前全局；
	// 这里显式落会话记录保证「面板目录 ≠ 顶栏项目」时也按面板目录执行。
	if isNew {
		now := time.Now()
		s := &session.Session{
			ID:        sid,
			ChannelID: "desktop",
			Title:     truncateTitle(task, 40),
			Workspace: root,
			CreatedAt: now,
			UpdatedAt: now,
		}
		a.store.Save(s)
		a.pushSessionsChanged()
	} else if s, err := a.store.Get(sid); err == nil && s != nil {
		if s.Snapshot().Workspace == "" {
			s.SetWorkspace(root)
			a.store.Save(s)
		}
	}

	// 启动任务（sendMessageOpts：普通人工审批桥；事件经 cline:message 路由）。
	// 运行中复用会话由 sendMessageOpts 自动入队。
	go func() {
		if _, err := a.sendMessageOpts(sid, prompt, nil, "", nil); err != nil {
			// 启动失败：清除登记（避免悬挂 hook）并推送错误事件。
			wsLogMu.Lock()
			delete(wsLogSessions, sid)
			wsLogMu.Unlock()
			slog.Warn("工作区执行启动失败", "session", sid, "err", err)
			a.emitEvent("kaelvoris:toast", "工作区执行失败："+err.Error())
		}
	}()
	return sid, nil
}

// maybeWSProjectLog 会话 hook 收尾路径：工作区面板会话任务结束后回写 README.md。
// 总结素材直接用引擎产出（result）+ 任务文本，不额外调 LLM（任务结束时的
// result 本身就是收尾摘要；会话总结 hook 已有面向用户的 TaskSummary，此处
// 只做「工作内容 → 项目 README」的落档回写）。
// 失败静默（日志），绝不阻塞任务收尾链。
func (a *App) maybeWSProjectLog(sessionID, userTask, result string, runErr error) {
	root := wsWorkspaceOf(sessionID)
	if root == "" {
		return
	}
	// 收尾后即可清除登记（一次任务一次回写；续跑轮再次经过 hook 会重复登记？
	// 不会——登记只在 WSRunProject 入口发生，续跑轮沿用同会话同工作区，
	// 重复回写反而符合「每轮任务一条日志」的日志语义，因此保留登记不清）。

	headline := strings.TrimSpace(result)
	if headline == "" {
		headline = "（无输出）"
	}
	status := "✅"
	if runErr != nil {
		status = "❌"
	}
	row := fmt.Sprintf("- %s %s · %s\n  - 任务：%s\n  - 结果：%s",
		status, time.Now().Format("2006-01-02 15:04"),
		truncateRunes(headline, 200),
		truncateRunes(oneLine(userTask), 200),
		truncateRunes(oneLine(headline), 400),
	)

	readmePath := ""
	for _, name := range []string{"README.md", "readme.md", "Readme.md", "README.MD"} {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			readmePath = p
			break
		}
	}
	if readmePath == "" {
		// README 不存在：自动创建（含区块骨架）。
		readmePath = filepath.Join(root, "README.md")
		if err := os.WriteFile(readmePath, []byte("# 工作区\n\n"+wsProjectLogBegin+"\n"+wsProjectLogEnd+"\n"), 0o644); err != nil {
			slog.Warn("工作区 README 创建失败（跳过回写）", "err", err)
			return
		}
	}
	b, err := os.ReadFile(readmePath)
	if err != nil {
		slog.Warn("工作区 README 读取失败（跳过回写）", "err", err)
		return
	}
	content := string(b)
	begin := strings.Index(content, wsProjectLogBegin)
	end := strings.Index(content, wsProjectLogEnd)
	var out string
	if begin >= 0 && end > begin {
		// 区块已存在：提取旧行，新行插最前，超过上限滚动丢弃旧行。
			block := content[begin+len(wsProjectLogBegin) : end]
			var oldRows []string
			for _, ln := range strings.Split(strings.TrimSpace(block), "\n") {
				ln = strings.TrimRight(ln, "\r")
				if strings.TrimSpace(ln) != "" && !strings.HasPrefix(strings.TrimSpace(ln), wsProjectLogTitle) {
					oldRows = append(oldRows, ln)
				}
			}
				// 行结构："- ✅ 时间 · 摘要" 为条目首行（无缩进），"  - 任务/结果"
				// 为从属行（带缩进）；重建时按条目分组（首行命中上限后连同
				// 从属行整体丢弃）。
				var entries []string
				var cur []string
				for _, ln := range oldRows {
					if strings.HasPrefix(ln, "- ") && len(cur) > 0 {
						entries = append(entries, strings.Join(cur, "\n"))
						cur = nil
					}
					cur = append(cur, ln)
				}
				if len(cur) > 0 {
					entries = append(entries, strings.Join(cur, "\n"))
				}
		entries = append([]string{row}, entries...)
		if len(entries) > wsProjectLogMaxRows {
			entries = entries[:wsProjectLogMaxRows]
		}
		block = "\n" + wsProjectLogTitle + "\n\n" + strings.Join(entries, "\n") + "\n"
		out = content[:begin+len(wsProjectLogBegin)] + block + content[end:]
	} else {
		// 区块不存在：追加到文末（空行分隔）。
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		out = content + "\n" + wsProjectLogBegin + "\n" + wsProjectLogTitle + "\n\n" + row + "\n" + wsProjectLogEnd + "\n"
	}
	if err := os.WriteFile(readmePath, []byte(out), 0o644); err != nil {
		slog.Warn("工作区 README 回写失败", "path", readmePath, "err", err)
		return
	}
	// 面板文件树刷新（README 变更外部发生，前端据此同步）。
	a.emitEvent("kaelvoris:ws_changed", map[string]string{"action": "write", "path": filepath.Base(readmePath)})
	slog.Info("工作区 README 已回写", "session", sessionID, "path", readmePath)
}
