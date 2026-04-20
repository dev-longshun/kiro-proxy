// auto_reply.js — 自动回复规则管理

async function loadAutoReply() {
  const rules = await api('/api/auto-reply');
  if (!rules) return;

  const container = document.getElementById('auto-reply-list');
  if (!rules.length) {
    container.innerHTML = '<div class="text-muted" style="text-align:center;padding:16px">暂无自动回复规则</div>';
    return;
  }

  let html = '<table><thead><tr><th>关键词</th><th>回复内容</th><th>匹配方式</th><th>状态</th><th>操作</th></tr></thead><tbody>';
  rules.forEach(function(rule, i) {
    html += '<tr>';
    html += '<td><code>' + esc(rule.keyword) + '</code></td>';
    html += '<td style="max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(rule.reply) + '">' + esc(rule.reply) + '</td>';
    html += '<td>' + (rule.exact ? '精确' : '包含') + '</td>';
    html += '<td>' + (rule.enabled ? '<span class="badge badge-green">启用</span>' : '<span class="badge badge-gray">禁用</span>') + '</td>';
    html += '<td>';
    html += '<button class="btn btn-sm btn-outline" onclick="toggleAutoReply(' + i + ',' + !rule.enabled + ')" style="margin-right:4px">' + (rule.enabled ? '禁用' : '启用') + '</button>';
    html += '<button class="btn btn-sm btn-outline" style="color:#f87171" onclick="deleteAutoReply(' + i + ')">删除</button>';
    html += '</td>';
    html += '</tr>';
  });
  html += '</tbody></table>';
  container.innerHTML = html;
}

async function addAutoReply() {
  const keyword = document.getElementById('ar-keyword').value.trim();
  const reply = document.getElementById('ar-reply').value.trim();
  const exact = document.getElementById('ar-exact').value === 'true';

  if (!keyword || !reply) {
    toast('关键词和回复内容不能为空', 'error');
    return;
  }

  const data = await api('/api/auto-reply', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ keyword: keyword, reply: reply, exact: exact, enabled: true })
  });

  if (data && data.ok) {
    document.getElementById('ar-keyword').value = '';
    document.getElementById('ar-reply').value = '';
    toast('自动回复规则已添加');
    loadAutoReply();
  }
}

async function deleteAutoReply(index) {
  if (!confirm('确定删除这条规则？')) return;
  const data = await api('/api/auto-reply', {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ index: index })
  });
  if (data && data.ok) {
    toast('已删除');
    loadAutoReply();
  }
}

async function toggleAutoReply(index, enabled) {
  const rules = await api('/api/auto-reply');
  if (!rules || !rules[index]) return;
  rules[index].enabled = enabled;
  const data = await api('/api/auto-reply', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(rules)
  });
  if (data && data.ok) {
    toast(enabled ? '已启用' : '已禁用');
    loadAutoReply();
  }
}
