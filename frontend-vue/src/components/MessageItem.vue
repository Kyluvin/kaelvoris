<script setup>
// 单条消息渲染：按 kind 分派（user/assistant/reasoning/tool/tag/error/image/video/audio/diff/done）
import { computed, ref } from "vue";
import { Message } from "@arco-design/web-vue";
import { IconCopy, IconBulb, IconDown, IconCloseCircle, IconCheckCircleFill } from "@arco-design/web-vue/es/icon";
import { renderMarkdownHtml, parseDiffLines, parseToolMessage } from "../utils/markdown.js";
import { fmtMsgStamp } from "../utils/format.js";
import { bridgeCall } from "../bridge/wails.js";
const props = defineProps({ msg: { type: Object, required: true } });

const toolOpen = ref(false);
const reasoningOpen = ref(false);

// 媒体消息（video/audio）：text 为 MediaPayload JSON 载荷（kind/path/fileName/mime）。
// 实时推送用消息 Images[0]（data URL）直播；历史回放只有 text 里的磁盘路径，
// 经 /kaelvoris-media/ 路由回挂（main.go 白名单目录内静态回放）。
const mediaInfo = computed(() => {
  if (props.msg.kind !== "video" && props.msg.kind !== "audio") return null;
  try {
    const arr = JSON.parse(props.msg.text || "[]");
    return Array.isArray(arr) && arr.length ? arr[0] : null;
  } catch { return null; }
});
const mediaPath = computed(() => (mediaInfo.value && mediaInfo.value.path) || props.msg.text || "");
const mediaMime = computed(() => (mediaInfo.value && mediaInfo.value.mime) || "");
const isGif = computed(() => mediaMime.value === "image/gif");
const mediaSrc = computed(() => {
  if (props.msg.src) return props.msg.src; // data URL（实时推送）
  const p = mediaPath.value;
  if (!p) return "";
  return "/kaelvoris-media/" + encodeURIComponent(p);
});

// 图片卡磁盘路径：实时/历史消息 text 均为载荷 JSON [{path,fileName}]。
const imagePath = computed(() => {
  if (props.msg.path) return props.msg.path;
  try {
    const arr = JSON.parse(props.msg.text || "[]");
    return Array.isArray(arr) && arr[0] && arr[0].path ? arr[0].path : "";
  } catch { return ""; }
});

function previewImage(src) {
  const div = document.createElement("div");
  div.className = "lightbox-backdrop";
  div.innerHTML = '<div class="lightbox"><img src="' + src + '"></div>';
  div.addEventListener("click", () => div.remove());
  document.body.appendChild(div);
}

// 媒体「另存为」：弹系统保存对话框把生成产物复制到用户选定路径
//（SaveFileAs：白名单校验 + 系统保存对话框；取消静默返回）。
function saveAs(p, label) {
  if (!p) return;
  bridgeCall("SaveFileAs", p)
    .then(() => Message.success(label || "已另存"))
    .catch((err) => Message.error(String((err && err.message) || err)));
}

// 媒体卡「打开」：用系统默认程序打开原图/原片。
function openMedia(p) {
  if (!p) return;
  bridgeCall("OpenFileWithDefault", p)
    .then(() => Message.success("已用系统程序打开"))
    .catch((err) => Message.error(String((err && err.message) || err)));
}

const mdHtml = computed(() => renderMarkdownHtml(props.msg.text));
const isStreaming = computed(() => !!props.msg.streaming);

// 消息时间戳（ns/ms 双兼容，空值静默）与头像字符。
const stamp = computed(() => fmtMsgStamp(props.msg.ts));
const avatarChar = computed(() => (props.msg.kind === "user" ? "你" : "H"));

// 代码块复制：v-html 内无法绑定 Vue 事件，统一在根节点委托 data-code-copy 点击；
// 文件链接（data-file-link）点击：统一用系统默认程序打开（2026-11 用户反馈
// 「老是自动用 Zed 打开文件」已移除自动拉起 Zed 的行为；line 参数保留自
// markdown 渲染层但不再用于分派编辑器）。
// 行内图片（data-file-img）点击 → OpenFileWithDefault 打开原图。
function onMdClick(e) {
  const fl = e.target.closest ? e.target.closest("[data-file-link]") : null;
  if (fl) {
    e.preventDefault();
    const path = fl.getAttribute("data-file-link") || "";
    bridgeCall("OpenFileWithDefault", path)
      .then(() => Message.success("已用系统程序打开: " + path))
      .catch((err) => Message.error(String((err && err.message) || err)));
    return;
  }
  const img = e.target.closest ? e.target.closest("[data-file-img]") : null;
  if (img) {
    e.preventDefault();
    const p = img.getAttribute("data-file-img") || "";
    if (!p) return; // 外链图片不响应打开
    bridgeCall("OpenFileWithDefault", p)
      .then(() => Message.success("已用系统程序打开: " + p))
      .catch((err) => Message.error(String((err && err.message) || err)));
    return;
  }
  const btn = e.target.closest ? e.target.closest("[data-code-copy]") : null;
  if (!btn) return;
  const wrap = btn.closest(".md-code");
  const code = wrap ? wrap.querySelector("pre code") : null;
  if (!code) return;
  copyText(code.textContent || "");
}
const toolInfo = computed(() => {
  if (props.msg.kind !== "tool") return null;
  if (props.msg.name) return props.msg;
  return parseToolMessage(props.msg.text);
});

const diffLines = computed(() => (props.msg.diff ? parseDiffLines(props.msg.diff) : []));

async function copyText(t) {
  try {
    await navigator.clipboard.writeText(t);
    Message.success("已复制");
  } catch { Message.error("复制失败"); }
}

// 消息级复制（用户气泡 / 助手正文悬停按钮）：文本 + 行内图片同时写入剪贴板。
// 图片经 ClipboardItem(image/png) 写入（浏览器仅支持 image/png：gif/webp 等先
// 转 png；写图失败静默降级为纯文本，不影响复制主流程）。
async function copyMessage(msg) {
  const t = msg.text || "";
  try {
    await navigator.clipboard.writeText(t);
  } catch { Message.error("复制失败"); return; }
  const imgs = msg.images || [];
  for (const src of imgs) {
    try {
      const blob = await imgSrcToPngBlob(src);
      await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
    } catch { /* 图片写剪贴板失败（非 png/权限）：静默，文本已复制 */ }
  }
  Message.success(imgs.length ? "已复制文本和图片" : "已复制");
}

// data URL / http 图片 → PNG Blob（画布重绘转码）。
function imgSrcToPngBlob(src) {
  return new Promise((resolve, reject) => {
    const im = new Image();
    im.onload = () => {
      try {
        const c = document.createElement("canvas");
        c.width = im.naturalWidth; c.height = im.naturalHeight;
        c.getContext("2d").drawImage(im, 0, 0);
        c.toBlob((b) => (b ? resolve(b) : reject(new Error("toBlob failed"))), "image/png");
      } catch (e) { reject(e); }
    };
    im.onerror = reject;
    im.src = src;
  });
}
</script>

<template>
  <!-- 用户消息：右对齐暖色气泡 + 头像 + 时间戳；hover 出复制按钮（文本+图片一并复制） -->
  <div v-if="msg.kind === 'user'" class="msg msg-user">
    <div class="msg-copy-wrap">
      <div class="msg-user-bubble">
        <div>{{ msg.text }}</div>
        <div v-if="msg.images && msg.images.length" class="msg msg-image" style="margin-top: 6px">
          <img v-for="(src, i) in msg.images" :key="i" :src="src" @click="previewImage(src)" />
        </div>
      </div>
      <button class="msg-copy-btn" title="复制消息" @click="copyMessage(msg)"><IconCopy /></button>
    </div>
    <div class="msg-meta">
      <span v-if="stamp" class="msg-time">{{ stamp }}</span>
      <span class="msg-avatar msg-avatar-user">{{ avatarChar }}</span>
    </div>
  </div>

  <!-- 助手正文（Markdown）：左对齐 + 头像 + 时间戳；md-body 承载代码块复制委托 -->
  <div
    v-else-if="msg.kind === 'assistant'"
    class="msg msg-assistant"
    :class="{ streaming: msg.streaming }"
  >
    <span class="msg-avatar msg-avatar-bot">{{ avatarChar }}</span>
    <div class="msg-assistant-main msg-copy-host">
      <div v-if="stamp" class="msg-time msg-time-above">{{ stamp }}</div>
      <div class="msg-assistant-card md-body" v-html="mdHtml" @click="onMdClick"></div>
      <!-- 流式生成进度：细进度条（视觉提示，reduced-motion 时静止） -->
      <div v-if="msg.streaming" class="msg-progress" aria-hidden="true"></div>
      <!-- 消息复制（非流式）：悬停卡右上角浮现 -->
      <button v-if="!msg.streaming" class="msg-copy-btn msg-copy-float" title="复制消息" @click="copyMessage(msg)"><IconCopy /></button>
    </div>
  </div>

  <!-- 思考过程（可折叠） -->
  <div v-else-if="msg.kind === 'reasoning'" class="msg msg-reasoning">
    <div class="reasoning-head" @click="reasoningOpen = !reasoningOpen">
      <IconBulb class="head-icon" />
      <span>思考过程</span>
      <span v-if="msg.streaming" class="reasoning-hint">流式中…</span>
      <IconDown class="chev" :class="{ open: reasoningOpen || msg.streaming }" />
    </div>
    <div v-show="reasoningOpen || msg.streaming" class="reasoning-body">{{ msg.text }}</div>
  </div>

  <!-- 工具调用（可折叠） -->
  <div v-else-if="msg.kind === 'tool'" class="msg msg-tool">
    <div class="tool-head" @click="toolOpen = !toolOpen">
      <span class="tool-name">{{ toolInfo.name }}</span>
      <span class="tool-summary">{{ toolInfo.summary }}</span>
      <IconDown class="chev" :class="{ open: toolOpen }" />
    </div>
    <div v-if="toolOpen" class="tool-body">
      <div v-if="toolInfo.diff">
        <div v-for="(l, i) in parseDiffLines(toolInfo.diff)" :key="i" class="diff-line" :class="l.cls">{{ l.text }}</div>
      </div>
      <pre v-else-if="toolInfo.args" class="diff-plain">{{ toolInfo.args }}</pre>
    </div>
  </div>

  <!-- 系统标签（子任务 / API / 模式切换等） -->
  <div v-else-if="msg.kind === 'tag'" class="msg msg-tag">
    <span class="tag-badge">{{ msg.badge }}</span>
    <span class="tag-text">{{ msg.text }}</span>
  </div>

  <!-- 错误 -->
  <div v-else-if="msg.kind === 'error'" class="msg msg-error"><IconCloseCircle class="err-icon" /> {{ msg.text }}</div>

  <!-- 任务完成 -->
  <div v-else-if="msg.kind === 'done'" class="msg msg-done">
    <div class="done-head">
      <span class="done-badge"><IconCheckCircleFill /> 任务完成</span>
      <a-button size="mini" type="text" @click="copyText(msg.text)">
        <template #icon><IconCopy /></template>
        复制
      </a-button>
    </div>
    <div class="md-body" v-html="mdHtml" @click="onMdClick"></div>
  </div>

		  <!-- 图片卡片 -->
		  <div v-else-if="msg.kind === 'image'" class="msg msg-image">
		    <img
		      v-if="msg.src"
		      :src="msg.src"
		      @click="previewImage(msg.src)"
		    />
		    <div v-else class="msg-tag"><span class="tag-badge">图片</span>{{ imagePath || msg.text }}</div>
		    <div v-if="imagePath" class="media-actions">
		      <a-button size="mini" type="text" @click="saveAs(imagePath)">另存为</a-button>
		      <a-button size="mini" type="text" @click="openMedia(imagePath)">打开</a-button>
		    </div>
		  </div>

	  <!-- 视频卡片（generate_video）：优先 data URL 直播，回放经 /kaelvoris-media/ 回挂；
	       GIF 降级产物（视频模型未配置时本地合成）用 <img> 渲染（<video> 播不了 GIF） -->
	  <div v-else-if="msg.kind === 'video'" class="msg msg-media">
	    <img
      v-if="isGif"
	      :src="mediaSrc"
	      @click="previewImage(mediaSrc)"
	    />
	    <video
      v-else-if="mediaSrc"
	      :src="mediaSrc"
	      :type="mediaMime"
	      controls
	      preload="metadata"
	      playsinline
	    ></video>
		    <div v-else class="msg-tag"><span class="tag-badge">视频</span>{{ mediaPath }}</div>
		    <div v-if="mediaPath" class="media-actions">
		      <a-button size="mini" type="text" @click="saveAs(mediaPath)">另存为</a-button>
		      <a-button size="mini" type="text" @click="openMedia(mediaPath)">打开</a-button>
		    </div>
		  </div>

		  <!-- 音频卡片（generate_speech）：同上，<audio> 播放器 -->
		  <div v-else-if="msg.kind === 'audio'" class="msg msg-media">
		    <audio
		      v-if="mediaSrc"
		      :src="mediaSrc"
		      controls
		      preload="metadata"
		    ></audio>
		    <div v-else class="msg-tag"><span class="tag-badge">音频</span>{{ mediaPath }}</div>
		    <div v-if="mediaPath" class="media-actions">
		      <a-button size="mini" type="text" @click="saveAs(mediaPath)">另存为</a-button>
		    </div>
		  </div>

  <!-- 会话总结卡（会话 hook 产出；生机入流尾部） -->
  <div v-else-if="msg.kind === 'summary'" class="msg msg-summary">
    <div class="summary-head">
      <span class="summary-badge"><IconBulb /> 会话总结</span>
      <a-button size="mini" type="text" @click="copyText(msg.text)">
        <template #icon><IconCopy /></template>
        复制
      </a-button>
    </div>
    <div class="md-body" v-html="mdHtml" @click="onMdClick"></div>
  </div>

  <!-- turn_diff 折叠卡 -->
  <div v-else-if="msg.kind === 'diff'" class="msg msg-tool">
    <div class="tool-head" @click="toolOpen = !toolOpen">
      <span class="tool-name">本轮变更</span>
      <span class="tool-summary">文件变更净摘要</span>
      <IconDown class="chev" :class="{ open: toolOpen }" />
    </div>
    <div v-if="toolOpen" class="tool-body">
      <div v-for="(l, i) in parseDiffLines(msg.text)" :key="i" class="diff-line" :class="l.cls">{{ l.text }}</div>
    </div>
  </div>
</template>

<script>
export default { name: "MessageItem" };
</script>

<style>
/* 图片放大预览（lightbox）全局样式 */
.lightbox-backdrop {
  position: fixed; inset: 0; z-index: 1000;
  background: rgba(0, 0, 0, 0.72);
  display: flex; align-items: center; justify-content: center;
}
		.lightbox img {
		  max-width: 88vw; max-height: 88vh;
		  border-radius: var(--radius);
		}
		.msg-media video {
		  max-width: 100%; max-height: 60vh; border-radius: var(--radius);
		  background: #000;
		}
		.msg-media img {
		  max-width: 100%; max-height: 60vh; border-radius: var(--radius);
		}
		.msg-media audio {
		  width: 100%; max-width: 480px;
		}
</style>

<style scoped>
/* ── 消息复制（用户气泡 / 助手正文悬停浮现）───────────────── */
.msg-copy-wrap { display: flex; align-items: flex-end; gap: 4px; max-width: 76%; }
.msg-copy-wrap .msg-user-bubble { max-width: 100%; }
/* 助手主体相对定位：浮动复制按钮锚定右上角 */
.msg-copy-host { position: relative; }
.msg-copy-btn {
  border: none; background: transparent; color: var(--text-3);
  cursor: pointer; padding: 3px; line-height: 0; border-radius: var(--radius-sm);
  opacity: 0; transition: opacity var(--transition), color var(--transition), background var(--transition);
  flex-shrink: 0;
}
.msg-copy-btn:hover { color: var(--text-1); background: var(--bg-active); }
/* 悬停所在行/卡才显现（触屏无 hover：保持隐藏不误触） */
.msg:hover .msg-copy-btn,
.msg-copy-host:hover .msg-copy-float { opacity: 1; }
.msg-copy-float { position: absolute; top: 0; right: 0; z-index: 5; background: var(--bg-card); box-shadow: var(--ring); }

/* 媒体卡操作行（另存为/打开）：右对齐细按钮 */
.media-actions {
  display: flex; gap: 4px; justify-content: flex-end;
  margin-top: 2px;
}
/* 流式中：卡体微降透明度 + 末尾呼吸光标（生成中的位置指示；
   reduced-motion 时光标静止不闪烁，仅保留静态可见性） */
.streaming {
  opacity: 0.92;
}
.streaming .md-body > :last-child::after {
  content: "▍";
  margin-left: 2px;
  color: var(--primary);
  animation: cursor-blink 1s steps(2, start) infinite;
}
@keyframes cursor-blink {
  to { visibility: hidden; }
}
@media (prefers-reduced-motion: reduce) {
  .streaming .md-body > :last-child::after {
    animation: none;
  }
}
.head-icon {
  color: var(--text-3);
  font-size: 14px;
  flex-shrink: 0;
}
.chev {
  margin-left: auto;
  color: var(--text-3);
  font-size: 12px;
  transition: transform var(--transition);
}
.chev.open { transform: rotate(180deg); }
.err-icon {
  color: var(--danger);
  vertical-align: -2px;
  margin-right: 4px;
}
.done-badge .arco-icon {
  vertical-align: -2px;
}
.msg-image img {
  max-width: 320px;
  border-radius: var(--radius);
  border: 1px solid var(--border-weak);
  cursor: zoom-in;
}
.msg-summary {
  margin: 12px 0;
  padding: 12px 14px;
  background: var(--bg-inset);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.summary-head {
  display: flex; align-items: center; justify-content: space-between;
  margin-bottom: 8px;
}
.summary-badge {
  display: inline-flex; align-items: center; gap: 4px;
  color: var(--primary); font-size: var(--fs-caption); font-weight: 600;
}
</style>
