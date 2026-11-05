// ═════════════════════════════════════════════════════════════════
// 浏览器预览 mock 桩：window.go 不存在时注入假 Wails 绑定。
// 真实桌面端（window.go 已由 Wails 注入）完全跳过本文件逻辑。
// 用法：npm run dev 后浏览器打开 http://localhost:5175 预览 UI。
// ═════════════════════════════════════════════════════════════════

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 假任务流：模拟一次任务从用户输入到完成的全过程（say/reasoning/tool/done）
const DEMO_STEPS = [
  { type: "say", say: "user_feedback", text: "__TASK__" },
  { type: "say", say: "reasoning", text: "先了解项目结构，再总结要点。", partial: true },
  { type: "say", say: "tool", text: "fs_list | 列出根目录" },
  { type: "say", say: "text", partial: true, text: "## 项目结构\n\n- `main.go` — 程序入口\n- `internal/` — 业务内核\n\n**结论**：结构清晰，可以开始任务。" },
  { type: "say", say: "api_req_started", text: "" },
  { type: "say", say: "api_req_finished", text: JSON.stringify({ prompt_tokens: 1832, completion_tokens: 256, total_tokens: 2088 }), ts: Date.now() * 1e6 },
];

const mockSessions = [];
// 数字员工 mock 数据（模块级可变；SaveEmployee 整存整取写回，供浏览器 mock 预览验证保存→刷新链路）
let mockEmployees = [
  { id: "emp-lark", name: "小飞·客服", avatar: "🛡️", runnable: true, status: "enabled", empStatus: "active", empNo: "E1001", deptId: "dep-rd", deptName: "研发部", positionName: "后端工程师", positionId: "pos-dev", roleIds: ["role-dev"] },
  { id: "emp-wecom", name: "小企·研发", avatar: "🛠️", runnable: true, status: "enabled", empStatus: "probation", empNo: "E1002", deptName: "研发部", managerName: "小飞·客服", positionId: "pos-dev", roleIds: ["role-dev", "role-ro"] },
  { id: "emp-off", name: "小初·待启", avatar: "🌱", runnable: false, status: "draft", positionId: "pos-dev" },
];
const runningSet = new Set();
const sessionMsgs = new Map();
const sessionTodos = new Map();
let mockMode = "code";
let seq = 0;

// 预置一条带 new_task 子会话的示例（侧栏折叠树预览：父会话下拉箭头 + 子任务缩进）。
	// 会话卡片信息升级（规划板 2026-10）：mock 会话附带 lastMessage/hasError/pinnedAt，
	// 覆盖分组（今天/昨天/更早）、摘要、错误态与置顶的浏览器预览形态。
	(function seedDemoTree() {
	  const now = Date.now();
	  const parent = "mock-parent";
		  mockSessions.push(
			    { id: parent, title: "重构登录模块并补齐测试", mode: "orchestrator", workspace: "D:/code/gocoder", lastMessage: "登录模块改造完成：拆解 5 处改动点，测试全绿", done: true, createdAt: new Date(now - 3600e3).toISOString(), updatedAt: new Date(now - 60e3).toISOString(), messageCount: 12 },
	    { id: parent + "-s1", parentId: parent, title: "↳ 拆解登录模块改动点", mode: "architect", lastMessage: "改动点清单已产出，待实现", createdAt: new Date(now - 3500e3).toISOString(), updatedAt: new Date(now - 3000e3).toISOString(), messageCount: 8 },
	    { id: parent + "-s2", parentId: parent, title: "↳ 实现 token 刷新逻辑", mode: "code", lastMessage: "token 刷新逻辑已实现，单测通过", hasError: true, createdAt: new Date(now - 2900e3).toISOString(), updatedAt: new Date(now - 1200e3).toISOString(), messageCount: 21 },
	  );
	})();
	// 更早分组示例（昨天/更早边界预览）
	mockSessions.push(
	  { id: "mock-earlier", title: "昨天归档的会话", lastMessage: "已归档到知识库", createdAt: new Date(Date.now() - 26e7).toISOString(), updatedAt: new Date(Date.now() - 26e7).toISOString(), messageCount: 3, pinnedAt: Date.now() },
	);

function listSessions() {
	  // 置顶优先排序（对齐后端 Store.List 语义；前端分组前先排好）
	  const list = mockSessions.map((s) => ({ ...s, running: runningSet.has(s.id) }));
	  list.sort((a, b) => {
	    const pa = a.pinnedAt > 0, pb = b.pinnedAt > 0;
	    if (pa !== pb) return pa ? -1 : 1;
	    if (pa && pb) return b.pinnedAt - a.pinnedAt;
	    return new Date(b.updatedAt) - new Date(a.updatedAt);
	  });
	  return list;
}

// 简易事件总线（模拟 Wails runtime.EventsOn）
function on(name, cb) {
  (window.__kaelvoris_listeners[name] = window.__kaelvoris_listeners[name] || []).push(cb);
  // 返回取消订阅函数（对齐真实 Wails EventsOn 语义）：组件 onBeforeUnmount 调用
  // off()，缺失会导致面板切换时 "offXxx is not a function" 未捕获异常。
  return () => {
    const list = window.__kaelvoris_listeners[name] || [];
    const i = list.indexOf(cb);
    if (i >= 0) list.splice(i, 1);
  };
}
window.__emit = (name, data) => {
  for (const fn of window.__kaelvoris_listeners[name] || []) fn(data);
};

function emitCline(sid, m) {
  window.__emit("cline:message", { sessionId: sid, message: { ts: Date.now() * 1e6, ...m } });
}

// 模拟一次任务执行（流式推进）
function fakeRun(sid, text) {
  // user_feedback 气泡由 store.sendCurrent 本地乐观追加，mock 不重复发
  let i = 1;
  const step = () => {
    if (i >= DEMO_STEPS.length) {
      window.__emit("kaelvoris:done", { sessionId: sid });
      window.__emit("kaelvoris:status", { sessionId: sid, running: false });
      runningSet.delete(sid);
      window.__emit("kaelvoris:sessions", listSessions());
      return;
    }
    const m = DEMO_STEPS[i];
    emitCline(sid, m);
    if (m.partial && m.say === "text") {
      setTimeout(() => { emitCline(sid, { ...m, partial: false }); i += 1; setTimeout(step, 700); }, 600);
      return;
    }
    i += 1;
    setTimeout(step, 800);
  };
  setTimeout(step, 400);
}

function installMock() {
  if (window.go && window.go.desktop && window.go.desktop.App) return; // 桌面端不装桩

  window.__kaelvoris_listeners = {};

  const App = {
    // ── 会话 ──
    async ListSessions() { return listSessions(); },
	    async OpenSession(id) {
	      const msgs = sessionMsgs.get(id) || [];
	      return { id, workspace: "D:/code/gocoder", model: "", mode: "", messages: msgs.map((m, i) => ({ ts: (i + 1) * 1e9, ...m })) };
	    },
	    // 会话自有配置（2026-11）：节点桩，空实现即可（mock 无运行态）
	    async SetSessionNodeID() {},
	    async SetSessionEmployee() {},
	    async SetSessionACP() {},
    async SendMessageWithEmployee(sid, text) {
      await sleep(120);
      if (!sid) {
        sid = "mock-" + Date.now();
        seq += 1;
	        mockSessions.unshift({
	          id: sid, title: text.slice(0, 24) || "新会话 " + seq,
	          lastMessage: text, // 卡片摘要同步（规划板 2026-10）
	          createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(), messageCount: 0,
	        });
        sessionMsgs.set(sid, []);
        sessionTodos.set(sid, [
          { content: "理解任务需求", status: "completed" },
          { content: "梳理实现方案", status: "in_progress" },
          { content: "交付并验证", status: "pending" },
        ]);
      }
      sessionMsgs.get(sid).push({ type: "say", say: "user_feedback", text });
      runningSet.add(sid);
      window.__emit("kaelvoris:sessions", listSessions());
      window.__emit("kaelvoris:status", { sessionId: sid, running: true });
      window.__emit("kaelvoris:todos", { sessionId: sid, items: sessionTodos.get(sid) || [] });
      fakeRun(sid, text);
      return sid;
    },
    async ApproveAsk() {},
    async GetMessages(sid) {
      return (sessionMsgs.get(sid) || []).map((m, i) => ({ ts: (i + 1) * 1e9, ...m }));
    },
    async SendMessage(sid, task, images) {
      // 与 SendMessageWithEmployee 同语义（mock 无普通/员工通道差异）
      return this.SendMessageWithEmployee(sid, task, images || [], "");
    },
	    async RenameSession(id, title) {
	      const s = mockSessions.find((x) => x.id === id);
	      if (s) s.title = title;
	    },
	    // 会话卡片信息升级（规划板 2026-10）：置顶/摘要 mock 桩（浏览器预览）
	    async SetSessionPinned(id, pinned) {
	      const s = mockSessions.find((x) => x.id === id);
	      if (s) s.pinnedAt = pinned ? Date.now() : 0;
	      window.__emit("kaelvoris:sessions", listSessions());
	    },
	    async SetSessionManualPreview(id, preview) {
	      const s = mockSessions.find((x) => x.id === id);
	      if (s) s.lastMessage = (preview || "").trim();
	      window.__emit("kaelvoris:sessions", listSessions());
	    },
    async DeleteSession(id) {
      const i = mockSessions.findIndex((x) => x.id === id);
      if (i >= 0) mockSessions.splice(i, 1);
      sessionMsgs.delete(id);
      sessionTodos.delete(id);
      runningSet.delete(id);
      window.__emit("kaelvoris:sessions", listSessions());
    },
    async CleanupSession(sid) {
      return { sessionId: sid, title: "（mock）会话", ok: true, skipped: false, reason: "", docPath: "docs/mock-cleanup.md", analysis: "（mock）已归档。" };
    },
    async CleanupSessions() {
      return { total: 0, deleted: 0, skipped: 0, reports: [] };
    },
    async ClearAllSessions() {
      const n = mockSessions.length;
      mockSessions.splice(0, mockSessions.length);
      for (const k of [...sessionMsgs.keys()]) sessionMsgs.delete(k);
      for (const k of [...sessionTodos.keys()]) sessionTodos.delete(k);
      for (const id of [...runningSet]) runningSet.delete(id);
      window.__emit("kaelvoris:sessions", listSessions());
      return n;
    },
    async SummarizeClearSessions() {
      // mock：异步模拟（真实后端为 LLM 汇总 + 清空 + 续作会话，经 kaelvoris:cleanup-done 通知）
      // 对齐后端：完成时清空数据并广播会话列表，再发 cleanup-done（复位忙碌态）
      setTimeout(() => {
        const n = mockSessions.length;
        mockSessions.splice(0, mockSessions.length);
        for (const k of [...sessionMsgs.keys()]) sessionMsgs.delete(k);
        for (const k of [...sessionTodos.keys()]) sessionTodos.delete(k);
        for (const id of [...runningSet]) runningSet.delete(id);
        window.__emit("kaelvoris:sessions", listSessions());
        window.__emit("kaelvoris:cleanup-done", { ok: true, deleted: n, summarized: true });
      }, 600);
      return;
    },
    async ListCleanupCandidates() { return []; },
    async Stop(sid) {
      runningSet.delete(sid);
      window.__emit("kaelvoris:status", { sessionId: sid, running: false });
      window.__emit("kaelvoris:sessions", listSessions());
    },
    async StopAll() {
      for (const id of [...runningSet]) {
        runningSet.delete(id);
        window.__emit("kaelvoris:status", { sessionId: id, running: false });
      }
      window.__emit("kaelvoris:sessions", listSessions());
    },
    async IsRunning() { return runningSet.size > 0; },
    async IsSessionRunning(id) { return runningSet.has(id); },
    async RunningSessions() { return [...runningSet]; },

    async SetEditorPreference() {},

    // ── 模式 / 模型 ──
    async GetMode() { return mockMode; },
    async SetMode(m) { mockMode = m; },
    async ListModes() {
      return [
        { Name: "architect", Description: "规划与设计", Icon: "🏗️" },
        { Name: "code", Description: "写代码改代码", Icon: "💻" },
        { Name: "ask", Description: "答疑不改动", Icon: "❓" },
        { Name: "debug", Description: "排查问题", Icon: "🪲" },
        { Name: "orchestrator", Description: "拆解与编排", Icon: "🪃" },
      ];
    },
	    async ListModels() { return [{ Id: "gpt-4o" }, { Id: "deepseek-chat" }]; },
	    async SetModel() {},
	    async SetSessionModel() {},
	    async SetSessionMode() {},

    // ── 待办 ──
    async GetTodos(sid) { return sessionTodos.get(sid) || []; },

    // ── 工作区文件补全（composer_bridge.go）──
    async ListWorkspaceFiles() {
      return ["main.go", "app.go", "flows.go", "go.mod", "doc/roadmap.md", "frontend-vue/src/App.vue"];
    },

    // ── 项目 ──
    async ListProjects() {
      return [
        { path: "D:/code/gocoder", current: true },
        { path: "D:/code/frp-deploy", current: false },
      ];
    },
    async PickWorkspace() { return "D:/code/gocoder"; },
    async SwitchProject() {},
    async OpenProject() { return "D:/code/gocoder"; },
	    async SetSessionWorkspace(id, dir) {
	      const s = mockSessions.find((x) => x.id === id);
	      if (s) s.workspace = dir;
	    },
	    async SetSessionMCPDisabled(id, disabled) {
	      const s = mockSessions.find((x) => x.id === id);
	      if (s) s.mcpDisabled = disabled;
	    },
	    async OpenWorkspaceInExplorer() {},
	    async OpenProjectInEditor() {},
	    async OpenFileAtLine() {},          // mock：浏览器预览无文件打开能力
	    async OpenFileWithDefault() {},
	    async SaveFileAs() {},

    // ── Git ──
    async GitStatus() {
      return {
        branch: "main", isRepo: true, ahead: 1, behind: 0,
        staged: [{ Path: "app.go", Status: "M", Insertions: 12, Deletions: 3 }],
        unstaged: [
          { Path: "main.go", Status: "M", Insertions: 4, Deletions: 2 },
          { Path: "frontend-vue/src/App.vue", Status: "A", Insertions: 128, Deletions: 0 },
        ],
      };
    },
    async GitFileDiff() {
      return "@@ -12,6 +12,10 @@ func main() {\n-  旧实现逻辑\n+  新实现：Arco Design 令牌体系\n+  新增：三栏布局壳\n+  新增：消息流虚拟化";
    },
    async GitLog(n) {
      return Array.from({ length: Math.min(n, 6) }, (_, i) => ({
        Hash: "a1b2c3d" + i, Message: "feat: commit " + i, Author: "kaelvoris",
        Date: new Date(Date.now() - i * 86400e3).toISOString(),
      }));
    },
    async GitStage() {}, async GitUnstage() {},
    async GitCommit() { return "committed"; },
    async GitPull() { return "Already up to date."; },
    async GitPush() { return "pushed"; },
    async GitDiscard() {},
    async GitBranches() { return [{ Name: "main", Current: true }, { Name: "dev", Current: false }]; },
    async GitCheckout() { return "switched"; },

    // ── Cron 定时任务（cron_panel.go）──
    async AddCronJob(name, expr, workspace, mode, task, enabled) {
      return { id: "cron_mock", name, expr, workspace: workspace || "D:/code/gocoder", mode: mode || "code", task, enabled: enabled !== false, model: "", createdAt: 0, updatedAt: 0 };
    },
    async UpdateCronJob(job) { return job; },
    async DeleteCronJob() {},
    async ToggleCronJob(id, enabled) { return { id, enabled }; },
    async RunCronJobNow() {},
    async DescribeCron(expr) {
      return "（mock）表达式解析：" + expr;
    },
    async CreateSelfReviewJob(expr, workspace) {
      return { id: "cron_mock_self", name: "（mock）自审阅", expr, workspace: workspace || "D:/code/gocoder", mode: "ask", task: "", enabled: true, createdAt: 0, updatedAt: 0 };
    },
    async CreateHourlyReviewJob(expr) {
      return { id: "cron_mock_hourly", name: "（mock）每小时复盘", expr, workspace: "", mode: "", task: "", enabled: true, createdAt: 0, updatedAt: 0 };
    },

    // ── Roadmap 迭代规划板（roadmap_panel.go）──
    async ListRoadmapItems() { return []; },
    async RoadmapStats() { return { total: 0, proposed: 0, approved: 0, developing: 0, done: 0, failed: 0, rejected: 0 }; },
    async GetAutoDevConfig() { return { enabled: false, intervalMin: 0, dailyLimit: 0, maxPlanItems: 0, reviewPrompt: "", devMode: "parallel", parallel: 2, autoReview: false, rejectDelete: false, minDeveloping: 0 }; },
    async SetAutoDevConfig() {},
    async AddRoadmapItem(title, detail, priority) {
      return { id: "rm_mock", title, detail, priority, status: "proposed", createdAt: 0, updatedAt: 0 };
    },
    async UpdateRoadmapItem(id, title, detail, priority, category) {
      return { id, title, detail, priority, category, status: "proposed", createdAt: 0, updatedAt: 0 };
    },
    async DeleteRoadmapItem() {},
    async DeleteRoadmapRejected() {},
    async ApproveRoadmapItems() {},
    async RejectRoadmapItem() {},
    async RetryRoadmapItem() {},
    async ReviewRoadmapProposals() {},
    async StartRoadmapDev() {},
    async PlanRoadmap() {},
    async PlanRoadmapWithGuidance() {},
    async RoadmapData() { return { items: [], stats: null, auto: null }; },

    // ── 文档中心（doc_panel.go）──
    async CreateDocFile(req) {},
    async DeleteDocFile() {},
    async RenameDocFile(req) {},
    async ReadDocFile(rel) {
      return { path: rel, name: "（mock）.md", size: 0, modTime: "", content: "", isBinary: false, truncated: false };
    },
    async WriteDocFile(req) {},

    // ── 工作区面板（workspace_panel.go / ws_tasks.go）──
    async SetWSPanelDir(dir) {},
    async GetWSTree() { return { root: "", exists: false, nodes: [] }; },
    async ReadWSFile(rel) { return { path: rel, name: "（mock）", size: 0, modTime: "", content: "", isBinary: false, truncated: false }; },
    async WriteWSFile(req) {},
    async CreateWSFile(req) {},
    async DeleteWSFile(rel) {},
    async RenameWSFile(req) {},
    async WSRunProject(task) { return ""; },
    async WSSessionFor() { return null; },

    // ── 向量索引（vector_panel.go）──
    async SetVectorConfig() {},
    async VectorIndexNow() {},
    async VectorClear() {},
    async PickVectorDir() { return ""; },

    // ── 数字员工提案（employee_panel.go）──
    async ListProposals() { return []; },
    async ApproveProposal() {},
    async RejectProposal() {},

    // ── Gh PR 工具（gh_pr.go）──
    async GhAvailable() { return false; },
    async GhPRList() { return []; },
    async GhPRView() { return null; },
    async GhPRCreate() { return null; },
    async GhPRMerge() { return "（mock）"; },
    async GhCIOf() { return []; },

	    // ── 扩展（extension_panel.go）──
	    async InstallExtension(req) { return { id: "ext_mock", repo: req.repo, name: "（mock）扩展", description: "", skills: [] }; },
	    async UninstallExtension() {},
	    async DeleteExtension() {},
	    async AddExtensionByAI(req) { return { id: "ext_mock_ai", repo: req.repo, name: "（mock）AI 扩展", description: "", skills: [] }; },

	    // ── 插件（plugin_panel.go）──
	    async ListPlugins() { return []; },
	    async InstallPluginFromDir(req) { return { id: "plugin_mock", name: "（mock）插件", description: "" }; },
	    async UninstallPlugin() {},
	    async TogglePlugin() {},
	    async ListAvailablePlugins() { return []; },
	    async InstallPluginFromURL(req) { return { id: "plugin_url_mock", name: "（mock）下载插件", description: "" }; },
	    // ── 插件市场（服务端市场；ListMarketItems 后端自带内置目录兜底）──
	    async ListMarketItems() {
	      return [
	        { id: "rea", kind: "plugin", name: "REA 逆向工程", version: "1.0.0", description: "逆向工程工具链（MCP 桥接 rea-agents）", author: "Kaelvoris 官方", publishedBy: "system", publishedAt: 0, installed: false },
	        { id: "office", kind: "plugin", name: "Office 办公", version: "1.0.0", description: "Word/Excel/PPT 文档读写插件", author: "Kaelvoris 官方", publishedBy: "system", publishedAt: 0, installed: false },
	      ];
	    },
	    async InstallFromMarket(req) { return { id: req.id, name: req.id, description: "" }; },
	    async PublishSkillToMarket(name) { return { id: name, kind: "skill", name }; },
	    async PublishPluginToMarket(id) { return { id, kind: "plugin", name: id }; },
	    async PublishMCPToMarket(req) { return { id: "mcp-" + req.name, kind: "mcp", name: req.name }; },

	    // ── 待办 ──
		async GetConfig() {
		      return {
		        baseURL: "https://api.example.com/v1", apiKey: "sk-demo", model: "gpt-4o",
		        modelProfiles: [
		          { kind: "chat", name: "演示主模型", baseURL: "https://api.example.com/v1", apiKey: "sk-demo", model: "gpt-4o", disabled: false },
		          { kind: "chat", name: "备用（停用）", baseURL: "https://api.example.com/v2", apiKey: "sk-demo2", model: "deepseek-chat", disabled: true },
		        ],
	        workspace: "D:/code/gocoder", autoApprove: false, dangerAutoApprove: false,
	        contextWindow: 128000, autoCondensePercent: 90, condenseDisabled: false,
		        taskSummary: { enabled: true },
	        editorPreference: "folder", recentProjects: ["D:/code/gocoder"],
	      };
	    },
    async SaveConfig() {},
    async GetVersionInfo() { return { version: "1.0.0", changelog: "Vue3 + Arco Design 重构版" }; },
    // 字段名对齐 version.CheckResult：update/latest（前端读 r.update / r.latest）
    async CheckUpdate() { return { current: "1.0.0", latest: "1.0.0", update: false, notes: [] }; },
    async LastUpdateCheck() { return { current: "1.0.0", latest: "1.0.0", update: false, notes: [] }; },
    async ApplyUpdate() {},
    async DismissUpdate() {},
    async SaveUpdateConfig() {},

	    // ── 列表类（空实现：面板显示空态）──
		    async ListSkillViews() { return []; },
		    async ListSkills() { return []; },
		    async ListSkillCategories() { return [{ id: "", name: "未分类", sort: 1, count: 0 }]; },
		    async GetSkillAssignments() { return {}; },
		    async SetSkillCategory() {},
    async SetSkillEnabled() {},
		    async SaveSkillCategories() {},
		    async CategoryAdd() { return { id: "cat_mock", name: "", sort: 1 }; },
		    async CategoryRename() {},
		    async CategoryDelete() {},
		    async SkillRaw() { return ""; },
		    async SaveSkill() {},
		    async DeleteSkill() {},
	    // 会话技能面板（右栏技能 TAB）：mock 桩——空列表 + 全部操作静默成功
	    async GetSessionSkills() { return []; },
	    async AddSessionSkill() {},
	    async SetSessionSkillParams() {},
	    async RemoveSessionSkill() {},
	    async RunSessionSkill() { return "mock-" + Date.now(); },
	    async ListCronJobs() { return []; },
	    async ListCronRuns() { return []; },
		async ListEmployees() { return JSON.parse(JSON.stringify(mockEmployees)); },
		// 整存整取（后端 SaveEmployee 同语义）：按 id 替换，未存在则追加
		async SaveEmployee(e) {
		  const i = mockEmployees.findIndex((x) => x.id === (e && e.id));
		  if (i >= 0) mockEmployees[i] = { ...mockEmployees[i], ...e };
		  else mockEmployees.push({ ...e, id: e && e.id || "emp_mock" });
		  return { id: e && e.id || "emp_mock" };
		},
	    async SetSessionEmployee() {},
	    async GetEmployee() { return {}; },
	    async DeleteEmployee() {},
	    async DuplicateEmployee() { return {}; },
	    async SetEmployeeStatus() { return {}; },
	    async ListAllProposals() { return []; },
	    // ── EHR 组织架构（部门/岗位/角色）mock 桩 ──
	    async GetOrg() {
	      return {
	        departments: [
	          { id: "dep-rd", code: "RD-01", name: "研发部", parentId: "", status: "active" },
	          { id: "dep-fe", code: "", name: "前端组", parentId: "dep-rd", status: "active" },
	        ],
	        positions: [
	          { id: "pos-dev", code: "ENG-001", name: "后端工程师", deptId: "dep-rd", level: "P5", status: "active", scopeAll: true },
	          { id: "pos-qa", code: "ENG-002", name: "测试工程师", deptId: "dep-rd", level: "P4", status: "active", scopeDeptIds: ["dep-rd"] },
	        ],
	        roles: [
	          { id: "role-dev", code: "R-DEV", name: "开发工程师", mcpMode: "whitelist", mcpTools: ["fs_read"], status: "active" },
	          { id: "role-ro", code: "R-RO", name: "只读审计", readOnly: true, status: "active" },
	        ],
	      };
	    },
	    async OrgTree() {
	      return [
	        { id: "dep-rd", code: "RD-01", name: "研发部", parentId: "", status: "active", children: [
	          { id: "dep-fe", code: "", name: "前端组", parentId: "dep-rd", status: "active", children: [] },
	        ] },
	      ];
	    },
	    async SaveDepartment(d) { return { ...d, id: d.id || "dep_mock" }; },
	    async DeleteDepartment() {},
	    async SavePosition(p) { return { ...p, id: p.id || "pos_mock" }; },
	    async DeletePosition() {},
	    async SaveRole(r) { return { ...r, id: r.id || "role_mock" }; },
	    async DeleteRole() {},
	    async SetEmpStatus() { return {}; },
	    // ── A2A 群聊（桌面端；mock 演示数据）──
	    async GroupList() {
	      return [
	        { id: "g_mock", name: "市场文案协作群", owner: "user:remote", nodeId: "node-mock", members: [
	          { id: "user:remote", name: "用户", kind: "user" },
	          { id: "emp-lark", name: "小飞·客服", kind: "employee", avatar: "🛎️" },
	          { id: "bot:group", name: "群助手", kind: "bot" },
	        ], createdAt: Date.now(), updatedAt: Date.now() },
	      ];
	    },
	    async GroupCreate(name, members) { return { id: "g_mock_new", name, owner: "user:remote", members: (members || []).map((id) => ({ id, name: id, kind: "employee" })) }; },
	    async GroupMsg() { return; },
	    async GroupTask() { return; },
	    async GroupJoin() { return; },
	    async GroupLeave() { return; },
	    async GroupMessages() {
	      return [
	        { id: "m1", groupId: "g_mock", author: "bot:group", authorName: "群助手", authorKind: "bot", text: "群聊已创建，欢迎加入！", ts: Date.now() },
	        { id: "m2", groupId: "g_mock", author: "user:remote", authorName: "用户", authorKind: "user", text: "大家好，今天一起整理周报。", ts: Date.now() },
	      ];
	    },
	    async ListExtensions() { return []; },
	    async ListFlows() { return []; },
	    // ── 流程编排 P1（应用/画布/调试）mock 桩 ──
	    async ListApps() { return []; },
	    async CreateApp(name, desc, icon) { return { id: "app_mock", name, desc, icon, flowCount: 0, createdAt: 0, updatedAt: 0 }; },
	    async UpdateApp(id, name, desc, icon) { return; },
	    async DeleteApp(id) { return; },
	    async ListFlowsByApp(appID) { return []; },
	    async AssignFlow(flowID, appID) { return; },
	    async RunNode(req) { return { flowId: req.flowId, output: "（mock）单节点调试", steps: [{ index: 0, type: "passthrough", render: "（mock）" }], durationMs: 0, dryRun: !!req.dryRun }; },
	    async ListFlowTools() { return ["fs_read", "fs_write", "shell"]; },
	    async GetFlow(id) { return { id, name: "（mock）流程", description: "", steps: [], canvas: null, appId: "", createdAt: 0, updatedAt: 0 }; },
	    async SaveFlow(f) { return { ...f, id: f.id || "flow_mock" }; },
	    async DeleteFlow(id) { return; },
	    async RunFlow(req) { return { flowId: req.id, output: "（mock）运行结果", steps: [], durationMs: 0, dryRun: !!req.dryRun }; },
	    async FlowVars(id) { return []; },
	    // ── AI Flow（嵌入式 coze 内核）mock 桩 ──
	    async CozeInit() { return { ready: true, modelConfigured: false, dataDir: "（mock）~/.kaelvoris/coze" }; },
	    async CozeStatus() { return { ready: true, modelConfigured: false, dataDir: "（mock）" }; },
	    async CozeListWorkflows(keyword) {
	      return [
	        { id: "7700077332693778432", name: "示例：回显工作流", desc: "start→end 基础模板", hasPublished: true, latestVersion: "0.0.1", mode: 0, updatedAtUnix: Date.now() },
	      ];
	    },
	    async CozeCreateWorkflow(name, desc) { return { id: "7700077332693778433", name, desc, hasPublished: false, mode: 0, updatedAtUnix: Date.now() }; },
	    async CozeGetWorkflow(id) {
	      return { id, name: "示例：回显工作流", desc: "", hasPublished: true, latestVersion: "0.0.1", mode: 0, commitId: "mock",
	        canvas: '{"nodes":[{"id":"100001","type":"1","data":{"outputs":[{"name":"query","type":"string"}]}},{"id":"900001","type":"2","data":{"inputs":{"terminatePlan":"returnVariables"}}}],"edges":[]}',
	        inputJSON: "", outputJSON: "" };
	    },
	    async CozeSaveWorkflow(id, canvas) { return; },
	    async CozePublishWorkflow(id, version, desc, force) { return; },
	    async CozeRunWorkflow(id, input, fromDraft) {
	      return { status: "success", output: { echo: input?.query ?? "（mock）" }, outputRaw: "", durationMs: 12, execId: 1 };
	    },
	    async CozeDeleteWorkflow(id) { return; },
	    async CozeDraftParams(id) { return "", "", null; },
	    async CozeTestNode(workflowId, nodeId, input) {
	      return { status: "success", output: { mock: true }, outputRaw: "", durationMs: 8, execId: "1",
	        nodeExecs: [{ nodeId, nodeType: "mock", status: "success", output: JSON.stringify(input || {}), durationMs: 8 }] };
	    },
	    async CozeListModels() {
	      return [{ id: "69010", name: "agnes-2.5-flash" }];
	    },
	    async CozeListMCPTools(serverName) {
	      return [{ name: "mock_tool", description: "mock MCP 工具（浏览器预览）" }];
	    },
	    async CozeGetImageGenConfig() {
	      return { configured: false, baseURL: "", model: "", apiKey: "" };
	    },
	    async CozeListKnowledgeBases() {
	      return [{ id: "1", name: "示例知识库", description: "mock（浏览器预览）" }];
	    },
	    async CozeCreateKnowledge(name, content) {
	      return { id: String(Date.now()), name, description: "" };
	    },
	    async CozeListDatabases() {
	      return [{ id: "-900000000000000001", name: "示例连接", desc: "root@192.168.1.10:3306/user_orders（MySQL）", fields: [], tableName: "user_orders" }];
	    },
	    async CozeListWorkflows() {
	      return [{ id: "1001", name: "示例子流程", desc: "mock（浏览器预览）", updatedAtUnix: 0 }];
	    },
	    async CozeListDatasources() {
	      return [{ id: "-900000000000000001", name: "示例连接", host: "192.168.1.10", port: 3306, user: "root", password: "", dbName: "user_orders", jdbcURL: "jdbc:mysql://192.168.1.10:3306/user_orders" }];
	    },
	    async CozeSaveDatasource(ds) {
	      return { ...ds, id: ds.id || "-900000000000000001", jdbcURL: `jdbc:mysql://${ds.host}:${ds.port}/${ds.dbName}` };
	    },
	    async CozeDeleteDatasource(id) {},
	    async CozeTestDatasource() {},
	    async ListRemoteNodes() { return []; },
	    async ListChannels() {
	      return [
	        { type: "lark", name: "团队飞书", enabled: true, employeeId: "emp-lark", positionId: "pos-dev", appId: "cli_a123456", appSecret: "sk-secret-demo", allowedUsers: "" },
	        { type: "wecom", name: "研发企微", enabled: true, employeeId: "emp-wecom", positionId: "pos-dev", botId: "bot-888", botSecret: "bs-demo", allowedUsers: "ou_abc" },
	        { type: "wechat", name: "个人微信（预留）", enabled: false, employeeId: "", deviceId: "" },
	      ];
	    },
		    async SaveChannels() {},
		    // ── 资源绑定（RBAC 优化版：会话/渠道/定时任务三资源 × 岗位/员工）──
		    async ListResourceBindings() {
		      return [
		        { type: "channel", id: "lark:团队飞书", name: "团队飞书", enabled: true, employeeId: "emp-lark", positionId: "pos-dev" },
		        { type: "channel", id: "wecom:研发企微", name: "研发企微", enabled: true, employeeId: "emp-wecom", positionId: "pos-dev" },
		        { type: "cron", id: "job-1", name: "每日晨报推送", enabled: true, employeeId: "", positionId: "pos-qa", assignRule: "round_robin" },
		        { type: "session", id: "mock-parent", name: "重构登录模块并补齐测试", running: false, employeeId: "emp-lark", positionId: "pos-dev" },
		        { type: "session", id: "mock-earlier", name: "昨天归档的会话", running: false, employeeId: "", positionId: "" },
		      ];
		    },
		    async SaveResourceBinding(type, id, employeeId, positionId, assignRule) {
		      console.log("[mock] SaveResourceBinding", type, id, employeeId, positionId, assignRule);
		      return;
		    },
		    async SetSessionBinding(id, employeeId, positionId, assignRule) {
		      console.log("[mock] SetSessionBinding", id, employeeId, positionId, assignRule);
		      return;
		    },
	    async GetLocalIP() { return ""; },
    async ListChannelStatus() {
      return [
        { type: "lark", name: "团队飞书", enabled: true, online: true },
        { type: "wecom", name: "研发企微", enabled: true, online: false },
        { type: "wechat", name: "个人微信（预留）", enabled: false, online: false },
      ];
    },
		    async ListAllTasks() { return []; },
		    async ListMCPServers() { return []; },
		    async SaveMCPServers() {},
		    async TestMCPServer() { return { name: "mock", ok: false, error: "mock 环境" }; },
		    async ListACPAgents() { return []; },
		    async GetPromptConfig() { return {}; },
		    async SavePromptConfig() {},
		    // mock：内置默认提示词桩（浏览器预览提示词页折叠区展示用）
		    async GetPromptDefaults() {
		      return [
		        { key: "system", default: "You are Kaelvoris, a highly skilled software engineer...（mock 桩示例：真实环境返回完整系统提示词）" },
		        { key: "summarize", default: "You are a conversation summarizer. Summarize the conversation above into a structured context record..." },
		        { key: "taskSummary", default: "你是任务收尾总结器。根据上面的任务对话，为本会话生成一段简洁的任务总结…" },
		        { key: "selfReview", default: "你是主 Agent 的后台复盘器（self-improvement review）…" },
		        { key: "hourlyReview", default: "你是 Kaelvoris 桌面 Agent 的每小时复盘器…" },
		        { key: "roadmapReview", default: "（本段无内置默认。内置审核准则始终生效，本段文本作为最高优先级补充判据追加其后；留空 = 不注入补充判据。）" },
		        { key: "selfCheckTask", default: "【定时自检】复盘本工作区近期任务，把可复用的经验沉淀为经验卡…" },
		        { key: "roadmapSystem", default: "# 自主开发守则（无人值守）…（mock 桩示例：真实环境返回完整守则文本）" },
		      ];
		    },
		    async SetSessionACP() {},
		    async SendMessageWithACP(id, task, images, acpAgent) {
		      // mock：与 SendMessageWithEmployee 同语义（agent 绑定仅记录）。
		      return this.SendMessageWithEmployee(id, task, images, "");
		    },
		    async TestACPAgent() { return { name: "mock", ok: false, error: "mock 环境" }; },
		    async SaveACPAgents() {},
	    async GetMemory() { return { content: "# 会话记忆\n\n- （mock 桩示例数据）" }; },
	    async SaveMemory() {},
	    async GetWorkspaceMemory() { return { workspace: "F:/mock/project", content: "# 工作空间记忆\n\n- （mock 桩示例：跨会话共享的项目级记忆）\n\n## 最近交互\n\n- 2026-10-31 10:00 · 会话 mock 有交互" }; },
	    async SaveWorkspaceMemory() {},
			    async GetRemoteStatus() { return { connected: false }; },
			    // 账号态 mock（对齐 RemoteAccountState）：登录后返回 loggedIn 状态供设置页按钮渲染验证。
			    async GetRemoteAccount() { return { enabled: false, loggedIn: !!(window.__remoteCfg && window.__remoteCfg.loggedIn), user: (window.__remoteCfg && window.__remoteCfg.user) || "", displayName: (window.__remoteCfg && window.__remoteCfg.user) || "" }; },
		    // 服务中心连接 mock（2026-11 表单清空修复对齐）：mock 态保留 user/pass，
		    // 与真实后端行为一致（保存后回显凭据），浏览器 mock 链路可验证表单回显。
		    async GetRemoteConfig() { return window.__remoteCfg || (window.__remoteCfg = { enabled: false, serverURL: "", token: "", nodeID: "", name: "", user: "", pass: "" }); },
		    async SaveRemoteConfig(c) { Object.assign(window.__remoteCfg || (window.__remoteCfg = { enabled: false, serverURL: "", token: "", nodeID: "", name: "", user: "", pass: "" }), c); },
			    async RemoteLogin(u, p) { Object.assign(window.__remoteCfg || (window.__remoteCfg = { enabled: false, serverURL: "", token: "", nodeID: "", name: "", user: "", pass: "" }), { user: u, pass: p, loggedIn: true }); return { enabled: true, loggedIn: true, user: u, displayName: u, role: "client" }; },
			    async RemoteLogout() { const c = window.__remoteCfg; if (c) { c.user = ""; c.pass = ""; c.loggedIn = false; } return { enabled: false, loggedIn: false }; },
	    async RemoteSendTask() { return { taskId: "mock-task", sessionId: "remote-mock" }; },
	    async RemoteCancelTask() {},
	    async RemoteApproveTaskAsk() {},
	    async RemoteContinueTask() {},
	    async RemoteSessionMessages() { return { sessionID: "", messages: [] }; },
    async ListDocTree() { return []; },
    async GetDocInfo() { return {}; },
    async ListAuditLogs() { return []; },
    async EvolveStats() { return {}; },
    async ListEvolveCards() { return []; },
    async DeleteEvolveCard() {},
	async VectorStatus() { return { enabled: false }; },
	async GetVectorConfig() { return { enabled: false }; },
	async SetAIContextVisible() {}, // mock：上下文面板可见性上报（引擎热路径门控）
  };

  window.go = { desktop: { App } }; // 绑定 key 与真实桌面端对齐（包名 desktop）

  window.runtime = { EventsOn: on, EventsOff() {}, EventsEmit: window.__emit };

  // 等应用订阅完成后广播初始数据
  setTimeout(() => {
    if (!mockSessions.length) {
      seq = 2;
      mockSessions.push(
        { id: "mock-seed-2", title: "Kaelvoris UI 设计系统对齐", createdAt: new Date(Date.now() - 7200e3).toISOString(), updatedAt: new Date(Date.now() - 3600e3).toISOString(), messageCount: 12 },
        { id: "mock-seed-1", title: "修复 Wails 桥接事件丢失", createdAt: new Date(Date.now() - 86400e3).toISOString(), updatedAt: new Date(Date.now() - 43200e3).toISOString(), messageCount: 8 },
      );
      for (const s of mockSessions) {
        sessionMsgs.set(s.id, []);
        sessionTodos.set(s.id, []);
      }
    }
    window.__emit("kaelvoris:sessions", listSessions());
  }, 300);
}

// 模块加载即安装（桌面端 window.go 已存在时自动跳过）
installMock();

export default installMock;
