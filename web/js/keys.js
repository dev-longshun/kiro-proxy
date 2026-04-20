// keys.js
async function loadKeys() {
  const data = await api('/api/keys');
  if (!data) return;
  const tbody = document.getElementById('keys-tbody');
  tbody.innerHTML = '';
  (data.keys || []).forEach(k => {
    const statusBadge = k.enabled
      ? '<span class="badge badge-green">启用</span>'
      : '<span class="badge badge-red">禁用</span>';
    const lastUsed = k.last_used && k.last_used !== '0001-01-01T00:00:00Z'
      ? new Date(k.last_used).toLocaleString('zh-CN') : '-';
    tbody.innerHTML += '<tr>' +
      '<td>' + esc(k.name) + '</td>' +
      '<td class="mono"><span title="' + esc(k.key) + '">' + esc(k.key_masked) + '</span> <button class="copy-btn" onclick="copyText(\'' + esc(k.key) + '\')">📋</button></td>' +
      '<td>' + esc(k.description || '-') + '</td>' +
      '<td>' + statusBadge + '</td>' +
      '<td>' + (k.rate_limit || '无限') + '</td>' +
      '<td>' + (k.total_usage || 0) + '</td>' +
      '<td>' + lastUsed + '</td>' +
      '<td class="flex gap-2">' +
        '<button class="btn btn-sm btn-outline" onclick="toggleKey(\'' + esc(k.key) + '\')">' + (k.enabled ? '禁用' : '启用') + '</button>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + esc(k.key) + '\')">删除</button>' +
      '</td></tr>';
  });
}

async function toggleKey(key) {
  await api('/api/keys/' + encodeURIComponent(key) + '/toggle', { method: 'POST' });
  loadKeys();
}

async function deleteKey(key) {
  if (!confirm('确定删除此密钥?')) return;
  await api('/api/keys/' + encodeURIComponent(key), { method: 'DELETE' });
  toast('已删除');
  loadKeys();
}

function showCreateKeyModal() {
  document.getElementById('modal-container').innerHTML =
    '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
    '<div class="modal"><h3>创建 API 密钥</h3>' +
    '<label>名称 *</label><input type="text" id="m-kname" placeholder="例如: 我的应用">' +
    '<label>自定义密钥 (留空自动生成 sk-ant-api03-xxx 格式)</label><input type="text" id="m-kcustom" placeholder="可填任意字符串，如 my-secret-key-123">' +
    '<label>描述</label><input type="text" id="m-kdesc" placeholder="可选描述">' +
    '<label>速率限制 (请求/分钟, 0=无限)</label><input type="number" id="m-klimit" value="0" min="0">' +
    '<div class="modal-actions"><button class="btn btn-outline" onclick="closeModal()">取消</button>' +
    '<button class="btn btn-primary" onclick="createKey()">创建</button></div></div></div>';
}

async function createKey() {
  const name = document.getElementById('m-kname').value.trim();
  if (!name) { toast('名称不能为空', 'error'); return; }
  const r = await api('/api/keys', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      name: name,
      custom_key: document.getElementById('m-kcustom').value.trim(),
      description: document.getElementById('m-kdesc').value.trim(),
      rate_limit: parseInt(document.getElementById('m-klimit').value) || 0
    })
  });
  if (r && r.ok) {
    closeModal();
    document.getElementById('modal-container').innerHTML =
      '<div class="modal-overlay" onclick="if(event.target===this)closeModal()">' +
      '<div class="modal"><h3>密钥已创建</h3>' +
      '<p style="margin-bottom:12px;color:#94a3b8">请保存此密钥，它不会再次显示完整内容:</p>' +
      '<div style="background:#0f172a;padding:12px;border-radius:8px;word-break:break-all" class="mono">' + esc(r.key) + '</div>' +
      '<div class="modal-actions"><button class="btn btn-primary" onclick="copyText(\'' + esc(r.key) + '\');closeModal()">复制并关闭</button></div></div></div>';
    loadKeys();
  } else toast('创建失败', 'error');
}
