<script setup>
// ═════════════════════════════════════════════════════════════════
// 插件面板（PC 端左侧入口「插件」）— 插件管理独立面板
//   已安装列表：启用/停用开关 · 卸载 · 详情（版本/作者/MCP 接入/技能数）
//   安装：本地目录路径输入 + 从 URL 下载安装
//   「市场」页签：从远程服务端市场拉取（后端 ListMarketItems，含插件/技能/
//     MCP 三类条目，kind 徽标区分），未配置远程或拉取失败回落内置目录（后端
//     已内置兑底，前端只管展示与安装）；技能条目走 InstallFromMarket
//     （后端按 kind 分流：技能装技能库，插件/MCP 走插件安装链路）。
// 后端：plugin_panel.go + market_panel.go（ListMarketItems / InstallFromMarket）
// 事件：kaelvoris:plugins（安装/卸载/开关后后端广播刷新）
// ═════════════════════════════════════════════════════════════════
import { onBeforeUnmount, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import {
  IconPlus, IconDelete, IconRefresh, IconDownload, IconFolderAdd,
  IconCheckCircle, IconApps,
} from "@arco-design/web-vue/es/icon";
import { bridgeCall, onEvent } from "../bridge/wails.js";

const plugins = ref([]);        // 已安装插件（[]PluginView）
const market = ref([]);         // 市场条目（[]MarketItem，插件/技能/MCP）
const marketLoading = ref(false);
const loading = ref(false);
const installDir = ref("");     // 本地目录安装输入
const installURL = ref("");     // URL 下载安装输入
const installing = ref(false);
const uninstalling = ref("");   // 正在卸载的插件 ID

const VIEW_TABS = [
  { key: "installed", label: "已安装" },
  { key: "market", label: "插件市场" },
];
const activeTab = ref("installed");

// ── 数据加载 ─────────────────────────────────────────────────────
async function load() {
  loading.value = true;
  try {
    plugins.value = (await bridgeCall("ListPlugins").catch(() => [])) || [];
  } finally {
    loading.value = false;
  }
}

// ── 市场加载（远程市场；失败回落内置目录已由后端兑底）──────────────
async function loadMarket() {
  marketLoading.value = true;
  try {
    market.value = (await bridgeCall("ListMarketItems").catch(() => [])) || [];
  } finally {
    marketLoading.value = false;
  }
}

// kind 徽标（插件/技能/MCP）
function kindLabel(k) {
  return { plugin: "插件", skill: "技能", mcp: "MCP" }[k] || "插件";
}
function kindColor(k) {
  return { plugin: "arcoblue", skill: "green", mcp: "purple" }[k] || "gray";
}

// 内置目录项是否已安装（ID 匹配）。
function isInstalled(id) {
  return plugins.value.some((p) => p.id === id);
}

// ── 安装（本地目录）───────────────────────────────────────────────
async function installFromDir() {
  if (!installDir.value.trim()) {
    Message.warning("请填写插件目录路径（含 plugin.json 的目录）");
    return;
  }
  installing.value = true;
  try {
    const r = await bridgeCall("InstallPluginFromDir", { dir: installDir.value.trim() });
    Message.success(`插件已安装：${r.name || r.id}`);
    installDir.value = "";
    await load();
  } catch (e) {
    Message.error("安装失败：" + String(e));
  } finally {
    installing.value = false;
  }
}

// ── 安装（URL 下载 zip）──────────────────────────────────────────
async function installFromURL() {
  if (!installURL.value.trim()) {
    Message.warning("请填写插件 zip 下载地址（http/https）");
    return;
  }
  installing.value = true;
  try {
    const r = await bridgeCall("InstallPluginFromURL", { url: installURL.value.trim() });
    Message.success(`插件下载安装成功：${r.name || r.id}`);
    installURL.value = "";
    await load();
  } catch (e) {
    Message.error("下载安装失败：" + String(e));
  } finally {
    installing.value = false;
  }
}

// ── 市场一键安装（后端按 kind 分流）─────────────────────────────
async function installMarket(item) {
  installing.value = item.id;
  try {
    await bridgeCall("InstallFromMarket", { id: item.id });
    Message.success(`已从市场安装：${item.name || item.id}`);
    await load();
    await loadMarket();
  } catch (e) {
    Message.error("安装失败：" + String(e));
  } finally {
    installing.value = "";
  }
}

// ── 卸载 ─────────────────────────────────────────────────────────
async function uninstallPlugin(p) {
  uninstalling.value = p.id;
  try {
    await bridgeCall("UninstallPlugin", p.id);
    Message.success(`已卸载：${p.name || p.id}`);
    await load();
  } catch (e) {
    Message.error("卸载失败：" + String(e));
  } finally {
    uninstalling.value = "";
  }
}

// ── 启用/停用 ────────────────────────────────────────────────────
async function togglePlugin(p, enabled) {
  try {
    await bridgeCall("TogglePlugin", p.id, enabled);
    p.enabled = enabled;
    Message.success(`${p.name || p.id} 已${enabled ? "启用" : "停用"}（下一任务生效）`);
    await load();
  } catch (e) {
    Message.error("切换失败：" + String(e));
  }
}

// ── 事件订阅：后端广播刷新 ───────────────────────────────────────
let offPlugins = null;
onBeforeUnmount(() => {
  if (offPlugins) offPlugins();
});
load();
loadMarket();
</script>

<template>
  <div class="plg-panel">
    <!-- 面板头 -->
    <div class="plg-head panel-head">
      <div class="plg-head-title">
        <span class="panel-icon"><IconApps /></span>
        <span class="panel-title">插件</span>
        <span class="panel-sub">已安装 {{ plugins.length }} · 市场 {{ market.length }}</span>
      </div>
      <div class="plg-head-actions">
        <a-button size="small" :loading="loading" @click="load">
          <template #icon><IconRefresh /></template>刷新
        </a-button>
      </div>
    </div>

    <!-- 页签：已安装 / 插件市场 -->
    <div class="plg-tabs">
      <button
        v-for="t in VIEW_TABS"
        :key="t.key"
        class="plg-tab"
        :class="{ active: activeTab === t.key }"
        @click="activeTab = t.key"
      >
        {{ t.label }}
        <span v-if="t.key === 'installed'" class="plg-tab-count">{{ plugins.length }}</span>
        <span v-if="t.key === 'market'" class="plg-tab-count">{{ market.length }}</span>
      </button>
    </div>

    <!-- ══════════ 已安装 ══════════ -->
    <template v-if="activeTab === 'installed'">
      <!-- 安装区 -->
      <div class="plg-section">
        <div class="plg-section-title">安装插件</div>
        <div class="plg-install-card">
          <div class="plg-install-row">
            <a-input
              v-model="installDir"
              placeholder="本地插件目录（含 plugin.json）"
              size="small"
              class="plg-input mono"
              allow-clear
            />
            <a-button size="small" type="primary" :loading="installing" @click="installFromDir">
              <template #icon><IconFolderAdd /></template>从目录安装
            </a-button>
          </div>
          <div class="plg-install-row" style="margin-top: 8px">
            <a-input
              v-model="installURL"
              placeholder="插件 zip 下载地址（http/https）"
              size="small"
              class="plg-input mono"
              allow-clear
            />
            <a-button size="small" :loading="installing" @click="installFromURL">
              <template #icon><IconDownload /></template>从 URL 安装
            </a-button>
          </div>
          <div class="plg-install-hint">
            插件为独立进程通过 MCP 协议接入（实现语言不限），完全隔离于主工程。启用后自动注册
            mcp_&lt;插件&gt;_&lt;工具&gt; 工具；开关下一任务生效。
          </div>
        </div>
      </div>

      <!-- 已安装列表 -->
      <div class="plg-section">
        <div class="plg-section-title">已安装插件（{{ plugins.length }}）</div>
        <div v-if="!plugins.length" class="plg-empty empty-block">
          <div class="empty-ico-chip"><IconApps /></div>
          <div class="empty-block-title">暂无插件</div>
          <div class="empty-block-hint">从上方安装，或前往「插件市场」下载官方插件</div>
        </div>
        <div v-for="p in plugins" :key="p.id" class="plg-card">
          <div class="plg-card-head">
            <div class="plg-card-info">
              <span class="plg-card-name">{{ p.name || p.id }}</span>
              <span v-if="p.version" class="plg-pill">v{{ p.version }}</span>
              <span v-if="p.mcpInfo" class="plg-pill plg-pill-blue">{{ p.mcpInfo }}</span>
              <span v-if="p.skillCount" class="plg-pill plg-pill-green">{{ p.skillCount }} 技能</span>
            </div>
            <div class="plg-card-actions">
              <a-switch v-model="p.enabled" size="small" :disabled="uninstalling === p.id" @change="(v) => togglePlugin(p, v)" />
              <a-button size="mini" status="danger" :loading="uninstalling === p.id" @click="uninstallPlugin(p)">卸载</a-button>
            </div>
          </div>
          <div class="plg-card-desc">{{ p.description || "（无描述）" }}</div>
          <div class="plg-card-meta">
            <span>ID: {{ p.id }}</span>
            <span v-if="p.author" style="margin-left: 8px">· {{ p.author }}</span>
            <span v-if="p.source" style="margin-left: 8px">· 来源: {{ p.source }}</span>
          </div>
        </div>
      </div>
    </template>

    <!-- ══════════ 插件市场（远程服务端市场：插件/技能/MCP 三类条目）══════════ -->
    <template v-else>
      <div class="plg-section">
        <div class="plg-section-title">
          插件市场（{{ market.length }}）
          <a-button size="mini" style="margin-left: 8px" :loading="marketLoading" @click="loadMarket">刷新</a-button>
        </div>
        <div v-if="!market.length" class="plg-empty empty-block">
          <div class="empty-ico-chip"><IconDownload /></div>
          <div class="empty-block-title">市场暂无条目</div>
          <div class="empty-block-hint">未配置远程服务时展示内置目录；可在技能面板/设置页将技能与 MCP 发布到市场</div>
        </div>
        <div v-for="item in market" :key="item.id" class="plg-card">
          <div class="plg-card-head">
            <div class="plg-card-info">
              <span class="plg-card-name">{{ item.name || item.id }}</span>
              <a-tag size="small" :color="kindColor(item.kind)">{{ kindLabel(item.kind) }}</a-tag>
              <span v-if="item.version" class="plg-pill">v{{ item.version }}</span>
              <span v-if="item.installed" class="plg-pill plg-pill-green">
                <IconCheckCircle style="margin-right: 2px" />已安装
              </span>
            </div>
            <div class="plg-card-actions">
              <a-button
                size="mini"
                type="primary"
                :disabled="item.installed"
                :loading="installing === item.id"
                @click="installMarket(item)"
              >
                <template #icon><IconDownload /></template>
                {{ item.installed ? "已安装" : "下载安装" }}
              </a-button>
            </div>
          </div>
          <div class="plg-card-desc">{{ item.description || "（无描述）" }}</div>
          <div class="plg-card-meta">
            <span>ID: {{ item.id }}</span>
            <span v-if="item.author" style="margin-left: 8px">· {{ item.author }}</span>
            <span v-if="item.publishedBy" style="margin-left: 8px">· 发布者: {{ item.publishedBy }}</span>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
	.plg-panel {
	  display: flex;
	  flex-direction: column;
	  height: 100%;
	  overflow: hidden;
	  background: var(--bg-page);
	  padding: 16px 20px;
	  box-sizing: border-box;
	}

.plg-head { flex-shrink: 0; padding-left: 0; padding-right: 0; border-bottom: none; margin-bottom: 2px; }
		.plg-head-title {
		  display: flex;
		  align-items: center;
		  gap: 8px;
		  min-width: 0;
		}

.plg-tabs {
  display: flex;
  gap: 4px;
  border-bottom: 1px solid var(--border-weak);
  margin-bottom: 14px;
  flex-shrink: 0;
}
	.plg-tab {
	  padding: 8px 12px;
	  border: none;
	  background: transparent;
	  color: var(--text-3);
	  font-size: var(--fs-caption);
	  cursor: pointer;
  border-bottom: 2px solid transparent;
  display: flex;
  align-items: center;
  gap: 4px;
}
.plg-tab.active {
  color: var(--primary);
  border-bottom-color: var(--primary);
  font-weight: 500;
}
	.plg-tab-count {
	  background: var(--primary-tint);
	  color: var(--primary);
	  border-radius: 9px;
		  font-size: var(--fs-micro);
		  padding: 0 6px;
		  line-height: 18px;
		}

.plg-section {
  margin-bottom: 20px;
  flex-shrink: 0;
}
	.plg-section-title {
	  font-size: var(--fs-caption);
	  font-weight: 500;
	  color: var(--text-2);
	  margin-bottom: 10px;
	}

.plg-install-card {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 12px;
}
.plg-install-row {
  display: flex;
  gap: 8px;
  align-items: center;
}
.plg-input {
  flex: 1;
}
	.plg-input.mono :deep(input) {
	  font-family: var(--font-mono);
	  font-size: var(--fs-mono);
	}
	.plg-install-hint {
	  margin-top: 10px;
	  font-size: var(--fs-caption);
	  color: var(--text-3);
	  line-height: 1.7;
	}

.plg-card {
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: var(--radius);
  padding: 12px 14px;
  margin-bottom: 10px;
  transition: var(--transition);
}
.plg-card:hover {
  border-color: var(--primary);
  transform: translateY(-1px);
  box-shadow: var(--shadow-sm);
}
.plg-card:active { transform: translateY(0); }
.plg-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.plg-card-info {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  min-width: 0;
}
.plg-card-name {
  font-size: var(--fs-body);
  font-weight: 500;
  color: var(--text-1);
}
.plg-pill {
  font-size: var(--fs-micro);
  color: var(--text-3);
  background: var(--bg-card);
  border: 1px solid var(--border-weak);
  border-radius: 9px;
  padding: 0 6px;
  line-height: 18px;
}
.plg-pill-blue {
  color: var(--primary);
  border-color: var(--primary-tint);
}
.plg-pill-green {
  color: var(--success);
  border-color: var(--success-tint);
}
.plg-card-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}
.plg-card-desc {
  margin-top: 8px;
  font-size: var(--fs-caption);
  color: var(--text-2);
  line-height: 1.6;
}
.plg-card-meta {
  margin-top: 6px;
  font-size: var(--fs-micro);
  color: var(--text-3);
}

.plg-empty.empty-block {
  padding: 32px 16px;
}
.plg-empty.empty-block .empty-ico-chip { margin: 0 auto 10px; }

/* 插件市场滚动区：允许内部滚动 */
.plg-panel > .plg-tabs + .plg-section,
.plg-panel > .plg-section + .plg-section {
  flex-shrink: 0;
}
</style>
