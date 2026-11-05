<script setup>
// Git 面板：分支切换 / 暂存与更改列表 / diff 预览 / 历史记录 / PR·CI（gh CLI）
// 字段对齐 git_panel.go：GitStatus{repo,branch,upstream,ahead,behind,unstaged[],staged[]}
//   GitFileEntry{path,status,staged} / GitCommitInfo{hash,author,date,subject}
// PR/CI：gh_pr.go —— GhAvailable/GhPRList/GhPRView/GhPRCreate/GhPRMerge/GhCIOf
import { computed, onMounted, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import { bridgeCall } from "../bridge/wails.js";

const git = ref(null);
const diffFile = ref("");
const diffText = ref("");
const commitMsg = ref("");
const logList = ref([]);
const branches = ref([]);
const loading = ref(false);
const gitError = ref(false); // GitStatus 读取失败（无仓库之外的真实错误）

// PR/CI 状态（gh CLI 可用时启用）
const ghOK = ref(false);
const prList = ref([]);
const prDetail = ref(null);
const prLoading = ref(false);
const prTitle = ref("");
const prDraft = ref(false);

// 后端 JSON 字段为 repo（git_panel.go）；兼容旧 mock 的 isRepo。
const hasRepo = computed(() => !!(git.value && (git.value.repo ?? git.value.isRepo)));
const staged = computed(() => (git.value && git.value.staged) || []);
const unstaged = computed(() => (git.value && git.value.unstaged) || []);
const branchName = computed(() => (git.value && git.value.branch) || "");
const ahead = computed(() => (git.value && git.value.ahead) || 0);
const behind = computed(() => (git.value && git.value.behind) || 0);

async function refresh() {
  loading.value = true;
  try {
    git.value = await bridgeCall("GitStatus");
    if (hasRepo.value) {
      try { branches.value = (await bridgeCall("GitBranches")) || []; } catch { branches.value = []; }
      try { logList.value = (await bridgeCall("GitLog", 50)) || []; } catch { logList.value = []; }
      // PR/CI：gh 可用时拉取 PR 列表（失败静默，非 GitHub 仓库属预期）。
      try {
        ghOK.value = await bridgeCall("GhAvailable");
        if (ghOK.value) prList.value = (await bridgeCall("GhPRList", "OPEN")) || [];
      } catch { ghOK.value = false; }
    }
  } catch (e) {
    console.warn("GitStatus failed:", e);
    git.value = null;
    gitError.value = true;
  } finally {
    loading.value = false;
  }
}

async function openPR(n) {
  prLoading.value = true;
  try { prDetail.value = await bridgeCall("GhPRView", n); } catch (e) { Message.error(String(e)); }
  finally { prLoading.value = false; }
}
async function createPR() {
  if (!prTitle.value.trim()) { Message.warning("请填写 PR 标题"); return; }
  prLoading.value = true;
  try {
    const r = await bridgeCall("GhPRCreate", prTitle.value.trim(), prBodyText(prDetail), prDraft.value, "");
    prTitle.value = ""; prDetail.value = null;
    Message.success("PR 已创建" + (r && r.url ? "：" + r.url : ""));
    await refresh();
    if (r && r.number) await openPR(r.number);
  } catch (e) { Message.error(String(e)); }
  finally { prLoading.value = false; }
}
// prBodyText：创建时用详情 body 或空串（bridgeCall 参数对齐）
function prBodyText(d) { return (d && d.value && d.value.body) || ""; }
async function mergePR(n, method) {
  prLoading.value = true;
  try { Message.success(await bridgeCall("GhPRMerge", n, method)); prDetail.value = null; await refresh(); }
  catch (e) { Message.error(String(e)); }
  finally { prLoading.value = false; }
}

const checkCls = (s) => s === "success" ? "ck-ok" : s === "failure" ? "ck-bad" : s === "pending" ? "ck-run" : "";
const checkC = (c) => (c.conclusion || "") === "SUCCESS" ? "ck-ok" : (c.conclusion || "") === "FAILURE" ? "ck-bad" : (c.conclusion || "") === "" ? "ck-run" : "";

async function showDiff(path) {
  diffFile.value = path;
  try { diffText.value = (await bridgeCall("GitFileDiff", path)) || ""; } catch { diffText.value = ""; }
}

async function stage(paths) {
  try { await bridgeCall("GitStage", paths); await refresh(); } catch (e) { Message.error(String(e)); }
}
async function unstage(paths) {
  try { await bridgeCall("GitUnstage", paths); await refresh(); } catch (e) { Message.error(String(e)); }
}
async function stageAll() {
  await stage(unstaged.value.map((f) => f.path || f.Path));
}
async function discard(paths) {
  try { await bridgeCall("GitDiscard", paths); Message.success("已丢弃"); await refresh(); }
  catch (e) { Message.error(String(e)); }
}
async function commit() {
  if (!commitMsg.value.trim()) { Message.warning("请填写提交信息"); return; }
  try {
    await bridgeCall("GitCommit", commitMsg.value.trim(), false);
    commitMsg.value = "";
    Message.success("已提交");
    await refresh();
  } catch (e) { Message.error(String(e)); }
}
async function pull() {
  try { const r = await bridgeCall("GitPull"); Message.info(String(r || "已拉取")); await refresh(); }
  catch (e) { Message.error(String(e)); }
}
async function push() {
  try { const r = await bridgeCall("GitPush"); Message.info(String(r || "已推送")); await refresh(); }
  catch (e) { Message.error(String(e)); }
}
async function checkout(name) {
  if (!name || name === branchName.value) return;
  try { await bridgeCall("GitCheckout", name); Message.success("已切换到 " + name); await refresh(); }
  catch (e) { Message.error(String(e)); }
}

function statusOf(f) {
  const s = (f.status || f.Status || "").toUpperCase();
  const cls = { M: "git-status-m", A: "git-status-a", D: "git-status-d", R: "git-status-r", C: "git-status-a", U: "git-status-d" };
  return { tag: s || "?", cls: cls[s] || "git-status-m" };
}
const pathOf = (f) => f.path || f.Path || "";

// diff 行着色：+ 绿 / - 红 / @@ 蓝 / 文件头灰
const diffLines = computed(() =>
  (diffText.value || "").split("\n").map((l) => ({
    text: l,
    cls: l.startsWith("+") && !l.startsWith("+++") ? "d-add"
      : l.startsWith("-") && !l.startsWith("---") ? "d-del"
      : l.startsWith("@@") ? "d-hunk"
      : l.startsWith("diff ") || l.startsWith("index ") ? "d-meta" : "",
  })),
);

onMounted(refresh);
</script>

<template>
  <div class="git-page">
    <!-- 工具条：分支 / 拉取 / 推送 / 刷新 -->
    <div class="git-toolbar">
      <a-select
        v-if="branches.length"
        :model-value="branchName"
        size="small"
        style="width: 180px"
        :fallback-option="false"
        @change="checkout"
      >
        <a-option v-for="b in branches" :key="b.name || b.Name" :value="b.name || b.Name">
          {{ (b.current || b.Current ? "● " : "") + (b.name || b.Name) }}
        </a-option>
      </a-select>
      <span v-else-if="branchName" class="branch-name">{{ branchName }}</span>
      <span v-if="ahead || behind" class="git-sync-badge">
        {{ ahead ? "↑" + ahead : "" }}{{ behind ? " ↓" + behind : "" }}
      </span>

      <span class="spacer"></span>
      <a-button size="small" @click="pull">拉取</a-button>
      <a-button size="small" @click="push">推送</a-button>
      <a-button size="small" :loading="loading" @click="refresh">刷新</a-button>
    </div>

    <div v-if="hasRepo" class="git-main">
      <!-- 左：提交区 + 文件列表 -->
      <div class="git-left">
        <div class="git-commit-box">
          <a-textarea
            v-model="commitMsg"
            placeholder="提交信息…"
            :max-length="200"
            :auto-size="{ minRows: 2, maxRows: 4 }"
          />
          <div class="git-commit-actions">
            <a-button size="small" :disabled="!unstaged.length" @click="stageAll">全部暂存</a-button>
            <a-button size="small" type="primary" :disabled="!staged.length" @click="commit">提交</a-button>
          </div>
        </div>

        <div class="git-section">
          <div class="git-section-head">已暂存（{{ staged.length }}）</div>
          <div
            v-for="f in staged"
            :key="'s-' + pathOf(f)"
            class="git-file"
            :class="{ active: diffFile === pathOf(f) }"
            @click="showDiff(pathOf(f))"
          >
            <span class="git-status" :class="statusOf(f).cls">{{ statusOf(f).tag }}</span>
            <span class="path" :title="pathOf(f)">{{ pathOf(f) }}</span>
            <a-button size="mini" type="text" title="移出暂存区" @click.stop="unstage([pathOf(f)])">↩</a-button>
            <a-popconfirm content="确定丢弃该文件的未提交修改？（不可恢复）" type="warning" @before-ok="discard([pathOf(f)])">
              <a-button size="mini" type="text" status="danger" title="丢弃修改" @click.stop>✕</a-button>
            </a-popconfirm>
          </div>
          <div v-if="!staged.length" class="git-empty">无已暂存文件</div>
        </div>

        <div class="git-section">
          <div class="git-section-head">更改（{{ unstaged.length }}）</div>
          <div
            v-for="f in unstaged"
            :key="'u-' + pathOf(f)"
            class="git-file"
            :class="{ active: diffFile === pathOf(f) }"
            @click="showDiff(pathOf(f))"
          >
            <span class="git-status" :class="statusOf(f).cls">{{ statusOf(f).tag }}</span>
            <span class="path" :title="pathOf(f)">{{ pathOf(f) }}</span>
            <a-button size="mini" type="text" title="暂存" @click.stop="stage([pathOf(f)])">＋</a-button>
          </div>
          <div v-if="!unstaged.length" class="git-empty">工作区干净</div>
        </div>
      </div>

      <!-- 右：diff 预览（弹性） + 历史记录 -->
      <div class="git-right">
        <div class="git-diff-wrap">
          <div v-if="diffFile" class="git-diff-head">{{ diffFile }}</div>
          <pre v-if="diffText" class="git-diff-view"><span
            v-for="(l, i) in diffLines"
            :key="i"
            class="d-line"
            :class="l.cls"
          >{{ l.text }}
</span></pre>
          <div v-else class="git-empty" style="text-align: center; padding: 32px 0">← 点击左侧文件查看变更</div>
        </div>

        <div class="git-section">
          <div class="git-section-head">历史（最近 {{ logList.length }} 条）</div>
          <div v-for="c in logList" :key="c.hash || c.Hash" class="git-log-item" :title="(c.author || c.Author) + ' · ' + (c.date || c.Date)">
            <span class="git-hash">{{ (c.hash || c.Hash || "").slice(0, 7) }}</span>
            <span class="git-msg">{{ c.subject || c.Subject || c.message || c.Message }}</span>
            <span class="git-date">{{ c.date || c.Date }}</span>
          </div>
          <div v-if="!logList.length" class="git-empty">暂无提交</div>
        </div>
      </div>
    </div>

    <!-- PR / CI 区（gh CLI 可用时展示；Claude Code cloud→PR 对齐） -->
    <div v-if="hasRepo && ghOK" class="git-section pr-section">
      <div class="git-section-head">Pull Requests · CI（{{ prList.length }}）</div>
      <div v-if="prDetail" class="pr-detail">
        <div class="pr-detail-head">
          <span class="git-hash">#{{ prDetail.number }}</span>
          <span class="git-msg">{{ prDetail.title }}</span>
          <span class="pr-state" :class="(prDetail.state || '').toLowerCase()">{{ prDetail.state }}</span>
          <span v-if="prDetail.mergeable === 'CONFLICTING'" class="pr-conflict">冲突</span>
          <a class="pr-link" :href="prDetail.url" target="_blank">在 GitHub 打开 ↗</a>
        </div>
        <div class="pr-meta">{{ prDetail.headRef }} → {{ prDetail.baseRef }} · {{ prDetail.author }}<template v-if="prDetail.review"> · 评审：{{ prDetail.review }}</template></div>
        <div v-if="prDetail.checks && prDetail.checks.length" class="pr-checks">
          <span v-for="c in prDetail.checks" :key="c.name" class="pr-check" :class="checkC(c)">
            {{ c.name }}{{ c.conclusion ? " · " + c.conclusion.toLowerCase() : " · " + (c.status || "").toLowerCase() }}
          </span>
        </div>
        <div class="pr-actions">
          <a-button size="small" @click="mergePR(prDetail.number, 'squash')">Squash 合并</a-button>
          <a-button size="small" @click="mergePR(prDetail.number, 'merge')">Merge 合并</a-button>
          <a-button size="small" @click="mergePR(prDetail.number, 'rebase')">Rebase 合并</a-button>
          <a-button size="small" @click="prDetail = null">关闭</a-button>
        </div>
      </div>
      <div v-for="p in prList" :key="p.number" class="git-log-item pr-item" @click="openPR(p.number)">
        <span class="git-hash">#{{ p.number }}</span>
        <span class="pr-state" :class="(p.state || '').toLowerCase()">{{ p.isDraft ? "draft" : (p.state || "").toLowerCase() }}</span>
        <span class="git-msg">{{ p.title }}</span>
        <span v-if="p.checkState" class="pr-check" :class="checkCls(p.checkState)">{{ p.checkState }}</span>
      </div>
      <div v-if="!prList.length && !prDetail" class="git-empty">无开放 PR</div>
      <div class="pr-create">
        <a-input v-model="prTitle" size="small" placeholder="从当前分支创建 PR：输入标题…" @keyup.enter="createPR" />
        <a-checkbox v-model="prDraft" class="pr-draft">草稿</a-checkbox>
        <a-button size="small" type="primary" :loading="prLoading" @click="createPR">创建 PR</a-button>
      </div>
    </div>

    <div v-if="!hasRepo" class="git-norepo">
      <div v-if="loading">加载中…</div>
      <template v-else-if="gitError">
        <div>无法读取 Git 状态</div>
        <a-button size="small" :loading="loading" @click="refresh" style="margin-top: 8px">重试</a-button>
      </template>
      <div v-else>当前目录不是 Git 仓库</div>
    </div>
  </div>
</template>

<style scoped>
.git-page {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding: var(--pad-block);
  gap: 12px;
  overflow: hidden;
}
.git-toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-shrink: 0;
}
.git-toolbar .spacer { flex: 1; }
.branch-name {
  font-size: var(--fs-caption);
  color: var(--text-1);
  font-weight: 600;
}
.git-sync-badge {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-2);
}

.git-norepo {
  color: var(--text-3);
  text-align: center;
  padding: 48px 0;
}
.git-main {
  flex: 1;
  display: grid;
  grid-template-columns: minmax(300px, 5fr) 7fr;
  gap: 12px;
  min-height: 0;
}
.git-left, .git-right {
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
}

/* 左：提交框 */
.git-commit-box {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.git-commit-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

/* 文件列表 */
.git-section {
  display: flex;
  flex-direction: column;
  gap: 2px;
  flex-shrink: 0;
}
.git-section-head {
  font-size: 12px;
  color: var(--text-2);
  padding: 4px;
}
.git-file {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-size: var(--fs-caption);
  color: var(--text-1);
  transition: background 0.15s;
}
.git-file:hover { background: var(--bg-hover); }
.git-file.active { background: var(--bg-active); }
.git-file .path {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.git-status { font-family: var(--font-mono); font-weight: 600; flex-shrink: 0; }
.git-status-m { color: var(--warning); }
.git-status-a { color: var(--success); }
.git-status-d { color: var(--danger); }
.git-status-r { color: var(--primary); }
.git-empty {
  color: var(--text-3);
  font-size: var(--fs-caption);
  padding: 4px 8px;
}

/* 右：diff 预览 */
.git-diff-head {
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text-2);
  margin-bottom: 6px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.git-right .git-diff-view {
  font-family: var(--font-mono);
  font-size: 12px;
  line-height: 1.5;
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 10px;
  overflow: auto;
  margin: 0;
  white-space: pre;
  flex: 1;
  min-height: 0;
}
.git-diff-view .d-line { display: block; }
.git-diff-view .d-add { color: var(--success); background: color-mix(in srgb, var(--success) 10%, transparent); }
.git-diff-view .d-del { color: var(--danger); background: color-mix(in srgb, var(--danger) 10%, transparent); }
.git-diff-view .d-hunk { color: var(--primary); }
.git-diff-view .d-meta { color: var(--text-3); }

/* 历史 */
.git-log-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px;
  border-radius: var(--radius-sm);
  font-size: var(--fs-caption);
  color: var(--text-1);
}
.git-hash {
  font-family: var(--font-mono);
  color: var(--primary);
  flex-shrink: 0;
}
.git-log-item .git-msg {
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.git-log-item .git-date {
  flex-shrink: 0;
  color: var(--text-3);
  font-size: 11px;
}

/* PR / CI 区块 */
.pr-section { border-top: 1px solid var(--border-weak); padding-top: 8px; }
.pr-create { display: flex; gap: 8px; align-items: center; margin-top: 6px; }
.pr-draft { flex-shrink: 0; font-size: 12px; }
.pr-item { cursor: pointer; }
.pr-item:hover { background: var(--bg-hover); }
.pr-state {
  font-size: 11px;
  font-family: var(--font-mono);
  padding: 0 4px;
  border-radius: var(--radius-sm);
  flex-shrink: 0;
}
.pr-state.open { color: var(--success); }
.pr-state.merged { color: var(--primary); }
.pr-state.closed, .pr-state.draft { color: var(--text-3); }
.pr-check, .ck-ok { color: var(--success); font-size: 11px; font-family: var(--font-mono); flex-shrink: 0; }
.ck-bad, .pr-check { color: var(--danger); }
.pr-check.ck-run, .ck-run { color: var(--warning); }
.pr-check.ck-ok { color: var(--success); }
.pr-detail {
  background: var(--bg-inset);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius-sm);
  padding: 8px 10px;
  margin: 4px 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.pr-detail-head { display: flex; align-items: center; gap: 8px; }
.pr-detail-head .git-msg { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pr-conflict { color: var(--danger); font-size: 11px; }
.pr-link { font-size: 12px; color: var(--primary); flex-shrink: 0; }
.pr-meta { font-size: 11px; color: var(--text-3); }
.pr-checks { display: flex; flex-wrap: wrap; gap: 6px; }
.pr-check { border: 1px solid var(--border-weak); border-radius: var(--radius-sm); padding: 1px 6px; }
.pr-actions { display: flex; gap: 8px; }
</style>
