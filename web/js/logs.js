// logs.js
async function loadLogs() {
  const data = await api('/api/logs');
  if (!data) return;
  const container = document.getElementById('logs-container');
  container.innerHTML = '';
  (data.logs || []).forEach(l => {
    const time = new Date(l.timestamp * 1000).toLocaleString('zh-CN');
    const statusColor = l.status < 400 ? '#4ade80' : '#f87171';
    container.innerHTML += '<div class="log-entry">' +
      '<span style="color:#64748b">' + time + '</span> ' +
      '<span style="color:#60a5fa">' + esc(l.method) + '</span> ' +
      '<span>' + esc(l.path) + '</span> ' +
      '<span style="color:' + statusColor + '">' + l.status + '</span> ' +
      '<span style="color:#94a3b8">' + (l.duration_ms || 0).toFixed(0) + 'ms</span> ' +
      '<span class="mono" style="color:#fbbf24">' + esc(l.model || '') + '</span></div>';
  });
  if (!data.logs || data.logs.length === 0) {
    container.innerHTML = '<div style="padding:20px;text-align:center;color:#64748b">暂无日志</div>';
  }
}
