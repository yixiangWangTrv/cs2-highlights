// cs2hi 图形界面交互逻辑
// 流程：选择 demo → 解析 → 选玩家 → 检测片段 → 勾选 + 配置 → 渲染 → 进度 → 完成

'use strict';

const $ = (id) => document.getElementById(id);
const api = async (path, body) => {
  const res = await fetch(path, body ? {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  } : undefined);
  return res.json();
};

// ---------- 全局状态 ----------
const state = {
  demoPath: '',
  players: [],
  player: null,     // 选中的玩家
  clips: [],        // 检测出的片段
  rendering: false,
  pollTimer: null,
  lastPct: 0,       // 进度条已展示的最大百分比（只增不减，避免编码阶段回退）
};

// refreshHistory 扫描当前 demo 的输出目录，展示所有已存在的视频（含历史渲染）
async function refreshHistory() {
  if (!state.demoPath) return;
  const r = await fetch('/api/videos?demoPath=' + encodeURIComponent(state.demoPath)
    + '&outDir=' + encodeURIComponent($('outDir').value.trim())).then(x => x.json());
  renderHistory(r.videos || []);
}

// 输出目录变化时刷新历史区
$('outDir').addEventListener('change', refreshHistory);

// ---------- 步骤切换（底部 Dock 导航，页面左右滑动） ----------
// 4 个步骤页并排成横向页面条，translateX 滑动切换：
// 向后一步（1→2→3→4）内容从右滑入（页面条左移），向前一步自动反向（右移），
// 与手机左右滑动切换 tab 的方向逻辑一致。
const PAGE_ORDER = ['cardDemo', 'cardPlayers', 'cardInfo', 'cardClips'];

function showCard(id) {
  const idx = PAGE_ORDER.indexOf(id);
  if (idx < 0) return;
  $('pages').style.transform = `translateX(-${idx * 100}%)`;
  document.querySelectorAll('.page').forEach(p => p.classList.toggle('active', p.dataset.card === id));
  // Dock 高亮当前步骤（按钮 data-card 与卡片 id 对应）
  document.querySelectorAll('.dock-btn').forEach(b => b.classList.toggle('active', b.dataset.card === id));
  syncPageHeight();
}

// 视口高度跟随当前页内容（切换时高度平滑过渡，短页/长页不跳变）
function syncPageHeight() {
  const act = document.querySelector('.page.active');
  if (act) $('pagesViewport').style.height = act.offsetHeight + 'px';
}
// 页面内容动态变化（解析信息/片段列表/历史视频）时自动同步高度
const pageResizer = new ResizeObserver(() => syncPageHeight());
document.querySelectorAll('.page').forEach(p => pageResizer.observe(p));
syncPageHeight();

// 底部 Dock：前置步骤未完成时置灰，不可点击
function refreshDock() {
  $('dockPlayers').disabled = !state.players.length;
  $('dockInfo').disabled = !state.player;
  $('dockClips').disabled = !state.player;
}
document.querySelectorAll('.dock-btn').forEach(btn => {
  btn.addEventListener('click', () => { if (!btn.disabled) showCard(btn.dataset.card); });
});

// ---------- 步骤 1：解析 demo ----------
$('demoPath').addEventListener('input', () => {
  state.demoPath = $('demoPath').value.trim();
  $('btnParse').disabled = !state.demoPath;
});
$('btnParse').addEventListener('click', async () => {
  $('btnParse').disabled = true;
  // 压缩包需要先解压（大 demo 可能要几秒），按钮文案给出预期
  const isArchive = /\.(zip|rar|7z|tar|tgz|gz|bz2|xz)$/i.test(state.demoPath);
  $('btnParse').textContent = isArchive ? '解压并解析…' : '解析中…';
  const r = await api('/api/parse', { demoPath: state.demoPath });
  $('btnParse').textContent = '解析';
  $('btnParse').disabled = !state.demoPath;
  if (r.error) { toast(r.error, true); return; }
  // 选择压缩包时后端已自动解压：切换到实际 .dem 路径，后续检测/渲染都用它
  if (r.demoPath && r.demoPath !== state.demoPath) {
    state.demoPath = r.demoPath;
    $('demoPath').value = r.demoPath;
    toast('已解压到 ' + r.demoPath);
  }
  state.players = r.players;
  $('matchInfo').textContent = `${r.mapName} · ${r.rounds} 回合`;
  renderPlayers();
  refreshDock();
  showCard('cardPlayers');
});

function renderPlayers() {
  const grid = $('playerGrid');
  grid.innerHTML = '';
  // 按阵营分组：T 方 / CT 方，组内按击杀排序
  for (const [team, label] of [['T', 'T 方'], ['CT', 'CT 方']]) {
    const members = state.players.filter(p => p.team === team);
    if (!members.length) continue;
    const wrap = document.createElement('div');
    wrap.className = 'player-group';
    wrap.innerHTML = `<div class="player-group-head">${label}<span class="hist-count">${members.length}</span></div><div class="player-grid"></div>`;
    const inner = wrap.querySelector('.player-grid');
    for (const p of members) {
      const el = document.createElement('div');
      el.className = 'player' + (state.player && state.player.steamId64 === p.steamId64 ? ' selected' : '');
      // 每位玩家展示 T/CT 两段数据（半场换边），胶囊徽章式布局
      const block = (tag, s, cls) => s
        ? `<div class="ts-block ${cls}">
             <span class="ts-badge">${tag}</span>
             <span class="ts-kd">${s.kills} / ${s.deaths}</span>
             <span class="ts-kdr">${s.kdr.toFixed(2)}</span>
           </div>`
        : `<div class="ts-block ${cls} dim"><span class="ts-badge">${tag}</span><span class="ts-kd">—</span></div>`;
      el.innerHTML = `
        <div class="p-name">${esc(p.name)}</div>
        <div class="p-team-stats">
          ${block('T', p.t, 't')}
          ${block('CT', p.ct, 'ct')}
        </div>`;
      el.addEventListener('click', () => selectPlayer(p));
      inner.appendChild(el);
    }
    grid.appendChild(wrap);
  }
}

async function selectPlayer(p) {
  state.player = p;
  renderPlayers();
  refreshDock();
  // 跳到步骤 3：解析信息卡片先展示"检测中"，完成后填充完整数据
  showCard('cardInfo');
  $('infoTitle').textContent = `${p.name} · 解析信息`;
  $('infoMatch').textContent = '检测集锦片段中…';
  $('infoBody').innerHTML = '<div class="hint">正在解析玩家数据与集锦片段…</div>';
  $('playerGrid').querySelectorAll('.player').forEach(x => x.style.pointerEvents = 'none');
  const r = await api('/api/detect', { demoPath: state.demoPath, steamId64: p.steamId64, maxClips: 40, minScore: 20 });
  $('playerGrid').querySelectorAll('.player').forEach(x => x.style.pointerEvents = '');
  if (r.error) { toast(r.error, true); return; }
  state.clips = r.clips || [];
  renderClips();
  // 默认输出目录：填入 demo 所在目录（渲染时会在其下自动创建子文件夹）
  if (!$('outDir').value) {
    $('outDir').value = state.demoPath.replace(/[\\/][^\\/]*$/, '');
  }
  refreshHistory(); // 展示该 demo 输出目录里已存在的视频
  renderPlayerInfo(r.stats);
  $('infoMatch').textContent = `共检测到 ${state.clips.length} 个高光片段`;
}

// renderPlayerInfo 填充步骤 3 的玩家解析信息卡片（数据由 /api/detect 一并返回，避免重复解析 demo）
function renderPlayerInfo(st) {
  if (!st) {
    $('infoBody').innerHTML = '<div class="hint">未获取到该玩家的解析数据。</div>';
    return;
  }
  const tile = (label, value, sub) => `
    <div class="stat-tile">
      <span class="st-v">${value}</span>
      <span class="st-l">${label}</span>
      ${sub ? `<span class="st-s">${sub}</span>` : ''}
    </div>`;
  const mk = ['2k', '3k', '4k', '5k'].map(k => st.multi_kills?.[k] || 0).join(' / ');
  const fmt = (m) => Object.entries(m || {})
    .sort((a, b) => parseInt(a[0]) - parseInt(b[0]))
    .map(([k, v]) => `${k}×${v}`).join('  ') || '—';
  const teamName = st.team === 'T' ? 'T 方' : st.team === 'CT' ? 'CT 方' : st.team;
  const weps = Object.entries(st.weapons || {})
    .sort((a, b) => b[1].kills - a[1].kills).slice(0, 6)
    .map(([n, w]) => `${esc(n)} ${w.kills}杀${w.kills ? `（${Math.round(w.headshots / w.kills * 100)}%爆头）` : ''}`)
    .join('　') || '—';
  $('infoBody').innerHTML = `
    <div class="stat-grid">
      ${tile('击杀 / 死亡 / 助攻', `${st.kills} / ${st.deaths} / ${st.assists}`, `MVP ${st.mvps} · 得分 ${st.score}`)}
      ${tile('K/D', st.kdr.toFixed(2), '总击杀 / 总死亡')}
      ${tile('ADR', st.adr.toFixed(1), '每回合平均伤害')}
      ${tile('爆头率', Math.round(st.headshot_rate * 100) + '%', `${st.damage_dealt} 总伤害`)}
      ${tile('KAST', Math.round(st.kast * 100) + '%', '击杀/助攻/存活/被补回合占比')}
      ${tile('回合数', st.rounds_played, `${teamName} · 道具伤害 ${st.utility_damage}`)}
      ${tile('首杀 / 补枪 / 被补', `${st.entry_kills} / ${st.trade_kills} / ${st.traded_deaths}`)}
      ${tile('多杀（2k/3k/4k/5k）', mk)}
      ${tile('残局胜 ｜ 负', `${fmt(st.clutch_wins)} ｜ ${fmt(st.clutch_losses)}`)}
      ${tile('道具（闪/HE/烟/火）', `${st.flashes_thrown}/${st.he_thrown}/${st.smoke_thrown}/${st.molotov_thrown}`, `闪白敌人 ${st.enemies_flashed} 次`)}
    </div>
    <div class="info-section">
      <h3>常用武器</h3>
      <p class="info-line">${weps}</p>
    </div>
    <div class="action-row">
      <button class="btn primary big" id="btnGotoClips">下一步：选择集锦片段 →</button>
    </div>`;
  $('btnGotoClips').addEventListener('click', () => showCard('cardClips'));
}

// ---------- 步骤 3：片段勾选 ----------
let clipSort = 'score'; // 'score'（按得分）/ 'round'（按回合数）

function renderClips() {
  const list = $('clipList');
  // 保留已有勾选状态（重排/重渲染不清空用户选择）
  const checked = new Set();
  list.querySelectorAll('input[type=checkbox]').forEach(b => { if (b.checked) checked.add(+b.dataset.i); });
  const firstRender = checked.size === 0 && list.children.length === 0;
  list.innerHTML = '';
  if (!state.clips.length) {
    list.innerHTML = '<div class="hint">未检测到高光片段，可换一个玩家试试。</div>';
    return;
  }
  // 排序：按得分降序（精彩程度）或按回合升序（时间线顺序）
  const ordered = [...state.clips].sort((a, b) =>
    clipSort === 'round' ? (a.round - b.round) : (b.score - a.score));
  const grid = document.createElement('div');
  grid.className = 'clip-grid';
  for (const c of ordered) {
    const el = document.createElement('div');
    el.className = 'clip-card';
    // JSON 字段是 snake_case（与 CLI 输出一致）
    const secs = (c.start_sec ?? 0);
    const mm = Math.floor(secs / 60), ss = String(Math.floor(secs % 60)).padStart(2, '0');
    const dur = c.duration_sec ?? 0;
    const tags = (c.tags || []).slice(0, 5).map(t =>
      `<span class="tag${t.includes('杀') || t.includes('残局') ? ' gold' : ''}">${esc(t)}</span>`).join('');
    const isChecked = firstRender || checked.has(c.id);
    // 标题清洗：去"第N回合"前缀与" 等N个高光"后缀；
    // 剩余内容若已全部由标签覆盖，则标题不再重复展示
    let titleClean = (c.title || '')
      .replace(/ 等\d+个高光$/, '')
      .replace(/^第\d+回合\s*/, '');
    const tagsText = (c.tags || []).join(' ');
    const words = titleClean.split(/[·\s]+/).filter(Boolean);
    const unique = words.filter(w => !tagsText.includes(w));
    titleClean = unique.join('·');
    el.innerHTML = `
      <input type="checkbox" ${isChecked ? 'checked' : ''} data-i="${c.id}">
      <div class="clip-card-head">
        <span class="clip-round">第 ${c.round} 回合</span>
        <span class="clip-score">${Math.round(c.score)} 分</span>
      </div>
      ${titleClean ? `<div class="clip-card-title">${esc(titleClean)}</div>` : ''}
      <div class="clip-card-tags">${tags}</div>
      <div class="clip-card-meta">
        <span>⏱ ${mm}:${ss}</span><span>${dur.toFixed(1)}s</span><span>💾 ${estMB(c)} MB</span>
      </div>`;
    if (!isChecked) el.classList.add('off');
    el.addEventListener('click', (e) => {
      if (e.target.tagName === 'INPUT') return;
      const box = el.querySelector('input');
      box.checked = !box.checked;
      el.classList.toggle('off', !box.checked);
      updateSelCount();
    });
    el.querySelector('input').addEventListener('change', () => {
      el.classList.toggle('off', !el.querySelector('input').checked);
      updateSelCount();
    });
    grid.appendChild(el);
  }
  list.appendChild(grid);
  updateSelCount();
}

// 分段控件：切换排序方式
document.querySelectorAll('#clipSortSeg .seg-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    clipSort = btn.dataset.sort;
    document.querySelectorAll('#clipSortSeg .seg-btn').forEach(b => b.classList.toggle('active', b === btn));
    renderClips();
  });
});

function selectedClips() {
  const boxes = $('clipList').querySelectorAll('input[type=checkbox]');
  const sel = [];
  boxes.forEach(b => { if (b.checked) sel.push(state.clips[b.dataset.i - 1]); });
  return sel;
}

// estMB 按当前渲染设置预估片段成片大小（MB）。
// 码率模型：1080p60 crf18 ≈ 20Mbps，随分辨率/帧率线性、CRF 每 ±2 约 ×1.5/0.7。
function estMB(c) {
  const [w, h] = $('res').value.split('x').map(Number);
  const fps = +$('fps').value;
  const crf = +$('crf').value;
  const crfFactor = crf <= 16 ? 1.5 : crf >= 20 ? 0.7 : 1;
  const br = 20 * (w * h / (1920 * 1080)) * (fps / 60) * crfFactor; // Mbps
  return Math.max(1, Math.round((c.duration_sec ?? 0) * br / 8));
}

// 渲染设置变化时刷新片段列表（预估大小实时更新）
['res', 'fps', 'crf'].forEach(id => {
  $(id).addEventListener('change', () => {
    if ($('cardClips').classList.contains('active')) renderClips();
  });
});
function updateSelCount() {
  $('selCount').textContent = `已选 ${selectedClips().length}/${state.clips.length}`;
}
$('btnSelectAll').addEventListener('click', () => {
  $('clipList').querySelectorAll('input').forEach(b => { b.checked = true; b.closest('.clip-card').classList.remove('off'); });
  updateSelCount();
});
$('btnSelectNone').addEventListener('click', () => {
  $('clipList').querySelectorAll('input').forEach(b => { b.checked = false; b.closest('.clip-card').classList.add('off'); });
  updateSelCount();
});

// ---------- 渲染 ----------
$('btnRender').addEventListener('click', async () => {
  const sel = selectedClips();
  if (!sel.length) { toast('请至少勾选一个片段', true); return; }
  const estGB = sel.reduce((a, c) => a + (c.duration_sec || 0), 0) * ($('fps').value / 60) *
    (() => { const [w, h] = $('res').value.split('x'); return w * h * 3 / 1e9; })() * 1.05;
  if (estGB > 40) {
    if (!await askConfirm(`预计录制期间临时占用约 ${estGB.toFixed(0)} GB 磁盘空间，继续？`, '继续', '磁盘空间提示')) return;
  }
  const r = await api('/api/render', {
    demoPath: state.demoPath,
    steamId64: state.player.steamId64,
    clips: sel,
    outDir: $('outDir').value.trim(),
    res: $('res').value,
    fps: +$('fps').value,
    crf: +$('crf').value,
    preset: $('preset').value,
    view: $('view').value,
    maxPerPass: 6,
    hideWindow: true,
    skipConcat: $('skipConcat').checked,
  });
  if (r.error) { toast(r.error, true); return; }
  state.rendering = true;
  state.lastPct = 0;
  // 进度以弹窗形式悬浮在片段选择界面上，主界面保持可见
  $('progressModal').hidden = false;
  $('ringPct').textContent = '0%';
  $('ringStage').textContent = '准备中';
  $('progressDetail').textContent = '正在初始化…';
  $('logBox').textContent = '';
  $('btnCancel').hidden = false;
  $('btnCancel').textContent = '取消渲染';
  pollTask();
});

function pollTask() {
  state.pollTimer = setInterval(async () => {
    const t = await fetch('/api/task').then(r => r.json());
    const total = t.total || 1;
    // 优先使用后端总体进度（录制/编码各占该轮一半，阶段间单调）；
    // 缺失时退回按「已完成片段 + 当前录制帧进度」估算。
    let pct = typeof t.percent === 'number'
      ? Math.round(t.percent * 100)
      : Math.min(100, Math.round(((t.done || 0) + (t.partial || 0)) / total * 100));
    // 重录/重试会让整体进度回落，界面只增不减，避免进度条来回跳
    if (pct < state.lastPct) pct = state.lastPct;
    else state.lastPct = pct;
    $('ringPct').textContent = pct + '%';
    const CIRC = 326.7;
    $('ringFg').style.strokeDashoffset = CIRC * (1 - pct / 100);
    const stageNames = {
      pass: '启动游戏', record: '录制片段', encode: '编码视频', concat: '合成成片',
      done: '完成', warn: '自动恢复',
    };
    $('ringStage').textContent = stageNames[t.stage] || t.stage || '处理中';
    $('progressDetail').textContent = t.detail || '';
    // 结构化进度：当前轮次 / 全部中的第几个片段 / 当前片段帧进度
    $('pmPass').textContent = t.pass > 0 ? `第 ${t.pass} 轮` : '—';
    const curClip = t.clipIndex > 0 ? t.clipIndex : (t.done || 0);
    $('pmClip').textContent = t.total > 0 ? `${curClip} / ${t.total}` : '— / —';
    let curPct = '—';
    if (t.stage === 'record') curPct = Math.round((t.partial || 0) * 100) + '%';
    else if (t.stage === 'encode') curPct = '编码中';
    else if (t.stage === 'concat') curPct = '合成中';
    else if (t.stage === 'done') curPct = '100%';
    else if (t.state === 'cancelled') curPct = '已取消';
    else if (t.state === 'error') curPct = '失败';
    $('pmPartial').textContent = curPct;
    if (t.logs && t.logs.length) {
      $('logBox').textContent = t.logs.join('\n');
      $('logBox').scrollTop = $('logBox').scrollHeight;
    }
    if (t.state === 'done' || t.state === 'error' || t.state === 'cancelled') {
      clearInterval(state.pollTimer);
      state.rendering = false;
      $('btnCancel').hidden = true;
      if (t.state === 'cancelled') {
        // 用户主动取消：保持进度弹窗，显示结果与关闭按钮（不当作失败）
        $('ringStage').textContent = '已取消';
        $('progressDetail').textContent = '渲染已取消（已录制的片段保留在输出目录 raw/）';
        $('btnProgressClose').hidden = false;
        $('btnProgressClose').textContent = '✕';
        $('btnProgressClose').title = '关闭';
        toast('已取消渲染');
        return;
      }
      if (t.state === 'error') {
        // 失败：弹窗内展示错误详情与日志，显示 ✕ 供用户关闭（避免界面卡死在渲染弹窗）
        $('ringStage').textContent = '失败';
        $('progressDetail').textContent = t.error || '渲染失败';
        if (t.logs && t.logs.length) {
          $('logBox').textContent = t.logs.join('\n');
          $('logBox').scrollTop = $('logBox').scrollHeight;
        }
        $('btnProgressClose').hidden = false;
        $('btnProgressClose').textContent = '✕';
        $('btnProgressClose').title = '关闭';
        toast(t.error || '渲染失败', true);
        return;
      }
      // 完成：关闭进度弹窗，刷新下方历史区（扫描输出目录）
      $('progressModal').hidden = true;
      refreshHistory();
      toast('✅ 渲染完成，视频已加入下方列表');
    }
  }, 600);
}

// 历史视频区：按目录分组 + 网格卡片展示
let historyVideos = [];
let historySort = 'time';
let historyOrder = []; // 当前展示顺序（预览左右切换按这个顺序走）

function sortVids(vids) {
  const arr = [...vids];
  if (historySort === 'size') arr.sort((a, b) => (b.sizeMb || 0) - (a.sizeMb || 0));
  else if (historySort === 'name') arr.sort((a, b) => (a.name || '').localeCompare(b.name || '', 'zh'));
  else arr.sort((a, b) => (b.modTime || 0) - (a.modTime || 0)); // 最新在前
  return arr;
}

function renderHistory(vids) {
  historyVideos = vids || [];
  const list = $('historyList');
  if (!historyVideos.length) {
    historyOrder = [];
    list.innerHTML = '<div class="hint">该输出目录还没有视频。</div>';
    return;
  }
  // 按相对目录分组：根目录成片 + clips/多杀、clips/常规 等
  const groups = new Map();
  for (const v of sortVids(historyVideos)) {
    const dir = v.relDir === '.' || !v.relDir ? '成片' : v.relDir;
    if (!groups.has(dir)) groups.set(dir, []);
    groups.get(dir).push(v);
  }
  const dirs = [...groups.keys()].sort((a, b) =>
    a === '成片' ? -1 : b === '成片' ? 1 : a.localeCompare(b, 'zh'));
  // 记录与界面一致的展示顺序（分组顺序 + 组内顺序），供预览左右切换
  historyOrder = [];
  for (const dir of dirs) {
    for (const v of groups.get(dir)) historyOrder.push({ path: v.path, name: v.name });
  }
  list.innerHTML = dirs.map(dir => `
    <div class="hist-group">
      <div class="hist-group-head">${dir === '成片' ? '🎞 成片' : '📁 ' + esc(dir)}<span class="hist-count">${groups.get(dir).length}</span></div>
      <div class="hist-grid">
        ${groups.get(dir).map(v => videoCardHTML(v)).join('')}
      </div>
    </div>`).join('');
  list.querySelectorAll('.vcard').forEach(el => {
    // 批量删除模式：点击卡片 = 勾选切换；普通模式：点击 = 播放
    if (batchDeleteMode) {
      el.classList.add('batch-mode');
      if (batchSelected.has(el.dataset.path)) {
        el.classList.add('selected');
        el.querySelector('.vcard-checkbox').checked = true;
      }
    }
    el.addEventListener('click', () => {
      if (batchDeleteMode) {
        toggleBatchSelect(el);
        return;
      }
      openVideo(el.dataset.path, el.dataset.name);
    });
    el.querySelector('.vcard-del').addEventListener('click', async (e) => {
      e.stopPropagation();
      if (!await askConfirm('删除「' + el.dataset.name + '」？', '删除', '删除视频')) return;
      const r = await fetch('/api/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path: el.dataset.path }),
      }).then(x => x.json());
      if (r.ok === 'true') { toast('已删除'); refreshHistory(); }
      else toast(r.err || '删除失败', true);
    });
  });
}

function videoCardHTML(v) {
  const dur = v.durationSec
    ? Math.floor(v.durationSec / 60) + ':' + String(Math.floor(v.durationSec % 60)).padStart(2, '0')
    : '';
  return `
    <div class="vcard" data-path="${esc(v.path)}" data-name="${esc(v.name)}">
      <div class="vcard-thumb">
        <input type="checkbox" class="vcard-checkbox" tabindex="-1">
        <img class="vcard-img" src="/api/thumb?path=${encodeURIComponent(v.path)}" loading="lazy" alt="">
        <div class="vcard-play">▶</div>
        ${dur ? `<span class="vcard-dur">${dur}</span>` : ''}
        <button class="vcard-del" title="删除">✕</button>
      </div>
      <div class="vcard-name">${esc(v.name)}</div>
      <div class="vcard-meta">${v.sizeMb} MB</div>
    </div>`;
}

$('historySort').addEventListener('change', () => {
  historySort = $('historySort').value;
  renderHistory(historyVideos);
});

// ---------- 批量删除（选择模式） ----------
let batchDeleteMode = false;
const batchSelected = new Set(); // 选中视频的 path

// updateBatchCount 刷新批量删除计数与"全选/取消全选"按钮文案。
function updateBatchCount() {
  $('batchCount').textContent = `已选 ${batchSelected.size} 个`;
  const allSelected = historyVideos.length > 0 &&
    historyVideos.every(v => batchSelected.has(v.path));
  $('btnBatchSelectAll').textContent = allSelected ? '取消全选' : '全选';
}

function setBatchMode(on) {
  batchDeleteMode = on;
  if (!on) batchSelected.clear();
  $('btnBatchDelete').textContent = on ? '退出批量删除' : '批量删除';
  $('batchBar').hidden = !on;
  renderHistory(historyVideos); // 重渲染应用/清除选择模式样式
  updateBatchCount();
}

function toggleBatchSelect(card) {
  const path = card.dataset.path;
  if (batchSelected.has(path)) {
    batchSelected.delete(path);
    card.classList.remove('selected');
  } else {
    batchSelected.add(path);
    card.classList.add('selected');
  }
  card.querySelector('.vcard-checkbox').checked = batchSelected.has(path);
  updateBatchCount();
}

$('btnBatchDelete').addEventListener('click', () => setBatchMode(!batchDeleteMode));

// 全选 / 取消全选：一次勾选当前输出目录下的全部已渲染视频
$('btnBatchSelectAll').addEventListener('click', () => {
  const allSelected = historyVideos.length > 0 &&
    historyVideos.every(v => batchSelected.has(v.path));
  if (allSelected) batchSelected.clear();
  else historyVideos.forEach(v => batchSelected.add(v.path));
  renderHistory(historyVideos); // 应用/清除选中样式与复选框
  updateBatchCount();
});

$('btnBatchDelConfirm').addEventListener('click', async () => {
  const paths = [...batchSelected];
  if (!paths.length) { toast('请先勾选要删除的视频', true); return; }
  if (!await askConfirm(`确定删除所选 ${paths.length} 个视频？此操作不可恢复。`, '删除所选', '批量删除')) return;
  let ok = 0;
  const errs = [];
  for (const p of paths) {
    const r = await fetch('/api/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: p }),
    }).then(x => x.json());
    if (r.ok === 'true') ok++;
    else errs.push(r.err || '未知错误');
  }
  if (errs.length) {
    // 失败原因要展示出来（常见：视频仍在播放器里被占用），不能只报成功数
    toast(`已删除 ${ok} 个，失败 ${errs.length} 个：${errs[0]}`, true);
  } else {
    toast(`已删除 ${ok} 个视频`);
  }
  setBatchMode(false);
  refreshHistory();
});

// 玻璃弹窗播放视频：支持在历史列表内左右切换，无需退出预览
let previewList = [];
let previewIndex = -1;

function openVideo(path, name) {
  // 使用历史区当前展示顺序（分组顺序 + 组内顺序）；找不到则退化为单条
  previewList = historyOrder.length ? historyOrder.slice() : [];
  previewIndex = previewList.findIndex(v => v.path === path);
  if (previewIndex < 0) {
    previewList = [{ path, name }];
    previewIndex = 0;
  }
  showPreview();
}

// showPreview 加载预览列表的当前项（标题附带 序号/总数）
function showPreview() {
  const v = previewList[previewIndex];
  if (!v) return;
  $('videoTitle').textContent = previewList.length > 1
    ? `${v.name}  ·  ${previewIndex + 1}/${previewList.length}`
    : v.name;
  const player = $('videoPlayer');
  player.src = '/api/video?path=' + encodeURIComponent(v.path);
  player.play().catch(() => {}); // 自动播放被拦截时保持暂停，用户可手动播放
  const multi = previewList.length > 1;
  $('btnVideoPrev').hidden = !multi;
  $('btnVideoNext').hidden = !multi;
  $('videoModal').hidden = false;
}

// stepPreview 切换到上/下一个视频（循环）
function stepPreview(delta) {
  if (previewList.length <= 1) return;
  previewIndex = (previewIndex + delta + previewList.length) % previewList.length;
  showPreview();
}

$('btnVideoPrev').addEventListener('click', (e) => { e.stopPropagation(); stepPreview(-1); });
$('btnVideoNext').addEventListener('click', (e) => { e.stopPropagation(); stepPreview(1); });
// 关闭播放器必须调用 load()：仅 removeAttribute('src') 不会立即释放媒体资源，
// Windows 下文件句柄仍被占用，随后删除该视频会失败（批量删除看似"没反应"）。
function closeVideo() {
  $('videoModal').hidden = true;
  const v = $('videoPlayer');
  v.pause();
  v.removeAttribute('src');
  v.load();
}
$('btnVideoClose').addEventListener('click', closeVideo);
$('videoModal').addEventListener('click', (e) => {
  if (e.target === $('videoModal')) closeVideo();
});
document.addEventListener('keydown', (e) => {
  if ($('videoModal').hidden) return;
  if (e.key === 'Escape') { $('btnVideoClose').click(); return; }
  if (e.key === 'ArrowLeft') { e.preventDefault(); stepPreview(-1); }
  else if (e.key === 'ArrowRight') { e.preventDefault(); stepPreview(1); }
});

$('btnCancel').addEventListener('click', async () => {
  await fetch('/api/cancel');
  $('btnCancel').textContent = '正在取消…';
});

// 进度弹窗关闭按钮：仅任务结束后（失败）可用，渲染中只能取消
$('btnProgressClose').addEventListener('click', () => {
  if (!state.rendering) $('progressModal').hidden = true;
});

$('btnOpenDir').addEventListener('click', async () => {
  const t = await fetch('/api/task').then(r => r.json());
  const first = t.history?.[0]?.path || t.clipVideos?.[0] || t.finalVideo;
  if (first) await fetch('/api/opendir?path=' + encodeURIComponent(first));
});

// ---------- 文件浏览器 ----------
// browserTarget：弹窗用途——'demo'（选择 demo 文件）/ 'outDir'（选择输出目录，显示"选择此目录"）
let browserTarget = 'demo';
$('btnBrowse').addEventListener('click', () => openBrowser('', 'demo'));
// 输出目录浏览：默认定位到当前输入框路径（未填时用 demo 所在目录）
$('btnOutDirBrowse').addEventListener('click', () => {
  const init = $('outDir').value.trim()
    || (state.demoPath ? state.demoPath.replace(/[\\/][^\\/]*$/, '') : '');
  openBrowser(init, 'outDir');
});
$('btnBrowserPick').addEventListener('click', () => {
  const p = $('browserPath').value.trim();
  if (!p) return;
  if (browserTarget === 'outDir') {
    $('outDir').value = p;
    refreshHistory();
    toast('输出目录：' + p);
  }
  $('browser').hidden = true;
});
$('btnBrowserClose').addEventListener('click', () => $('browser').hidden = true);
// 点击遮罩空白处 / ESC 关闭（标准对话框习惯）
$('browser').addEventListener('click', (e) => {
  if (e.target === $('browser')) $('browser').hidden = true;
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('browser').hidden) $('browser').hidden = true;
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('confirmModal').hidden) resolveConfirm(false);
});
$('btnBrowserUp').addEventListener('click', async () => {
  const r = await fetch('/api/browse?path=' + encodeURIComponent($('browserPath').value)).then(r => r.json());
  if (r.parent === '') { openBrowser(''); return; } // 根目录 → 回到盘符列表
  if (r.parent) openBrowser(r.parent);
});
// 路径输入框：回车直接跳转到输入的路径
$('browserPath').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    const p = $('browserPath').value.trim();
    if (p) openBrowser(p);
  }
});
async function openBrowser(path, target) {
  if (target) browserTarget = target;
  $('browser').hidden = false;
  // 两种模式共用同一弹窗结构：标题、提示、按钮全部随模式切换
  $('browserTitle').textContent = browserTarget === 'outDir' ? '选择输出目录' : '选择 Demo 文件';
  $('btnBrowserPick').hidden = (browserTarget !== 'outDir');
  $('browserHint').textContent = browserTarget === 'outDir'
    ? '选择当前目录作为输出目录' : '单击 demo / 压缩包即选择（压缩包自动解压）';
  const q = path ? '?path=' + encodeURIComponent(path) : '';
  const r = await fetch('/api/browse' + q).then(r => r.json());
  if (r.error) { toast(r.error, true); return; }
  const list = $('browserList');
  list.innerHTML = '';
  if (r.drives) {
    for (const d of r.drives) {
      const el = document.createElement('div');
      el.className = 'browser-item dir';
      el.innerHTML = `<span class="b-name">🗄 ${esc(d)}</span>`;
      el.addEventListener('click', () => openBrowser(d));
      list.appendChild(el);
    }
  } else {
    $('browserPath').value = r.path;
    for (const e of r.entries || []) {
      const el = document.createElement('div');
      el.className = 'browser-item ' + (e.isDir ? 'dir' : '');
      const meta = [];
      if (e.modTime) meta.push(`<span class="b-time">${fmtModTime(e.modTime)}</span>`);
      if (e.sizeMb) meta.push(`<span class="b-size">${e.sizeMb} MB</span>`);
      el.innerHTML = `<span class="b-name">${e.isDir ? '📁 ' : (e.isArchive ? '📦 ' : '🎬 ')}${esc(e.name)}</span>` +
        (meta.length ? `<span class="b-meta">${meta.join('')}</span>` : '');
      el.addEventListener('click', () => {
        if (e.isDir) { openBrowser(e.path); return; }
        $('demoPath').value = e.path;
        state.demoPath = e.path;
        $('btnParse').disabled = false;
        $('browser').hidden = true;
        toast('已选择 ' + e.name);
      });
      list.appendChild(el);
    }
  }
  // 切换目录后重置滚动位置：列表内容被整体替换时浏览器会保留（或按滚动锚点
  // 恢复）上一次的 scrollTop，滚到底再进入另一个目录会停在底部看不到开头。
  list.scrollTop = 0;
}

// fmtModTime 把 Unix 秒格式化为本地时间 YYYY-MM-DD HH:mm（文件浏览器的修改时间）。
function fmtModTime(ts) {
  const d = new Date(ts * 1000);
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
    `${p(d.getHours())}:${p(d.getMinutes())}`;
}

// ---------- 工具 ----------
function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// askConfirm 玻璃风格确认弹窗（替代浏览器原生 confirm）。
// 返回 Promise<boolean>：确认 true / 取消或关闭 false。
let confirmResolver = null;
function askConfirm(msg, okText, title) {
  return new Promise(resolve => {
    confirmResolver = resolve;
    $('confirmTitle').textContent = title || '确认操作';
    $('confirmMsg').textContent = msg;
    $('btnConfirmOk').textContent = okText || '确认';
    $('confirmModal').hidden = false;
  });
}
function resolveConfirm(v) {
  $('confirmModal').hidden = true;
  if (confirmResolver) { confirmResolver(v); confirmResolver = null; }
}
$('btnConfirmOk').addEventListener('click', () => resolveConfirm(true));
$('btnConfirmCancel').addEventListener('click', () => resolveConfirm(false));
$('btnConfirmClose').addEventListener('click', () => resolveConfirm(false));
let toastTimer;
function toast(msg, isErr) {
  let el = document.querySelector('.toast');
  if (!el) {
    el = document.createElement('div');
    el.className = 'toast glass';
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.style.borderColor = isErr ? 'rgba(255,69,58,.5)' : 'var(--glass-border)';
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 3200);
}
