// proxies.js
async function loadProxies() {
  loadPreProxy();
  const data = await api('/api/proxies');
  if (!data) return;
  const tbody = document.getElementById('proxies-tbody');
  tbody.innerHTML = '';
  (data.proxies || []).forEach(p => {
    const statusBadge = p.enabled
      ? '<span class="badge badge-green">启用</span>'
      : '<span class="badge badge-red">禁用</span>';
    const typeBadge = p.type === 'socks5'
      ? '<span class="badge badge-yellow">SOCKS5</span>'
      : '<span class="badge badge-blue">HTTP</span>';
    const maxLabel = p.max_accounts > 0 ? p.max_accounts : '∞';
    const bindInfo = '<span class="mono">' + (p.bound_count || 0) + '/' + maxLabel + '</span>';
    const lastErr = p.last_error ? '<span title="' + esc(p.last_error) + '" style="color:#f87171;cursor:help">⚠</span>' : '';
    const expiresInfo = p.expires_at ? '<span class="text-muted" style="font-size:11px">到期: ' + esc(p.expires_at) + '</span>' : '';

    // Test result display
    let testCell = '';
    if (p.last_test_ip) {
      const latColor = p.last_latency_ms < 500 ? '#4ade80' : p.last_latency_ms < 2000 ? '#fbbf24' : '#f87171';
      testCell = '<span class="mono" style="color:#4ade80">' + esc(p.last_test_ip) + '</span><br>' +
        '<span style="color:' + latColor + ';font-size:11px">' + (p.last_latency_ms || 0) + 'ms</span>';
    } else {
      testCell = '<span class="text-muted">未测试</span>';
    }

    tbody.innerHTML += '<tr id="proxy-row-' + esc(p.id) + '">' +
      '<td>' + esc(p.name) + (expiresInfo ? '<br>' + expiresInfo : '') + '</td>' +
      '<td class="mono" style="max-width:180px;overflow:hidden;text-overflow:ellipsis" title="' + esc(p.url) + '">' + esc(p.url) + '</td>' +
      '<td>' + typeBadge + '</td>' +
      '<td>' + bindInfo + '</td>' +
      '<td id="proxy-test-' + esc(p.id) + '">' + testCell + '</td>' +
      '<td style="color:#4ade80">' + (p.success_count || 0) + '</td>' +
      '<td style="color:#f87171">' + (p.error_count || 0) + ' ' + lastErr + '</td>' +
      '<td>' + statusBadge + '</td>' +
      '<td class="flex gap-2">' +
        '<button class="btn btn-sm btn-outline" id="test-btn-' + esc(p.id) + '" onclick="testSingleProxy(\'' + esc(p.id) + '\')">测试</button>' +
        '<button class="btn btn-sm btn-outline" onclick="showEditProxyModal(\'' + esc(p.id) + '\')">编辑</button>' +
        '<button class="btn btn-sm btn-outline" onclick="toggleProxy(\'' + esc(p.id) + '\')">' + (p.enabled ? '禁用' : '启用') + '</button>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteProxy(\'' + esc(p.id) + '\')">删除</button>' +
      '</td></tr>';
  });
  if (!data.proxies || data.proxies.length === 0) {
    tbody.innerHTML = '<tr><td colspan="9" style="text-align:center;color:#64748b;padding:20px">暂无代理。点击上方按钮添加。</td></tr>';
  }
  window._proxies = data.proxies || [];
}

// Test single proxy with inline UI feedback
async function testSingleProxy(id) {
  const btn = document.getElementById('test-btn-' + id);
  const cell = document.getElementById('proxy-test-' + id);
  if (btn) btn.disabled = true;
  if (btn) btn.textContent = '测试中...';
  if (cell) cell.innerHTML = '<span style="color:#fbbf24">⏳ 测试中...</span>';

  const r = await api('/api/proxies/' + id + '/test', { method: 'POST' });

  if (r && r.ok) {
    const latColor = r.latency < 500 ? '#4ade80' : r.latency < 2000 ? '#fbbf24' : '#f87171';
    if (cell) cell.innerHTML = '<span class="mono" style="color:#4ade80">' + esc(r.ip || '?') + '</span><br>' +
      '<span style="color:' + latColor + ';font-size:11px">' + r.latency + 'ms</span>';
    toast('✅ ' + (r.ip || '?') + ' - ' + r.latency + 'ms');
  } else {
    const errMsg = r ? (r.error || '未知错误') : '请求失败';
    if (cell) cell.innerHTML = '<span style="color:#f87171;font-size:11px">❌ ' + esc(errMsg).substring(0, 30) + '</span>';
    toast('❌ ' + errMsg, 'error');
  }

  if (btn) { btn.disabled = false; btn.textContent = '测试'; }
}

// Test all proxies concurrently
async function testAllProxies() {
  const resultsDiv = document.getElementById('proxy-test-results');
  if (!resultsDiv) return;

  // Show loading state
  resultsDiv.innerHTML = '<div class="card" style="margin-bottom:16px">' +
    '<div class="card-title">🔍 正在测试所有代理...</div>' +
    '<div id="test-all-progress" style="color:#fbbf24;font-size:13px">⏳ 并发测试中，请稍候...</div></div>';

  // Mark all test cells as loading
  (window._proxies || []).forEach(p => {
    const cell = document.getElementById('proxy-test-' + p.id);
    if (cell) cell.innerHTML = '<span style="color:#fbbf24">⏳</span>';
    const btn = document.getElementById('test-btn-' + p.id);
    if (btn) { btn.disabled = true; btn.textContent = '...'; }
  });

  const r = await api('/api/proxies/test-all', { method: 'POST' });

  if (!r) {
    resultsDiv.innerHTML = '<div class="card" style="margin-bottom:16px;border-color:#f87171">' +
      '<div class="card-title" style="color:#f87171">测试失败</div></div>';
    return;
  }

  // Build results summary
  const results = r.results || [];
  let html = '<div class="card" style="margin-bottom:16px"><div class="card-title">测试结果 — ' +
    '<span style="color:#4ade80">' + (r.ok || 0) + ' 可用</span> / ' +
    '<span style="color:#f87171">' + (r.fail || 0) + ' 失败</span> / ' +
    '共 ' + (r.total || 0) + ' 个</div><table>' +
    '<thead><tr><th>名称</th><th>状态</th><th>出口IP</th><th>延迟</th><th>错误</th></tr></thead><tbody>';

  results.forEach(res => {
    const statusBadge = res.ok
      ? '<span class="badge badge-green">可用</span>'
      : '<span class="badge badge-red">失败</span>';
    const latColor = res.latency_ms < 500 ? '#4ade80' : res.latency_ms < 2000 ? '#fbbf24' : '#f87171';
    const latText = res.latency_ms > 0 ? '<span style="color:' + latColor + '">' + res.latency_ms + 'ms</span>' : '-';

    html += '<tr>' +
      '<td>' + esc(res.name) + '</td>' +
      '<td>' + statusBadge + '</td>' +
      '<td class="mono">' + esc(res.ip || '-') + '</td>' +
      '<td>' + latText + '</td>' +
      '<td style="color:#f87171;font-size:12px;max-width:200px;overflow:hidden;text-overflow:ellipsis" title="' + esc(res.error || '') + '">' + esc(res.error || '-') + '</td></tr>';

    // Update inline cells
    const cell = document.getElementById('proxy-test-' + res.id);
    if (cell) {
      if (res.ok) {
        cell.innerHTML = '<span class="mono" style="color:#4ade80">' + esc(res.ip) + '</span><br>' +
          '<span style="color:' + latColor + ';font-size:11px">' + res.latency_ms + 'ms</span>';
      } else {
        cell.innerHTML = '<span style="color:#f87171;font-size:11px">❌ ' + esc(res.error || '').substring(0, 25) + '</span>';
      }
    }
    const btn = document.getElementById('test-btn-' + res.id);
    if (btn) { btn.disabled = false; btn.textContent = '测试'; }
  });

  html += '</tbody></table>' +
    '<div style="margin-top:8px;text-align:right"><button class="btn btn-sm btn-outline" onclick="document.getElementById(\'proxy-test-results\').innerHTML=\'\'">关闭</button></div></div>';
  resultsDiv.innerHTML = html;
}

async function toggleProxy(id) {
  await api('/api/proxies/' + id + '/toggle', { method: 'POST' });
  loadProxies();
}

async function deleteProxy(id) {
  if (!confirm('确定删除此代理? 绑定的账号将自动解绑。')) return;
  await api('/api/proxies/' + id, { method: 'DELETE' });
  toast('已删除');
  loadProxies();
}

// Keep old name for backward compat
async function testProxy(id) { return testSingleProxy(id); }

function showAddProxyModal() {
  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>添加代理</h3>' +
    '<div style="display:flex;gap:8px;margin-bottom:12px">' +
      '<button class="btn btn-sm btn-primary" onclick="switchProxyMode(\'single\')" id="pm-single">单个添加</button>' +
      '<button class="btn btn-sm btn-outline" onclick="switchProxyMode(\'batch\')" id="pm-batch">批量导入</button>' +
    '</div>' +
    '<div id="proxy-single-mode">' +
      '<label>名称</label><input type="text" id="m-pname" placeholder="可选，留空自动生成">' +
      '<label>代理地址 *</label><input type="text" id="m-purl" placeholder="IP|端口|用户名|密码|过期 或 http://user:pass@host:port">' +
      '<label>类型</label><select id="m-ptype"><option value="http">HTTP</option><option value="socks5">SOCKS5</option></select>' +
    '</div>' +
    '<div id="proxy-batch-mode" style="display:none">' +
      '<label>批量导入 (每行一个，支持 IP|端口|用户名|密码|过期 格式)</label>' +
      '<textarea id="m-pbatch" rows="6" style="width:100%;background:#0f172a;border:1px solid #334155;color:#e2e8f0;padding:8px;border-radius:6px;font-family:monospace;font-size:12px" placeholder="113.108.88.5|5858|2222530|678958|2026-4-10\n14.29.217.98|5858|2222530|678958|2026-4-10"></textarea>' +
    '</div>' +
    '<label>每个代理最多绑定账号数 (0=不限)</label><input type="number" id="m-pmax" value="3" min="0">' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" onclick="addProxy()">添加</button></div></div></div>';
}

function switchProxyMode(mode) {
  document.getElementById('proxy-single-mode').style.display = mode === 'single' ? 'block' : 'none';
  document.getElementById('proxy-batch-mode').style.display = mode === 'batch' ? 'block' : 'none';
  document.getElementById('pm-single').className = 'btn btn-sm ' + (mode === 'single' ? 'btn-primary' : 'btn-outline');
  document.getElementById('pm-batch').className = 'btn btn-sm ' + (mode === 'batch' ? 'btn-primary' : 'btn-outline');
}

async function addProxy() {
  const maxAccounts = parseInt(document.getElementById('m-pmax').value) || 0;
  const batchEl = document.getElementById('m-pbatch');
  const batchMode = document.getElementById('proxy-batch-mode').style.display !== 'none';

  if (batchMode && batchEl) {
    const batch = batchEl.value.trim();
    if (!batch) { toast('请输入代理列表', 'error'); return; }
    const r = await api('/api/proxies', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ batch: batch, max_accounts: maxAccounts })
    });
    if (r && r.ok) { toast('已导入 ' + (r.added || 0) + ' 个代理'); closeModal(); loadProxies(); }
    else toast('导入失败', 'error');
    return;
  }

  const url = document.getElementById('m-purl').value.trim();
  if (!url) { toast('代理地址不能为空', 'error'); return; }
  const r = await api('/api/proxies', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      name: document.getElementById('m-pname').value.trim(),
      url: url,
      type: document.getElementById('m-ptype').value,
      max_accounts: maxAccounts
    })
  });
  if (r && r.ok) { toast('代理已添加'); closeModal(); loadProxies(); }
  else toast('添加失败', 'error');
}

function showEditProxyModal(id) {
  const p = (window._proxies || []).find(x => x.id === id);
  if (!p) { toast('代理不存在', 'error'); return; }
  const maxAcc = p.max_accounts || 0;
  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>编辑代理</h3>' +
    '<label>名称</label><input type="text" id="m-ename" value="' + esc(p.name) + '">' +
    '<label>代理地址 (支持 IP|端口|用户名|密码|过期 格式)</label><input type="text" id="m-eurl" placeholder="留空不修改">' +
    '<p class="text-muted" style="font-size:11px;margin:-8px 0 8px">当前: ' + esc(p.url) + '</p>' +
    '<label>类型</label><select id="m-etype"><option value="http"' + (p.type==='http'?' selected':'') + '>HTTP</option><option value="socks5"' + (p.type==='socks5'?' selected':'') + '>SOCKS5</option></select>' +
    '<label>最多绑定账号数 (0=不限)</label><input type="number" id="m-emax" value="' + maxAcc + '" min="0">' +
    '<label>过期时间</label><input type="text" id="m-eexpires" value="' + esc(p.expires_at || '') + '" placeholder="如 2026-4-10">' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" onclick="updateProxy(\'' + esc(id) + '\')">保存</button></div></div></div>';
}

async function updateProxy(id) {
  const body = {};
  const name = document.getElementById('m-ename').value.trim();
  const url = document.getElementById('m-eurl').value.trim();
  const type = document.getElementById('m-etype').value;
  const max = parseInt(document.getElementById('m-emax').value);
  const expires = document.getElementById('m-eexpires').value.trim();

  if (name) body.name = name;
  if (url) body.url = url;
  body.type = type;
  body.max_accounts = isNaN(max) ? 0 : max;
  body.expires_at = expires;

  const r = await api('/api/proxies/' + id, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  if (r && r.ok) { toast('已更新'); closeModal(); loadProxies(); }
  else toast(r?.error || '更新失败', 'error');
}

// ==================== Pre-Proxy ====================
async function loadPreProxy() {
  const data = await api('/api/pre-proxy');
  if (!data) return;
  const input = document.getElementById('pre-proxy-input');
  const status = document.getElementById('pre-proxy-status');
  if (input) input.value = data.pre_proxy || '';
  if (status) {
    if (data.pre_proxy) {
      status.innerHTML = '<span style="color:#4ade80">✓ 已配置: ' + esc(data.pre_proxy) + '</span>';
    } else {
      status.innerHTML = '<span class="text-muted">未配置，SOCKS5 代理将直连</span>';
    }
  }
}

async function savePreProxy() {
  const val = document.getElementById('pre-proxy-input').value.trim();
  const r = await api('/api/pre-proxy', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pre_proxy: val })
  });
  if (r && r.ok) {
    toast('前置代理已保存');
    loadPreProxy();
  } else {
    toast(r?.error || '保存失败', 'error');
  }
}

async function clearPreProxy() {
  const r = await api('/api/pre-proxy', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pre_proxy: '' })
  });
  if (r && r.ok) {
    document.getElementById('pre-proxy-input').value = '';
    toast('前置代理已清除');
    loadPreProxy();
  }
}
