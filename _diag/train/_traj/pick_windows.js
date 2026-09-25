// 选 5fps 轨迹评估窗：混合片（预标含 dance 段 + closeup/none 段且源片还在盘上），
// 每片抽 1 个 dance 窗 + 1 个非舞窗（各 32s），输出 ffmpeg 抽帧清单。
const fs = require('fs'), path = require('path');
const cfg = JSON.parse(fs.readFileSync('D:/upload/_diag/train/_pose_pilot/clips_config.json', 'utf8'));
const gold = new Set(Object.keys(JSON.parse(fs.readFileSync('D:/upload/_diag/train/_pose_pilot/gold_review.json', 'utf8'))));

function findSrc(stem) {
  const root = 'D:/upload/downloads';
  let hit = null;
  (function walk(d) {
    if (hit) return;
    let es; try { es = fs.readdirSync(d, { withFileTypes: true }); } catch (e) { return; }
    for (const e of es) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name.toLowerCase().endsWith('.ts') && !e.name.includes('高光') && e.name.replace(/\.ts$/i, '') === stem) hit = p;
    }
  })(root);
  return hit;
}

const WIN = 32;
const picks = [];
const perStreamer = {};
for (const c of cfg) {
  if (picks.length >= 12) break;
  if (gold.has(c.clip) || !c.model) continue; // 只要新批自动预标片（非金标，源还在）
  const st = c.clip.split('_2026')[0];
  if ((perStreamer[st] || 0) >= 2) continue; // 每主播最多 2 片，保证多样性
  const spans = (c.spans || []).filter(s => (s.end - s.start) >= WIN);
  const dance = spans.find(s => s.label === 'dance');
  const non = spans.find(s => s.label === 'closeup' || s.label === 'none');
  if (!dance || !non) continue;
  const src = findSrc(c.clip);
  if (!src) continue;
  perStreamer[st] = (perStreamer[st] || 0) + 1;
  // 取段中部，避免边缘
  const dc = Math.round((dance.start + dance.end) / 2 - WIN / 2);
  const nc = Math.round((non.start + non.end) / 2 - WIN / 2);
  picks.push({ clip: c.clip, src, windows: [
    { id: c.clip + '__d' + dc, label: 'dance', start: Math.max(0, dc) },
    { id: c.clip + '__n' + nc, label: 'non', start: Math.max(0, nc) },
  ] });
}
fs.writeFileSync('D:/upload/_diag/train/_traj/windows.json', JSON.stringify(picks, null, 1));
for (const p of picks) console.log(p.clip.slice(0, 44), '→ dance@' + p.windows[0].start + 's, non@' + p.windows[1].start + 's');
console.log('共', picks.length, '片 /', picks.length * 2, '窗');
