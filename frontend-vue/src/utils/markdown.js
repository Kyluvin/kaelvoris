	// ═════════════════════════════════════════════════════════════════
	// Markdown 渲染：复用原版 Kaelvoris 的轻量解析器（无外部依赖，与
	// 后端消息格式完全兼容）。输出 Vue 可用的 HTML 字符串。
	// 安全性：所有动态片段均经 escapeHtml 转义后拼接，无 raw 注入。
	// ═════════════════════════════════════════════════════════════════

	function esc(s) {
	  return String(s ?? "")
	    .replace(/&/g, "&amp;")
	    .replace(/</g, "&lt;")
	    .replace(/>/g, "&gt;")
	    .replace(/"/g, "&quot;");
	}

	// 解析文件链接：两种形态——
	//   1. 代码定位链接 `path:line`（可带 `:col`）→ {path, line, column}；
	//   2. 纯文件链接（无行号但带扩展名，如 报告.pptx / 图片.png）→
	//      {path, line: 0, column: null}（line 0 = 无行号，点击按扩展名分派）。
	// 兼容 Wails WebView 下被补全的 `http://wails.localhost/...` 前缀；协议型
	// 外链返回 null。路径无需转义（行内已对 & < > " 转义）。
	export function parseFileUrl(url) {
	  let u = String(url || "").trim();
	  if (!u) return null;
	  // Wails WebView 相对链接补全前缀（用户实测格式）。
	  const mlocal = u.match(/^https?:\/\/wails\.localhost\/(.+)$/i);
	  if (mlocal) u = mlocal[1];
	  const m = u.match(/^(.+?):(\d{1,6})(?::(\d{1,6}))?$/);
	  if (m) {
	    // 协议型链接（https://example.com/page.html:62）非文件定位。
	    if (/^[a-z][a-z0-9+.-]*:\/\//i.test(m[1])) return null;
	    return { path: m[1], line: parseInt(m[2], 10), column: m[3] ? parseInt(m[3], 10) : null };
	  }
	  // 无行号形态：末段带 1-5 位字母数字扩展名即视为本地文件链接
	  //（如 a.md / 报告.pptx / img.png / file.tar.gz），协议型/锚点除外。
	  if (/^[a-z][a-z0-9+.-]*:/i.test(u)) return null; // mailto: https: 等协议型
	  const mf = u.match(/^(.+\.[A-Za-z0-9]{1,10})$/);
	  if (!mf) return null;
	  // 无斜杠的裸单词（如 README）不算文件链接，必须有路径或扩展名分隔。
	  if (!/[\\/.]/.test(mf[1]) && !/\.[A-Za-z0-9]{1,10}$/.test(mf[1].split(/[\\/]/).pop() || "")) return null;
	  return { path: mf[1], line: 0, column: null };
	}

	// 媒体/文档类扩展名表（isMediaExt）：历史上用于区分 Zed/系统程序打开，
	// 现文件链接点击已统一走系统默认程序（不再自动拉 Zed），保留导出供
	// 渲染层语义判断（如需要区分媒体/代码类展示样式）。
	export function isMediaExt(ext) {
	  const e = String(ext || "").toLowerCase().replace(/^\./, "");
	  if (!e) return false;
	  return "png jpg jpeg gif webp bmp svg ico avif heic mp4 mov avi mkv webm mp3 wav ogg flac m4a aac " +
	    "ppt pptx doc docx xls xlsx pdf csv tsv rtf zip 7z rar tar gz".split(" ").includes(e);
	}

	// 文件扩展名提取（a.b.c 取末段；无扩展名返回空串）。
	export function fileExt(path) {
	  const base = String(path || "").split(/[\\/]/).pop() || "";
	  const m = base.match(/\.([A-Za-z0-9]{1,10})$/);
	  return m ? m[1].toLowerCase() : "";
	}

	// 行内：`code` **bold** *italic* [text](url) ![alt](path)
	// 文件链接识别：`[text](path:line)` 与 `[text](file.ext)` 渲染为
	// data-file-link 锚点（点击 → 统一系统默认程序打开，不再自动拉 Zed；
	// 图片语法 `![alt](file)` 渲染为可点击缩略图（点击 →
	// 系统默认程序打开原图，data-file-img 标记路径）；其余链接保持普通外链。
	function inline(s) {
	  let out = esc(s);
	  out = out.replace(/`([^`]+)`/g, '<code class="md-code-inline">$1</code>');
	  out = out.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
	  out = out.replace(/\*([^*\n]+)\*/g, "<em>$1</em>");
	  // 图片：![alt](src) → 缩略图（点击经 data-file-img 打开原图）。
	  out = out.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (m, alt, src) => {
	    return '<img class="md-inline-img" src="' + src + '" alt="' + alt + '" data-file-img="' + esc(inlineFileSrc(src)) + '" loading="lazy">';
	  });
	  out = out.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, label, url) => {
	    const fi = parseFileUrl(url);
	    if (fi) {
	      return '<a href="#" data-file-link="' + esc(fi.path) + '" data-file-line="' + fi.line + '" data-file-column="' + (fi.column || "") + '">' + label + "</a>";
	    }
	    return '<a href="' + url + '" target="_blank" rel="noopener">' + label + "</a>";
	  });
	  return out;
	}

	// inlineFileSrc 行内图片 src → 磁盘路径：/kaelvoris-image/、/kaelvoris-media/
	// 回放路由内的路径（encodeURIComponent 编入）还原为绝对路径；工作区
	// 相对路径原样返回（后端按工作区拼接）。协议型外链返回空串（不响应点击）。
	function inlineFileSrc(src) {
	  const s = String(src || "").trim();
	  const m = s.match(/^\/(?:kaelvoris-image|kaelvoris-media)\/(.+)$/i);
	  if (m) {
	    try { return decodeURIComponent(m[1]); } catch { return ""; }
	  }
	  if (/^[a-z][a-z0-9+.-]*:/i.test(s) || /^\/\//.test(s)) return ""; // 外链不点击
	  return s;
	}

	export function renderMarkdownHtml(text) {
	  const lines = String(text || "").split("\n");
	  const html = [];
	  let para = [];
	  const flushPara = () => {
	    if (!para.length) return;
	    html.push("<p>" + para.map(inline).join("<br>") + "</p>");
	    para = [];
	  };
	  for (let i = 0; i < lines.length; i++) {
	    const line = lines[i];
	    const fence = line.match(/^\s*```(\w*)\s*$/);
	    if (fence) {
	      para = [];
	      const lang = (fence[1] || "").toLowerCase();
	      const body = [];
	      for (i++; i < lines.length && !/^\s*```\s*$/.test(lines[i]); i++) body.push(lines[i]);
	      // 代码块卡片：头部（语言标签 + 复制按钮）+ 深色高亮正文；
	      // 原文存 data-code 属性（esc 后浏览器解析实体还原），复制时直接读取。
	      const raw = body.join("\n");
	      html.push(
	        '<div class="md-code">' +
	          '<div class="md-code-head"><span class="md-code-lang">' + esc(langLabel(lang)) + "</span>" +
	          '<button type="button" class="md-code-copy" data-code-copy>复制</button></div>' +
	          '<pre class="md-pre"><code>' + highlightCode(raw, lang) + "</code></pre>" +
	        "</div>",
	      );
	      continue;
	    }
	    if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) { flushPara(); html.push("<hr>"); continue; }
	    const h = line.match(/^(#{1,6})\s+(.*)$/);
	    if (h) {
	      flushPara();
	      const lv = Math.min(h[1].length + 3, 6);
	      html.push(`<h${lv}>` + inline(h[2]) + `</h${lv}>`);
	      continue;
	    }
	    // 表格：表头行 + |---| 分隔行（与移动端/管理端同一解析约定）。
	    if (/^\s*\|.*\|\s*$/.test(line)) {
	      const next = lines[i + 1] || "";
	      if (/^\s*\|[\s:|-]+\|?\s*$/.test(next) && /-/.test(next.replace(/\|/g, ""))) {
	        flushPara();
	        const cells = (l) => l.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
	        let t = "<table><thead><tr>";
	        for (const c of cells(line)) t += "<th>" + inline(c) + "</th>";
	        t += "</tr></thead><tbody>";
	        let j = i + 2;
	        for (; j < lines.length && /^\s*\|.*\|\s*$/.test(lines[j]); j++) {
	          t += "<tr>";
	          for (const c of cells(lines[j])) t += "<td>" + inline(c) + "</td>";
	          t += "</tr>";
	        }
	        t += "</tbody></table>";
	        html.push(t);
	        i = j - 1;
	        continue;
	      }
	    }
	    if (/^\s*[-*+]\s+/.test(line)) {
	      flushPara();
	      const items = [];
	      for (; i < lines.length && /^\s*[-*+]\s+/.test(lines[i]); i++) items.push("<li>" + inline(lines[i].replace(/^\s*[-*+]\s+/, "")) + "</li>");
	      i--;
	      html.push("<ul>" + items.join("") + "</ul>");
	      continue;
	    }
	    if (/^\s*\d+[.)]\s+/.test(line)) {
	      flushPara();
	      const items = [];
	      for (; i < lines.length && /^\s*\d+[.)]\s+/.test(lines[i]); i++) items.push("<li>" + inline(lines[i].replace(/^\s*\d+[.)]\s+/, "")) + "</li>");
	      i--;
	      html.push("<ol>" + items.join("") + "</ol>");
	      continue;
	    }
	    if (/^>\s?/.test(line)) {
	      flushPara();
	      const content = [];
	      for (; i < lines.length && /^>\s?/.test(lines[i]); i++) content.push(lines[i].replace(/^>\s?/, ""));
	      i--;
	      html.push("<blockquote>" + inline(content.join(" ")) + "</blockquote>");
	      continue;
	    }
	    if (line.trim() === "") { flushPara(); continue; }
	    para.push(line);
	  }
	  flushPara();
	  return html.join("");
	}

	// ---------- 代码块深色高亮 + 复制按钮支撑 ----------

	// langLabel 语言显示名（未识别语言显示原文大写）。
	function langLabel(lang) {
	  const map = { js: "JavaScript", jsx: "JSX", ts: "TypeScript", tsx: "TSX", go: "Go", py: "Python", python: "Python",
	    sh: "Shell", bash: "Shell", shell: "Shell", powershell: "PowerShell", ps1: "PowerShell", json: "JSON",
	    html: "HTML", css: "CSS", vue: "Vue", sql: "SQL", yml: "YAML", yaml: "YAML", md: "Markdown", java: "Java",
	    c: "C", cpp: "C++", cs: "C#", rs: "Rust", rust: "Rust", toml: "TOML", ini: "INI", diff: "Diff" };
	  if (!lang) return "文本";
	  return map[lang] || lang.toUpperCase();
	}

	// 轻量语法高亮（无依赖）：字符串 → 注释 → 关键字/数字 四类 token，
	// 输出 span.hl-*。输入为未转义原文，内部先 esc 再分词；识别失败的极端
	// 情形（正则异常等）整体降级为转义纯文本，不影响可读性。
	// 注释族按语言区分：clike(// /* */) / hash(#) / sql(--)，避免 CSS 色值等误判。
	const HL_STR_RE = /&quot;[^&]*?&quot;|`[^`\n]*`/g;
	const HL_CMT = {
	  clike: /\/\*[\s\S]*?\*\/|\/\/[^\n]*/g,
	  hash: /#[^\n]*/g,
	  sql: /\/\*[\s\S]*?\*\/|--[^\n]*/g,
	};
	const HL_CMT_FAMILY = {
	  py: "hash", python: "hash", rb: "hash", ruby: "hash", sh: "hash", bash: "hash", shell: "hash",
	  yml: "hash", yaml: "hash", toml: "hash", ini: "hash", conf: "hash", makefile: "hash", dockerfile: "hash",
	  sql: "sql",
	};
	// 通用关键字集合（多语言并集，词边界匹配；Go 单列）。
	const HL_KW = new Set(("function return if else for while do switch case break continue new class struct interface " +
	  "extends implements import export from as try catch finally throw throws def self public private protected static " +
	  "final const let var type void int string bool boolean float double char byte short long true false nil null " +
	  "undefined async await yield package defer chan select range make func default in of not and or is " +
	  "this super namespace using enum union sizeof lambda with pass raise elif println len append delete where " +
	  "insert into update values select begin end then elsif endif foreach").split(" "));
	const HL_KW_GO = new Set(("package import func var const type struct interface map chan go defer select range " +
	  "if else for switch case default break continue return goto fallthrough string int int8 int16 int32 int64 uint " +
	  "uint8 uint16 uint32 uint64 uintptr byte rune float32 float64 bool complex64 complex128 error any true false " +
	  "nil make new len cap append copy delete panic recover iota").split(" "));

	// 索引 ↔ 纯字母编码：占位符内不得出现数字/符号，否则被关键字/数字分词破坏。
	function enc26(n) {
	  let s = "";
	  n = Number(n) + 1;
	  while (n > 0) { s = String.fromCharCode(64 + ((n - 1) % 26) + 1) + s; n = Math.floor((n - 1) / 26); }
	  return s;
	}
	function dec26(s) {
	  let n = 0;
	  for (const ch of s) n = n * 26 + (ch.charCodeAt(0) - 64);
	  return n - 1;
	}

	function highlightCode(src, lang) {
	  const out = esc(src);
	  try {
	    if (lang === "diff") return highlightDiff(out);
	    return lang === "go" ? tokenize(out, HL_KW_GO, "clike") : tokenize(out, HL_KW, HL_CMT_FAMILY[lang] || "clike");
	  } catch { return out; }
	}

	// tokenize 分词主流程：字符串占位 → 注释占位 → 关键字/数字 → 回填。
	function tokenize(out, kwSet, cmtFamily) {
	  // 1) 字符串（先于注释：注释里的 URL/引号不被注释规则吞；引号内容先保护）
	  const strSpans = [];
	  let o = out.replace(HL_STR_RE, (m) => { strSpans.push(m); return "\u0000K" + enc26(strSpans.length - 1) + "J\u0000"; });
	  // 2) 注释（按语言族；占位符内无注释起始符，不会被二次命中）
	  const cmtSpans = [];
	  const cmtRe = cmtFamily === "hash" ? HL_CMT.hash : cmtFamily === "sql" ? HL_CMT.sql : HL_CMT.clike;
	  o = o.replace(cmtRe, (m) => { cmtSpans.push(m); return "\u0000K" + enc26(cmtSpans.length - 1) + "H\u0000"; });
	  // 3) 关键字 + 数字（占位符为纯字母组合，不在关键字集，原样保留）
	  o = o.replace(/[A-Za-z_][A-Za-z0-9_]*|\d[\d_.]*/g, (w) => {
	    if (kwSet.has(w)) return '<span class="hl-kw">' + w + "</span>";
	    if (/^\d/.test(w)) return '<span class="hl-num">' + w + "</span>";
	    return w;
	  });
	  // 4) 回填字符串/注释（先字符串后注释：注释占位符可能包裹字符串占位符的场景不存在，
	  //    字符串先占位已保证嵌套内容整体为字符串）
	  o = o.replace(/\u0000K([A-Z]+)J\u0000/g, (_, i) => '<span class="hl-str">' + strSpans[dec26(i)] + "</span>");
	  o = o.replace(/\u0000K([A-Z]+)H\u0000/g, (_, i) => '<span class="hl-cmt">' + cmtSpans[dec26(i)] + "</span>");
	  return o;
	}

	// highlightDiff 行级高亮（+绿 -红 @@青，与 .diff-line 语义一致）。
	function highlightDiff(out) {
	  return out.split("\n").map((l) => {
	    if (l.startsWith("@@")) return '<span class="hl-hunk">' + l + "</span>";
	    if (l.startsWith("+")) return '<span class="hl-add">' + l + "</span>";
	    if (l.startsWith("-")) return '<span class="hl-del">' + l + "</span>";
	    return l;
	  }).join("\n");
	}

	// ---------- 工具调用文本解析（对齐原版 addToolBlock） ----------
	// 引擎 tool 消息格式："工具名 | 摘要" 或 JSON 载荷（含 diff/args）。
	export function parseToolMessage(text) {
	  const t = String(text || "");
	  const sep = t.indexOf("|");
	  if (sep > 0) {
	    return { name: t.slice(0, sep).trim(), summary: t.slice(sep + 1).trim(), diff: "", args: "" };
	  }
	  try {
	    const info = JSON.parse(t);
	    if (info && typeof info === "object") {
	      return { name: info.tool || "工具", summary: info.summary || info.desc || "", diff: info.diff || "", args: info.args || "" };
	    }
	  } catch { /* 纯文本 */ }
	  return { name: "工具", summary: t, diff: "", args: "" };
	}

	// diff 文本 → 行数组（带 add/del/hunk 分类）
	export function parseDiffLines(diffText) {
	  return String(diffText || "").split("\n").map((line) => ({
	    text: line,
	    cls: line.startsWith("+") ? "add" : line.startsWith("-") ? "del" : line.startsWith("@@") ? "hunk" : "",
	  }));
	}
