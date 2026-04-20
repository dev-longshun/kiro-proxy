// accounts.js — 账号管理 + 实时状态轮询
let _accountStatusTimer = null;

function fmtAgo(ms) {
  if (!ms || ms <= 0) return '-';
  if (ms < 1000) return ms + 'ms';
  var s = Math.floor(ms / 1000);
  if (s < 60) return s + '秒前';
  var m = Math.floor(s / 60);
  if (m < 60) return m + '分钟前';
  return Math.floor(m / 60) + '小时前';
}

function statusBadgeHtml(displayStatus, rawStatus, reason) {
  var map = {
    'streaming': '<span class="badge badge-green" style="animation:pulse 1.5s infinite">⚡ 流式中</span>',
    'active': '<span class="badge badge-green">✅ 活跃</span>',
    'idle': '<span class="badge" style="background:#f3f4f6;color:#9ca3af;border:1px solid #e5e7eb">💤 空闲</span>',
    '429-cooldown': '<span class="badge badge-yellow">⚠️ 429限流</span>',
    'cooldown': '<span class="badge badge-yellow">⏸️ 冷却</span>',
    'suspended': '<span class="badge badge-red" title="' + esc(reason || '') + '">⛔ 封禁</span>',
    'disabled': '<span class="badge badge-red">🚫 禁用</span>'
  };
  return map[displayStatus] || map[rawStatus] || '<span class="badge badge-red">' + esc(displayStatus || rawStatus) + '</span>';
}

function lastReqHtml(status, agoMs, durationMs) {
  if (!status) return '<span class="text-muted">-</span>';
  var icon = {'success':'✅','streaming':'⚡','429':'⚠️','error':'❌','EOF':'💀'}[status] || '❓';
  var ago = fmtAgo(agoMs);
  var dur = durationMs > 0 ? ' ' + (durationMs > 1000 ? (durationMs/1000).toFixed(1)+'s' : durationMs+'ms') : '';
  return '<span style="font-size:12px">' + icon + ' ' + esc(status) + dur + '</span> <span class="text-muted" style="font-size:11px">' + ago + '</span>';
}

function slotBarHtml(active, max, queued) {
  var pct = max > 0 ? Math.min(100, (active / max) * 100) : 0;
  var color = pct < 50 ? '#10b981' : pct < 80 ? '#f59e0b' : '#ef4444';
  var queueHtml = queued > 0 ? '<span style="color:#8b5cf6;font-size:10px;margin-left:4px" title="排队等待中">⏳' + queued + '</span>' : '';
  return '<div style="display:flex;align-items:center;gap:6px">' +
    '<span class="mono" style="font-size:12px;min-width:32px">' + active + '/' + max + '</span>' + queueHtml +
    '<div style="flex:1;height:6px;background:#e5e7eb;border-radius:3px;min-width:40px;overflow:hidden">' +
    '<div style="width:' + pct + '%;height:100%;background:' + color + ';border-radius:3px;transition:width .3s"></div>' +
    '</div></div>';
}

function statsRowHtml(ok, err429, errOther) {
  return '<span style="font-size:11px">' +
    '<span style="color:#10b981">✅' + (ok||0) + '</span> ' +
    '<span style="color:#f59e0b">⚠️' + (err429||0) + '</span> ' +
    '<span style="color:#ef4444">❌' + (errOther||0) + '</span>' +
    '</span>';
}

async function loadAccounts() {
  var accData = await api('/api/accounts');
  var proxyData = await api('/api/proxies');
  if (!accData) return;
  window._proxies = (proxyData && proxyData.proxies) || [];
  window._proxiesCache = window._proxies;
  window._accountsData = accData;

  var tbody = document.getElementById('accounts-tbody');
  tbody.innerHTML = '';
  (accData.accounts || []).forEach(function(a) {
    // Compute display status
    var ds = a.status;
    if (!a.enabled) ds = 'disabled';
    else if ((a.active_requests||0) > 0 && a.last_request_status === 'streaming') ds = 'streaming';
    else if (a.status === 'active' && (a.active_requests||0) === 0) ds = 'idle';

    var badge = statusBadgeHtml(ds, a.status, a.suspended_reason);
    var lastUsed = a.last_used ? new Date(a.last_used).toLocaleString('zh-CN') : '-';
    var slots = slotBarHtml(a.active_requests || 0, a.max_concurrent || 2, a.queued || 0);
    var models = (a.supported_models && a.supported_models.length > 0)
      ? a.supported_models.map(function(m) { return '<span class="badge badge-blue" style="font-size:10px;margin:1px">' + esc(m) + '</span>'; }).join('')
      : '<span class="text-muted" style="font-size:11px">全部</span>';

    // Last request info
    var reqAgoMs = a.last_request_time ? (Date.now() - new Date(a.last_request_time).getTime()) : 0;
    var lastReq = lastReqHtml(a.last_request_status, reqAgoMs, a.last_request_duration_ms);

    // Stats row
    var stats = statsRowHtml(a.total_success, a.total_429, a.total_errors);

    // Proxy info
    var proxyCell = '';
    if (a.proxy_id && a.proxy_name) {
      proxyCell = '<span class="badge badge-blue" title="' + esc(a.proxy_url || '') + '">' + esc(a.proxy_name) + '</span>' +
        ' <button class="btn btn-sm btn-outline" style="padding:1px 6px;font-size:10px" onclick="testAccountProxy(\'' + esc(a.proxy_id) + '\')">测试</button>' +
        ' <button class="btn btn-sm btn-outline" style="padding:1px 6px;font-size:10px;color:#f87171" onclick="unbindProxy(\'' + esc(a.id) + '\')">解绑</button>';
    } else {
      proxyCell = '<button class="btn btn-sm btn-outline" style="padding:2px 8px;font-size:11px" onclick="showBindProxyModal(\'' + esc(a.id) + '\')">绑定代理</button>';
    }

    // Combined usage cell: subscription + all credits in one progress bar
    var usageCell = '';
    var ku = a.kiro_usage;
    if (ku) {
      var totalUsed = 0, totalLimit = 0;
      if (ku.free_trial_limit > 0) { totalUsed += ku.free_trial_usage || 0; totalLimit += ku.free_trial_limit; }
      if (ku.usage_limit > 0) { totalUsed += ku.current_usage || 0; totalLimit += ku.usage_limit; }
      var title = ku.subscription_title || ku.subscription_type || '';
      if (ku.free_trial_status === 'ACTIVE') title += ' 试用';
      var pct = totalLimit > 0 ? Math.min(100, totalUsed / totalLimit * 100) : 0;
      var color = pct < 50 ? '#10b981' : pct < 80 ? '#f59e0b' : '#ef4444';
      usageCell = '<div style="min-width:100px">' +
        (title ? '<div style="font-size:10px;color:#6b7280;margin-bottom:2px">' + esc(title) + '</div>' : '') +
        '<div style="display:flex;justify-content:space-between;font-size:11px;margin-bottom:2px">' +
        '<span style="color:' + color + ';font-weight:600">' + totalUsed.toFixed(1) + '</span>' +
        '<span class="text-muted">/' + totalLimit.toFixed(0) + '</span></div>' +
        '<div style="height:4px;background:#e5e7eb;border-radius:2px;overflow:hidden">' +
        '<div style="width:' + pct + '%;height:100%;background:' + color + ';border-radius:2px"></div></div>';
      if (a.credits_used > 0) {
        usageCell += '<div style="font-size:10px;color:#8b5cf6;margin-top:2px">💎 ' + a.credits_used.toFixed(1) + '</div>';
      }
      if (ku.queried_at) {
        var ago = Math.round((Date.now() - new Date(ku.queried_at).getTime()) / 60000);
        usageCell += '<div class="text-muted" style="font-size:9px;margin-top:1px">' + ago + '分钟前</div>';
      }
      usageCell += '</div>';
    } else {
      usageCell = '<span class="text-muted">-</span>';
    }

    // Detail button: shows models, proxy info, etc.
    var models = (a.supported_models && a.supported_models.length > 0) ? a.supported_models.join(', ') : '全部';
    var detailCell = '<button class="btn btn-sm btn-outline" onclick="showAccountDetail(\'' + esc(a.id) + '\')" title="模型: ' + esc(models) + '">查看</button>';

    var actions = '<div style="display:flex;flex-wrap:wrap;gap:2px">' +
      '<button class="btn btn-sm btn-outline" onclick="showEditAccountModal(\'' + a.id + '\')">编辑</button>' +
      '<button class="btn btn-sm btn-outline" onclick="queryUsageLimits(\'' + a.id + '\')">用量</button>' +
      '<button class="btn btn-sm btn-outline" onclick="toggleAccount(\'' + a.id + '\')">' + (a.enabled ? '禁用' : '启用') + '</button>' +
      '<button class="btn btn-sm btn-outline" onclick="refreshAccount(\'' + a.id + '\')">刷新</button>';
    if (a.status === 'suspended') {
      actions += '<button class="btn btn-sm btn-success" onclick="unsuspendAccount(\'' + a.id + '\')">解封</button>';
    }
    actions += '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.id + '\')">删除</button></div>';

    tbody.innerHTML += '<tr data-account-id="' + esc(a.id) + '">' +
      '<td class="mono" style="max-width:180px;overflow:hidden;text-overflow:ellipsis" title="' + esc(a.email) + '">' + esc(a.email) + '</td>' +
      '<td class="td-status">' + badge + '</td>' +
      '<td class="td-slots" style="min-width:80px">' + slots + '</td>' +
      '<td class="td-lastreq" style="min-width:120px">' + lastReq + '</td>' +
      '<td class="td-stats">' + stats + '</td>' +
      '<td style="min-width:110px">' + usageCell + '</td>' +
      '<td style="min-width:100px">' + proxyCell + '</td>' +
      '<td>' + detailCell + '</td>' +
      '<td style="min-width:120px">' + actions + '</td></tr>';
  });

  if (accData.stats) {
    var s = accData.stats;
    var summary = document.getElementById('accounts-stats');
    if (summary) {
      summary.innerHTML =
        statCard(s.total, '总计') +
        statCard(s.active, '活跃') +
        statCard(s.suspended, '封禁') +
        statCard(s.cooldown, '冷却') +
        statCard(s.queued || 0, '排队') +
        statCard(s.disabled || 0, '禁用');
    }
  }

  // Start real-time status polling
  startAccountStatusPolling();
}

// Lightweight real-time status update (every 2s)
function startAccountStatusPolling() {
  if (_accountStatusTimer) return;
  _accountStatusTimer = setInterval(updateAccountStatus, 5000);
}

function stopAccountStatusPolling() {
  if (_accountStatusTimer) {
    clearInterval(_accountStatusTimer);
    _accountStatusTimer = null;
  }
}

async function updateAccountStatus() {
  // Only poll when accounts tab is visible
  var panel = document.getElementById('panel-accounts');
  if (!panel || !panel.classList.contains('active')) return;

  var data = await api('/api/accounts/status');
  if (!data || !data.accounts) return;

  data.accounts.forEach(function(s) {
    var row = document.querySelector('tr[data-account-id="' + s.id + '"]');
    if (!row) return;

    // Update status badge
    var tdStatus = row.querySelector('.td-status');
    if (tdStatus) {
      tdStatus.innerHTML = statusBadgeHtml(s.display_status, s.status, '');
    }

    // Update slots
    var tdSlots = row.querySelector('.td-slots');
    if (tdSlots) {
      tdSlots.innerHTML = slotBarHtml(s.active_requests || 0, s.max_concurrent || 2, s.queued || 0);
    }

    // Update last request
    var tdLastReq = row.querySelector('.td-lastreq');
    if (tdLastReq) {
      tdLastReq.innerHTML = lastReqHtml(s.last_request_status, s.last_request_ago_ms, s.last_request_duration_ms);
    }

    // Update stats
    var tdStats = row.querySelector('.td-stats');
    if (tdStats) {
      tdStats.innerHTML = statsRowHtml(s.total_success, s.total_429, s.total_errors);
    }
  });

  // Update global queue indicator
  var queueBanner = document.getElementById('queue-banner');
  var totalQueued = data.total_queued || 0;
  if (totalQueued > 0) {
    if (!queueBanner) {
      queueBanner = document.createElement('div');
      queueBanner.id = 'queue-banner';
      queueBanner.style.cssText = 'background:#f5f3ff;border:1px solid #c4b5fd;border-radius:8px;padding:8px 16px;margin-bottom:12px;display:flex;align-items:center;gap:8px;font-size:13px;color:#6d28d9';
      var panel = document.getElementById('panel-accounts');
      if (panel) panel.insertBefore(queueBanner, panel.querySelector('table') || panel.firstChild);
    }
    queueBanner.innerHTML = '<span style="font-size:18px">⏳</span> 当前有 <b>' + totalQueued + '</b> 个请求正在排队等待';
  } else if (queueBanner) {
    queueBanner.remove();
  }

  // 实时更新统计卡片中的排队数
  var summary = document.getElementById('accounts-stats');
  if (summary) {
    var queueCard = summary.querySelector('[data-stat="queued"]');
    if (queueCard) {
      queueCard.querySelector('.stat-value').textContent = totalQueued;
    }
  }
}

function showAccountDetail(id) {
  var data = window._accountsData;
  if (!data) return;
  var a = (data.accounts || []).find(function(x) { return x.id === id; });
  if (!a) return;

  var models = (a.supported_models && a.supported_models.length > 0)
    ? a.supported_models.map(function(m) { return '<span class="badge badge-blue" style="margin:2px">' + esc(m) + '</span>'; }).join('')
    : '<span class="text-muted">全部模型</span>';

  var ku = a.kiro_usage;
  var usageInfo = '';
  if (ku) {
    usageInfo = '<div style="margin-bottom:8px"><b>套餐:</b> ' + esc(ku.subscription_title || ku.subscription_type || '?') + '</div>';
    if (ku.free_trial_limit > 0) usageInfo += '<div>试用额度: ' + (ku.free_trial_usage||0).toFixed(1) + ' / ' + ku.free_trial_limit.toFixed(0) + (ku.free_trial_status === 'ACTIVE' ? ' (活跃)' : '') + '</div>';
    if (ku.usage_limit > 0) usageInfo += '<div>月度额度: ' + (ku.current_usage||0).toFixed(1) + ' / ' + ku.usage_limit.toFixed(0) + '</div>';
  } else {
    usageInfo = '<div class="text-muted">未查询用量</div>';
  }

  var proxyInfo = a.proxy_name ? esc(a.proxy_name) + ' (' + esc(a.proxy_url || '') + ')' : '无代理';

  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal" style="min-width:500px">' +
    '<h3>账号详情</h3>' +
    '<div style="display:grid;grid-template-columns:auto 1fr;gap:8px 16px;font-size:13px">' +
    '<b>邮箱</b><span class="mono">' + esc(a.email) + '</span>' +
    '<b>ID</b><span class="mono" style="font-size:11px">' + esc(a.id) + '</span>' +
    '<b>Machine ID</b><span class="mono" style="font-size:11px">' + esc(a.machine_id || '-') + '</span>' +
    '<b>认证方式</b><span>' + esc(a.auth_method || '?') + '</span>' +
    '<b>Credits</b><span style="color:#8b5cf6">' + (a.credits_used > 0 ? a.credits_used.toFixed(2) : '-') + '</span>' +
    '<b>代理</b><span>' + proxyInfo + '</span>' +
    '<b>模型</b><div>' + models + '</div>' +
    '</div>' +
    '<div style="margin-top:12px;padding:12px;background:#f9fafb;border-radius:8px">' + usageInfo + '</div>' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">关闭</button></div>' +
    '</div></div>';
}

function showBindProxyModal(accountId) {
  var proxies = window._proxies || [];
  if (proxies.length === 0) {
    toast('请先添加代理', 'error');
    return;
  }
  // 过滤掉可用 slot 为 0 的代理
  var available = proxies.filter(function(p) {
    if (!p.enabled) return false;
    if (p.max_accounts > 0 && (p.bound_count || 0) >= p.max_accounts) return false;
    return true;
  });
  if (available.length === 0) {
    toast('没有可用的代理（所有代理已满或已禁用）', 'error');
    return;
  }
  var options = available.map(function(p) {
    var slots = p.max_accounts > 0 ? (p.max_accounts - (p.bound_count || 0)) : '∞';
    var label = esc(p.name) + ' (' + esc(p.url) + ') [剩余: ' + slots + ']';
    return '<option value="' + esc(p.id) + '">' + label + '</option>';
  }).join('');

  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>绑定代理</h3>' +
    '<label>选择代理</label><select id="m-bind-proxy">' + options + '</select>' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" onclick="bindProxy(\'' + esc(accountId) + '\')">绑定</button></div></div></div>';
}

async function bindProxy(accountId) {
  var proxyId = document.getElementById('m-bind-proxy').value;
  var r = await api('/api/accounts/' + accountId + '/bind-proxy', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ proxy_id: proxyId })
  });
  if (r && r.ok) { toast('代理已绑定'); closeModal(); loadAccounts(); }
  else toast(r?.error || '绑定失败', 'error');
}

async function unbindProxy(accountId) {
  var r = await api('/api/accounts/' + accountId + '/unbind-proxy', { method: 'POST' });
  if (r && r.ok) { toast('已解绑'); loadAccounts(); }
  else toast('解绑失败', 'error');
}

async function testAccountProxy(proxyId) {
  toast('正在测试代理...');
  var r = await api('/api/proxies/' + proxyId + '/test', { method: 'POST' });
  if (!r) return;
  if (r.ok) toast('代理可用! IP: ' + (r.ip || '?') + ' 延迟: ' + r.latency + 'ms');
  else toast('代理不可用: ' + (r.error || ''), 'error');
}

async function queryUsageLimits(id) {
  toast('正在查询用量...');
  var r = await api('/api/accounts/' + id + '/usage-limits', { method: 'POST' });
  if (r && r.ok) {
    var u = r.usage;
    var msg = u.subscription_title || '?';
    if (u.free_trial_limit > 0) msg += ' 试用: ' + u.free_trial_usage.toFixed(1) + '/' + u.free_trial_limit.toFixed(0);
    if (u.usage_limit > 0) msg += ' 月: ' + u.current_usage.toFixed(1) + '/' + u.usage_limit.toFixed(0);
    toast('✅ ' + msg);
  } else {
    toast('查询失败: ' + (r?.error || ''), 'error');
  }
  loadAccounts();
}

async function queryAllUsageLimits() {
  var data = await api('/api/accounts');
  if (!data) return;
  var accounts = data.accounts || [];
  var active = accounts.filter(function(a) { return a.has_token && a.enabled; });
  if (active.length === 0) { toast('没有可查询的账号', 'error'); return; }
  toast('正在查询 ' + active.length + ' 个账号的用量...');
  var done = 0;
  for (var i = 0; i < active.length; i++) {
    await api('/api/accounts/' + active[i].id + '/usage-limits', { method: 'POST' });
    done++;
  }
  toast('✅ 已查询 ' + done + ' 个账号');
  loadAccounts();
}

async function toggleAccount(id) {
  await api('/api/accounts/' + id + '/toggle', { method: 'POST' });
  loadAccounts();
}

async function refreshAccount(id) {
  var r = await api('/api/accounts/' + id + '/refresh', { method: 'POST' });
  if (r && r.ok) toast('Token已刷新');
  else toast('刷新失败', 'error');
  loadAccounts();
}

async function deleteAccount(id) {
  if (!confirm('确定删除此账号? 代理绑定将自动解除。')) return;
  await api('/api/accounts/' + id, { method: 'DELETE' });
  toast('已删除');
  loadAccounts();
}

async function unsuspendAccount(id) {
  var r = await api('/api/accounts/' + id + '/restore', { method: 'POST' });
  if (r && r.ok) toast('已解除封禁');
  else toast('操作失败', 'error');
  loadAccounts();
}

async function refreshAllTokens() {
  var r = await api('/api/accounts/refresh-all', { method: 'POST' });
  if (r) toast('已刷新 ' + (r.refreshed || 0) + '/' + (r.total || 0) + ' 个账号');
  loadAccounts();
}

function showAddAccountModal() {
  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>添加账号</h3>' +
    '<div style="display:flex;gap:8px;margin-bottom:12px">' +
      '<button class="btn btn-sm btn-outline" onclick="switchAddMode(\'simple\')" id="add-mode-simple" style="border-color:#60a5fa">简单模式</button>' +
      '<button class="btn btn-sm btn-outline" onclick="switchAddMode(\'json\')" id="add-mode-json">JSON模式</button>' +
      '<button class="btn btn-sm btn-outline" onclick="switchAddMode(\'kam\')" id="add-mode-kam">KAM导入</button>' +
    '</div>' +
    '<div id="add-simple">' +
      '<label>Access Token *</label><input type="text" id="m-token" placeholder="粘贴access_token">' +
      '<label>Refresh Token</label><input type="text" id="m-refresh" placeholder="可选">' +
      '<label>Client ID</label><input type="text" id="m-clientid" placeholder="可选，Google登录需要">' +
      '<label>Client Secret</label><input type="text" id="m-clientsecret" placeholder="可选，Google登录需要">' +
    '</div>' +
    '<div id="add-json" style="display:none">' +
      '<label>粘贴完整 Credentials JSON</label>' +
      '<textarea id="m-creds-json" rows="8" placeholder=\'{"clientId":"...","clientSecret":"...","accessToken":"...","refreshToken":"..."}\'></textarea>' +
    '</div>' +
    '<div id="add-kam" style="display:none">' +
      '<div id="kam-drop-zone" style="border:2px dashed #4b5563;border-radius:8px;padding:32px 16px;text-align:center;cursor:pointer;transition:border-color .2s,background .2s">' +
        '<p style="margin:0;color:#9ca3af">拖拽 KAM JSON 文件到此处，或点击选择文件</p>' +
        '<input type="file" id="kam-file-input" accept=".json" style="display:none">' +
      '</div>' +
      '<label style="margin-top:12px;display:block">或直接粘贴 JSON</label>' +
      '<textarea id="kam-json-area" rows="6" placeholder=\'{"version":"1.0.0","accounts":[...]}\'></textarea>' +
      '<div id="kam-preview" style="margin-top:8px;color:#9ca3af;font-size:13px"></div>' +
    '</div>' +
    '<div id="kam-extra-fields">' +
      '<label>邮箱</label><input type="text" id="m-email" placeholder="可选">' +
      '<label>昵称</label><input type="text" id="m-nick" placeholder="可选">' +
    '</div>' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" id="add-account-btn" onclick="addAccount()">添加</button></div></div></div>';
  initKamDropZone();
}

function switchAddMode(mode) {
  document.getElementById('add-simple').style.display = mode === 'simple' ? '' : 'none';
  document.getElementById('add-json').style.display = mode === 'json' ? '' : 'none';
  document.getElementById('add-kam').style.display = mode === 'kam' ? '' : 'none';
  document.getElementById('add-mode-simple').style.borderColor = mode === 'simple' ? '#60a5fa' : '';
  document.getElementById('add-mode-json').style.borderColor = mode === 'json' ? '#60a5fa' : '';
  document.getElementById('add-mode-kam').style.borderColor = mode === 'kam' ? '#60a5fa' : '';
  // KAM 模式隐藏邮箱/昵称字段，显示导入按钮
  document.getElementById('kam-extra-fields').style.display = mode === 'kam' ? 'none' : '';
  var btn = document.getElementById('add-account-btn');
  btn.textContent = mode === 'kam' ? '导入' : '添加';
  btn.setAttribute('onclick', mode === 'kam' ? 'handleKamImport()' : 'addAccount()');
}

function initKamDropZone() {
  setTimeout(function() {
    var zone = document.getElementById('kam-drop-zone');
    var fileInput = document.getElementById('kam-file-input');
    var textarea = document.getElementById('kam-json-area');
    if (!zone) return;

    zone.addEventListener('click', function() { fileInput.click(); });

    zone.addEventListener('dragover', function(e) {
      e.preventDefault();
      zone.style.borderColor = '#60a5fa';
      zone.style.background = 'rgba(96,165,250,0.05)';
    });
    zone.addEventListener('dragleave', function() {
      zone.style.borderColor = '#4b5563';
      zone.style.background = '';
    });
    zone.addEventListener('drop', function(e) {
      e.preventDefault();
      zone.style.borderColor = '#4b5563';
      zone.style.background = '';
      var file = e.dataTransfer.files[0];
      if (file) readKamFile(file);
    });

    fileInput.addEventListener('change', function() {
      if (fileInput.files[0]) readKamFile(fileInput.files[0]);
    });

    textarea.addEventListener('input', function() {
      updateKamPreview(textarea.value);
    });
  }, 0);
}

function readKamFile(file) {
  var reader = new FileReader();
  reader.onload = function(e) {
    var text = e.target.result;
    document.getElementById('kam-json-area').value = text;
    updateKamPreview(text);
  };
  reader.readAsText(file);
}

function updateKamPreview(text) {
  var preview = document.getElementById('kam-preview');
  if (!text.trim()) { preview.textContent = ''; return; }
  try {
    var accounts = parseKamJson(text);
    preview.style.color = '#34d399';
    preview.textContent = '检测到 ' + accounts.length + ' 个账号';
  } catch(e) {
    preview.style.color = '#f87171';
    preview.textContent = '解析失败: ' + e.message;
  }
}

function parseKamJson(text) {
  var data = JSON.parse(text);
  var items = [];

  if (data.accounts && Array.isArray(data.accounts)) {
    items = data.accounts;
  } else if (Array.isArray(data)) {
    items = data;
  } else if (data.credentials) {
    items = [data];
  } else if (data.refreshToken || data.accessToken || data.clientId) {
    items = [{ credentials: data }];
  } else {
    throw new Error('无法识别的 JSON 格式');
  }

  var result = [];
  for (var i = 0; i < items.length; i++) {
    var item = items[i];
    var creds = item.credentials || item;
    if (!creds.refreshToken && !creds.accessToken) continue;
    var acc = {};
    if (creds.clientId) acc.clientId = creds.clientId;
    if (creds.clientSecret) acc.clientSecret = creds.clientSecret;
    if (creds.accessToken) acc.accessToken = creds.accessToken;
    if (creds.refreshToken) acc.refreshToken = creds.refreshToken;
    if (creds.region) acc.region = creds.region;
    if (item.email) acc.email = item.email;
    if (item.machineId) acc.machineId = item.machineId;
    if (item.nickname) acc.nickname = item.nickname;
    result.push(acc);
  }
  if (result.length === 0) throw new Error('未找到有效账号');
  return result;
}

async function handleKamImport() {
  var textarea = document.getElementById('kam-json-area');
  var text = textarea.value.trim();
  if (!text) { toast('请先拖入文件或粘贴 JSON', 'error'); return; }

  var accounts;
  try {
    accounts = parseKamJson(text);
  } catch(e) {
    toast('解析失败: ' + e.message, 'error');
    return;
  }

  console.log('[KAM] 准备导入', accounts.length, '个账号, payload:', JSON.stringify(accounts, null, 2));

  var r = await api('/api/accounts', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(accounts)
  });

  console.log('[KAM] 响应:', JSON.stringify(r));

  if (r && r.ok) {
    var msg = '导入成功: ' + (r.imported || 0) + ' 个账号';
    if (r.skipped) msg += '，跳过 ' + r.skipped + ' 个重复';
    toast(msg);
    closeModal();
    loadAccounts();
  } else {
    var errMsg = (r && r.error) ? r.error : '导入失败（无响应）';
    if (r && r.status) errMsg += ' [HTTP ' + r.status + ']';
    toast(errMsg, 'error');
  }
}

async function addAccount() {
  var body = {};
  var jsonArea = document.getElementById('m-creds-json');
  var isJsonMode = jsonArea && jsonArea.offsetParent !== null;

  if (isJsonMode) {
    var raw = jsonArea.value.trim();
    if (!raw) { toast('JSON不能为空', 'error'); return; }
    try {
      var parsed = JSON.parse(raw);
      if (parsed.credentials) {
        body.credentials = parsed.credentials;
      } else if (parsed.accessToken || parsed.clientId) {
        body = parsed;
      } else {
        body.credentials = parsed;
      }
    } catch(e) {
      toast('JSON格式错误: ' + e.message, 'error');
      return;
    }
  } else {
    var token = document.getElementById('m-token').value.trim();
    if (!token) { toast('Token不能为空', 'error'); return; }
    body.accessToken = token;
    var rt = document.getElementById('m-refresh').value.trim();
    if (rt) body.refreshToken = rt;
    var cid = document.getElementById('m-clientid').value.trim();
    if (cid) body.clientId = cid;
    var cs = document.getElementById('m-clientsecret').value.trim();
    if (cs) body.clientSecret = cs;
  }

  body.email = document.getElementById('m-email').value.trim();
  body.nickname = document.getElementById('m-nick').value.trim();

  var r = await api('/api/accounts', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (r && r.ok) { toast('账号已添加'); closeModal(); loadAccounts(); }
  else toast(r?.error || '添加失败', 'error');
}

async function exportAccounts() {
  var data = await api('/api/accounts/export');
  if (!data) return;
  var blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
  var a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'kiro-accounts-export.json';
  a.click();
  toast('已导出');
}

async function importAccounts(event) {
  var file = event.target.files[0];
  if (!file) return;
  var text = await file.text();
  var r = await api('/api/accounts/import', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: text
  });
  if (r && r.ok) { toast('已导入 ' + (r.imported || 0) + ' 个账号'); loadAccounts(); }
  else toast('导入失败', 'error');
  event.target.value = '';
}

function showEditAccountModal(id) {
  var data = window._accountsData;
  if (!data) return;
  var a = (data.accounts || []).find(function(x) { return x.id === id; });
  if (!a) { toast('账号不存在', 'error'); return; }

  var proxyOptions = '<option value="">无代理</option>';
  if (window._proxiesCache) {
    window._proxiesCache.forEach(function(p) {
      var sel = p.id === a.proxy_id ? ' selected' : '';
      var slots = p.max_accounts > 0 ? ' (' + (p.bound_accounts || []).length + '/' + p.max_accounts + ')' : '';
      proxyOptions += '<option value="' + esc(p.id) + '"' + sel + '>' + esc(p.name) + slots + '</option>';
    });
  }

  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>编辑账号</h3>' +
    '<label>邮箱</label><input type="text" id="m-aemail" value="' + esc(a.email) + '">' +
    '<label>昵称</label><input type="text" id="m-anick" value="' + esc(a.nickname || '') + '">' +
    '<label>最大并发数</label><input type="number" id="m-amc" value="' + (a.max_concurrent || 2) + '" min="1" max="20">' +
    '<label>Machine ID</label><input type="text" id="m-amid" value="' + esc(a.machine_id || '') + '" placeholder="留空自动生成">' +
    '<label>Access Token</label><input type="text" id="m-aat" placeholder="留空不修改" title="' + esc(a.access_token_preview || '') + '">' +
    '<div class="text-muted" style="font-size:11px;margin-top:2px">当前: ' + esc(a.access_token_preview || '无') + ' | 认证: ' + esc(a.auth_method || '?') + ' | Refresh: ' + (a.has_refresh_token ? '有' : '无') + '</div>' +
    '<label>Refresh Token</label><input type="text" id="m-art" placeholder="留空不修改">' +
    '<label>支持的模型 <span style="font-weight:normal;color:#888">(逗号分隔，留空=支持所有)</span></label><input type="text" id="m-amodels" value="' + esc((a.supported_models || []).join(', ')) + '" placeholder="例: claude-sonnet-4, claude-sonnet-4.5">' +
    '<label>代理</label><select id="m-aproxy">' + proxyOptions + '</select>' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" onclick="saveEditAccount(\'' + esc(id) + '\')">保存</button></div></div></div>';

  if (!window._proxiesCache) {
    api('/api/proxies').then(function(data) {
      if (data && data.proxies) {
        window._proxiesCache = data.proxies;
        showEditAccountModal(id);
      }
    });
  }
}

async function saveEditAccount(id) {
  var email = document.getElementById('m-aemail').value.trim();
  var nickname = document.getElementById('m-anick').value.trim();
  var mc = parseInt(document.getElementById('m-amc').value);
  var proxyId = document.getElementById('m-aproxy').value;
  var machineId = document.getElementById('m-amid').value.trim();
  var accessToken = document.getElementById('m-aat').value.trim();
  var refreshToken = document.getElementById('m-art').value.trim();
  var modelsStr = document.getElementById('m-amodels').value.trim();
  var supportedModels = modelsStr ? modelsStr.split(',').map(function(s) { return s.trim(); }).filter(Boolean) : [];

  var body = {
    email: email || undefined,
    nickname: nickname,
    max_concurrent: isNaN(mc) ? 2 : mc,
    proxy_id: proxyId,
    supported_models: supportedModels
  };
  if (machineId) body.machine_id = machineId;
  if (accessToken) body.access_token = accessToken;
  if (refreshToken) body.refresh_token = refreshToken;

  var r = await api('/api/accounts/' + id + '/edit', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (r && r.ok) {
    toast('已更新');
    closeModal();
    window._proxiesCache = null;
    loadAccounts();
  } else {
    toast(r?.error || '更新失败', 'error');
  }
}
