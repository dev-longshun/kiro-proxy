// api.js — shared utilities
const BASE = window.location.origin;

function toast(msg, type) {
  const t = document.createElement('div');
  t.className = 'toast toast-' + (type || 'success');
  t.textContent = msg;
  document.body.appendChild(t);
  setTimeout(() => t.remove(), 3000);
}

async function api(path, opts) {
  try {
    const r = await fetch(BASE + path, opts);
    if (!r.ok) return null;
    return await r.json();
  } catch (e) {
    // 静默处理轮询接口的错误
    if (path === '/api/metrics' || path === '/api/accounts/status') return null;
    toast('请求失败: ' + e.message, 'error');
    return null;
  }
}

function closeModal() {
  document.getElementById('modal-container').innerHTML = '';
}

function esc(s) {
  if (!s) return '';
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function fmtNum(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n;
}

// Icon + color config for stat cards
var _statIcons = {
  '总计': { icon: '📊', cls: 'stat-icon-blue' },
  '活跃': { icon: '✅', cls: 'stat-icon-green' },
  '封禁': { icon: '⛔', cls: 'stat-icon-red' },
  '冷却': { icon: '⏸️', cls: 'stat-icon-amber' },
  '排队': { icon: '⏳', cls: 'stat-icon-purple' },
  '禁用': { icon: '🚫', cls: 'stat-icon-gray' },
  '总请求': { icon: '📈', cls: 'stat-icon-blue' },
  '成功': { icon: '✅', cls: 'stat-icon-green' },
  '失败': { icon: '❌', cls: 'stat-icon-red' },
  'API 密钥': { icon: '🔑', cls: 'stat-icon-purple' },
  '代理': { icon: '🌐', cls: 'stat-icon-blue' }
};

function statCard(val, label) {
  var cfg = _statIcons[label] || { icon: '📋', cls: 'stat-icon-gray' };
  var dataAttr = '';
  if (label === '排队') dataAttr = ' data-stat="queued"';
  return '<div class="stat-card"' + dataAttr + '>' +
    '<div class="stat-icon ' + cfg.cls + '">' + cfg.icon + '</div>' +
    '<div class="stat-info"><div class="stat-value">' + val + '</div><div class="stat-label">' + label + '</div></div>' +
    '</div>';
}

function copyText(t) {
  navigator.clipboard.writeText(t);
  toast('已复制');
}
