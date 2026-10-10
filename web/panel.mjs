const $ = (id) => document.getElementById(id);
let session;
let provider = 'zen';
let editing = null;
let accounts = [];

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
  if (base.origin !== location.origin || !saved.managementKey) throw new Error('请在同一地址登录 CPA，并勾选“记住登录”。');
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

function message(error = '') { $('message').textContent = error instanceof Error ? error.message : error; }

function validateProxy(value) {
  if (!value || value === 'direct') return;
  let parsed;
  try { parsed = new URL(value); } catch { throw new Error('代理地址格式无效。'); }
  if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(parsed.protocol) || !parsed.hostname || (parsed.pathname && parsed.pathname !== '/') || parsed.search || parsed.hash) {
    throw new Error('代理须为 http、https、socks5 或 socks5h 地址，不能包含路径或查询参数。');
  }
}

function displayProxy(value) {
  if (!value) return '使用 CPA 全局代理';
  if (value === 'direct') return '直连';
  try { const parsed = new URL(value); parsed.username = ''; parsed.password = ''; return parsed.href; }
  catch { return '代理地址无效，请编辑'; }
}

function resetForm() {
  editing = null;
  $('key-form').reset();
  $('form-title').textContent = '添加 Key';
  $('cancel').hidden = true;
}

async function removeLegacyKey(key) {
  const config = await api('/plugins/zen/config');
  const configured = Array.isArray(config['api-keys']) ? config['api-keys'] : String(config['api-keys'] ?? '').split(/[,;\s]+/).filter(Boolean);
  if (configured.includes(key)) {
    await api('/plugins/zen/config', { method: 'PATCH', body: JSON.stringify({ 'api-keys': configured.filter((value) => value !== key) }) });
  }
}

async function loadAccounts() {
  const config = await api('/plugins/zen/config');
  provider = config.provider || 'zen';
  const result = await api('/auth-files');
  accounts = await Promise.all(result.files.filter((file) => (file.provider || file.type) === provider && !file.runtime_only).map(async (file) => ({
    ...file,
    stored: await api(`/auth-files/download?name=${encodeURIComponent(file.name)}`),
  })));
  renderAccounts();
}

function button(text, action) {
  const node = document.createElement('button');
  node.type = 'button'; node.textContent = text;
  node.addEventListener('click', () => run(action));
  return node;
}

function renderAccounts() {
  const root = $('accounts'); root.replaceChildren();
  if (!accounts.length) { const p = document.createElement('p'); p.textContent = '尚未添加 Key。'; root.append(p); }
  for (const account of accounts) {
    const card = document.createElement('article');
    const key = String(account.stored.api_key || account.stored.key || '');
    const title = document.createElement('h3'); title.textContent = `Key ····${key.slice(-4)}`;
    const detail = document.createElement('p');
    detail.textContent = `${account.disabled ? '已停用' : '已启用'} · Key 尾号 ${key.slice(-4)} · ${displayProxy(account.stored.proxy_url)}`;
    const status = document.createElement('p'); status.textContent = account.status_message || '';
    const actions = document.createElement('div'); actions.className = 'actions';
    actions.append(button('编辑', async () => {
      editing = account;
      $('api-key').value = key;
      $('proxy-url').value = account.stored.proxy_url || '';
      $('base-url').value = account.stored.base_url || account.stored.url || 'https://opencode.ai/zen/v1';
      $('form-title').textContent = '编辑 Key'; $('cancel').hidden = false;
      $('key-form').scrollIntoView({ behavior: 'smooth', block: 'center' });
    }), button(account.disabled ? '启用' : '停用', async () => {
      await api('/auth-files/status', { method: 'PATCH', body: JSON.stringify({ name: account.name, auth_index: account.auth_index, disabled: !account.disabled }) });
      await loadAccounts();
    }), button('删除', async () => {
      if (!confirm(`删除尾号 ${key.slice(-4)} 的 Key？`)) return;
      await removeLegacyKey(key);
      await api(`/auth-files?name=${encodeURIComponent(account.name)}`, { method: 'DELETE' });
      if (editing?.name === account.name) resetForm();
      await loadAccounts();
    }));
    card.append(title, detail, status, actions); root.append(card);
  }
}

async function saveAccount() {
  const key = $('api-key').value.trim();
  const proxy = $('proxy-url').value.trim();
  const base = new URL($('base-url').value.trim());
  if (!key) throw new Error('请填写 API Key。');
  if (!['https:', 'http:'].includes(base.protocol) || base.username || base.password || base.search || base.hash) throw new Error('服务地址须为 HTTP 或 HTTPS 地址，不能包含用户名、密码、查询参数或片段。');
  validateProxy(proxy);
  if (accounts.some((account) => account.name !== editing?.name && (account.stored.api_key || account.stored.key) === key)) throw new Error('此 Key 已存在，请编辑已有条目。');
  const fields = { api_key: key, proxy_url: proxy, base_url: base.href.replace(/\/+$/, '') };
  if (editing) {
    await api('/auth-files/fields', { method: 'PATCH', body: JSON.stringify({ name: editing.name, ...fields }) });
    await removeLegacyKey(String(editing.stored.api_key || editing.stored.key || ''));
  } else {
    const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) => byte.toString(16).padStart(2, '0')).join('');
    await api(`/auth-files?name=zen-${id}.json`, { method: 'POST', body: JSON.stringify({ type: provider, provider, ...fields }) });
  }
  resetForm();
  await loadAccounts();
}

async function run(action) {
  message();
  const buttons = [...document.querySelectorAll('button')];
  buttons.forEach((button) => { button.disabled = true; });
  try { await action(); } catch (error) { message(error); }
  finally { buttons.forEach((button) => { button.disabled = false; }); }
}

$('key-form').addEventListener('submit', (event) => { event.preventDefault(); run(saveAccount); });
$('cancel').addEventListener('click', resetForm);
$('refresh').addEventListener('click', () => run(loadAccounts));
run(async () => { session = readSession(); await loadAccounts(); });
