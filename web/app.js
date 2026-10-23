import { renderMarkdown, escapeHtml as esc } from './md.js';

const $ = (id) => document.getElementById(id);

const KV_CONTAINER = { pathVars: 'pathTable', query: 'queryTable', headers: 'headerTable', formData: 'bodyFormWrap' };
const KV_PLACEHOLDER = {
  pathVars: ['参数名', '参数值'],
  query: ['参数名', '参数值'],
  headers: ['Header 名', 'Header 值'],
  formData: ['字段名', '字段值'],
};

const state = {
  interfaces: [],
  methods: ['GET', 'POST', 'PUT', 'DELETE', 'PATCH', 'HEAD', 'OPTIONS', 'TRACE'],
  dataDir: '',
  currentId: null,
  draft: null,
  view: 'empty', // empty | editor | snapshot
  snapshot: null,
  lastResponse: null,
  pretty: true,
  respTab: 'body',
  filter: '',
  collapsed: new Set(),
  dirty: false,
  sending: false,
};

/* ============ 基础工具 ============ */

// 深拷贝纯数据结构（不依赖 structuredClone，兼容旧版 Edge/Chrome 内核）
function deepClone(v) {
  if (v === null || typeof v !== 'object') return v;
  if (Array.isArray(v)) {
    const arr = new Array(v.length);
    for (let i = 0; i < v.length; i++) arr[i] = deepClone(v[i]);
    return arr;
  }
  if (v instanceof Date) return new Date(v.getTime());
  const out = {};
  for (const k in v) {
    if (Object.prototype.hasOwnProperty.call(v, k)) out[k] = deepClone(v[k]);
  }
  return out;
}

async function api(path, options = {}) {
  const opts = { ...options };
  if (opts.body) opts.headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  const res = await fetch('/api' + path, opts);
  let json = null;
  try {
    json = await res.json();
  } catch (_) {
    json = null;
  }
  if (!res.ok || (json && json.ok === false)) {
    throw new Error((json && json.msg) || '请求失败：HTTP ' + res.status);
  }
  return json ? json.data : null;
}

let toastTimer = null;
function toast(msg, isErr, duration) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.toggle('err', !!isErr);
  el.hidden = false;
  clearTimeout(toastTimer);
  const ms = duration || (isErr ? 4600 : 2200);
  toastTimer = setTimeout(() => { el.hidden = true; }, ms);
}

function promptModal(title, fields, okText) {
  return new Promise((resolve) => {
    $('modalTitle').textContent = title;
    $('modalBody').innerHTML = fields
      .map((f) => {
        const input = f.type === 'textarea'
          ? '<textarea data-f="' + f.name + '">' + esc(f.value || '') + '</textarea>'
          : '<input data-f="' + f.name + '" value="' + esc(f.value || '') + '">';
        return '<label><span>' + esc(f.label) + '</span>' + input + '</label>';
      })
      .join('');
    $('modalOk').textContent = okText || '确定';
    $('modalMask').hidden = false;
    const finish = (val) => {
      $('modalMask').hidden = true;
      $('modalOk').onclick = null;
      $('modalCancel').onclick = null;
      resolve(val);
    };
    $('modalOk').onclick = () => {
      const out = {};
      $('modalBody').querySelectorAll('[data-f]').forEach((el) => { out[el.dataset.f] = el.value; });
      finish(out);
    };
    $('modalCancel').onclick = () => finish(null);
    const first = $('modalBody').querySelector('[data-f]');
    if (first) { first.focus(); first.select && first.select(); }
  });
}

function confirmModal(title, text, okText) {
  return new Promise((resolve) => {
    $('modalTitle').textContent = title;
    $('modalBody').innerHTML = '<div>' + esc(text) + '</div>';
    $('modalOk').textContent = okText || '确定';
    $('modalMask').hidden = false;
    const finish = (val) => {
      $('modalMask').hidden = true;
      $('modalOk').onclick = null;
      $('modalCancel').onclick = null;
      resolve(val);
    };
    $('modalOk').onclick = () => finish(true);
    $('modalCancel').onclick = () => finish(false);
  });
}

function formatBytes(n) {
  if (!n && n !== 0) return '-';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(2) + ' MB';
}

function pad(n) { return n < 10 ? '0' + n : '' + n; }

function timeLabel(ts) {
  const d = new Date(ts);
  return d.getFullYear() + pad(d.getMonth() + 1) + pad(d.getDate()) + '-' + pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds());
}

function timePretty(ts) {
  const d = new Date(ts);
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function tryPrettyJSON(text) {
  if (!text) return null;
  const t = text.trim();
  if (!t.startsWith('{') && !t.startsWith('[')) return null;
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch (_) {
    return null;
  }
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('已复制');
  } catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); toast('已复制'); } catch (e) { toast('复制失败', true); }
    ta.remove();
  }
}

function blankRequest() {
  return {
    method: 'GET',
    url: '',
    pathVars: [],
    query: [],
    headers: [],
    bodyMode: 'none',
    body: '',
    formData: [],
    contentType: '',
    timeout: 30,
    tlsSkip: false,
    followRedirect: true,
  };
}

function markDirty() { state.dirty = true; }

function confirmLeave() {
  if (!state.dirty) return true;
  return window.confirm('当前接口有未保存的修改，确定要离开并放弃修改吗？');
}

/* ============ 数据加载与渲染 ============ */

async function reload() {
  const ws = await api('/workspace');
  state.interfaces = ws.interfaces || [];
  state.methods = ws.methods || state.methods;
  state.dataDir = ws.dataDir || '';
  $('dataDir').textContent = ws.dataDir ? '数据：' + ws.dataDir : '';
  $('dataDir').title = ws.dataFile || '';
}

function visibleInterfaces() {
  const f = state.filter.trim().toLowerCase();
  if (!f) return state.interfaces;
  return state.interfaces.filter((it) => {
    const name = (it.name || '').toLowerCase();
    const url = ((it.request && it.request.url) || '').toLowerCase();
    const group = (it.group || '').toLowerCase();
    return name.includes(f) || url.includes(f) || group.includes(f);
  });
}

function renderTree() {
  const tree = $('tree');
  const list = visibleInterfaces();
  if (!list.length) {
    tree.innerHTML = '<div class="tree-empty">' + (state.interfaces.length ? '没有匹配的接口' : '暂无接口<br>点击左上角「＋ 新建接口」开始') + '</div>';
    return;
  }
  const groups = new Map();
  list.forEach((it) => {
    const g = (it.group || '').trim() || '未分组';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(it);
  });

  const names = [...groups.keys()].sort((a, b) => {
    if (a === '未分组') return 1;
    if (b === '未分组') return -1;
    return a.localeCompare(b, 'zh-CN');
  });

  let html = '';
  names.forEach((g) => {
    const items = groups.get(g).sort((a, b) => (a.createdAt || 0) - (b.createdAt || 0));
    const collapsed = state.collapsed.has(g);
    html += '<div class="group">';
    html += '<div class="group-head" data-group="' + esc(g) + '">';
    html += '<span class="caret">' + (collapsed ? '▶' : '▼') + '</span>';
    html += '<span class="gname">' + esc(g) + '</span>';
    html += '<span class="gcount">' + items.length + '</span>';
    html += '</div>';
    if (collapsed) { html += '</div>'; return; }

    items.forEach((it) => {
      const active = state.view === 'editor' && state.currentId === it.id;
      const method = (it.request && it.request.method) || 'GET';
      const snapCount = (it.snapshots || []).length;
      html += '<div class="node' + (active ? ' active' : '') + '" data-id="' + it.id + '">';
      html += '<span class="m ' + esc(method) + '">' + esc(method) + '</span>';
      html += '<span class="n" title="' + esc((it.request && it.request.url) || '') + '">' + esc(it.name || '未命名接口') + '</span>';
      if (snapCount) html += '<span class="snapcount">' + snapCount + ' 副本</span>';
      html += '</div>';

      (it.snapshots || []).forEach((s) => {
        const isActive = state.view === 'snapshot' && state.snapshot && state.snapshot.sid === s.id;
        const code = (s.response && s.response.status) || '';
        html += '<div class="snap' + (isActive ? ' active' : '') + '" data-id="' + it.id + '" data-sid="' + s.id + '">';
        html += '<span class="dot">●</span>';
        html += '<span class="st" title="' + esc(s.name) + '">' + esc(s.name) + '</span>';
        if (code) html += '<span class="sc">' + code + '</span>';
        html += '</div>';
      });
    });
    html += '</div>';
  });
  tree.innerHTML = html;
}

function renderFoot() {
  const total = state.interfaces.length;
  const snaps = state.interfaces.reduce((n, it) => n + (it.snapshots || []).length, 0);
  $('sidebarFoot').innerHTML = '<span>接口 ' + total + '</span><span>副本 ' + snaps + '</span>';
}

function renderView() {
  $('empty').hidden = state.view !== 'empty';
  $('editor').hidden = !(state.view === 'editor' || state.view === 'snapshot');
}

function renderAll() {
  renderTree();
  renderFoot();
  renderView();
}

/* ============ 编辑器 ============ */

function showResponse(resp) {
  if (resp && resp.status) {
    state.lastResponse = resp;
    state.pretty = true;
    state.respTab = 'body';
    renderResponse(resp);
  } else {
    resetResponse();
  }
}

function applyViewUI() {
  const isSnap = state.view === 'snapshot';
  $('snapBanner').hidden = !isSnap;
  $('noteField').hidden = !isSnap;
  $('btnSaveSnapshot').hidden = isSnap;
  $('ifaceGroup').disabled = isSnap;
  $('btnDelete').textContent = isSnap ? '删除副本' : '删除';
  if (isSnap) {
    $('snapNote').value = state.draft.note || '';
    updateSnapBanner();
  }
}

function updateSnapBanner() {
  if (state.view !== 'snapshot' || !state.draft) return;
  $('snapBannerName').textContent = state.draft.name || '副本';
  $('snapBannerTime').textContent = state.draft.createdAt ? '保存于 ' + timePretty(state.draft.createdAt) : '';
}

function fillEditor() {
  const d = state.draft;
  const r = d.request;
  $('method').innerHTML = state.methods.map((m) => '<option value="' + m + '">' + m + '</option>').join('');
  $('method').value = r.method || 'GET';
  $('url').value = r.url || '';
  $('ifaceName').value = d.name || '';
  $('ifaceGroup').value = d.group || '';
  $('bodyMode').value = r.bodyMode || 'none';
  $('bodyText').value = r.body || '';
  $('rawContentType').value = r.contentType || '';
  $('docText').value = d.doc || '';
  $('optTimeout').value = r.timeout || 30;
  $('optTLSSkip').checked = !!r.tlsSkip;
  $('optRedirect').checked = r.followRedirect !== false;
  renderGroupOptions();
  syncBodyUI();
  renderKV('pathVars');
  renderKV('query');
  renderKV('headers');
  renderKV('formData');
  renderDoc();
  showResponse(d.response);
  applyViewUI();
  renderBadges();
}

function renderGroupOptions() {
  const names = [...new Set(state.interfaces.map((it) => (it.group || '').trim()).filter(Boolean))];
  $('groupList').innerHTML = names.map((n) => '<option value="' + esc(n) + '"></option>').join('');
}

function renderBadges() {
  const r = state.draft.request;
  const count = (rows) => rows.filter((x) => x.key && x.enabled).length;
  const n1 = count(r.pathVars) + count(r.query);
  const n2 = count(r.headers);
  $('badgeParams').textContent = n1 ? String(n1) : '';
  $('badgeHeaders').textContent = n2 ? String(n2) : '';
  $('badgeBody').textContent = r.bodyMode === 'none' ? '' : '1';
}

function syncBodyUI() {
  const m = state.draft.request.bodyMode;
  const isText = m === 'json' || m === 'raw';
  $('bodyTextWrap').hidden = !isText;
  $('bodyFormWrap').hidden = !(m === 'form' || m === 'urlencoded');
  $('rawCtWrap').hidden = m !== 'raw';
  $('btnFormatJson').hidden = m !== 'json';
  $('bodyText').placeholder = m === 'json' ? '{\n  "name": "value"\n}' : '请求体原文…';
}

function renderKV(kind) {
  const box = $(KV_CONTAINER[kind]);
  if (!box || !state.draft) return;
  const rows = state.draft.request[kind] || (state.draft.request[kind] = []);
  const [pk, pv] = KV_PLACEHOLDER[kind];
  let html = '<table class="kv-table"><thead><tr>' +
    '<th class="chk"></th><th>参数名</th><th>值</th><th>说明</th><th class="act"></th>' +
    '</tr></thead><tbody>';
  rows.forEach((row, idx) => {
    html += '<tr class="' + (row.enabled ? '' : 'off') + '">';
    html += '<td class="chk"><input type="checkbox" data-kv="' + kind + '" data-idx="' + idx + '" data-field="enabled"' + (row.enabled ? ' checked' : '') + '></td>';
    html += '<td><input type="text" spellcheck="false" data-kv="' + kind + '" data-idx="' + idx + '" data-field="key" value="' + esc(row.key) + '" placeholder="' + pk + '"></td>';
    html += '<td><input type="text" spellcheck="false" data-kv="' + kind + '" data-idx="' + idx + '" data-field="value" value="' + esc(row.value) + '" placeholder="' + pv + '"></td>';
    html += '<td><input type="text" spellcheck="false" data-kv="' + kind + '" data-idx="' + idx + '" data-field="desc" value="' + esc(row.desc || '') + '" placeholder="备注（可选）"></td>';
    html += '<td class="act"><button class="kv-del" data-del="' + kind + '" data-idx="' + idx + '" title="删除该行">×</button></td>';
    html += '</tr>';
  });
  html += '</tbody></table>';
  html += '<button class="kv-add" data-add="' + kind + '">＋ 添加一行</button>';
  box.innerHTML = html;
}

function restoreFocus(kind, idx, field) {
  const el = document.querySelector(
    '[data-kv="' + kind + '"][data-idx="' + idx + '"][data-field="' + field + '"]'
  );
  if (el) {
    el.focus();
    if (el.setSelectionRange && typeof el.value === 'string') {
      const len = el.value.length;
      el.setSelectionRange(len, len);
    }
  }
}

function syncPathVars() {
  const url = state.draft.request.url || '';
  const found = [...url.matchAll(/\{([^{}]+)\}/g)].map((m) => m[1]);
  const rows = state.draft.request.pathVars || [];
  const exists = new Map(rows.map((r) => [r.key, r]));
  const next = [];
  found.forEach((name) => {
    if (next.some((r) => r.key === name)) return;
    next.push(exists.get(name) || { key: name, value: '', desc: '', enabled: true });
  });
  state.draft.request.pathVars = next;
  renderKV('pathVars');
}

function renderDoc() {
  $('docPreview').innerHTML = renderMarkdown(state.draft ? state.draft.doc : '');
}

function resetResponse() {
  state.lastResponse = null;
  state.pretty = true;
  state.respTab = 'body';
  $('respStat').className = 'resp-stat';
  $('respStat').textContent = '尚未发送请求';
  $('respBody').textContent = '';
  $('respHeaders').innerHTML = '';
  $('respBody').hidden = false;
  $('respHeaders').hidden = true;
  document.querySelectorAll('[data-rtab]').forEach((b) => b.classList.toggle('active', b.dataset.rtab === 'body'));
}

/* ============ 打开 / 新建 / 保存 ============ */

async function openInterface(id) {
  const it = state.interfaces.find((x) => x.id === id);
  if (!it) return;
  if (state.view === 'editor' && state.currentId === id) return;
  if (!confirmLeave()) return;
  state.currentId = id;
  state.draft = deepClone(it);
  state.snapshot = null;
  state.view = 'editor';
  state.dirty = false;
  renderAll();
  fillEditor();
}

async function createInterface() {
  if (!confirmLeave()) return;
  const group = state.draft ? state.draft.group || '' : '';
  const it = await api('/interfaces', {
    method: 'POST',
    body: JSON.stringify({ name: '未命名接口', group, request: blankRequest() }),
  });
  state.interfaces.push(it);
  state.currentId = null;
  state.view = 'empty';
  await openInterface(it.id);
  $('ifaceName').focus();
  $('ifaceName').select();
}

async function saveInterface(silent) {
  if (!state.draft || !state.draft.id) return null;
  const d = state.draft;
  if (!String(d.name || '').trim()) d.name = '未命名接口';
  d.request.bodyMode = d.request.bodyMode || 'none';

  let saved;
  if (state.view === 'snapshot' && d.sid) {
    // 保存副本：全量更新（名称/备注/文档/请求配置/出参）
    saved = await api('/interfaces/' + d.id + '/snapshots/' + d.sid, {
      method: 'PUT',
      body: JSON.stringify({
        name: d.name,
        note: d.note || '',
        doc: d.doc || '',
        request: d.request,
        response: d.response || { status: 0 },
      }),
    });
    const it = state.interfaces.find((x) => x.id === d.id);
    if (it) {
      it.snapshots = it.snapshots || [];
      const idx = it.snapshots.findIndex((s) => s.id === d.sid);
      if (idx >= 0) it.snapshots[idx] = saved;
      else it.snapshots.push(saved);
    }
  } else {
    // 保存接口：请求配置 + 最近一次出参一起落库
    saved = await api('/interfaces/' + d.id, { method: 'PUT', body: JSON.stringify(d) });
    const idx = state.interfaces.findIndex((x) => x.id === d.id);
    if (idx >= 0) state.interfaces[idx] = saved;
    state.draft.snapshots = saved.snapshots || [];
  }

  state.dirty = false;
  renderTree();
  renderFoot();
  if (!silent) toast('已保存');
  return saved;
}

async function duplicateInterface() {
  if (!state.draft || !state.draft.id) return;

  // 副本模式下「复制接口」= 以副本配置新建一个普通接口
  if (state.view === 'snapshot') {
    const cp = await api('/interfaces', {
      method: 'POST',
      body: JSON.stringify({
        name: (state.draft.name || '接口') + '_副本',
        group: state.draft.group || '',
        request: state.draft.request,
        doc: state.draft.doc || '',
      }),
    });
    state.interfaces.push(cp);
    toast('已复制为接口「' + cp.name + '」');
    state.currentId = null;
    state.view = 'empty';
    await openInterface(cp.id);
    return;
  }

  await saveInterface(true);
  const cp = await api('/interfaces/' + state.draft.id + '/duplicate', { method: 'POST' });
  state.interfaces.push(cp);
  toast('已复制为「' + cp.name + '」');
  state.currentId = null;
  state.view = 'empty';
  await openInterface(cp.id);
}

async function deleteInterface() {
  if (!state.draft || !state.draft.id) return;

  // 副本模式下 = 删除副本
  if (state.view === 'snapshot') {
    const go = await confirmModal('删除副本', '确定删除副本「' + (state.draft.name || '') + '」？删除后不可撤销。', '删除');
    if (!go) return;
    const ifaceId = state.draft.id;
    await api('/interfaces/' + ifaceId + '/snapshots/' + state.draft.sid, { method: 'DELETE' });
    const it = state.interfaces.find((x) => x.id === ifaceId);
    if (it) it.snapshots = (it.snapshots || []).filter((s) => s.id !== state.draft.sid);
    state.draft = null;
    state.snapshot = null;
    state.currentId = null;
    state.view = 'empty';
    state.dirty = false;
    renderAll();
    toast('已删除副本');
    if (it) await openInterface(it.id);
    return;
  }

  const go = await confirmModal('删除接口', '确定删除「' + (state.draft.name || '未命名接口') + '」及其下全部副本？删除后不可撤销。', '删除');
  if (!go) return;
  await api('/interfaces/' + state.draft.id, { method: 'DELETE' });
  state.interfaces = state.interfaces.filter((x) => x.id !== state.draft.id);
  state.draft = null;
  state.currentId = null;
  state.view = 'empty';
  state.dirty = false;
  renderAll();
  toast('已删除');
}

/* ============ 发送请求 ============ */

function statusClass(code) {
  if (!code) return 'err';
  if (code >= 200 && code < 400) return 'ok';
  return 'err';
}

async function send() {
  if (!state.draft || state.sending) return;
  state.sending = true;
  $('btnSend').disabled = true;
  $('btnSend').textContent = '发送中…';
  $('respStat').className = 'resp-stat';
  $('respStat').textContent = '请求中…';
  try {
    const resp = await api('/execute', { method: 'POST', body: JSON.stringify(state.draft.request) });
    state.lastResponse = resp;
    state.draft.response = resp; // 出参随下一次保存一起落库
    renderResponse(resp);
  } catch (err) {
    state.lastResponse = null;
    $('respStat').className = 'resp-stat err';
    $('respStat').textContent = '请求失败';
    $('respBody').textContent = err.message;
    $('respBody').hidden = false;
    $('respHeaders').hidden = true;
    toast(err.message, true);
  } finally {
    state.sending = false;
    $('btnSend').disabled = false;
    $('btnSend').textContent = '发送';
  }
}

function renderResponse(resp) {
  const stat = $('respStat');
  if (!resp.status) {
    stat.className = 'resp-stat err';
    stat.textContent = resp.error || '请求失败';
    $('respBody').textContent = resp.error || '';
    $('respBody').hidden = false;
    $('respHeaders').innerHTML = '';
    $('respHeaders').hidden = true;
    return;
  }
  const bits = [resp.status + (resp.statusText ? ' ' + resp.statusText : ''), resp.durationMs + ' ms', formatBytes(resp.bodyBytes)];
  if (resp.contentType) bits.push(resp.contentType.split(';')[0]);
  if (resp.charset && resp.charset !== 'utf-8') bits.push(resp.charset);
  if (resp.truncated) bits.push('响应过大已截断');
  stat.className = 'resp-stat ' + statusClass(resp.status);
  stat.textContent = bits.join('  ·  ');

  renderRespBody($('respBody'), resp);
  $('respHeaders').innerHTML = headersHtml(resp.headers || []);
  applyRespTab();
}

function renderRespBody(el, resp) {
  let text = resp.body || '';
  if (state.pretty) {
    const p = tryPrettyJSON(resp.body);
    if (p) text = p;
  }
  if (!text) text = '（响应体为空）';
  if (resp.error) text += '\n\n⚠ ' + resp.error;
  el.textContent = text;
}

function headersHtml(headers) {
  if (!headers || !headers.length) return '<div style="color:#6e6e6e">（无响应头）</div>';
  let html = '<table>';
  headers.forEach((h) => {
    html += '<tr><td>' + esc(h.key) + '</td><td>' + esc(h.value) + '</td></tr>';
  });
  html += '</table>';
  return html;
}

function applyRespTab() {
  const isBody = state.respTab === 'body';
  $('respBody').hidden = !isBody;
  $('respHeaders').hidden = isBody;
  document.querySelectorAll('[data-rtab]').forEach((b) => b.classList.toggle('active', b.dataset.rtab === state.respTab));
}

/* ============ 副本 ============ */

async function saveSnapshot() {
  if (!state.draft || !state.draft.id) return;
  if (state.view === 'snapshot') return; // 副本下不允许再保存副本（按钮已隐藏，双保险）
  const resp = state.draft.response;
  if (!resp || !resp.status) {
    toast('请先点「发送」拿到出参，副本会把接口配置和出参一起保存', true);
    return;
  }
  const fields = await promptModal('保存接口副本', [
    { name: 'name', label: '副本名称', value: '副本-' + timeLabel(Date.now()) },
    { name: 'note', label: '备注（可选）', value: '', type: 'textarea' },
  ], '保存副本');
  if (!fields) return;
  if (!String(fields.name || '').trim()) fields.name = '副本-' + timeLabel(Date.now());
  const snap = await api('/interfaces/' + state.draft.id + '/snapshots', {
    method: 'POST',
    body: JSON.stringify({
      name: fields.name,
      note: fields.note || '',
      doc: state.draft.doc || '',
      request: state.draft.request,
      response: resp,
    }),
  });
  // 先把当前编辑内容（含出参）落库，再同步树
  await saveInterface(true);
  const it = state.interfaces.find((x) => x.id === state.draft.id);
  if (it) {
    it.snapshots = it.snapshots || [];
    it.snapshots.push(snap);
    state.draft.snapshots = it.snapshots;
  }
  renderAll();
  toast('已保存副本，挂在接口下级');
}

async function openSnapshot(ifaceId, sid) {
  if (!confirmLeave()) return;
  const snap = await api('/interfaces/' + ifaceId + '/snapshots/' + sid);
  const iface = state.interfaces.find((x) => x.id === ifaceId);
  state.currentId = ifaceId;
  state.snapshot = { ifaceId, sid };
  state.draft = {
    id: ifaceId,
    sid,
    name: snap.name || '',
    note: snap.note || '',
    group: iface ? iface.group || '' : '',
    request: deepClone(snap.request || blankRequest()),
    response: snap.response ? deepClone(snap.response) : null,
    doc: snap.doc || '',
    createdAt: snap.createdAt || Date.now(),
    snapshots: [],
  };
  state.view = 'snapshot';
  state.dirty = false;
  renderAll();
  fillEditor();
}

/* ============ 导入导出 ============ */

function exportBackup() {
  const a = document.createElement('a');
  a.href = '/api/export';
  a.download = 'interfaces-backup-' + timeLabel(Date.now()) + '.json';
  document.body.appendChild(a);
  a.click();
  a.remove();
  toast('已导出备份文件');
}

async function importBackup(file) {
  const text = await file.text();
  const go = await confirmModal('导入备份', '导入将覆盖当前全部接口数据（现有数据会自动进入 data/backup 备份）。确定继续？', '覆盖导入');
  if (!go) return;
  await api('/import', { method: 'POST', body: text });
  state.currentId = null;
  state.draft = null;
  state.view = 'empty';
  state.dirty = false;
  await reload();
  renderAll();
  toast('导入完成');
}

/* ============ 事件绑定 ============ */

function onEdit(e) {
  const t = e.target;

  if (t.dataset && t.dataset.kv) {
    const kind = t.dataset.kv;
    const idx = Number(t.dataset.idx);
    const field = t.dataset.field;
    const rows = state.draft.request[kind];
    if (!rows || !rows[idx]) return;
    if (field === 'enabled') {
      rows[idx].enabled = t.checked;
      const tr = t.closest('tr');
      if (tr) tr.classList.toggle('off', !t.checked);
      renderBadges();
      markDirty();
      return;
    }
    rows[idx][field] = t.value;
    markDirty();
    renderBadges();
    // 末行有内容时自动追加空行（Postman 习惯）
    const last = rows[rows.length - 1];
    if (idx === rows.length - 1 && (last.key || last.value)) {
      rows.push({ key: '', value: '', desc: '', enabled: true });
      renderKV(kind);
      restoreFocus(kind, idx, field);
    }
    return;
  }

  if (t.id === 'search') {
    state.filter = t.value;
    renderTree();
    return;
  }

  if (!t.id || !state.draft) return;
  const r = state.draft.request;
  switch (t.id) {
    case 'url':
      r.url = t.value;
      syncPathVars();
      markDirty();
      break;
    case 'method':
      r.method = t.value;
      markDirty();
      renderTree();
      break;
    case 'ifaceName':
      state.draft.name = t.value;
      markDirty();
      renderTree();
      break;
    case 'ifaceGroup':
      if (state.view === 'snapshot') break; // 副本分组跟随所属接口，只读
      state.draft.group = t.value;
      markDirty();
      break;
    case 'snapNote':
      state.draft.note = t.value;
      markDirty();
      break;
    case 'bodyMode':
      r.bodyMode = t.value;
      syncBodyUI();
      renderBadges();
      markDirty();
      break;
    case 'bodyText':
      r.body = t.value;
      markDirty();
      break;
    case 'rawContentType':
      r.contentType = t.value;
      markDirty();
      break;
    case 'docText':
      state.draft.doc = t.value;
      markDirty();
      renderDoc();
      break;
    case 'optTimeout': {
      const n = Number(t.value);
      r.timeout = Number.isFinite(n) && n > 0 ? n : 30;
      markDirty();
      break;
    }
    case 'optTLSSkip':
      r.tlsSkip = t.checked;
      markDirty();
      break;
    case 'optRedirect':
      r.followRedirect = t.checked;
      markDirty();
      break;
    default:
      break;
  }
}

function onClick(e) {
  const t = e.target;

  const addBtn = t.closest('[data-add]');
  if (addBtn) {
    const kind = addBtn.dataset.add;
    state.draft.request[kind].push({ key: '', value: '', desc: '', enabled: true });
    renderKV(kind);
    markDirty();
    return;
  }

  const delBtn = t.closest('[data-del]');
  if (delBtn) {
    const kind = delBtn.dataset.del;
    const idx = Number(delBtn.dataset.idx);
    state.draft.request[kind].splice(idx, 1);
    renderKV(kind);
    renderBadges();
    markDirty();
    return;
  }

  const groupHead = t.closest('.group-head');
  if (groupHead) {
    const g = groupHead.dataset.group;
    if (state.collapsed.has(g)) state.collapsed.delete(g);
    else state.collapsed.add(g);
    renderTree();
    return;
  }

  const snapNode = t.closest('.snap');
  if (snapNode) {
    openSnapshot(snapNode.dataset.id, snapNode.dataset.sid);
    return;
  }

  const node = t.closest('.node');
  if (node) {
    openInterface(node.dataset.id);
    return;
  }

  const subTab = t.closest('[data-tab]');
  if (subTab) {
    const tab = subTab.dataset.tab;
    document.querySelectorAll('#subtabs .subtab').forEach((b) => b.classList.toggle('active', b === subTab));
    document.querySelectorAll('.pane').forEach((p) => { p.hidden = p.dataset.pane !== tab; });
    return;
  }

  const rtab = t.closest('[data-rtab]');
  if (rtab) {
    state.respTab = rtab.dataset.rtab;
    applyRespTab();
    return;
  }

  const quick = t.closest('[data-qt]');
  if (quick) {
    const key = quick.dataset.qt;
    const val = quick.dataset.qv || '';
    const rows = state.draft.request.headers;
    const exist = rows.find((r) => r.key.toLowerCase() === key.toLowerCase());
    if (exist) {
      exist.value = val;
      exist.enabled = true;
    } else {
      rows.unshift({ key, value: val, desc: '', enabled: true });
    }
    renderKV('headers');
    renderBadges();
    markDirty();
    return;
  }

  const docQuick = t.closest('[data-doc]');
  if (docQuick) {
    const kind = docQuick.dataset.doc;
    const ta = $('docText');
    const snippets = {
      h2: '\n## 接口说明\n\n在这里补充接口用途、注意事项。\n',
      table: '\n| 字段 | 类型 | 必填 | 说明 |\n| --- | --- | --- | --- |\n| id | string | 是 | 主键 |\n',
      code: '\n```json\n{\n  "code": 0,\n  "data": {}\n}\n```\n',
    };
    ta.value += snippets[kind] || '';
    state.draft.doc = ta.value;
    renderDoc();
    markDirty();
    ta.focus();
    return;
  }
}

function bindEvents() {
  document.addEventListener('input', onEdit);
  document.addEventListener('change', onEdit);
  document.addEventListener('click', onClick);

  $('btnNew').onclick = () => createInterface().catch((e) => toast(e.message, true));
  $('btnSave').onclick = () => saveInterface(false).catch((e) => toast(e.message, true));
  $('btnSend').onclick = () => send();
  $('btnDuplicate').onclick = () => duplicateInterface().catch((e) => toast(e.message, true));
  $('btnDelete').onclick = () => deleteInterface().catch((e) => toast(e.message, true));
  $('btnSaveSnapshot').onclick = () => saveSnapshot().catch((e) => toast(e.message, true));
  $('btnExport').onclick = () => exportBackup();
  $('btnImport').onclick = () => $('fileInput').click();
  $('fileInput').onchange = (e) => {
    const f = e.target.files && e.target.files[0];
    e.target.value = '';
    if (f) importBackup(f).catch((err) => toast(err.message, true));
  };

  $('btnPretty').onclick = () => {
    state.pretty = !state.pretty;
    if (state.lastResponse) renderRespBody($('respBody'), state.lastResponse);
    toast(state.pretty ? '已格式化显示' : '显示原始内容');
  };
  $('btnCopyResp').onclick = () => copyText($('respBody').textContent);

  $('btnFormatJson').onclick = () => {
    const ta = $('bodyText');
    const p = tryPrettyJSON(ta.value);
    if (!p) { toast('当前内容不是合法 JSON', true); return; }
    ta.value = p;
    state.draft.request.body = p;
    markDirty();
    toast('已格式化');
  };

  $('btnBackToInterface').onclick = () => {
    const ifaceId = state.snapshot && state.snapshot.ifaceId;
    state.snapshot = null;
    state.view = 'empty';
    state.currentId = null;
    renderAll();
    if (ifaceId) openInterface(ifaceId).catch((e) => toast(e.message, true));
  };

  // Ctrl+S 保存
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      if (state.view === 'editor' || state.view === 'snapshot') saveInterface(false).catch((err) => toast(err.message, true));
    }
  });

  window.addEventListener('beforeunload', (e) => {
    if (state.dirty) {
      e.preventDefault();
      e.returnValue = '';
    }
  });
}

/* ============ 启动 ============ */

// 3 秒心跳：只让后端知道「页面窗口是否还开着」，供托盘判断要不要重复开窗。
// 心跳绝不会导致程序退出 —— 退出统一由托盘右键菜单「退出」控制。
function startHeartbeat() {
  const ping = () => {
    fetch('/api/ping', { cache: 'no-store' }).catch(() => {});
  };
  ping();
  setInterval(ping, 3000);
}

const TRAY_HINT_KEY = 'interface-go.tray-hint-shown';

// 首次运行时提示一次「关窗口不等于退出程序」，避免用户找不到退出入口。
function showTrayHintOnce() {
  try {
    if (localStorage.getItem(TRAY_HINT_KEY)) return;
    localStorage.setItem(TRAY_HINT_KEY, '1');
  } catch (_) {
    return; // 隐私模式等 localStorage 不可用时静默跳过
  }
  toast('提示：关闭窗口不会退出程序，程序会继续在系统托盘运行；双击托盘图标可重新打开窗口，右键菜单可退出。若看不到图标，请点击任务栏右下角的 ^ 展开。', false, 12000);
}

async function boot() {
  bindEvents();
  startHeartbeat();
  showTrayHintOnce();
  try {
    await reload();
  } catch (err) {
    toast('加载数据失败：' + err.message, true);
    return;
  }
  renderAll();
  if (state.interfaces.length) {
    await openInterface(state.interfaces[0].id);
  }
}

boot();
