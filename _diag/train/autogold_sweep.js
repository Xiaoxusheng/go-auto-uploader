// §22 重定标 + 自动标签质量评估（270 金标片 / 14376 窗，含开封#5 盲区片）
// 门口径：8s 窗，det率<detmin → 非舞；否则检出秒 vis 均值≥V 且 face 均值≤F → 舞
// 输出：阈值网格 P/R/F1（dance 类），标注 §22 与 live 现值位置
// gesture=手势聊天/轻晃（小妤_2026-09-25_20-13-29_010，开封#5 定标盲区）：窗按非舞计入
// P/R/F1，另单独盯门在该类上的误判舞率（UNSEAL5_20260926.md）
const fs = require('fs');
// 生产门现值（config.json highlight_pose_gate），供自动应用防抖比对（F1 在 evalAt 定义后回填）
let liveCfg = null;
try {
  const cfg = JSON.parse(fs.readFileSync('D:/upload/config.json', 'utf8'));
  const g = cfg && cfg.builtin && cfg.builtin.highlight_pose_gate;
  if (g && g.enable) liveCfg = { vis: +g.vis_min, face: +g.face_max, det: +g.det_min };
} catch (e) {}
const gold = JSON.parse(fs.readFileSync('D:/upload/_diag/train/_pose_pilot/gold_review.json', 'utf8'));
const pose = JSON.parse(fs.readFileSync('D:/upload/_diag/train/pose_features_go.json', 'utf8'));
const WIN = 8;

// 预计算每窗 (det率, vis均值, face均值, gold Dance?)
const rows = [];
for (const [clip, wmap] of Object.entries(gold)) {
  const pf = pose[clip] && pose[clip].feats;
  if (!pf) continue;
  for (const [idx, lb] of Object.entries(wmap)) {
    const s = parseInt(idx, 10);
    const seg = pf.slice(s, s + WIN);
    if (seg.length === 0) continue;
    const det = seg.filter(x => x[4] === 1);
    const dr = det.length / seg.length;
    const mv = det.length ? det.reduce((a, x) => a + x[0], 0) / det.length : 0;
    const mf = det.length ? det.reduce((a, x) => a + x[1], 0) / det.length : 0;
    rows.push({ dr, mv, mf, goldDance: lb === 'dance', lb });
  }
}
console.log('可比窗数:', rows.length, '(dance 窗:', rows.filter(r => r.goldDance).length + ')');

function evalAt(V, F, D) {
  let tp = 0, fp = 0, fn = 0;
  for (const r of rows) {
    const pred = r.dr >= D && r.mv >= V && r.mf <= F;
    if (pred && r.goldDance) tp++;
    else if (pred && !r.goldDance) fp++;
    else if (!pred && r.goldDance) fn++;
  }
  const P = tp + fp ? tp / (tp + fp) : 0, R = tp + fn ? tp / (tp + fn) : 0;
  const F1 = P + R ? 2 * P * R / (P + R) : 0;
  return { P, R, F1, tp, fp, fn };
}

const Vs = [0.5, 0.55, 0.6, 0.65, 0.7], Fs = [0.10, 0.12, 0.14, 0.16, 0.18], Ds = [0.1, 0.2, 0.3];
const all = [];
for (const V of Vs) for (const F of Fs) for (const D of Ds) all.push({ V, F, D, ...evalAt(V, F, D) });
all.sort((a, b) => b.F1 - a.F1);
console.log('\nTOP 8（按 F1）:');
console.log(' vis   face  detmin |   P     R     F1   | TP/FP/FN');
all.slice(0, 8).forEach(r => console.log(
  ` ${r.V.toFixed(2)}  ${r.F.toFixed(2)}  ${r.D.toFixed(2)}  | ${r.P.toFixed(3)} ${r.R.toFixed(3)} ${r.F1.toFixed(3)} | ${r.tp}/${r.fp}/${r.fn}`));
const refs = [['§22 定标', 0.6, 0.14, 0.2]];
let live = null;
if (liveCfg) {
  const lr = evalAt(liveCfg.vis, liveCfg.face, liveCfg.det);
  live = { vis: liveCfg.vis, face: liveCfg.face, det: liveCfg.det, P: +lr.P.toFixed(3), R: +lr.R.toFixed(3), F1: +lr.F1.toFixed(3) };
  refs.push(['live 现值(生产)', liveCfg.vis, liveCfg.face, liveCfg.det]);
}
for (const [name, V, F, D] of refs) {
  const r = name.indexOf('live') === 0 ? live : all.find(x => x.V === V && x.F === F && x.D === D);
  console.log(` ${name}: vis${V}/face${F}/det${D} → P=${r.P.toFixed(3)} R=${r.R.toFixed(3)} F1=${r.F1.toFixed(3)} (FP=${r.fp} FN=${r.fn})`);
}

// 自动标签一致性：整体窗标签一致率（预测四类 vs 金标主类，dance 判定同上，closeup 预测对应 gold closeup/chat）
const best = all[0];
let agree = 0;
for (const r of rows) {
  const predDance = r.dr >= best.D && r.mv >= best.V && r.mf <= best.F;
  if (predDance === r.goldDance) agree++;
}
console.log(`\n自动标签 dance/非舞 二分类一致率: ${(100 * agree / rows.length).toFixed(1)}%（阈值 vis${best.V}/face${best.F}/det${best.D}）`);

// 手势聊天/轻晃类别：门判舞=误报（该类含 13 真舞窗，真舞误杀另计在 FN）
const gest = rows.filter(r => r.lb === 'gesture');
function gestPredDance(V, F, D) {
  return gest.filter(r => r.dr >= D && r.mv >= V && r.mf <= F).length;
}
const gestBest = gestPredDance(best.V, best.F, best.D);
const gestLive = liveCfg ? gestPredDance(liveCfg.vis, liveCfg.face, liveCfg.det) : null;
if (gest.length) {
  const pct = n => gest.length ? (100 * n / gest.length).toFixed(1) + '%' : '-';
  console.log(`gesture 窗: ${gest.length}，判舞率 best(vis${best.V}/face${best.F}/det${best.D}) ${gestBest}=${pct(gestBest)}` +
    (gestLive !== null ? ` / live ${gestLive}=${pct(gestLive)}` : ''));
}

// 机器可读结果（控制台「姿态训练」页读取）
fs.writeFileSync('D:/upload/_diag/train/autogold_result.json', JSON.stringify({
  generated_at: new Date().toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }),
  windows: rows.length,
  dance_windows: rows.filter(r => r.goldDance).length,
  gold_clips: Object.keys(gold).length,
  best: { vis: best.V, face: best.F, det: best.D, P: +best.P.toFixed(3), R: +best.R.toFixed(3), F1: +best.F1.toFixed(3) },
  live: live,
  agree_pct: +(100 * agree / rows.length).toFixed(1),
  gesture: gest.length ? {
    windows: gest.length,
    pred_dance_best: gestBest,
    pred_dance_best_pct: +(100 * gestBest / gest.length).toFixed(1),
    pred_dance_live: gestLive,
    pred_dance_live_pct: gestLive !== null ? +(100 * gestLive / gest.length).toFixed(1) : null,
  } : null,
  top: all.slice(0, 10).map(r => ({ vis: r.V, face: r.F, det: r.D, P: +r.P.toFixed(3), R: +r.R.toFixed(3), F1: +r.F1.toFixed(3) })),
}, null, 1));
