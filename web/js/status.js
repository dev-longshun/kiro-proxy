// status.js — kept for backward compatibility (status tab removed)
function endpointRow(label, url) {
  return '<div class="endpoint-box"><span class="endpoint-label">' + label +
    '</span><span class="endpoint-url">' + url +
    '</span><button class="copy-btn" onclick="copyText(\'' + url + '\')">📋</button></div>';
}

async function loadStatus() {
  // Status tab removed — this is now a no-op
  // Stats are shown in the accounts panel instead
}
