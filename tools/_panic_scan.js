// 挖掘 panic / SEARCH未命中 / 写冲突的上下文细节
const fs = require('fs');
const path = require('path');
const os = require('os');
const dir = path.join(os.homedir(), '.kaelvoris', 'sessions');
const files = ['1793113061938.json', '1793198173853.json', '1793201809331.json', '1793199193138.json'];
let out = [];
for (const f of files) {
  let j;
  try { j = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')); } catch (e) { console.error('parse fail', f); continue; }
  const msgs = j.messages || [];
  for (let i = 0; i < msgs.length; i++) {
    const t = msgs[i].text || '';
    if (/panic|runtime error/i.test(t)) {
      out.push('PANIC ' + f.slice(0, 13) + '#' + i + ' | ' + (msgs[i].say || msgs[i].type) + ' | ' + t.slice(0, 400).replace(/\n/g, ' \\n '));
    }
    if (/SEARCH block not found/i.test(t)) {
      out.push('SEARCHMISS ' + f.slice(0, 13) + '#' + i + ' | ' + t.slice(0, 260).replace(/\n/g, ' \\n '));
    }
  }
}
fs.writeFileSync('_panic_detail.txt', out.join('\n=====\n'));
console.log('done, matches:', out.length);
