// app.js — tab switching, auto-refresh, and real-time metrics
let refreshTimer = null;

function switchTab(name) {
  document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  // 高亮对应的 tab 按钮
  document.querySelectorAll('.tab').forEach(t => {
    if (t.textContent.toLowerCase().includes(name) || t.getAttribute('onclick')?.includes("'" + name + "'")) {
      t.classList.add('active');
    }
  });
  var panel = document.getElementById('panel-' + name);
  if (panel) panel.classList.add('active');

  var loaders = {
    accounts: typeof loadAccounts === 'function' ? loadAccounts : null,
    keys: typeof loadKeys === 'function' ? loadKeys : null,
    proxies: typeof loadProxies === 'function' ? loadProxies : null,
    usage: typeof loadUsage === 'function' ? loadUsage : null,
    models: function() { loadKiroModels(); },
    settings: typeof loadSettings === 'function' ? loadSettings : null
  };
  if (loaders[name]) loaders[name]();
}

function startAutoRefresh() {
  if (refreshTimer) clearInterval(refreshTimer);
  refreshTimer = setInterval(() => {
    const active = document.querySelector('.panel.active');
    if (active) {
      const id = active.id.replace('panel-', '');
      if (id === 'accounts') loadAccounts();
    }
  }, 30000);
}

// ===== Real-time Metrics =====
function fmtTokens(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n;
}

function fmtCredits(n) {
  if (n === undefined || n === null || n < 0) return '-';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n.toFixed(1);
}

function fmtUptime(s) {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h' + Math.floor((s % 3600) / 60) + 'm';
  return Math.floor(s / 86400) + 'd' + Math.floor((s % 86400) / 3600) + 'h';
}

function drawMetricsChart(history) {
  var canvas = document.getElementById('metrics-chart');
  if (!canvas) return;
  var ctx = canvas.getContext('2d');
  var dpr = window.devicePixelRatio || 1;
  var w = canvas.clientWidth;
  var h = canvas.clientHeight;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  ctx.scale(dpr, dpr);

  ctx.clearRect(0, 0, w, h);

  if (!history || history.length < 2) {
    // 没有足够数据时画一条基线
    ctx.beginPath();
    ctx.strokeStyle = '#334155';
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 4]);
    ctx.moveTo(4, h / 2);
    ctx.lineTo(w - 4, h / 2);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.font = '10px sans-serif';
    ctx.fillStyle = '#64748b';
    ctx.fillText('等待数据...', w / 2 - 25, h / 2 - 6);
    return;
  }

  // 计算最大值，确保至少为 1 避免除零
  var maxReq = 0, maxIn = 0, maxOut = 0;
  var hasAnyData = false;
  for (var i = 0; i < history.length; i++) {
    if (history[i].req > 0 || history[i]['in'] > 0 || history[i].out > 0) hasAnyData = true;
    if (history[i].req > maxReq) maxReq = history[i].req;
    if (history[i]['in'] > maxIn) maxIn = history[i]['in'];
    if (history[i].out > maxOut) maxOut = history[i].out;
  }

  // 如果所有数据都是 0，画一条基线
  if (!hasAnyData) {
    ctx.beginPath();
    ctx.strokeStyle = '#334155';
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 4]);
    ctx.moveTo(4, h - 8);
    ctx.lineTo(w - 4, h - 8);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.font = '10px sans-serif';
    ctx.fillStyle = '#64748b';
    ctx.fillText('暂无活动', w / 2 - 20, h / 2);
    return;
  }

  // 确保最大值至少为 1
  if (maxReq < 1) maxReq = 1;
  var maxTokens = Math.max(maxIn, maxOut, 1);

  var pad = 4;
  var plotW = w - pad * 2;
  var plotH = h - pad * 2;

  // 画网格线
  ctx.beginPath();
  ctx.strokeStyle = '#1e293b';
  ctx.lineWidth = 0.5;
  for (var g = 0; g < 3; g++) {
    var gy = pad + (plotH / 3) * g;
    ctx.moveTo(pad, gy);
    ctx.lineTo(w - pad, gy);
  }
  ctx.stroke();

  function drawLine(data, key, maxVal, color, alpha) {
    ctx.beginPath();
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.globalAlpha = alpha;
    for (var i = 0; i < data.length; i++) {
      var x = pad + (i / (data.length - 1)) * plotW;
      var y = pad + plotH - (data[i][key] / maxVal) * plotH;
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // 画面积填充
    ctx.globalAlpha = alpha * 0.1;
    ctx.lineTo(pad + plotW, pad + plotH);
    ctx.lineTo(pad, pad + plotH);
    ctx.closePath();
    ctx.fillStyle = color;
    ctx.fill();
    ctx.globalAlpha = 1;
  }

  // 画三条线：RPS（绿）、Input tokens/s（蓝）、Output tokens/s（紫）
  drawLine(history, 'req', maxReq, '#34d399', 0.9);
  drawLine(history, 'in', maxTokens, '#60a5fa', 0.7);
  drawLine(history, 'out', maxTokens, '#c084fc', 0.7);

  // 在最后一个数据点画圆点
  if (history.length > 0) {
    var last = history[history.length - 1];
    var lx = pad + plotW;
    var dots = [
      { val: last.req, max: maxReq, color: '#34d399' },
      { val: last['in'], max: maxTokens, color: '#60a5fa' },
      { val: last.out, max: maxTokens, color: '#c084fc' }
    ];
    dots.forEach(function(d) {
      if (d.val > 0) {
        var dy = pad + plotH - (d.val / d.max) * plotH;
        ctx.beginPath();
        ctx.arc(lx, dy, 2.5, 0, Math.PI * 2);
        ctx.fillStyle = d.color;
        ctx.fill();
      }
    });
  }

  // 图例
  ctx.font = '10px sans-serif';
  ctx.globalAlpha = 0.8;
  var labels = [
    { text: '请求/秒', color: '#34d399' },
    { text: '输入/秒', color: '#60a5fa' },
    { text: '输出/秒', color: '#c084fc' }
  ];
  var lx = w - 8;
  for (var i = labels.length - 1; i >= 0; i--) {
    var tw = ctx.measureText(labels[i].text).width;
    lx -= tw + 4;
    ctx.fillStyle = labels[i].color;
    ctx.fillRect(lx - 10, 4, 8, 8);
    ctx.fillText(labels[i].text, lx, 12);
    lx -= 14;
  }
  ctx.globalAlpha = 1;
}

async function pollMetrics() {
  var data = await api('/api/metrics');
  if (!data) return;

  var el = function(id) { return document.getElementById(id); };
  if (el('m-rpm')) el('m-rpm').textContent = data.rpm || 0;
  if (el('m-rps')) el('m-rps').textContent = data.rps || 0;
  if (el('m-active')) el('m-active').textContent = data.active || 0;
  if (el('m-queued')) {
    el('m-queued').textContent = data.queued || 0;
    el('m-queued').style.color = data.queued > 0 ? '#fbbf24' : '#fff';
  }
  if (el('m-in-s')) el('m-in-s').textContent = fmtTokens(data.input_tokens_s || 0);
  if (el('m-out-s')) el('m-out-s').textContent = fmtTokens(data.output_tokens_s || 0);
  if (el('m-in-m')) el('m-in-m').textContent = fmtTokens(data.input_tokens_m || 0);
  if (el('m-out-m')) el('m-out-m').textContent = fmtTokens(data.output_tokens_m || 0);
  if (el('m-total')) el('m-total').textContent = fmtTokens(data.total_requests || 0);
  if (el('m-uptime')) el('m-uptime').textContent = fmtUptime(data.uptime_seconds || 0);

  // 更新积分汇总
  var cs = data.credits_summary;
  if (cs) {
    if (el('m-credits-used')) el('m-credits-used').textContent = fmtCredits(cs.total_credits_used);
    if (el('m-credits-limit')) el('m-credits-limit').textContent = fmtCredits(cs.total_credits_limit);
    if (el('m-credits-remaining')) el('m-credits-remaining').textContent = fmtCredits(cs.total_credits_remaining);

    // 进度条
    var totalUsed = cs.total_credits_used || 0;
    var totalLimit = cs.total_credits_limit || 0;
    var pct = totalLimit > 0 ? Math.min(100, totalUsed / totalLimit * 100) : 0;
    var bar = el('credits-progress-bar');
    var pctEl = el('credits-pct');
    if (bar) {
      bar.style.width = pct + '%';
      bar.style.background = pct < 50 ? '#10b981' : pct < 80 ? '#f59e0b' : '#ef4444';
    }
    if (pctEl) {
      var pctText = totalLimit > 0 ? pct.toFixed(1) + '%' : '-';
      if (cs.queried_count < cs.account_count) {
        pctText += ' (' + cs.queried_count + '/' + cs.account_count + '号已查)';
      }
      pctEl.textContent = pctText;
    }

    // 有积分数据时才显示积分条
    var creditsBar = el('credits-bar');
    if (creditsBar) {
      creditsBar.style.display = (cs.total_credits_limit > 0 || cs.total_credits_used > 0) ? 'flex' : 'none';
    }
  }

  drawMetricsChart(data.history);
}

// ===== Cache Toggle =====
async function toggleCache() {
  var r = await api('/api/cache-toggle', { method: 'POST' });
  if (r) updateCacheBtn(r.enabled);
}

async function loadCacheStatus() {
  var r = await api('/api/cache-toggle');
  if (r) updateCacheBtn(r.enabled);
}

function updateCacheBtn(enabled) {
  var btn = document.getElementById('cache-toggle-btn');
  if (!btn) return;
  btn.textContent = '缓存: ' + (enabled ? '开' : '关');
  btn.style.background = enabled ? '#059669' : '#4338ca';
  btn.style.borderColor = enabled ? '#10b981' : '#6366f1';
}

// Start metrics polling (every 3s for smoother chart updates)
setInterval(pollMetrics, 3000);
pollMetrics();
loadCacheStatus();

// Init — accounts is the default tab
loadAccounts();
startAutoRefresh();
