// #3 时序动力学评估：5fps 四肢轨迹 → 节拍耦合特征 → dance vs 非舞窗 AUC
// 参考口径：预标窗（dance / closeup+none），配对方向一致性作为辅助证据
const fs = require('fs');
const traj = JSON.parse(fs.readFileSync('D:/upload/_diag/train/_traj/traj.json', 'utf8'));
const FPS = 5;

function dist(ax, ay, bx, by) { return Math.hypot(ax - bx, ay - by); }
function median(a) { const s = [...a].sort((x, y) => x - y); return s[s.length >> 1]; }

function feats(name, e) {
  const fr = e.frames;
  if (fr.length < 60) return null;
  // 躯干长度序列（肩中-髋中）
  const torso = fr.map(f => dist(f[13], f[14], f[15], f[16])).filter(x => x > 1);
  const T = median(torso);
  // 四肢归一化速度序列（躯干长/秒）
  const L = fr.length;
  const sp = [];
  for (let i = 1; i < L; i++) {
    let s = 0, c = 0;
    for (const [x, y] of [[1, 2], [4, 5], [7, 8], [10, 11]]) {
      if (fr[i][x + 2] > 0.3 && fr[i - 1][x + 2] > 0.3) {
        s += dist(fr[i][x], fr[i][y], fr[i - 1][x], fr[i - 1][y]) * FPS / T; c++;
      }
    }
    sp.push(c ? s / c : null);
  }
  const valid = sp.filter(x => x !== null);
  if (valid.length < 50) return null;
  const mean = valid.reduce((a, b) => a + b, 0) / valid.length;
  // 去均值 → 汉宁窗 → DFT 能谱
  const xs = sp.map(v => v === null ? mean : v);
  const m = xs.reduce((a, b) => a + b, 0) / xs.length;
  const N = xs.length;
  const win = xs.map((v, i) => (v - m) * (0.5 - 0.5 * Math.cos(2 * Math.PI * i / (N - 1))));
  let eBand = 0, eTot = 0, domFreq = 0, domE = 0;
  for (let k = 1; k < Math.floor(N / 2); k++) {
    let re = 0, im = 0;
    for (let n = 0; n < N; n++) { const ph = 2 * Math.PI * k * n / N; re += win[n] * Math.cos(ph); im -= win[n] * Math.sin(ph); }
    const p = re * re + im * im;
    const f = k * FPS / N;
    if (f >= 0.3 && f <= 2.5) eTot += p;
    if (f >= 1.5 && f <= 2.5) { eBand += p; if (p > domE) { domE = p; domFreq = f; } }
  }
  const fBand = eTot > 0 ? eBand / eTot : 0;
  // 自相关峰值（lag 2..4 帧 = 0.4~0.8s → 1.25~2.5Hz 周期）
  let ac = 0;
  const dtr = xs.map(v => v - m);
  const var0 = dtr.reduce((a, b) => a + b * b, 0);
  if (var0 > 0) for (const lag of [2, 3, 4]) {
    let s = 0; for (let i = 0; i + lag < N; i++) s += dtr[i] * dtr[i + lag];
    ac = Math.max(ac, s / var0);
  }
  return { mean, fBand, ac, domFreq, N, label: /__d\d+$/.test(name) ? 'dance' : 'non' };
}

const rows = [];
for (const [name, e] of Object.entries(traj)) {
  const f = feats(name, e);
  if (f) { f.name = name; rows.push(f); }
}
const dance = rows.filter(r => r.label === 'dance'), non = rows.filter(r => r.label === 'non');
console.log(`窗数: dance ${dance.length} / non ${non.length}`);

function AUC(vals, labels) {
  const d = vals.filter((_, i) => labels[i] === 'dance'), n = vals.filter((_, i) => labels[i] === 'non');
  let gt = 0, eq = 0;
  for (const a of d) for (const b of n) { if (a > b) gt++; else if (a === b) eq++; }
  return (gt + 0.5 * eq) / (d.length * n.length);
}
const labels = rows.map(r => r.label);
for (const k of ['fBand', 'ac', 'mean', 'domFreq']) {
  console.log(`${k.padEnd(9)} AUC = ${AUC(rows.map(r => r[k]), labels).toFixed(3)}`);
}
console.log('\n逐窗明细（band能量比 | 自相关峰 | 均速 | 主频）:');
rows.sort((a, b) => b.fBand - a.fBand).forEach(r =>
  console.log(` ${r.label.padEnd(5)} ${r.fBand.toFixed(3)}  ${r.ac.toFixed(3)}  ${r.mean.toFixed(2)}  ${r.domFreq.toFixed(2)}Hz  ${r.name.slice(0, 46)}`));
// 配对方向：同片 dance vs non
const byClip = {};
for (const r of rows) { const c = r.name.split('__')[0]; (byClip[c] = byClip[c] || {})[r.label] = r; }
let pairs = 0, agree = 0;
for (const [c, w] of Object.entries(byClip)) {
  if (w.dance && w.non) { pairs++; if (w.dance.fBand > w.non.fBand) agree++; }
}
console.log(`\n配对方向（dance 窗 band比 > 非舞窗）: ${agree}/${pairs}`);
