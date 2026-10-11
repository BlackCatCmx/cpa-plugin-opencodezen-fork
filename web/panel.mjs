const $ = (id) => document.getElementById(id);
const defaultBase = 'https://opencode.ai/zen/v1';
let session;
let provider = 'zen';
let globalProxy = '';
let modelPrefix = 'opencode';
let baseURL = defaultBase;
let testModel = '';
let availableModels = [];
let reasoningDirty = false;
let editing = null;
let deleting = null;
let accounts = [];
let pending = false;
let loaded = false;
const testResults = new Map();

function readSession() {
  let raw = localStorage.getItem('cli-proxy-auth');
  if (!raw) throw new Error('请先在 CPA 管理登录页勾选“记住登录”并登录。');
  if (raw.startsWith('enc::v1::')) {
    const bytes = Uint8Array.from(atob(raw.slice('enc::v1::'.length)), (char) => char.charCodeAt(0));
    const key = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`);
    raw = new TextDecoder().decode(bytes.map((byte, i) => byte ^ key[i % key.length]));
  }
  const parsed = JSON.parse(raw);
  const saved = parsed.state ?? parsed;
  if (!saved.apiBase || !saved.managementKey) throw new Error('请在 CPA 管理登录页勾选“记住登录”并登录。');
  const base = new URL(saved.apiBase, location.origin);
  if (base.origin !== location.origin) throw new Error('请在同一地址登录 CPA，并勾选“记住登录”。');
  return { base: base.href.replace(/\/+$/, ''), key: saved.managementKey };
}

async function api(path, options = {}) {
  const response = await fetch(`${session.base}/v0/management${path}`, {
    ...options,
    headers: { Authorization: `Bearer ${session.key}`, ...(options.body ? { 'Content-Type': 'application/json' } : {}) },
  });
  const body = await response.json();
  if (!response.ok) throw new Error(body.message || body.error || `HTTP ${response.status}`);
  return body;
}

function message(text = '', kind = 'success', id = 'message') {
  const node = $(id);
  node.textContent = text instanceof Error ? text.message : text;
  node.className = `notice ${kind}`;
  node.hidden = !node.textContent;
}

function validateProxy(value) {
  if (!value || value === 'direct') return;
  let parsed;
  try { parsed = new URL(value); } catch { throw new Error('代理地址格式无效。'); }
  if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(parsed.protocol) || !parsed.hostname || (parsed.pathname && parsed.pathname !== '/') || parsed.search || parsed.hash) {
    throw new Error('代理须为 http、https、socks5 或 socks5h 地址，不能包含路径或查询参数。');
  }
}

function accountKey(account) { return String(account.stored.api_key || account.stored.key || ''); }
function maskedKey(key) {
  const start = Math.min(8, Math.floor(key.length / 3)), end = Math.min(4, start);
  return `${key.slice(0, start)}••••${key.slice(key.length - end)}`;
}
function accountProxy(account) { return account.stored.proxy_url || ''; }
function proxyLabel(proxy) { return proxy === 'direct' ? '直连（direct）' : proxy || globalProxy || '直连（未设置全局代理）'; }

async function removeLegacyKey(key) {
  const config = await api('/plugins/zen/config');
  const configured = Array.isArray(config['api-keys']) ? config['api-keys'] : String(config['api-keys'] ?? '').split(/[,;\s]+/).filter(Boolean);
  if (configured.includes(key)) {
    await api('/plugins/zen/config', { method: 'PATCH', body: JSON.stringify({ 'api-keys': configured.filter((value) => value !== key) }) });
  }
}

async function loadAccounts() {
  const [config, proxy, result, models] = await Promise.all([api('/plugins/zen/config'), api('/proxy-url'), api('/auth-files'), api('/plugins/zen/models')]);
  const nextProvider = config.provider || 'zen';
  const nextAccounts = await Promise.all(result.files.filter((file) => (file.provider || file.type) === nextProvider && !file.runtime_only).map(async (file) => ({
    ...file,
    stored: await api(`/auth-files/download?name=${encodeURIComponent(file.name)}`),
  })));
  provider = nextProvider;
  globalProxy = proxy['proxy-url'] || '';
  modelPrefix = config['model-prefix'] ?? 'opencode';
  $('model-prefix').value = modelPrefix;
  baseURL = config['base-url'] || defaultBase;
  $('base-url').value = baseURL;
  accounts = nextAccounts;
  testModel = config['test-model'] || '';
  renderModels(models);
  $('reasoning-rules').replaceChildren();
  for (const [model, effort] of Object.entries(config['reasoning-overrides'] || {})) addReasoningRule(model, effort);
  reasoningDirty = false;
  loaded = true;
  renderAccounts();
}

function renderModels(result) {
  availableModels = result.models;
  const select = $('test-model');
  select.replaceChildren(new Option('请选择测试模型', ''));
  for (const model of result.models) select.add(new Option(`${model.id}${model.endpoint ? ` (${model.endpoint})` : ''}`, model.id));
  if (testModel && !result.models.some(model => model.id === testModel)) select.add(new Option(`${testModel}（不在当前列表）`, testModel));
  select.value = testModel;
  select.title = select.selectedOptions[0]?.textContent || '';
  $('pull-models').title = result.error || (result.updated_at && !result.updated_at.startsWith('0001') ? `最近拉取：${result.updated_at}` : '尚未拉取模型');
  if (result.error) message(`模型拉取失败：${result.error}；已保留已有列表。`, 'error');
}

function addReasoningRule(model = '', effort = '') {
  const row = element('div', 'reasoning-row');
  const select = element('select');
  select.setAttribute('aria-label', '覆盖模型');
  select.add(new Option('请选择模型', ''));
  for (const item of availableModels) select.add(new Option(item.id, item.id));
  if (model && !availableModels.some(item => item.id === model)) select.add(new Option(`${model}（不在当前列表）`, model));
  select.value = model;
  const renderEffort = (value) => {
    row.querySelector('[data-effort]')?.remove();
    const levels = availableModels.find(item => item.id === select.value)?.reasoning_levels || [];
    let control;
    if (levels.length) {
      control = element('select', 'effort-select');
      control.add(new Option('上游默认', 'default'));
      for (const level of levels) control.add(new Option(level, level));
      if (value && value !== 'default' && !levels.includes(value)) control.add(new Option(`${value}（当前目录未公布）`, value));
      control.value = value || 'default';
    } else {
      control = element('input');
      control.setAttribute('list', 'reasoning-suggestions');
      control.value = value;
      control.placeholder = '填写强度或 default';
      control.title = '官方未公布档位；default 表示上游默认';
    }
    control.dataset.effort = '';
    control.setAttribute('aria-label', '覆盖强度');
    row.insertBefore(control, remove);
  };
  const remove = element('button', 'btn compact', '删除');
  remove.type = 'button';
  remove.dataset.mutation = '';
  remove.addEventListener('click', () => { row.remove(); reasoningDirty = true; setControls(); });
  select.addEventListener('change', () => { renderEffort(''); select.title = select.value; });
  select.title = model;
  row.append(select, remove);
  renderEffort(effort);
  $('reasoning-rules').append(row);
}

function reasoningOverrides() {
  const rules = Object.create(null);
  for (const row of $('reasoning-rules').children) {
    const model = row.querySelector('select').value;
    const effort = row.querySelector('[data-effort]').value.trim();
    if (!model || !effort || /\s/.test(effort)) throw new Error('请选择覆盖模型，并填写不含空白的强度；default 表示上游默认。');
    if (Object.hasOwn(rules, model)) throw new Error(`模型 ${model} 只能有一条覆盖规则。`);
    rules[model] = effort;
  }
  return rules;
}

async function refreshAfterMutation(success) {
  try { await loadAccounts(); }
  catch (error) { throw new Error(`${success}但列表刷新失败：${error.message}。请点击刷新。`); }
  message(success);
}

function element(tag, className = '', text = '') {
  const node = document.createElement(tag);
  node.className = className;
  node.textContent = text;
  return node;
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.classList.add('icon');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', `#icon-${name}`);
  svg.append(use);
  return svg;
}

function actionButton(label, name, action, className = 'btn compact', mutation = true) {
  const node = element('button', className);
  node.type = 'button';
  node.title = label;
  node.setAttribute('aria-label', label);
  if (name) node.append(icon(name));
  if (!className.includes('icon-btn')) node.append(element('span', '', label));
  if (mutation) {
    node.dataset.mutation = '';
    node.disabled = pending;
  }
  node.addEventListener('click', action);
  return node;
}

async function copyValue(value, label) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
    } else {
      const input = element('textarea', 'copy-input');
      const focused = document.activeElement;
      input.value = value;
      input.setAttribute('readonly', '');
      document.body.append(input);
      input.select();
      try {
        if (!document.execCommand('copy')) throw new Error('浏览器不支持自动复制，请直接选中内容复制。');
      } finally { input.remove(); focused?.focus(); }
    }
    message(`${label}已复制。`);
  } catch (error) { message(error, 'error'); }
}

function renderAccounts() {
  const query = $('search').value.trim().toLowerCase();
  const filter = $('status-filter').value;
  const visible = accounts.filter((account) => {
    const matchesStatus = filter === 'all' || (filter === 'disabled' ? account.disabled : !account.disabled);
    return matchesStatus && [accountKey(account), proxyLabel(accountProxy(account))].some((value) => value.toLowerCase().includes(query));
  });
  $('summary').textContent = `${accounts.length} 把 Key · ${accounts.filter((account) => !account.disabled).length} 已启用`;
  $('result-count').textContent = visible.length === accounts.length ? `共 ${accounts.length} 条` : `显示 ${visible.length} / ${accounts.length} 条`;
  $('key-table').hidden = !visible.length;
  $('empty').hidden = !!visible.length;
  $('empty-title').textContent = accounts.length ? '没有匹配的 Key' : '尚未添加 Key';
  $('empty-description').textContent = accounts.length ? '更换搜索内容或状态筛选。' : '点击右上角“添加 Key”，设置对应代理即可使用。';
  const root = $('accounts');
  root.replaceChildren();
  for (const account of visible) {
    const key = accountKey(account), proxy = accountProxy(account);
    const row = document.createElement('tr');
    row.dataset.name = account.name;
    const keyCell = element('td'); keyCell.dataset.label = 'API Key';
    const keyLine = element('div', 'value-line');
    const keyValue = element('span', 'mono key-value', maskedKey(key));
    keyValue.title = maskedKey(key);
    keyLine.append(keyValue, actionButton('复制 API Key', 'copy', () => copyValue(key, 'API Key'), 'btn icon-btn', false));
    keyCell.append(keyLine);
    const proxyCell = element('td'); proxyCell.dataset.label = '代理';
    proxyCell.title = proxy === 'direct' ? '直连' : proxy ? '独立代理' : '全局代理';
    const effectiveProxy = proxy === 'direct' ? '' : proxy || globalProxy;
    let address = '直连';
    if (effectiveProxy) {
      const parsed = new URL(effectiveProxy);
      const port = parsed.port || { 'http:': '80', 'https:': '443', 'socks5:': '1080', 'socks5h:': '1080' }[parsed.protocol];
      address = `${parsed.hostname}:${port}`;
    }
    const proxyLine = element('div', 'value-line');
    const proxyValue = element('span', 'mono proxy-value', address);
    proxyValue.title = proxyLabel(proxy);
    proxyLine.append(proxyValue);
    if (effectiveProxy) proxyLine.append(actionButton('复制代理地址', 'copy', () => copyValue(effectiveProxy, '代理地址'), 'btn icon-btn', false));
    proxyCell.append(proxyLine);
    const statusCell = element('td'); statusCell.dataset.label = '状态';
    const toggle = actionButton(account.disabled ? '已停用' : '已启用', '', () => run(async () => {
      await api('/auth-files/status', { method: 'PATCH', body: JSON.stringify({ name: account.name, auth_index: account.auth_index, disabled: !account.disabled }) });
      await refreshAfterMutation(account.disabled ? 'Key 已启用。' : 'Key 已停用。');
    }), 'status-toggle');
    toggle.setAttribute('role', 'switch');
    toggle.setAttribute('aria-checked', String(!account.disabled));
    toggle.setAttribute('aria-label', account.disabled ? '启用 Key' : '停用 Key');
    toggle.title = account.disabled ? '点击启用' : '点击停用';
    toggle.prepend(element('span', 'switch-track'));
    statusCell.append(toggle);
    const actionsCell = element('td');
    const actions = element('div', 'row-actions');
    actions.append(actionButton('测试', 'play', () => run(() => testAccount(account))), actionButton('编辑', 'edit', () => openEditor(account)), actionButton('删除', 'trash', () => openDelete(account), 'btn compact danger'));
    actionsCell.append(actions);
    const resultCell = element('td'); resultCell.dataset.label = '结果';
    const result = testResults.get(account.name) || (account.status_message ? { kind: 'error-detail', text: account.status_message } : { kind: 'secondary', text: '未测试' });
    const detail = element('div', `test-result ${result.kind}`, result.text);
    detail.setAttribute('role', 'status');
    detail.title = result.detail ? `${result.text}\n${result.detail}` : result.text;
    resultCell.append(detail);
    row.append(keyCell, proxyCell, statusCell, resultCell, actionsCell);
    root.append(row);
  }
}

async function testAccount(account) {
  if (!testModel) throw new Error('请先选择测试模型并点击保存。');
  if ($('test-model').value !== testModel) throw new Error('测试模型已修改，请先点击保存。');
  testResults.set(account.name, { kind: 'secondary', text: '测试中…' });
  renderAccounts();
  try {
    const result = await api('/plugins/zen/test', { method: 'POST', body: JSON.stringify({ auth_index: account.auth_index }) });
    const status = result.http_status ? `HTTP ${result.http_status} · ` : '';
    testResults.set(account.name, {
      kind: result.ok ? 'test-success' : 'error-detail',
      text: result.ok ? `成功 · ${result.duration_ms} ms` : `失败 · ${status}${result.error}`,
      detail: `模型：${result.model}\n协议：${result.protocol || '未建立连接'}\n端点：${result.endpoint || ''}\n${result.reply || result.error || ''}${result.raw ? `\n原始响应：\n${result.raw}` : ''}`,
    });
  } catch (error) {
    testResults.set(account.name, { kind: 'error-detail', text: `失败 · ${error.message}` });
  } finally { renderAccounts(); }
}

function selectedProxy() {
  const mode = document.querySelector('input[name="proxy-mode"]:checked').value;
  return mode === 'global' ? '' : mode === 'direct' ? 'direct' : $('proxy-url').value.trim();
}

function updateEditor() {
  const mode = document.querySelector('input[name="proxy-mode"]:checked').value;
  $('proxy-url').hidden = mode !== 'custom';
  $('proxy-input-label').hidden = mode !== 'custom';
  $('proxy-url').required = mode === 'custom';
  $('proxy-hint').textContent = mode === 'global' ? `继承 CPA 设置：${proxyLabel('')}` : mode === 'direct' ? '此 Key 不经过代理，直接连接服务。' : '支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H，可包含用户名与密码。';
  const unchanged = editing && $('api-key').value.trim() === accountKey(editing) && selectedProxy() === accountProxy(editing);
  $('save').disabled = pending || !$('api-key').value.trim() || !!unchanged;
}

function openEditor(account = null) {
  editing = account;
  $('key-form').reset();
  $('api-key').value = account ? accountKey(account) : '';
  const proxy = account ? accountProxy(account) : '';
  const mode = !proxy ? 'global' : proxy === 'direct' ? 'direct' : 'custom';
  document.querySelector(`input[name="proxy-mode"][value="${mode}"]`).checked = true;
  $('proxy-url').value = mode === 'custom' ? proxy : '';
  $('form-title').textContent = account ? '编辑 Key' : '添加 Key';
  $('save').textContent = account ? '保存修改' : '添加 Key';
  message('', 'error', 'editor-message');
  updateEditor();
  $('editor').showModal();
}

function openDelete(account) {
  deleting = account;
  $('delete-key').textContent = maskedKey(accountKey(account));
  $('delete-proxy').textContent = proxyLabel(accountProxy(account));
  message('', 'error', 'delete-message');
  $('delete-dialog').showModal();
}

async function saveAccount() {
  const target = editing;
  const key = $('api-key').value.trim(), proxy = selectedProxy();
  if (!key) throw new Error('请填写 API Key。');
  validateProxy(proxy);
  if (accounts.some((account) => account.name !== target?.name && accountKey(account) === key)) throw new Error('此 Key 已存在，请编辑已有条目。');
  const fields = { api_key: key, proxy_url: proxy };
  if (target) {
    await api('/auth-files/fields', { method: 'PATCH', body: JSON.stringify({ name: target.name, ...fields }) });
    await removeLegacyKey(accountKey(target));
  } else {
    const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, '0')).join('');
    await api(`/auth-files?name=zen-${id}.json`, { method: 'POST', body: JSON.stringify({ type: provider, provider, ...fields }) });
  }
  $('editor').close();
  if (target) testResults.delete(target.name);
  await refreshAfterMutation(target ? '修改已保存。' : 'Key 已添加。');
}

async function run(action) {
  if (pending) return;
  pending = true;
  for (const id of ['message', 'editor-message', 'delete-message']) message('', 'success', id);
  setControls();
  try { await action(); }
  catch (error) {
    const id = $('editor').open ? 'editor-message' : $('delete-dialog').open ? 'delete-message' : 'message';
    message(error, 'error', id);
    if (!loaded) {
      $('summary').textContent = '未加载';
      $('empty-title').textContent = '无法加载 Key';
      $('empty-description').textContent = '请检查上方提示后点击刷新。';
    }
  } finally { pending = false; setControls(); }
}

function setControls() {
  $('reasoning-rules').querySelectorAll('input, select').forEach(node => { node.disabled = pending || !loaded; });
  document.querySelectorAll('[data-mutation]').forEach((node) => { node.disabled = pending || !loaded; });
  $('refresh').disabled = pending;
  $('editor-fields').disabled = pending;
  $('model-prefix').disabled = pending || !loaded;
  $('base-url').disabled = pending || !loaded;
  $('test-model').disabled = pending || !loaded;
  $('test-model').title = $('test-model').selectedOptions[0]?.textContent || '';
  $('base-url').title = $('base-url').value;
  $('model-prefix').title = $('model-prefix').value || '无前缀';
  $('save-settings').disabled = pending || !loaded || (!reasoningDirty && $('model-prefix').value.trim() === modelPrefix && $('base-url').value.trim() === baseURL && $('test-model').value === testModel);
  updateEditor();
}

$('key-form').addEventListener('submit', (event) => { event.preventDefault(); run(saveAccount); });
$('settings-form').addEventListener('submit', (event) => {
  event.preventDefault();
  run(async () => {
    const prefix = $('model-prefix').value.trim();
    if (prefix.includes('/') || /\s/.test(prefix)) throw new Error('模型前缀不能包含斜杠或空白。');
    let base;
    try { base = new URL($('base-url').value.trim()); } catch { throw new Error('服务地址格式无效。'); }
    if (!['https:', 'http:'].includes(base.protocol) || base.username || base.password || base.search || base.hash || base.href.includes('?') || base.href.includes('#')) throw new Error('服务地址须为 HTTP 或 HTTPS 地址，不能包含用户名、密码、查询参数或片段。');
    await api('/plugins/zen/config', { method: 'PATCH', body: JSON.stringify({ 'model-prefix': prefix, 'base-url': base.href.replace(/\/+$/, ''), 'test-model': $('test-model').value, 'reasoning-overrides': reasoningOverrides() }) });
    await refreshAfterMutation('插件设置已保存。');
  });
});
$('model-prefix').addEventListener('input', setControls);
$('base-url').addEventListener('input', setControls);
$('test-model').addEventListener('change', setControls);
$('add-reasoning').addEventListener('click', () => { addReasoningRule(); reasoningDirty = true; setControls(); });
for (const event of ['input', 'change']) $('reasoning-rules').addEventListener(event, () => { reasoningDirty = true; setControls(); });
$('pull-models').addEventListener('click', () => run(async () => {
  if ($('base-url').value.trim() !== baseURL) throw new Error('服务地址已修改，请先保存再拉取模型。');
  const result = await api('/plugins/zen/models/refresh', { method: 'POST' });
  const rules = [...$('reasoning-rules').children].map(row => [row.querySelector('select').value, row.querySelector('[data-effort]').value]);
  renderModels(result);
  $('reasoning-rules').replaceChildren();
  for (const [model, effort] of rules) addReasoningRule(model, effort);
  try { await api('/plugins/zen/config', { method: 'PATCH', body: JSON.stringify({}) }); }
  catch (error) { throw new Error(`模型已拉取，但 CPA 更新失败：${error.message}`); }
  message(`模型信息已更新，当前可选 ${result.models.length} 个模型。选择测试模型后点击上方保存。`);
}));
$('key-form').addEventListener('input', updateEditor);
$('search').addEventListener('input', () => { if (loaded) renderAccounts(); });
$('status-filter').addEventListener('change', () => { if (loaded) renderAccounts(); });
$('add').addEventListener('click', () => openEditor());
$('refresh').addEventListener('click', () => run(async () => { session = readSession(); await loadAccounts(); message('列表已刷新。'); }));
for (const id of ['cancel', 'close-editor']) $(id).addEventListener('click', () => $('editor').close());
$('cancel-delete').addEventListener('click', () => $('delete-dialog').close());
$('confirm-delete').addEventListener('click', () => run(async () => {
  const target = deleting;
  await removeLegacyKey(accountKey(target));
  await api(`/auth-files?name=${encodeURIComponent(target.name)}`, { method: 'DELETE' });
  $('delete-dialog').close();
  await refreshAfterMutation('Key 已删除。');
}));
for (const id of ['editor', 'delete-dialog']) $(id).addEventListener('cancel', (event) => { if (pending) event.preventDefault(); });

const themeRoot = window.parent.document.documentElement;
if (themeRoot !== document.documentElement) {
  const syncTheme = () => { document.documentElement.dataset.theme = themeRoot.dataset.theme || 'light'; };
  syncTheme();
  const observer = new MutationObserver(syncTheme);
  observer.observe(themeRoot, { attributes: true, attributeFilter: ['data-theme'] });
  window.addEventListener('pagehide', () => observer.disconnect(), { once: true });
} else {
  const preference = matchMedia('(prefers-color-scheme: dark)');
  const syncTheme = () => { document.documentElement.dataset.theme = preference.matches ? 'dark' : 'light'; };
  syncTheme();
  preference.addEventListener('change', syncTheme);
}

run(async () => { session = readSession(); await loadAccounts(); });
