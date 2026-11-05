// 分析所有会话执行情况：工具调用统计、错误信号聚类、用户反馈聚类
// 数据源：~/.kaelvoris/sessions/*.json
const fs = require('fs');
const path = require('path');
const os = require('os');

const dir = path.join(os.homedir(), '.kaelvoris', 'sessions');
const files = fs.readdirSync(dir).filter(f => f.endsWith('.json') && !f.includes('.memory.'));
const errPatterns = [
  { re: /SEARCH block not found/gi, name: 'SEARCH未命中' },
  { re: /file content has changed/gi, name: '写冲突(mtime)' },
  { re: /write conflict/gi, name: '写冲突' },
  { re: /escapes workspace/gi, name: '路径逃逸' },
  { re: /edits must be/gi, name: 'edits畸形' },
  { re: /path and edits are required/gi, name: '缺path/edits' },
  { re: /file not found/gi, name: 'file not found' },
  { re: /路径不存在|does not exist/gi, name: '路径不存在' },
  { re: /context deadline exceeded|LLM ?调用超时/gi, name: 'LLM超时' },
  { re: /api_req_failed|API Request Failed/gi, name: 'API失败' },
  { re: /max context length|exceeds context|context window/gi, name: '上下文超窗' },
  { re: /condense_context|context condensed/gi, name: '上下文压缩' },
  { re: /command was blocked|危险命令被拦截/gi, name: '危险命令拦截' },
  { re: /ToolRepetitionLimit|repetition limit/gi, name: '工具重复限制' },
  { re: /MistakeLimit|too many mistakes/gi, name: '错误上限' },
  { re: /fetch failed/gi, name: 'fetch failed(部署)' },
  { re: /panic|runtime error/gi, name: 'panic' },
  { re: /ECONNREFUSED|connection refused/gi, name: '连接拒绝' },
  { re: /interrupted/gi, name: '任务中断' },
];
const errCount = {};
const userFeedbacks = [];
const sessions = [];
let totalMsgs = 0, askTotal = 0, condenseTotal = 0;
const askCount = {};

for (const f of files) {
  let j;
  try { j = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')); } catch (e) { continue; }
  const msgs = j.messages || [];
  let condense = 0, feedback = 0;
  const errs = {};
  for (const m of msgs) {
    totalMsgs++;
    if (m.type === 'say' && m.say === 'user_feedback') {
      feedback++;
      const t = (m.text || '').trim();
      if (t) userFeedbacks.push({ s: f, t });
    }
    if (m.type === 'say' && (m.say === 'condense_context' || m.say === 'condense')) { condense++; condenseTotal++; }
    if (m.type === 'ask' && m.ask) { askTotal++; askCount[m.ask] = (askCount[m.ask] || 0) + 1; }
    const text = (m.text || '') + ' ' + (m.ask || '');
    for (const p of errPatterns) {
      p.re.lastIndex = 0;
      if (p.re.test(text)) { errCount[p.name] = (errCount[p.name] || 0) + 1; errs[p.name] = (errs[p.name] || 0) + 1; }
    }
  }
  sessions.push({ f, title: j.title, msgs: msgs.length, condense, feedback, errs, updated: j.updatedAt });
}

console.log('=== 总览 ===');
console.log('会话数:', sessions.length, '| 消息总数:', totalMsgs, '| ask总数:', askTotal, '| 压缩:', condenseTotal);
console.log('\n=== ask 类型 ===');
for (const [k, v] of Object.entries(askCount).sort((a, b) => b[1] - a[1])) console.log('  ' + k + ': ' + v);
console.log('\n=== 错误信号聚类（全库） ===');
for (const [k, v] of Object.entries(errCount).sort((a, b) => b[1] - a[1])) console.log('  ' + k + ': ' + v);
console.log('\n=== 用户反馈（原样, 前120字） ===');
for (const fb of userFeedbacks) console.log('  [' + fb.s.replace('.json','') + '] ' + fb.t.slice(0, 120).replace(/\n/g, ' '));
console.log('\n=== 会话摘要（按消息数 TOP20） ===');
for (const s of sessions.sort((a, b) => b.msgs - a.msgs).slice(0, 20)) {
  console.log('  ' + s.f.replace('.json','') + ' | "' + (s.title||'').slice(0,20) + '" | msgs=' + s.msgs + ' | 压缩=' + s.condense + ' | 反馈=' + s.feedback + ' | errs=' + JSON.stringify(s.errs));
}
