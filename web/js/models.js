// models.js — 左右分栏模型管理

var _modelsData = null;
var _selectedModel = null;

async function loadKiroModels() {
  var data = await api('/api/model-accounts');
  if (!data) return;
  _modelsData = data;
  renderSidebar();
}

function renderSidebar() {
  var sidebar = document.getElementById('kiro-models-sidebar');
  var models = _modelsData.models || [];
  var mapping = _modelsData.mapping || {};
  var html = '';

  models.forEach(function(m, i) {
    var count = (mapping[m] || []).length;
    var isDefault = i === 0;
    var isActive = m === _selectedModel;
    html += '<div class="model-item' + (isActive ? ' active' : '') + '" onclick="selectModel(\'' + esc(m) + '\')">' +
      '<div style="display:flex;align-items:center;gap:6px;overflow:hidden;min-width:0">' +
      '<span class="model-name">' + esc(m) + '</span>' +
      (isDefault ? '<span class="model-default-tag">默认</span>' : '') +
      '</div>' +
      '<span class="model-count">' + (count > 0 ? count : '全部') + '</span>' +
      '</div>';
  });

  sidebar.innerHTML = html || '<div style="text-align:center;padding:24px 0;color:#94a3b8;font-size:13px">暂无模型<br><span style="font-size:12px">点击下方添加</span></div>';

  if (!_selectedModel && models.length > 0) {
    selectModel(models[0]);
  } else if (_selectedModel) {
    renderModelDetail(_selectedModel);
  }
}

function selectModel(model) {
  _selectedModel = model;
  renderSidebar();
  renderModelDetail(model);
}

function renderModelDetail(model) {
  if (!_modelsData) return;
  var mapping = _modelsData.mapping || {};
  var accounts = _modelsData.accounts || [];
  var assignedIDs = mapping[model] || [];
  var models = _modelsData.models || [];
  var idx = models.indexOf(model);
  var isDefault = idx === 0;

  var header = document.getElementById('model-detail-header');
  header.innerHTML = '<div style="margin-bottom:20px">' +
    '<div style="display:flex;justify-content:space-between;align-items:flex-start">' +
    '<div>' +
    '<div class="model-detail-title">' + esc(model) + '</div>' +
    '<div style="display:flex;gap:6px;margin-top:8px">' +
    (isDefault ? '<span class="badge badge-blue">默认模型</span>' : '') +
    '<span class="badge ' + (assignedIDs.length > 0 ? 'badge-green' : '') + '">' +
    (assignedIDs.length > 0 ? assignedIDs.length + ' / ' + accounts.length + ' 个账号' : '使用全部 ' + accounts.length + ' 个账号') + '</span>' +
    '</div></div>' +
    '<div style="display:flex;gap:6px">' +
    (isDefault ? '' : '<button class="btn btn-sm btn-outline" onclick="setDefaultModel(\'' + esc(model) + '\')">设为默认</button>') +
    '<button class="btn btn-sm btn-outline" style="color:#dc2626;border-color:#fecaca" onclick="removeKiroModel(\'' + esc(model) + '\')">删除</button>' +
    '</div></div></div>';

  var content = document.getElementById('model-detail-content');
  if (accounts.length === 0) {
    content.innerHTML = '<div style="text-align:center;padding:40px 0;color:#94a3b8">暂无账号</div>';
    return;
  }

  var html = '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px">' +
    '<span style="font-size:13px;color:#64748b;font-weight:500">账号分配</span>' +
    '<div style="display:flex;gap:6px">' +
    '<button class="btn btn-sm btn-outline" onclick="toggleAllAccounts(\'' + esc(model) + '\', true)">全选</button>' +
    '<button class="btn btn-sm btn-outline" onclick="toggleAllAccounts(\'' + esc(model) + '\', false)">全不选</button>' +
    '</div></div>';

  html += '<div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:8px">';
  accounts.forEach(function(acc) {
    var checked = assignedIDs.indexOf(acc.id) >= 0 ? 'checked' : '';
    html += '<label class="model-acc-item">' +
      '<input type="checkbox" data-model="' + esc(model) + '" data-account="' + esc(acc.id) + '" ' + checked +
      ' onchange="saveModelAccount(\'' + esc(model) + '\')">' +
      '<span class="model-acc-email">' + esc(acc.email || acc.id.substring(0, 24)) + '</span>' +
      '</label>';
  });
  html += '</div>';
  content.innerHTML = html;
}

function toggleAllAccounts(model, checked) {
  document.querySelectorAll('input[data-model="' + model + '"]').forEach(function(cb) {
    cb.checked = checked;
  });
  saveModelAccount(model);
}

function showAddModelInput() {
  document.getElementById('add-model-box').style.display = '';
  document.getElementById('new-model-input').focus();
}

function hideAddModelInput() {
  document.getElementById('add-model-box').style.display = 'none';
  document.getElementById('new-model-input').value = '';
}

async function addKiroModel() {
  var input = document.getElementById('new-model-input');
  var name = input.value.trim();
  if (!name) { toast('请输入模型名', 'error'); return; }

  var data = await api('/api/settings/kiro-models');
  var models = (data && data.models) || [];
  if (models.indexOf(name) >= 0) { toast('模型已存在', 'error'); return; }
  models.push(name);

  var r = await api('/api/settings/kiro-models', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ models: models })
  });
  if (r && r.ok) {
    toast('已添加: ' + name);
    hideAddModelInput();
    _selectedModel = name;
    loadKiroModels();
  } else {
    toast('添加失败', 'error');
  }
}

async function removeKiroModel(name) {
  if (!confirm('确定删除模型 "' + name + '"？')) return;
  var data = await api('/api/settings/kiro-models');
  var models = (data && data.models) || [];
  models = models.filter(function(m) { return m !== name; });

  var r = await api('/api/settings/kiro-models', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ models: models })
  });
  if (r && r.ok) {
    toast('已删除: ' + name);
    if (_selectedModel === name) _selectedModel = null;
    loadKiroModels();
  }
}

async function setDefaultModel(name) {
  var data = await api('/api/settings/kiro-models');
  var models = (data && data.models) || [];
  models = models.filter(function(m) { return m !== name; });
  models.unshift(name);

  var r = await api('/api/settings/kiro-models', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ models: models })
  });
  if (r && r.ok) {
    toast(name + ' 已设为默认');
    loadKiroModels();
  }
}

async function saveModelAccount(model) {
  var checkboxes = document.querySelectorAll('input[data-model="' + model + '"]');
  var ids = [];
  checkboxes.forEach(function(cb) {
    if (cb.checked) ids.push(cb.getAttribute('data-account'));
  });

  var r = await api('/api/model-accounts', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: model, account_ids: ids })
  });

  if (r && r.ok) {
    // Update local data and refresh sidebar counts
    if (!_modelsData.mapping) _modelsData.mapping = {};
    _modelsData.mapping[model] = ids;
    renderSidebar();
    toast(model + ': ' + (ids.length > 0 ? ids.length + ' 个账号' : '全部账号'));
  } else {
    toast('保存失败', 'error');
  }
}

function loadModelAccounts() { loadKiroModels(); }
