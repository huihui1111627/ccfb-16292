let lastState = null;
let selectedStore = new URLSearchParams(location.search).get('store') || 'S01';
let traceLoadedFor = null;

const $ = (id) => document.getElementById(id);
const fmt = new Intl.NumberFormat('zh-CN');
function fi(x) { return (x === undefined || x === null) ? '-' : fmt.format(x); }
function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function pct(x, digits=1) { return (100 * Number(x || 0)).toFixed(digits) + '%'; }
function us(ns) { return ns >= 1000 ? (ns/1000).toFixed(2)+' µs' : ns+' ns'; }

async function api(url, opts) {
  const res = await fetch(url, opts);
  const data = await res.json();
  if (!res.ok || data.ok === false) throw new Error(data.error || ('HTTP '+res.status));
  return data;
}

function levelBadge(level) {
  const label = {hq:'总部', region:'区域', store:'门店'}[level] || level;
  return `<span class="badge ${esc(level)}">${label}</span>`;
}

function conditionsHTML(f) {
  const cs = (f.conditions || []).map(c => `<span class="cond-chip">${esc(c)}</span>`).join('');
  const miss = f.missing_as_match ? '<span class="badge warn">缺属性=匹配</span>' : '<span class="badge">缺属性=不匹配</span>';
  return `<div>${cs || '<span class="muted">无条件（全匹配）</span>'}</div><div style="margin-top:5px">${miss}</div>`;
}

function filterCard(f, detailed=false) {
  const sources = f.source_ids && f.source_ids.length
    ? `<span class="muted">来源 ${f.source_ids.length} 条：</span><span class="mono">${esc(f.source_ids.slice(0, detailed ? 30 : 8).join(', '))}${f.source_ids.length > (detailed?30:8)?' …':''}</span>`
    : '<span class="muted">单条/未展开来源</span>';
  const overhead = f.estimated_overhead ? `<span class="badge warn">样本额外 ${pct(f.estimated_overhead)}</span>` : '';
  return `<div class="filter-card">
    <div class="meta">
      <b class="mono">${esc(f.id)}</b>
      ${f.owner_id ? `<span class="badge">owner ${esc(f.owner_id)}</span>`:''}
      <span class="badge good">覆盖 ${fi(f.merged_count || 1)}</span>
      ${overhead}
    </div>
    ${conditionsHTML(f)}
    <div class="small" style="margin-top:5px">${sources}</div>
  </div>`;
}

function renderStoreSelect(count) {
  const sel = $('storeSelect');
  if (sel.options.length === count) return;
  sel.innerHTML = Array.from({length:count}, (_,i) => {
    const id = 'S'+String(i+1).padStart(2,'0');
    return `<option value="${id}" ${id===selectedStore?'selected':''}>${id}</option>`;
  }).join('');
  sel.value = selectedStore;
}

function renderTopology(snap) {
  const byParent = {};
  snap.nodes.forEach(n => { (byParent[n.parent_id || ''] ||= []).push(n); });
  function nodeLine(n, depth) {
    const kids = (byParent[n.id] || []).slice().sort((a,b)=>a.id.localeCompare(b.id));
    const arrow = depth ? '└ '.padStart(depth*2+2, ' ') : '';
    let html = `<div style="padding:3px 0;white-space:nowrap">${arrow}${levelBadge(n.level)} <b class="mono">${esc(n.id)}</b>
      <span class="muted small">收 ${fi(n.received_count)} / 报 ${fi(n.reported_count)}</span>
      <button class="mini" onclick="openNode('${esc(n.id)}')">看表</button>
      ${n.level==='region'?`<button class="mini danger" onclick="removeProxy('${esc(n.id)}')">退出</button>`:''}
    </div>`;
    kids.forEach(k => html += nodeLine(k, depth+1));
    return html;
  }
  const root = snap.nodes.find(n => n.id === snap.root_id) || snap.nodes[0];
  $('topology').innerHTML = root ? nodeLine(root,0) : '';
}

function compressionRatio(n) {
  const base = n.received_count || 0;
  const out = n.reported_count || 0;
  if (!base) return {text:'-', width:0, cls:''};
  const r = out / base;
  let cls = r < .25 ? 'good' : r < .6 ? '' : 'warn';
  return {text: (r*100).toFixed(1)+'%', width: Math.min(100,r*100), cls};
}

function renderNodes(snap) {
  $('nodesBody').innerHTML = snap.nodes.map(n => {
    const c = compressionRatio(n);
    const avg = n.stats?.avg_match_micros ? n.stats.avg_match_micros.toFixed(2)+' µs' : '-';
    return `<tr>
      <td><b class="mono">${esc(n.id)}</b></td>
      <td>${levelBadge(n.level)}</td>
      <td class="mono muted">${esc(n.parent_id || '-')}</td>
      <td class="right">${fi(n.raw_sub_count)}</td>
      <td class="right">${fi(n.received_count)}</td>
      <td class="right">${fi(n.reported_count)}</td>
      <td><div class="row"><div class="bar ${c.cls}" style="width:90px"><i style="width:${c.width}%"></i></div><span class="small">${c.text}</span></div></td>
      <td class="right mono small">${avg}</td>
      <td><button class="mini" onclick="openNode('${esc(n.id)}')">看表</button>
          ${n.level==='region'?`<button class="mini danger" onclick="removeProxy('${esc(n.id)}')">退出</button>`:''}
      </td>
    </tr>`;
  }).join('');
}

function renderLinks(snap) {
  const active = (snap.links || []).map(l => ({...l, retired:false}));
  const retired = (snap.retired_links || []).map(l => ({...l, retired:true}));
  const links = active.concat(retired).sort((a,b)=>a.child_id.localeCompare(b.child_id) || Number(a.retired)-Number(b.retired));
  $('linksBody').innerHTML = links.map(l => {
    const ratio = l.necessary ? l.extra / l.necessary : 0;
    const cls = ratio > .5 ? 'bad' : ratio > .15 ? 'warn' : 'good';
    const barW = Math.min(100, ratio*100);
    return `<tr style="${l.retired?'opacity:.55':''}">
      <td class="mono">${esc(l.parent_id)} → ${esc(l.child_id)} ${l.retired?'<span class="badge">已结束</span>':''}</td>
      <td class="right">${fi(l.up)}</td>
      <td class="right"><b>${fi(l.down)}</b></td>
      <td class="right muted">${fi(l.necessary)}</td>
      <td style="min-width:150px">
        <div class="small ${cls}">${fi(l.extra)} · ${pct(ratio)}</div>
        <div class="bar ${ratio>.5?'bad':ratio>.15?'warn':''}"><i style="width:${barW}%"></i></div>
      </td>
    </tr>`;
  }).join('');
  const totalDown = links.reduce((a,l)=>a+l.down,0);
  const totalNeed = links.reduce((a,l)=>a+l.necessary,0);
  const totalUp = links.reduce((a,l)=>a+l.up,0);
  $('kpiTraffic').textContent = fi(totalDown);
  $('kpiTrafficHint').textContent = `Up ${fi(totalUp)}；必要下行 ${fi(totalNeed)}；链路级超投 ${totalNeed? pct((totalDown-totalNeed)/totalNeed):'-'}`;
}

function renderHist(h) {
  if (!h || !h.count) { $('hist').innerHTML = '<span class="muted">尚无数据</span>'; return; }
  const max = Math.max(...h.counts, 1);
  let labels = ['≤'+h.bounds_ns[0]+'ns'];
  h.bounds_ns.slice(1).forEach((b,i)=> labels.push('≤'+us(b)));
  labels.push('>'+us(h.bounds_ns[h.bounds_ns.length-1]));
  $('hist').innerHTML = `
    <div class="small muted" style="margin-bottom:6px">count=${fi(h.count)} avg=${us(Math.round(h.total_ns/h.count))} p95≈${us(h_P95(h))} max=${us(h.max_ns)}</div>
    ${h.counts.map((c,i)=>`<div class="hist-row">
      <span class="muted">${esc(labels[i] || i)}</span>
      <div class="hist-bar"><i style="width:${100*c/max}%"></i></div>
      <span class="right">${fi(c)}</span>
    </div>`).join('')}`;
}
function h_P95(h) {
  // 后端也有分位数；state 直接给 Hist 对象方法未序列化，这里做粗桶展示。
  const want = h.count * .95; let acc=0;
  for (let i=0;i<h.counts.length;i++) { acc += h.counts[i]; if (acc>=want) return h.bounds_ns[Math.min(i,h.bounds_ns.length-1)] || h.max_ns; }
  return h.max_ns;
}

function renderTraces(recent) {
  $('tracesBody').innerHTML = (recent||[]).map(tr => {
    const attrs = (tr.display||[]).map(a => `<span class="attr-chip">${esc(a.label)}: <b>${esc(a.value)}</b></span>`).join('');
    const stores = (tr.delivered_stores||[]).map(s=>`<span class="badge store">${esc(s)}</span>`).join(' ');
    return `<tr>
      <td class="nowrap"><b>${esc(tr.message_id)}</b><div class="muted small">起源 ${esc(tr.origin_id)}</div><div class="small accent">${us(tr.match_ns)}</div></td>
      <td style="min-width:220px">${attrs}</td>
      <td style="min-width:300px"><div class="path">${esc((tr.path||[]).join('\n'))}</div></td>
      <td style="min-width:120px">${stores || '<span class="muted">无门店命中</span>'}<div class="small muted">${(tr.delivered_stores||[]).length} 家</div></td>
    </tr>`;
  }).join('');
}

function renderEvents(snap) {
  $('eventsBody').innerHTML = (snap.events||[]).map((e,i)=>`<tr><td class="muted">${fi(e.seq)}</td><td><span class="badge">${esc(e.kind)}</span> ${esc(e.message)}</td></tr>`).join('');
}

function renderReport(rep) {
  const box = $('verifyBanner');
  const online = $('onlineVerify');
  if (!rep) { box.innerHTML=''; online.innerHTML='<span class="muted">尚未运行在线核对。</span>'; return; }
  if (rep.ok) {
    box.innerHTML = `<div class="banner good">✅ 直判核对通过：${fi(rep.messages_checked)} 条消息 × ${fi(rep.stores_checked)} 家门店，逐条活跃订阅判断 ${fi(rep.active_pairs_evaluated)} 次，没有多投或漏投（${fi(rep.elapsed_ms)} ms）。</div>`;
    online.innerHTML = `<div class="banner good small">最近一次在线核对通过：${fi(rep.messages_checked)} 条，0 差异。</div>`;
  } else {
    box.innerHTML = `<div class="banner bad">❌ 发现 ${(rep.violations||[]).length} 条差异，详见 /api/verify。</div>`;
    online.innerHTML = `<pre class="mono small">${esc(JSON.stringify(rep.violations?.slice(0,5),null,2))}</pre>`;
  }
}

async function refresh() {
  try {
    const st = await api('/api/state');
    lastState = st;
    if (!st.ready) {
      $('statusBadge').textContent = '生成数据/建索引中';
      return;
    }
    $('loadingBanner').style.display = st.done ? 'none' : 'block';
    $('loadingBanner').textContent = `正在回放固定种子消息：${fi(st.progress)} / ${fi(st.config.message_count)} …`;
    $('statusBadge').textContent = st.done ? '仿真完成' : '消息回放中';
    $('statusBadge').className = 'badge ' + (st.done ? 'good' : 'warn');
    $('kpiSubs').textContent = fi(st.config.initial_per_store * st.config.store_count + st.config.extra_subs);
    $('kpiSeed').textContent = `seed=${st.config.seed}；初始 ${fi(st.config.initial_per_store*st.config.store_count)}`;
    $('kpiProgress').textContent = `${((st.progress/st.config.message_count)*100).toFixed(1)}%`;
    $('progressBar').style.width = (100*st.progress/st.config.message_count)+'%';
    $('kpiNodes').textContent = `${st.config.store_count} / ${st.network.nodes.length}`;
    if (st.benchmark && st.benchmark.subscriptions) {
      $('kpiAvgMatch').textContent = st.benchmark.avg_micros.toFixed(2)+' µs';
      $('kpiP95Match').textContent = `全量 ${fi(st.benchmark.subscriptions)} 条；p95 ${st.benchmark.p95_micros.toFixed(2)}µs，最大 ${st.benchmark.max_micros.toFixed(1)}µs`;
    }
    const traces = st.recent || [];
    if (traces[0]) $('kpiLastMatch').textContent = us(traces[0].match_ns);
    renderStoreSelect(st.config.store_count);
    renderTopology(st.network);
    renderNodes(st.network);
    renderLinks(st.network);
    renderHist(st.hist);
    renderTraces(traces);
    renderEvents(st.network);
    renderReport(st.report);
    if (traceLoadedFor !== selectedStore && st.done) loadStoreTrace();
  } catch (e) {
    $('statusBadge').textContent = '接口错误';
    console.error(e);
  }
}

async function loadStoreTrace() {
  selectedStore = $('storeSelect').value;
  const box = $('storeTrace');
  box.innerHTML = '<span class="muted">正在计算血缘……</span>';
  try {
    const tr = await api('/api/store-trace?store='+encodeURIComponent(selectedStore)+'&raw_limit=30');
    traceLoadedFor = selectedStore;
    box.innerHTML = `
      <div class="banner info small">门店 <b>${esc(tr.store_id)}</b> 当前有 <b>${fi(tr.raw_count)}</b> 条原始订阅。下面每一级都列出“从直接下级收到的过滤器”和“合并/路由表中包含该门店血缘的过滤器”。</div>
      ${(tr.levels||[]).map((lv,i)=>`
        <div class="level-card">
          <h3>${i+1}. ${esc(lv.title)}</h3>
          <div class="grid two-col">
            <div><b>本级收到（来自直接下级）</b><div style="margin-top:6px">${(lv.received||[]).map(f=>filterCard(f)).join('') || '<span class="muted">无</span>'}</div></div>
            <div><b>实际合并/上报中包含本门店的条目</b><div style="margin-top:6px">${(lv.merged||[]).map(f=>filterCard(f,true)).join('') || '<span class="muted">无</span>'}</div></div>
          </div>
        </div>`).join('')}
      <details><summary>查看前 30 条原始订阅</summary><div style="margin-top:8px">${(tr.raw_preview||[]).map(f=>filterCard(f)).join('')}</div></details>`;
  } catch(e) { box.innerHTML = `<div class="banner bad">${esc(e.message)}</div>`; }
}

async function openNode(id) {
  $('nodeModal').classList.add('open');
  $('nodeModalTitle').textContent = '节点 ' + id;
  $('nodeModalBody').innerHTML = '加载中……';
  try {
    const d = await api(`/api/node?id=${encodeURIComponent(id)}&limit=80`);
    $('nodeModalTitle').innerHTML = `节点 <span class="mono">${esc(d.id)}</span> ${levelBadge(d.level)}：收到 ${fi(d.received_count)}，上报 ${fi(d.reported_count)}`;
    $('nodeModalBody').innerHTML = `
      <h3>收到的订阅/下级上报（前 80 条）</h3>
      ${(d.received||[]).map(f=>filterCard(f,true)).join('') || '<span class="muted">空</span>'}
      <h3>实际往上游报 / 合并预览（前 80 条）</h3>
      ${(d.reported||[]).map(f=>filterCard(f,true)).join('') || '<span class="muted">空</span>'}`;
  } catch(e) { $('nodeModalBody').innerHTML = `<div class="banner bad">${esc(e.message)}</div>`; }
}
function closeNode() { $('nodeModal').classList.remove('open'); }

async function removeProxy(id) {
  if (!confirm(`确认让代理 ${id} 退出？其直接下级会自动改挂到它的父代理，订阅表随之重算。`)) return;
  try { await api('/api/proxy/remove', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id})}); await refresh(); }
  catch(e){ alert(e.message); }
}

$('tolRange').addEventListener('input', e => $('tolValue').textContent = e.target.value + '%');
$('tolRange').addEventListener('change', async e => {
  try { await api('/api/tolerance', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({percent:Number(e.target.value)})}); await refresh(); await loadStoreTrace(); }
  catch(err){ alert(err.message); }
});
$('storeSelect').addEventListener('change', () => { traceLoadedFor=null; loadStoreTrace(); });
$('loadStoreTraceBtn').addEventListener('click', loadStoreTrace);
$('replayBtn').addEventListener('click', async () => {
  $('replayBtn').disabled = true;
  try { await api('/api/replay',{method:'POST'}); await refresh(); }
  catch(e){ alert(e.message); } finally { $('replayBtn').disabled=false; }
});
$('verifyBtn').addEventListener('click', async () => {
  $('verifyBtn').disabled = true; $('verifyBtn').textContent='核对中……';
  try { const rep = await api('/api/verify?limit=20000',{method:'POST'}); renderReport(rep); }
  catch(e){ alert(e.message); }
  finally { $('verifyBtn').disabled=false; $('verifyBtn').textContent='直判核对前 2 万条'; }
});
$('addRegionBtn').addEventListener('click', async () => {
  const id = 'RD' + Math.floor(1000 + Math.random()*9000);
  try { await api('/api/proxy/add',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id,parent_id:'HQ'})}); await refresh(); }
  catch(e){ alert(e.message); }
});
$('nodeModal').addEventListener('click', e => { if (e.target.id==='nodeModal') closeNode(); });

refresh();
setInterval(refresh, 1500);
