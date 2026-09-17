'use strict';
const $ = (s, root = document) => root.querySelector(s);
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const json = v => typeof v === 'string' ? v : JSON.stringify(v, null, 2);
const date = v => v ? (Number.isNaN(Date.parse(v)) ? v : new Date(v).toLocaleString('zh-CN')) : '—';
const object = v => v !== null && typeof v === 'object' && !Array.isArray(v);
const pages = {overview:['概览','查看应用与请求统计，以及最近操作。'],apps:['应用 Token','为调用方配置权限、有效期与访问凭据。'],tables:['资源','查看已接入的数据表及服务端策略。'],debug:['调试','以应用身份发送请求，查看飞书原始响应。'],logs:['日志','最近 100 条操作记录。'],settings:['设置','配置网关使用的飞书应用凭据。']};
const scopes = {'requirements:read':'读取需求','requirements:status':'更新需求状态','tasks:read':'读取任务','tasks:write':'创建与更新任务','bugs:read':'读取 BUG','bugs:create':'创建 BUG','bugs:status':'更新 BUG 状态','bugs:edit':'修改 BUG 字段'};
const page = $('#page'), dialog = $('#dialog');
let apps = [], current = '', generation = 0, writing = false, timer;
async function api(path, method = 'GET', body) {
  const options = {method, credentials:'same-origin', headers:{Accept:'application/json'}};
  if (method !== 'GET') { options.headers['X-Requested-With'] = 'FeishuGateway'; options.headers['Content-Type'] = 'application/json'; options.body = JSON.stringify(body); }
  const response = await fetch('/admin-api/' + path, options);
  let envelope;
  try { envelope = await response.json(); } catch { throw Error(`HTTP ${response.status}：管理接口未返回 JSON`); }
  if (!response.ok || envelope?.error) { const err = Error(`HTTP ${response.status}：${envelope?.error?.message || response.statusText || '请求失败'}`); err.details=envelope; throw err; }
  if (!envelope || !Object.hasOwn(envelope, 'data')) throw Error('管理接口响应缺少 data');
  return envelope.data;
}
function notify(text) { clearTimeout(timer); $('#notice').textContent = text; $('#notice').hidden = false; timer = setTimeout(() => { $('#notice').hidden = true; }, 4000); }
function error(target, err) { target.innerHTML = `<div class="error" role="alert">${esc(err.message)}</div>`; }
function button(label, action, id = '', cls = '') { return `<button type="button" class="${cls}" data-action="${action}" data-id="${esc(id)}">${label}</button>`; }
function table(headers, rows) { return rows.length ? `<div class="table-wrap"><table><thead><tr>${headers.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.map(r => `<tr>${r.map(c => `<td>${c}</td>`).join('')}</tr>`).join('')}</tbody></table></div>` : '<div class="empty">暂无记录</div>'; }
function logsView(rows) { return table(['时间','应用','操作 / 资源','状态 / 结果','请求 ID'], rows.map(l => [esc(date(l.time)),esc(l.appName),`${esc(l.action)}<div class="hint">${esc(l.table)}</div>`,`${esc(l.status)}<div>${esc(json(l.result))}</div>`,`<code>${esc(l.requestId)}</code><div class="hint">${esc(l.id)}</div>`])); }
function metric(label, value) { return `<section class="card"><h2>${label}</h2>${object(value) || Array.isArray(value) ? `<pre class="metric-detail">${esc(json(value))}</pre>` : `<div class="value">${esc(value ?? '—')}</div>`}</section>`; }
async function load() {
  const requested = location.hash.slice(1), next = Object.hasOwn(pages, requested) ? requested : 'overview';
  if (writing) { history.replaceState(null, '', '#' + current); return; }
  current = next;
  if (dialog.open) closeDialog();
  const version = ++generation;
  $('#title').textContent = $('#crumb').textContent = pages[current][0]; $('#subtitle').textContent = pages[current][1];
  document.querySelectorAll('nav a').forEach(a => { if (a.hash === '#' + current) a.setAttribute('aria-current','page'); else a.removeAttribute('aria-current'); });
  page.innerHTML = '<div class="empty">正在加载…</div>';
  try {
    const data = await (current === 'debug' ? Promise.all([api('apps'), api('tables')]) : api(current === 'tables' ? 'tables' : current));
    if (version !== generation) return;
    if (current === 'overview') page.innerHTML = `<div class="stats">${metric('应用',data.apps)}${metric('请求',data.requests)}</div><section class="card"><h2>最近操作</h2>${logsView(data.recentLogs)}</section>`;
    if (current === 'apps') { apps = data; renderApps(); }
    if (current === 'tables') page.innerHTML = `<div class="resource-grid">${data.map(t => `<section class="card"><h2>${esc(t.name)}</h2><code>${esc(t.key)}</code><p>状态字段：${esc(t.statusField)}</p><span class="badge ${t.statusWritable ? '' : 'off'}">${t.statusWritable ? '状态可写' : '状态不可写'}</span><p class="hint">服务端策略</p><pre class="metric-detail">${esc(json(t.policy))}</pre></section>`).join('')}</div>`;
    if (current === 'logs') page.innerHTML = `<section class="card">${logsView(data)}</section>`;
    if (current === 'settings') renderSettings(data);
    if (current === 'debug') renderDebug(...data);
  } catch (err) { if (version === generation) error(page, err); }
}
async function once(container, action) {
  if (writing) return;
  writing = true;
  const controls = [...container.querySelectorAll('button,input,select,textarea')].filter(el => !el.disabled);
  controls.forEach(el => { el.disabled = true; }); $('#reload').disabled = true;
  const errors = $('.form-error', container); if (errors) errors.replaceChildren();
  try { await action(); } catch (err) { if (errors?.isConnected) error(errors, err); else notify(err.message); }
  finally { writing = false; controls.forEach(el => { el.disabled = false; }); $('#reload').disabled = false; }
}
function renderApps() {
  page.innerHTML = `<section class="card"><div class="card-head"><h2>访问应用 · ${apps.length}</h2>${button('创建应用','new','','primary')}</div>${table(['应用','权限','状态 / 有效期','创建 / 最近调用','操作'],apps.map(a => [
    `<strong>${esc(a.name)}</strong><div class="hint">${esc(a.description)}</div><code>${esc(a.tokenPrefix)}</code>`,a.scopes.map(s => `<div><code>${esc(s)}</code></div>`).join(''),
    `<span class="badge ${a.enabled ? '' : 'off'}">${a.enabled ? '已启用' : '已禁用'}</span><div class="hint">${a.expiresAt ? esc(date(a.expiresAt)) : '永不过期'}</div>`,`${esc(date(a.createdAt))}<div class="hint">${esc(date(a.lastUsedAt))}</div>`,
    `<div class="actions">${button('编辑','edit',a.id)}${button(a.enabled ? '禁用' : '启用','toggle',a.id)}${button('轮换 Token','rotate',a.id)}</div>`
  ]))}<p class="hint">完整 Token 仅在创建或轮换后显示一次。轮换后旧 Token 立即失效。</p><div class="form-error"></div></section>`;
}
function rememberApp(app) { const i = apps.findIndex(a => a.id === app.id); if (i < 0) apps.push(app); else apps[i] = app; if (current === 'apps') renderApps(); }
function openDialog(title, content) { dialog.innerHTML = `<div class="card-head"><h2 id="dialog-title">${title}</h2>${button('关闭','close')}</div>${content}`; if (!dialog.open) dialog.showModal(); }
function closeDialog() { dialog.close(); dialog.replaceChildren(); }
function showToken(data) {
  rememberApp(data.app);
  openDialog('Token 已生成',`<p>应用：${esc(data.app.name)}</p><p class="warning">完整 Token 只显示这一次，请在关闭前保存。</p><pre class="token" id="token">${esc(data.token)}</pre><div class="actions">${button('复制 Token','copy')}</div><div class="form-error"></div>`);
}
function localDate(value) { if (!value) return ''; const d = new Date(value); return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0,19); }
function editApp(app) {
  openDialog(app ? '编辑应用' : '创建应用',`<form id="app-form"><label>应用名称<input name="name" required value="${esc(app?.name)}"></label><label>用途说明<textarea name="description">${esc(app?.description)}</textarea></label><label>到期时间（本地时间，留空表示永不过期）<input type="datetime-local" step="1" name="expiresAt" value="${esc(localDate(app?.expiresAt))}"></label><fieldset><legend>访问权限</legend><div class="scopes">${Object.entries(scopes).map(([s,label]) => `<label><input type="checkbox" name="scope" value="${s}" ${app?.scopes.includes(s) ? 'checked' : ''}> ${label}<code>${s}</code></label>`).join('')}</div></fieldset><p class="hint">实际可写范围以资源策略和飞书字段能力为准。</p><div class="form-error"></div><button class="primary" type="submit">${app ? '保存修改' : '创建并生成 Token'}</button></form>`);
  $('#app-form').onsubmit = e => {
    e.preventDefault(); const form = e.currentTarget, f = new FormData(form);
    const payload = {name:f.get('name').trim(),description:f.get('description'),scopes:f.getAll('scope'),expiresAt:f.get('expiresAt') ? new Date(f.get('expiresAt')).toISOString() : null};
    if (!payload.name) return error($('.form-error',form), Error('请填写应用名称'));
    once(dialog, async () => { const data = await api(app ? 'apps/' + encodeURIComponent(app.id) : 'apps',app ? 'PATCH' : 'POST',payload); if (app) { rememberApp(data); closeDialog(); notify('应用已保存'); } else showToken(data); });
  };
}
function renderSettings(settings) {
  page.innerHTML = `<section class="card form-narrow"><h2>飞书应用</h2><p id="secret-state" class="hint">${settings.secretConfigured ? '已配置 App Secret' : '尚未配置 App Secret'}</p><form id="settings-form"><label>App ID<input name="appId" required value="${esc(settings.appId)}" autocomplete="off"></label><label>App Secret<input type="password" name="appSecret" autocomplete="new-password"></label><p class="hint">相同 App ID 留空保留已存密钥；更换 App ID 必须填写新密钥。密钥不会回显。</p><div class="form-error"></div><button type="submit" class="primary">保存设置</button></form></section>`;
  $('#settings-form').onsubmit = e => {
    e.preventDefault(); const form = e.currentTarget, appId = form.elements.appId.value.trim(), secret = form.elements.appSecret.value;
    form.elements.appSecret.value = '';
    if (!appId || (appId !== settings.appId && !secret)) return error($('.form-error',form),Error('请填写 App ID；更换 App ID 时必须填写 App Secret'));
    const payload = {appId}; if (secret) payload.appSecret = secret;
    once(form, async () => { await api('settings','POST',payload); settings = {appId,secretConfigured:!!secret || settings.secretConfigured}; $('#secret-state').textContent = settings.secretConfigured ? '已配置 App Secret' : '尚未配置 App Secret'; notify('设置已保存'); });
  };
}
function renderDebug(appList, tables) {
  page.innerHTML = `<div class="grid"><section class="card"><h2>请求</h2><form id="debug-form"><label>调用应用<select name="appId" required><option value="">选择应用</option>${appList.map(a => `<option value="${esc(a.id)}">${esc(a.name)}${a.enabled ? '' : ' · 已禁用'}</option>`).join('')}</select></label><div class="grid"><label>资源<select name="table" required>${tables.map(t => `<option value="${esc(t.key)}">${esc(t.name)} · ${esc(t.key)}</option>`).join('')}</select></label><label>操作<select name="action">${Object.entries({fields:'字段 / 连接测试',search:'查询记录',get:'读取单条',create:'创建记录',update:'更新记录'}).map(([key,label]) => `<option value="${key}">${label}</option>`).join('')}</select></label></div><label id="record-label" hidden>记录 ID<input name="recordId" placeholder="rec…"></label><div id="paging" class="grid" hidden><label>每页数量<input type="number" name="pageSize" min="1" step="1" placeholder="使用服务端默认值"></label><label>分页 Token<input name="pageToken" autocomplete="off"></label></div><label id="payload-label" hidden>payload · JSON 对象<textarea class="payload" name="payload" spellcheck="false">{}</textarea><span id="payload-hint" class="hint"></span></label><p id="write-note" class="warning" hidden>真实写入：点击发送将直接创建或更新飞书记录，每次点击发送一次请求。</p><div class="form-error"></div><button id="send" class="primary" type="submit">发送请求</button></form></section><section class="card"><h2>响应</h2><div id="debug-result" aria-live="polite"><p class="muted">发送后展示上游 HTTP、阶段、请求 ID 与原始响应。</p></div></section></div>`;
  const form = $('#debug-form');
  form.elements.action.onchange = () => {
    const action = form.elements.action.value, record = ['get','update'].includes(action), payload = ['search','create','update'].includes(action), write = ['create','update'].includes(action);
    $('#record-label').hidden = !record; form.elements.recordId.required = record;
    $('#paging').hidden = action !== 'search'; form.elements.pageSize.disabled = action !== 'search';
    $('#payload-label').hidden = !payload; $('#write-note').hidden = !write; $('#send').textContent = write ? '发送 · 真实写入' : '发送请求';
    form.elements.payload.value = write ? '{\n  "fields": {}\n}' : '{}';
    $('#payload-hint').textContent = action === 'search' ? '使用飞书原生 filter、field_names；分页参数在上方填写。' : '使用 {"fields": {...}}，字段名与值遵循飞书原生格式。';
  };
  form.elements.action.onchange();
  form.onsubmit = e => {
    e.preventDefault(); const f = new FormData(form), action = f.get('action'), payload = {appId:f.get('appId'),table:f.get('table'),action};
    try {
      if (['get','update'].includes(action)) { payload.recordId = f.get('recordId').trim(); if (!payload.recordId) throw Error('请填写记录 ID'); }
      if (['search','create','update'].includes(action)) { payload.payload = JSON.parse(f.get('payload')); if (!object(payload.payload)) throw Error('payload 必须是 JSON 对象'); }
      if (['create','update'].includes(action) && !object(payload.payload.fields)) throw Error('写入 payload 必须包含 fields 对象');
      if (action === 'search') { if (f.get('pageSize')) { payload.pageSize = Number(f.get('pageSize')); if (!Number.isSafeInteger(payload.pageSize) || payload.pageSize < 1) throw Error('每页数量必须是正整数'); } if (f.get('pageToken')) payload.pageToken = f.get('pageToken'); }
    } catch (err) { error($('.form-error',form),err); return; }
    once(form, async () => {
      $('#debug-result').innerHTML = '<p class="muted">请求进行中…</p>';
      try { showDebug(await api('debug','POST',payload)); } catch (err) { error($('#debug-result'),err); if(err.details)$('#debug-result').insertAdjacentHTML('beforeend',`<pre>${esc(json(err.details))}</pre>`); }
    });
  };
}
function showDebug(data) {
  let body = data.body; if (typeof body === 'string') { try { body = JSON.parse(body); } catch { /* 原始文本保持原样 */ } }
  const hasCode = object(body) && Object.hasOwn(body,'code'), codeOK = hasCode && (body.code === 0 || body.code === '0');
  const httpOK = data.httpStatus >= 200 && data.httpStatus < 300;
  const text = !httpOK ? '上游返回非 2xx HTTP 状态' : hasCode && !codeOK ? '飞书业务拒绝' : codeOK ? '飞书返回成功' : '已收到响应（未提供业务 code）';
  $('#debug-result').innerHTML = `<div class="${!httpOK || (hasCode && !codeOK) ? 'error' : codeOK ? 'success' : 'warning'}">${text}</div><p><strong>HTTP ${esc(data.httpStatus)}</strong> · 阶段 ${esc(data.stage)}</p><p class="hint">请求 ID：<code>${esc(data.requestId)}</code></p>${hasCode ? `<p>code：<code>${esc(body.code)}</code></p><p>msg：${esc(body.msg)}</p>` : ''}<h2>原始响应</h2><pre>${esc(json(data.body))}</pre>`;
}
document.addEventListener('click', async e => {
  if (writing && e.target.closest('a')) { e.preventDefault(); return; }
  const el = e.target.closest('[data-action]'); if (!el || writing) return;
  const {action,id} = el.dataset, app = apps.find(a => String(a.id) === id);
  if (action === 'close') closeDialog();
  if (action === 'new') editApp();
  if (action === 'edit' && app) editApp(app);
  if (action === 'toggle' && app) once(page,async () => { rememberApp(await api('apps/' + encodeURIComponent(id),'PATCH',{enabled:!app.enabled})); notify('应用状态已更新'); });
  if (action === 'rotate' && app) once(page,async () => { showToken(await api('apps/' + encodeURIComponent(id) + '/rotate','POST',{})); });
  if (action === 'copy') { try { await navigator.clipboard.writeText($('#token').textContent); notify('Token 已复制'); } catch { error($('.form-error',dialog),Error('无法访问剪贴板，请选中 Token 手动复制')); } }
});
dialog.addEventListener('cancel',e => { e.preventDefault(); if (!writing) closeDialog(); });
dialog.addEventListener('close',() => { if (!dialog.open) dialog.replaceChildren(); });
$('#reload').onclick = load;
window.addEventListener('hashchange', load);
load();
