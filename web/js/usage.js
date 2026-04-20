// usage.js
var _usageLogTimer = null;
var _logPollingOn = false;

async function loadUsage() {
  var usage = await api('/api/usage');
  if (usage) {
    const t = usage.total || {};
    document.getElementById('usage-stats').innerHTML =
      statCard(t.total_requests || 0, '总请求') +
      statCard(t.success_count || 0, '成功') +
      statCard(t.error_count || 0, '失败') +
      statCard(fmtNum(t.total_input_tokens || 0), '输入Token') +
      statCard(fmtNum(t.total_output_tokens || 0), '输出Token') +
      statCard(Math.round(t.avg_duration_ms || 0) + 'ms', '平均耗时');

    const mtbody = document.getElementById('usage-model-tbody');
    mtbody.innerHTML = '';
    const byModel = usage.by_model || {};
    Object.keys(byModel).forEach(m => {
      const s = byModel[m];
      mtbody.innerHTML += '<tr><td class="mono">' + esc(m) + '</td><td>' + s.total_requests +
        '</td><td>' + s.success_count + '</td><td>' + s.error_count +
        '</td><td>' + fmtNum(s.total_input_tokens) + '</td><td>' + fmtNum(s.total_output_tokens) + '</td></tr>';
    });
  }
  loadRequestLogs();
}

function toggleLogPolling() {
  if (_logPollingOn) {
    clearInterval(_usageLogTimer);
    _usageLogTimer = null;
    _logPollingOn = false;
  } else {
    _usageLogTimer = setInterval(function() {
      var panel = document.getElementById('panel-usage');
      if (!panel || !panel.classList.contains('active')) return;
      loadRequestLogs();
    }, 1000);
    _logPollingOn = true;
  }
  var btn = document.getElementById('log-auto-btn');
  if (btn) {
    btn.textContent = '自动刷新: ' + (_logPollingOn ? '开' : '关');
    btn.style.background = _logPollingOn ? '#059669' : '';
    btn.style.color = _logPollingOn ? '#fff' : '';
    btn.style.borderColor = _logPollingOn ? '#10b981' : '';
  }
}

// 新会话星星图标
var _sparkSvg = '<svg width="12" height="12" viewBox="0 0 24 24" fill="#f59e0b" style="vertical-align:middle;margin-right:3px"><path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/></svg>';

async function loadRequestLogs() {
  var data = await api('/api/logs?limit=30');
  if (!data || !data.logs) return;
  var container = document.getElementById('request-logs-tbody');
  if (!container) return;
  container.innerHTML = '';
  data.logs.forEach(function(log) {
    var time = log.timestamp ? new Date(log.timestamp).toLocaleString('zh-CN') : '-';
    var statusBadge = log.success
      ? '<span class="badge badge-green">OK</span>'
      : '<span class="badge badge-red">ERR</span>';
    var fileKey = esc(log._file || '');
    var convId = log.conversation_id || '-';
    var convShort = convId;
    if (convShort.length > 12) convShort = convShort.substring(0, 12) + '...';

    var convIcon = log.is_new_conv ? _sparkSvg : '';

    container.innerHTML += '<tr>' +
      '<td style="white-space:nowrap">' + time + '</td>' +
      '<td class="mono" style="font-size:10px" title="' + esc(convId) + '">' + convIcon + esc(convShort) + '</td>' +
      '<td class="mono">' + esc(log.model || '-') + '</td>' +
      '<td class="mono" style="font-size:11px">' + esc(log.account_email || '-') + '</td>' +
      '<td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(log.request_preview || '') + '">' + esc(log.request_preview || '-') + '</td>' +
      '<td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(log.response_preview || '') + '">' + esc(log.response_preview || '-') + '</td>' +
      '<td>' + fmtNum(log.input_tokens || 0) + '/' + fmtNum(log.output_tokens || 0) + '</td>' +
      '<td>' + (log.duration_ms || 0) + 'ms</td>' +
      '<td>' + statusBadge + '</td>' +
      '<td><button class="btn btn-sm btn-outline" onclick="showLogDetail(\'' + fileKey + '\')">详情</button></td>' +
      '</tr>';
  });
}

async function showLogDetail(fileKey) {
  if (!fileKey) return;
  toast('加载中...');
  var log = await api('/api/logs?file=' + encodeURIComponent(fileKey));
  if (!log) { toast('加载失败', 'error'); return; }

  var reqText = '';
  if (log.request && log.request.messages) {
    var msgs = log.request.messages;
    for (var i = 0; i < msgs.length; i++) {
      var m = msgs[i];
      var role = m.role || '?';
      var text = '';
      if (typeof m.content === 'string') text = m.content;
      else if (Array.isArray(m.content)) {
        for (var j = 0; j < m.content.length; j++) {
          if (m.content[j].text) text += m.content[j].text;
        }
      }
      if (text.length > 2000) text = text.substring(0, 2000) + '...[截断]';
      reqText += '<div style="margin-bottom:8px"><b style="color:' + (role === 'user' ? '#3b82f6' : role === 'assistant' ? '#10b981' : '#9ca3af') + '">' + esc(role) + ':</b> <span style="white-space:pre-wrap">' + esc(text) + '</span></div>';
    }
  } else if (log.request) {
    var raw = JSON.stringify(log.request, null, 2);
    if (raw.length > 5000) raw = raw.substring(0, 5000) + '\n...[截断]';
    reqText = '<pre style="max-height:300px;overflow:auto;font-size:12px">' + esc(raw) + '</pre>';
  }

  var respText = '';
  if (typeof log.response === 'string') {
    var r = log.response;
    if (r.length > 5000) r = r.substring(0, 5000) + '...[截断]';
    respText = '<div style="white-space:pre-wrap;max-height:300px;overflow:auto">' + esc(r) + '</div>';
  } else if (log.response) {
    var raw2 = JSON.stringify(log.response, null, 2);
    if (raw2.length > 5000) raw2 = raw2.substring(0, 5000) + '\n...[截断]';
    respText = '<pre style="max-height:300px;overflow:auto;font-size:12px">' + esc(raw2) + '</pre>';
  }

  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal" style="min-width:700px;max-width:90vw;max-height:90vh;overflow:auto">' +
    '<h3>请求详情</h3>' +
    '<div style="display:grid;grid-template-columns:auto 1fr;gap:4px 12px;font-size:12px;margin-bottom:12px">' +
    '<b>时间</b><span>' + esc(log.timestamp || '') + '</span>' +
    '<b>Kiro会话</b><span class="mono" style="font-size:11px">' + esc(log.conversation_id || '-') + '</span>' +
    '<b>会话ID</b><span class="mono" style="font-size:11px">' + esc(log.session_id || '-') + '</span>' +
    '<b>会话Key</b><span class="mono" style="font-size:11px">' + esc(log.session_key || '-') + '</span>' +
    '<b>模型</b><span class="mono">' + esc(log.model || '') + '</span>' +
    '<b>协议</b><span>' + esc(log.protocol || '') + '</span>' +
    '<b>账号</b><span class="mono">' + esc(log.account_email || '') + '</span>' +
    '<b>密钥</b><span class="mono">' + esc(log.api_key || '') + '</span>' +
    '<b>Token</b><span>输入 ' + (log.input_tokens || 0) + ' / 输出 ' + (log.output_tokens || 0) + '</span>' +
    '<b>耗时</b><span>' + (log.duration_ms || 0) + 'ms</span>' +
    '<b>状态</b><span>' + (log.success ? '成功' : '失败: ' + esc(log.error || '')) + '</span>' +
    '</div>' +
    '<div style="margin-bottom:12px"><div style="font-weight:600;margin-bottom:4px;color:#3b82f6">请求内容</div>' +
    '<div style="background:#f8fafc;border:1px solid #e2e8f0;border-radius:6px;padding:10px;font-size:12px;max-height:300px;overflow:auto">' + reqText + '</div></div>' +
    '<div><div style="font-weight:600;margin-bottom:4px;color:#10b981">响应内容</div>' +
    '<div style="background:#f0fdf4;border:1px solid #bbf7d0;border-radius:6px;padding:10px;font-size:12px;max-height:300px;overflow:auto">' + respText + '</div></div>' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">关闭</button></div>' +
    '</div></div>';
}
