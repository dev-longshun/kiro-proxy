// settings.js — rate limit & concurrent settings management

async function loadSettings() {
  const data = await api('/api/settings/ratelimit');
  if (!data) return;

  document.getElementById('rl-enabled').checked = data.enabled;
  document.getElementById('rl-min-interval').value = data.min_request_interval;
  document.getElementById('rl-max-rpm').value = data.max_requests_per_minute;
  document.getElementById('rl-global-rpm').value = data.global_max_requests_per_minute;
  document.getElementById('rl-cooldown').value = data.quota_cooldown_seconds;
  document.getElementById('rl-retry-timeout').value = data.retry_timeout_seconds || 60;
  document.getElementById('rl-429-delay').value = data.retry_429_delay_seconds || 0;
  document.getElementById('rl-max-attempts').value = data.retry_max_attempts || 100;
  document.getElementById('rl-error-delay').value = data.retry_error_delay_seconds || 1;
  document.getElementById('rl-cd-threshold').value = data.cooldown_threshold || 10;
  document.getElementById('rl-connect-timeout').value = data.connect_timeout_seconds || 15;

  // 加载并发设置
  const concData = await api('/api/settings/concurrent');
  if (concData) {
    document.getElementById('max-concurrent').value = concData.default_max_concurrent;
  }

  // Load model strip patterns
  loadModelStripPatterns();

  // Load model aliases
  loadModelAliases();

  // Load real-time stats
  const stats = await api('/api/stats');
  if (stats && stats.rate_limiter) {
    const rl = stats.rate_limiter;
    let html = '<div style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;margin-bottom:12px">';
    html += statCard(rl.enabled ? '✅ 已启用' : '❌ 已禁用', '限流状态');
    html += statCard(rl.global_rpm || 0, '当前全局 RPM');
    html += statCard(Object.keys(rl.accounts || {}).length, '活跃账号数');
    html += '</div>';

    if (rl.accounts && Object.keys(rl.accounts).length > 0) {
      html += '<table><thead><tr><th>账号ID</th><th>当前RPM</th></tr></thead><tbody>';
      for (const [aid, info] of Object.entries(rl.accounts)) {
        html += '<tr><td>' + esc(aid.substring(0, 20)) + '...</td><td>' + (info.rpm || 0) + '</td></tr>';
      }
      html += '</tbody></table>';
    }
    document.getElementById('ratelimit-stats').innerHTML = html;
  }

  // 加载自动回复规则
  if (typeof loadAutoReply === 'function') loadAutoReply();
}

async function saveRateLimitSettings() {
  const payload = {
    enabled: document.getElementById('rl-enabled').checked,
    min_request_interval: parseFloat(document.getElementById('rl-min-interval').value) || 0.5,
    max_requests_per_minute: parseInt(document.getElementById('rl-max-rpm').value) || 60,
    global_max_requests_per_minute: parseInt(document.getElementById('rl-global-rpm').value) || 120,
    quota_cooldown_seconds: parseInt(document.getElementById('rl-cooldown').value) || 30,
    retry_timeout_seconds: parseInt(document.getElementById('rl-retry-timeout').value) || 60,
    retry_429_delay_seconds: parseFloat(document.getElementById('rl-429-delay').value) || 0,
    retry_max_attempts: parseInt(document.getElementById('rl-max-attempts').value) || 100,
    retry_error_delay_seconds: parseFloat(document.getElementById('rl-error-delay').value) || 1,
    cooldown_threshold: parseInt(document.getElementById('rl-cd-threshold').value) || 10,
    connect_timeout_seconds: parseInt(document.getElementById('rl-connect-timeout').value) || 15
  };

  const data = await api('/api/settings/ratelimit', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });

  if (data && data.ok) {
    toast('限流配置已保存');
  } else {
    toast('保存失败', 'error');
  }
}

async function saveConcurrentSettings() {
  const maxConcurrent = parseInt(document.getElementById('max-concurrent').value) || 2;
  const data = await api('/api/settings/concurrent', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ max_concurrent: maxConcurrent })
  });

  if (data && data.ok) {
    toast('并发限制已保存，已更新 ' + data.updated + ' 个账号');
  } else {
    toast('保存失败', 'error');
  }
}

async function loadModelStripPatterns() {
  var data = await api('/api/settings/model-strip');
  if (data && data.patterns) {
    document.getElementById('model-strip-patterns').value = (data.patterns || []).join('\n');
  }
}

async function saveModelStripPatterns() {
  var text = document.getElementById('model-strip-patterns').value;
  var patterns = text.split('\n').map(function(s) { return s.trim(); }).filter(Boolean);
  var data = await api('/api/settings/model-strip', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ patterns: patterns })
  });
  if (data && data.ok) {
    toast('模型名清理规则已保存 (' + (data.patterns || []).length + ' 条)');
  } else {
    toast('保存失败', 'error');
  }
}

async function loadModelAliases() {
  var data = await api('/api/settings/model-aliases');
  if (data && data.text !== undefined) {
    document.getElementById('model-aliases').value = data.text;
  }
}

async function saveModelAliases() {
  var text = document.getElementById('model-aliases').value;
  var data = await api('/api/settings/model-aliases', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text: text })
  });
  if (data && data.ok) {
    toast('模型映射已保存 (' + data.count + ' 条)');
  } else {
    toast('保存失败', 'error');
  }
}
