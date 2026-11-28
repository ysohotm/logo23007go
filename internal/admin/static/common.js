function $(id) { return document.getElementById(id); }

function toast(msg, ok) {
  let box = $('toast');
  if (!box) {
    box = document.createElement('div');
    box.id = 'toast';
    document.body.appendChild(box);
  }
  const el = document.createElement('div');
  el.className = 'toast ' + (ok ? 'ok' : 'err');
  el.textContent = msg;
  box.appendChild(el);
  setTimeout(() => el.remove(), 3200);
}

function getToken() { return ''; }
function setToken(_t) {}
function clearToken() {}

async function api(method, path, body) {
  const opts = {
    method,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' }
  };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const res = await fetch(path, opts);
  let data = null;
  try { data = await res.json(); } catch (_) {}
  if (res.status === 401 && !path.startsWith('/api/auth/')) {
    location.href = './';
    throw new Error('unauthorized');
  }
  if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
  return data;
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[c]);
}

function fmtTime(s) {
  if (!s || s.startsWith('0001')) return '-';
  return new Date(s).toLocaleString('zh-CN', { hour12: false });
}

const NAV_ITEMS = [
  { href: 'ipguard.html', label: '访问限制' },
  { href: 'blocked.html', label: '拦截记录' },
  { href: 'traffic.html', label: '访问分析' },
  { href: 'system.html', label: '系统' },
  { href: 'covers.html', label: '封面图片池' },
  { href: 'account.html', label: '账号' }
];

function renderNav(active) {
  const el = $('nav');
  if (!el) return;
  el.innerHTML = '<span class="brand">logo23007</span>' +
    NAV_ITEMS.map((it) => `<a class="${it.href === active ? 'active' : ''}" href="${it.href}">${it.label}</a>`).join('') +
    '<span class="spacer"></span><a href="#" id="navLogout">退出</a>';
  $('navLogout').onclick = async (e) => {
    e.preventDefault();
    try { await api('POST', '/api/auth/logout', {}); } catch (_) {}
    location.href = './';
  };
}

async function requireAuth() {
  try {
    await api('GET', '/api/auth/me');
    return true;
  } catch (_) {
    location.href = './';
    return false;
  }
}

async function requireToken() {
  return requireAuth();
}
