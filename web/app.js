'use strict';
const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const json = value => JSON.stringify(value, null, 2);
const names = {overview:'概览',apps:'应用与 Token',resources:'资源与权限',debug:'API 调试台',logs:'操作日志',settings:'服务设置'};
const tables = {requirements:'需求管理',tasks:'任务管理',bugs:'BUG 管理'};
const scopes = {'requirements:read':'读取需求','tasks:read':'读取任务','tasks:write':'创建与更新任务','bugs:read':'读取 BUG','bugs:create':'创建 BUG','bugs:status':'更新 BUG 状态','bugs:edit':'修改 BUG 其他字段（需原因）'};
const state = {session:null,page:'overview',apps:[],logs:[],offset:0,result:'all',q:'',generation:0,busy:false};
const debug = {appId:'',table:'bugs',action:'search',recordId:'',expectedRevision:'',fields:'{}',filter:'',limit:'10',pageToken:'',reason:'',idempotencyKey:'',response:null};
const date = value => value ? (Number.isNaN(Date.parse(value)) ? String(value) : new Date(value).toLocaleString('zh-CN')) : '—';
const button = (text, action, extra='', cls='') => `<button type="button" class="btn ${cls}" data-action="${action}" ${extra}>${text}</button>`;
const badge = (text, kind='') => `<span class="badge ${kind}">${esc(text)}</span>`;
const head = (title, sub, actions='') => `<div class="page-head"><div><div class="eyebrow">FEISHU API CONSOLE</div><h1>${esc(title)}</h1><p class="subtitle">${esc(sub)}</p></div><div class="actions">${actions}</div></div>`;
const empty = text => `<div class="empty">${esc(text)}</div>`;
const errorText = error => `${error.message}${error.code ? `（${error.code}）` : ''}${error.requestId ? ` · 请求 ID：${error.requestId}` : ''}`;
function toast(text) { $('toast-root').textContent=text; $('toast-root').className='toast'; clearTimeout(toast.timer); toast.timer=setTimeout(()=>{$('toast-root').textContent='';$('toast-root').className='';},6000); }
async function api(path, body, method='POST') {
  const headers = {Accept:'application/json'};
  if (body !== undefined) {
    if (!state.session?.csrfToken) throw new Error('管理会话不可用，请刷新页面重新验证。');
    headers['Content-Type']='application/json'; headers['X-CSRF-Token']=state.session.csrfToken;
  }
  let response;
  try { response=await fetch(`/admin-api/${path}`,{method:body===undefined?'GET':method,headers,credentials:'same-origin',cache:'no-store',...(body===undefined?{}:{body:JSON.stringify(body)})}); }
  catch { throw new Error('网络请求失败。写入结果可能未知，请先检查日志或读取记录；创建重试请复用原幂等键。'); }
  let value;
  try { value=await response.json(); } catch { throw new Error(`服务返回非 JSON 响应（HTTP ${response.status}），请检查登录状态；写入结果尚未确认。`); }
  if (!response.ok || value.error) {
    const e = new Error(value.error?.message || `请求失败：HTTP ${response.status}`);
    e.code=value.error?.code; e.requestId=value.requestId; throw e;
  }
  if (!Object.hasOwn(value,'data')) throw new Error('响应缺少 data，无法确认操作结果。');
  return value.data;
}
function showModal(title, body, footer='') {
  const m=$('modal'); m.innerHTML=`<div class="modal-head"><h2 id="modal-title">${esc(title)}</h2>${button('×','close','','close')}</div><div class="modal-body">${body}<p id="modal-error" class="error" role="alert"></p></div><div class="modal-foot">${footer}</div>`;
  if (!m.open) m.showModal();
}
function closeModal() { if(state.busy)return; $('modal').close(); $('modal').replaceChildren(); }
function notice() { return '<div class="banner">需求状态、任务状态为流程字段，飞书 API 仅支持读取；BUG 状态可更新。实际可用操作由服务端权限和资源规则共同校验。</div>'; }
function logTable(rows) {
  return rows.length ? `<div class="table-wrap"><table><thead><tr><th>时间 / 请求 ID</th><th>应用</th><th>操作 / 数据表</th><th>结果</th><th>耗时</th><th>详情</th></tr></thead><tbody>${rows.map((l,i)=>`<tr><td>${esc(date(l.time))}<div class="mono">${esc(l.id)}</div></td><td>${esc(l.appName)}</td><td>${esc(l.action)} / ${esc(tables[l.table]||l.table)}</td><td>${badge(`${l.status ?? '—'} · ${l.result ?? '—'}`,Number(l.status)>=400?'red':'')}</td><td>${esc(l.durationMs ?? '—')} ms</td><td>${button('查看','log',`data-index="${i}"`,'link')}</td></tr>`).join('')}</tbody></table></div>` : empty('暂无操作日志');
}
function overview(data) {
  state.logs=data.recentLogs||[];
  const stats=[['今日请求',data.requestsToday],['成功率',data.successRate==null?'—':`${data.successRate}%`],['可用应用',`${data.activeApps ?? '—'} / ${data.totalApps ?? '—'}`],['接入数据表',data.tables]];
  return head('概览','统一管理飞书 API 的访问、权限与调用记录。',button('刷新','refresh'))+`<div class="stats">${stats.map(([k,v])=>`<div class="stat"><div class="stat-label">${k}</div><div class="stat-value">${esc(v ?? '—')}</div></div>`).join('')}</div>`+notice()+`<section class="card section"><div class="card-head"><h2>调用趋势</h2><span class="hint">服务端统计 · 按日读取与写入次数</span></div>${data.trend?.length?`<div class="table-wrap"><table><thead><tr><th>日期</th><th>读取</th><th>写入</th></tr></thead><tbody>${data.trend.map(t=>`<tr><td>${esc(t.day)}</td><td>${esc(t.read)}</td><td>${esc(t.write)}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无趋势数据')}</section><section class="card"><div class="card-head"><h2>最近操作</h2><a href="#logs">查看全部 →</a></div>${logTable(state.logs)}</section>`;
}
function appsPage() {
  return head('应用与 Token','每个调用方使用独立凭据。完整 Token 仅在创建或轮换后展示一次。',button('创建访问应用','new-app','','primary'))+`<section class="card"><div class="table-wrap"><table><thead><tr><th>应用 / Token 前缀</th><th>权限</th><th>状态</th><th>有效期 / 最近使用</th><th>操作</th></tr></thead><tbody>${state.apps.map((a,i)=>`<tr><td><strong>${esc(a.name)}</strong><div class="token-prefix">${esc(a.tokenPrefix)}</div><div class="hint wrap">${esc(a.description)}</div></td><td class="wrap">${(a.scopes||[]).map(s=>badge(s)).join(' ')}</td><td>${badge(!a.enabled?'已禁用':a.expiresAt&&Date.parse(a.expiresAt)<=Date.now()?'已过期':'已启用',a.enabled?'green':'')}</td><td>${a.expiresAt?esc(date(a.expiresAt)):'永不过期'}<div class="hint">最近使用：${esc(date(a.lastUsedAt))}</div></td><td><div class="actions">${button('管理','edit-app',`data-index="${i}"`,'link')}${button(a.enabled?'禁用':'启用','toggle',`data-index="${i}"`,'link')}${button('轮换','rotate',`data-index="${i}"`,'link')}</div></td></tr>`).join('')}</tbody></table></div>${state.apps.length?'':empty('尚无应用，请创建访问应用。')}</section>`;
}
function resourcesPage(data) {
  return head('资源与权限','以下资源信息与限制直接来自服务端。')+notice()+`<div class="resource-grid">${Object.entries(data.tables||{}).map(([key,t])=>`<section class="card"><div class="card-head"><h2>${esc(t.name||tables[key]||key)}</h2>${badge(key)}</div><div class="card-body"><dl><dt>数据表 ID</dt><dd class="mono">${esc(t.id)}</dd><dt>状态</dt><dd>${esc(typeof t.status==='object'?json(t.status):t.status)}</dd><dt>维护规则</dt><dd class="wrap">${esc(typeof t.policy==='object'?json(t.policy):t.policy)}</dd></dl></div></section>`).join('')}</div><div class="callout wrap">平台限制：${esc(typeof data.platformLimit==='object'?json(data.platformLimit):data.platformLimit)}</div>`;
}
function logsPage(data) {
  state.logs=data.items||[];
  return head('操作日志','查询真实请求的归属、结果和操作原因。',button('刷新','refresh'))+`<form id="log-form" class="toolbar"><input class="input search" name="q" aria-label="搜索日志" placeholder="搜索应用、记录或请求 ID" value="${esc(state.q)}"><select name="result" aria-label="结果筛选">${Object.entries({all:'全部结果',success:'成功',failure:'失败'}).map(([v,n])=>`<option value="${v}" ${state.result===v?'selected':''}>${n}</option>`).join('')}</select><button class="btn primary" type="submit">查询</button></form><section class="card">${logTable(state.logs)}<div class="table-foot"><span>共 ${esc(data.total)} 条 · 当前 ${data.items.length?state.offset+1:0}–${state.offset+data.items.length}</span><div class="actions">${button('上一页','prev',state.offset===0?'disabled':'')}${button('下一页','next',state.offset+50>=data.total?'disabled':'')}</div></div></section>`;
}
function settingsPage(data) {
  const rows=[['管理端认证',data.authMode==='pangolin'?'Pangolin':data.authMode==='local'?'本地认证':data.authMode],['公开服务地址',data.publicOrigin],['每应用每分钟请求限额',data.rateLimitPerMinute],['飞书凭据',data.credentialConfigured?'已配置（连接状态需测试）':'未配置'],['服务版本',data.version]];
  return head('服务设置','配置由服务端管理。连接测试将实际读取飞书数据表字段。')+`<section class="card"><div class="card-head"><h2>服务信息</h2></div><div class="card-body">${rows.map(([k,v])=>`<div class="rule"><span>${k}</span><b class="wrap">${esc(v ?? '—')}</b></div>`).join('')}<div class="endpoint">外部 API：<code>${esc((data.publicOrigin||'').replace(/\/$/,'')+'/api/v1/tables/...')}</code></div>${button('测试飞书连接','connection','','primary')}<pre id="connection-result" class="result-text" aria-live="polite">尚未测试连接</pre></div></section>`;
}
function input(id,label,value='',type='text',hint='') { return `<div class="field"><label for="${id}">${label}</label><input class="input" id="${id}" type="${type}" step="1" value="${esc(value)}">${hint?`<div class="hint">${hint}</div>`:''}</div>`; }
function debugPage() {
  if(!state.apps.some(a=>String(a.id)===debug.appId)) debug.appId=String(state.apps[0]?.id ?? '');
  const write=['create','update'].includes(debug.action);
  const path=`/api/v1/tables/${debug.table}/${debug.action==='fields'?'fields':'records'+(debug.action==='search'?'/search':['get','update'].includes(debug.action)?'/'+encodeURIComponent(debug.recordId||'{recordId}'):'')}`;
  return head('API 调试台','使用所选应用当前权限和有效期，向飞书发送真实请求。')+`<div class="banner"><strong>真实操作：</strong>创建和更新会修改飞书记录，提交前需确认。请求失败不一定意味着未写入；请核对结果后再重试。</div><div class="two-col debug-layout"><section class="card"><div class="card-head"><h2>请求</h2>${badge(write?'真实写入':'真实读取',write?'amber':'green')}</div><div class="card-body"><form id="debug-form"><div class="field section"><label for="debug-appId">调用应用</label><select id="debug-appId">${state.apps.map(a=>`<option value="${esc(a.id)}" ${String(a.id)===debug.appId?'selected':''}>${esc(a.name)}${!a.enabled?' · 已禁用':a.expiresAt&&Date.parse(a.expiresAt)<=Date.now()?' · 已过期':''}</option>`).join('')}</select></div><div class="form-row"><div class="field"><label for="debug-table">数据表</label><select id="debug-table">${Object.entries(tables).map(([k,v])=>`<option value="${k}" ${debug.table===k?'selected':''}>${v}</option>`).join('')}</select></div><div class="field"><label for="debug-action">操作</label><select id="debug-action">${Object.entries({search:'查询记录',get:'读取单条',fields:'读取字段',create:'创建记录',update:'更新记录'}).map(([k,v])=>`<option value="${k}" ${debug.action===k?'selected':''}>${v}</option>`).join('')}</select></div></div><div class="endpoint"><b>${({search:'POST',get:'GET',fields:'GET',create:'POST',update:'PATCH'})[debug.action]}</b><code>${esc(path)}</code></div>${['get','update'].includes(debug.action)?input('debug-recordId','记录 ID',debug.recordId):''}${debug.action==='update'?input('debug-expectedRevision','expectedRevision（必填）',debug.expectedRevision)+`<div class="section">${button('读取当前记录并填入 revision','read-revision')}<div class="hint">读取后请核对响应字段，再确认更新内容。</div></div>`:''}${write?`<div class="field section"><label for="debug-fields">fields · JSON 对象</label><textarea id="debug-fields" class="editor" spellcheck="false">${esc(debug.fields)}</textarea><div class="hint">请先读取字段定义，填写真实字段名、选项或人员 ID。</div></div>`:''}${debug.action==='search'?input('debug-limit','每页数量',debug.limit,'number')+input('debug-pageToken','下一页 pageToken',debug.pageToken)+`<div class="field section"><label for="debug-filter">filter · JSON 对象（可留空）</label><textarea id="debug-filter" class="editor" spellcheck="false">${esc(debug.filter)}</textarea></div>`:''}${write?input('debug-reason','操作原因 reason',debug.reason):''}${debug.action==='create'?input('debug-idempotencyKey','幂等键 idempotencyKey（必填）',debug.idempotencyKey,'text','同一次创建重试复用原键；下一次独立创建需生成新键。')+button('生成新幂等键','key'):''}<p id="debug-error" class="error" role="alert"></p><button class="btn primary" type="submit" ${state.apps.length?'':'disabled'}>${write?'检查并确认写入':'发送真实请求'}</button></form></div></section><section class="card"><div class="card-head"><h2>实际响应</h2><span id="response-status" class="hint">${debug.response?'最近一次请求':'等待请求'}</span></div><div class="card-body"><pre class="code" id="debug-response">${esc(debug.response?json(debug.response):'尚未发送请求。')}</pre><div class="callout">HTTP 状态和完整响应如下实展示。请检查 verified 等字段；核验失败或结果未知时，不应视为写入成功。</div></div></section></div>`;
}
async function render() {
  const generation=++state.generation;
  state.page=Object.hasOwn(names,location.hash.slice(1))?location.hash.slice(1):'overview';
  $('navigation').innerHTML=Object.entries(names).map(([k,v])=>`<a class="nav-button ${k===state.page?'active':''}" href="#${k}" ${k===state.page?'aria-current="page"':''}>${v}</a>`).join('');
  $('crumb').textContent=names[state.page]; $('sidebar').classList.remove('open'); $('menu-button').setAttribute('aria-expanded','false');
  $('app').innerHTML=empty('正在加载…');
  try {
    let html;
    const page=state.page;
    let data;
    if(page==='overview') data=await api('overview');
    else if(page==='apps'||page==='debug') data=await api('apps');
    else if(page==='resources') data=await api('resources');
    else if(page==='logs') data=await api(`logs?${new URLSearchParams({limit:'50',offset:String(state.offset),result:state.result,q:state.q})}`);
    else data=await api('settings');
    if(generation!==state.generation)return;
    if(page==='overview')html=overview(data);
    else if(page==='apps'||page==='debug'){state.apps=data;html=page==='apps'?appsPage():debugPage();}
    else if(page==='resources')html=resourcesPage(data);
    else if(page==='logs')html=logsPage(data);
    else html=settingsPage(data);
    $('app').innerHTML=html;
  } catch(e) { if(generation===state.generation)$('app').innerHTML=`<div class="banner error" role="alert">${esc(errorText(e))}</div>${button('重新加载','refresh')}`; }
}
function localDate(value) { if(!value)return ''; const d=new Date(value); if(Number.isNaN(d.getTime()))return ''; return new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,19); }
function editApp(index) {
  const a=state.apps[index];
  showModal(a?'管理访问应用':'创建访问应用',`<form id="app-form" data-index="${a?index:''}">${input('app-name','应用名称（必填）',a?.name||'')}${input('app-description','用途说明',a?.description||'')}${input('app-expiry','有效期（本地时间，留空为永不过期）',localDate(a?.expiresAt),'datetime-local')}<fieldset class="scope-group"><legend>访问权限</legend>${Object.entries({...scopes,...Object.fromEntries((a?.scopes||[]).filter(s=>!Object.hasOwn(scopes,s)).map(s=>[s,s]))}).map(([k,v])=>`<label class="check"><input type="checkbox" name="scope" value="${esc(k)}" ${(a?.scopes||[]).includes(k)?'checked':''}>${esc(v)}</label>`).join('')}</fieldset><div class="hint">BUG 其他字段修改须有明确授权原因；流程字段不能写入。</div></form>`,button('取消','close')+'<button class="btn primary" type="submit" form="app-form">保存</button>');
}
function tokenModal(data) {
  if(!data?.token) throw new Error('服务未返回 Token。请核对应用列表；不要盲目重复创建。');
  showModal('访问凭据已生成',`<div class="banner">完整 Token 仅展示本次。请立即复制并妥善保存，关闭后无法再次查看。</div><p>应用：${esc(data.app?.name)}</p><pre class="token-box" id="new-token" tabindex="0">${esc(data.token)}</pre>`,button('复制 Token','copy')+button('我已保存，关闭','close','','primary'));
}
function captureDebug() { Object.keys(debug).forEach(k=>{if($(`debug-${k}`))debug[k]=$(`debug-${k}`).value;}); }
function objectJSON(text,label) { let value; try {value=JSON.parse(text);} catch {throw new Error(`${label} 不是合法 JSON。`);} if(!value||typeof value!=='object'||Array.isArray(value))throw new Error(`${label} 必须是 JSON 对象。`);return value; }
function debugBody() {
  captureDebug();
  if(!debug.appId)throw new Error('请先创建或选择调用应用。');
  const body={appId:state.apps.find(a=>String(a.id)===debug.appId)?.id,action:debug.action,table:debug.table};
  if(['get','update'].includes(debug.action)) {if(!debug.recordId.trim())throw new Error('请输入记录 ID。');body.recordId=debug.recordId.trim();}
  if(['create','update'].includes(debug.action)) {body.fields=objectJSON(debug.fields,'fields');if(!Object.keys(body.fields).length)throw new Error('fields 不能为空。');if(debug.reason.trim())body.reason=debug.reason.trim();}
  if(debug.action==='update') {if(!debug.expectedRevision.trim())throw new Error('请先读取记录，填写 expectedRevision。');body.expectedRevision=debug.expectedRevision.trim();}
  if(debug.action==='create') {if(!debug.idempotencyKey.trim())throw new Error('请生成或输入幂等键。');body.idempotencyKey=debug.idempotencyKey.trim();}
  if(debug.action==='search') {const n=Number(debug.limit);if(!Number.isInteger(n)||n<1||n>100)throw new Error('每页数量须为 1–100 的整数。');body.limit=n;if(debug.filter.trim())body.filter=objectJSON(debug.filter,'filter');if(debug.pageToken.trim())body.pageToken=debug.pageToken.trim();}
  return body;
}
async function runDebug(body,fillRevision=false) {
  const generation=state.generation;
  let data;
  try { data=await api('debug',body); }
  catch(e) {
    debug.response={error:{code:e.code||'REQUEST_FAILED',message:e.message},...(e.requestId?{requestId:e.requestId}:{})};
    if(generation===state.generation){$('debug-response').textContent=json(debug.response);$('response-status').textContent='本次请求失败或结果未确认';}
    throw e;
  }
  debug.response=data;
  if(generation!==state.generation)return;
  $('debug-response').textContent=json(data); $('response-status').textContent=`HTTP ${data.status ?? '未知'} · ${new Date().toLocaleTimeString('zh-CN')}`;
  if(fillRevision) {
    const revision=data.body?.data?.revision ?? data.body?.revision;
    if(Number(data.status)>=200&&Number(data.status)<300&&revision!=null) {debug.expectedRevision=String(revision);$('debug-expectedRevision').value=debug.expectedRevision;toast('已读取记录并填入 revision，请核对实际响应。');}
    else throw new Error('未获得可用 revision，请检查实际响应。');
  }
}
let pending=null;
async function task(fn, target='modal-error') {
  if(state.busy)return;
  state.busy=true;
  const controls=[...document.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled)')]; controls.forEach(b=>b.disabled=true);
  try { await fn(); } catch(e) {const node=$(target); if(node)node.textContent=errorText(e);else toast(errorText(e));}
  finally {state.busy=false;controls.forEach(b=>{if(b.isConnected)b.disabled=false;});}
}
document.addEventListener('submit',e=>{
  e.preventDefault();
  if(e.target.id==='log-form') { const form=new FormData(e.target);state.q=String(form.get('q'));state.result=String(form.get('result'));state.offset=0;render(); }
  if(e.target.id==='app-form') {
    const index=e.target.dataset.index; const a=index===''?null:state.apps[index];
    const name=$('app-name').value.trim(),description=$('app-description').value.trim(),expiry=$('app-expiry').value;
    const selected=[...e.target.querySelectorAll('[name="scope"]:checked')].map(i=>i.value);
    if(!name){$('modal-error').textContent='请填写应用名称。';return;}
    if(!selected.length){$('modal-error').textContent='请至少选择一项权限。';return;}
    if(expiry&&Number.isNaN(new Date(expiry).getTime())){$('modal-error').textContent='有效期格式不正确。';return;}
    const expiresAt=expiry?new Date(expiry).toISOString():null;
    task(async()=>{const data=await api(a?`apps/${encodeURIComponent(a.id)}`:'apps',{name,description,scopes:selected,expiresAt},a?'PATCH':'POST');if(a){$('modal').close();$('modal').replaceChildren();toast('应用修改已保存。');}else tokenModal(data);await render();});
  }
  if(e.target.id==='debug-form') {
    try { const body=debugBody();$('debug-error').textContent='';if(['create','update'].includes(body.action)){pending=body;showModal('确认真实飞书写入',`<div class="banner">此操作会修改真实数据。请核对应用、数据表、字段和值。取消将不发送请求。</div><p>调用应用：${esc(state.apps.find(a=>a.id===body.appId)?.name)}</p><pre class="code">${esc(json(body))}</pre>`,button('取消','close')+button('确认发送真实写入','confirm-write','','danger'));}else task(()=>runDebug(body),'debug-error'); }
    catch(error){$('debug-error').textContent=errorText(error);}
  }
});
document.addEventListener('change',e=>{if(e.target.id.startsWith('debug-')) {const previous=debug[e.target.id.slice(6)];captureDebug();if(['debug-action','debug-table','debug-appId','debug-recordId'].includes(e.target.id)&&previous!==e.target.value){debug.expectedRevision='';if($('debug-expectedRevision'))$('debug-expectedRevision').value='';}if(['debug-action','debug-table'].includes(e.target.id)){if(e.target.id==='debug-table')debug.recordId='';$('app').innerHTML=debugPage();}}});
document.addEventListener('click',e=>{
  const el=e.target.closest('[data-action]');if(!el||state.busy)return;
  const action=el.dataset.action,index=el.dataset.index,a=state.apps[index];
  if(action==='close'){pending=null;closeModal();}
  else if(action==='refresh')render();
  else if(action==='new-app')editApp();
  else if(action==='edit-app')editApp(index);
  else if(action==='toggle'||action==='rotate') {
    pending={action,id:a.id,enabled:!a.enabled};
    showModal(action==='rotate'?'确认轮换 Token':`确认${a.enabled?'禁用':'启用'}应用`,`<p>应用：${esc(a.name)}</p><p>${action==='rotate'?'新 Token 生成后，旧 Token 立即失效，请及时更新调用方凭据。':a.enabled?'禁用后，该应用将无法调用外部 API。':'启用后，仍受有效期和权限限制。'}</p>`,button('取消','close')+button('确认','confirm-app','','primary'));
  }
  else if(action==='confirm-app') task(async()=>{const p=pending;if(p.action==='rotate')tokenModal(await api(`apps/${encodeURIComponent(p.id)}/rotate`,{}));else{await api(`apps/${encodeURIComponent(p.id)}`,{enabled:p.enabled},'PATCH');$('modal').close();$('modal').replaceChildren();toast('应用状态已更新。');}pending=null;await render();});
  else if(action==='copy') task(async()=>{try{await navigator.clipboard.writeText($('new-token').textContent);toast('Token 已复制。');}catch{throw new Error('无法访问剪贴板，请手动选择并复制 Token。');}});
  else if(action==='log') {const l=state.logs[index];showModal('请求详情',`<dl class="log-detail">${Object.entries({请求ID:l.id,时间:date(l.time),应用:l.appName,操作:l.action,数据表:l.table,记录ID:l.recordId,HTTP状态:l.status,结果:l.result,涉及字段:(l.fieldNames||[]).join('、'),原因:l.reason,耗时:`${l.durationMs ?? '—'} ms`}).map(([k,v])=>`<dt>${k}</dt><dd>${esc(v ?? '—')}</dd>`).join('')}</dl>`,button('关闭','close'));}
  else if(action==='prev'||action==='next'){state.offset=Math.max(0,state.offset+(action==='next'?50:-50));render();}
  else if(action==='connection')task(async()=>{const data=await api('connection-test',{});$('connection-result').textContent=json(data);if(data.ok===true)toast('连接测试通过，详情见实际响应。');},'connection-result');
  else if(action==='key'){captureDebug();debug.idempotencyKey=crypto.randomUUID();$('debug-idempotencyKey').value=debug.idempotencyKey;}
  else if(action==='read-revision') {captureDebug();if(!debug.recordId.trim()){$('debug-error').textContent='请输入记录 ID。';return;}task(()=>runDebug({appId:state.apps.find(a=>String(a.id)===debug.appId)?.id,action:'get',table:debug.table,recordId:debug.recordId.trim()},true),'debug-error');}
  else if(action==='confirm-write')task(async()=>{const body=pending;await runDebug(body);pending=null;$('modal').close();$('modal').replaceChildren();},'modal-error');
});
$('modal').addEventListener('cancel',e=>{e.preventDefault();if(!state.busy){pending=null;closeModal();}});
$('menu-button').addEventListener('click',()=>{const open=$('sidebar').classList.toggle('open');$('menu-button').setAttribute('aria-expanded',String(open));});
window.addEventListener('hashchange',()=>{if(state.session)render();});
async function start() {try {state.session=await api('session');$('admin-name').textContent=state.session.adminName||'管理员';$('session-state').textContent=state.session.authMode==='pangolin'?'Pangolin 管理会话':'本地管理会话';await render();}catch(e){$('session-state').textContent='管理会话不可用';$('app').innerHTML=`<div class="banner error" role="alert">${esc(errorText(e))}</div><p>请完成管理端登录后刷新页面。</p>`;}}
start();
