(function () {
  const esc = (v) => window.escapeHtml(String(v ?? ''));
  const fmt = (iso) => iso ? new Date(iso).toLocaleString() : '—';
  const state = { items: [] };
  async function load() {
    const data = await window.fetchDataWithAuth('/admin/channel-monitor'); state.items = data.items || [];
    const online = state.items.filter(x => x.status === 'online').length;
    document.getElementById('monitor-summary').innerHTML = `<div class="monitor-stat"><b>${state.items.length}</b><span>渠道</span></div><div class="monitor-stat good"><b>${online}</b><span>在线</span></div><div class="monitor-stat"><b>${state.items.filter(x => x.running).length}</b><span>检测中</span></div>`;
    document.getElementById('monitor-meta').textContent = `服务端时间 ${fmt(data.server_time)} · ${data.timezone || ''} · 自动检测每个渠道独立运行`;
    document.getElementById('monitor-rows').innerHTML = state.items.map(row).join('') || '<tr><td colspan="8">暂无渠道</td></tr>';
    document.querySelectorAll('[data-monitor-action]').forEach(btn => btn.addEventListener('click', () => action(btn.dataset.monitorAction, Number(btn.dataset.id), btn)));
  }
  function row(item) {
    const stat = item.stats || {}; const rate = stat.samples ? Math.round(stat.successes / stat.samples * 1000) / 10 : null; const last = stat.recent?.[0]; const schedule = item.schedule || {};
    return `<tr><td><b>${esc(item.name)}</b><small>#${item.id}</small></td><td><span class="monitor-status ${esc(item.status)}">${item.status === 'online' ? '在线' : item.status === 'offline' ? '离线' : item.status === 'disabled' ? '已禁用' : '未知'}</span></td><td>${esc(item.effective_model || '—')}</td><td>${rate == null ? '—' : rate + '%'}</td><td>${stat.average_latency ? Math.round(stat.average_latency * 1000) + ' ms' : '—'}</td><td>${last ? `${fmt(last.time)}<br><small>${esc(last.message || (last.status_code >= 200 && last.status_code < 300 ? '成功' : '失败'))}</small>` : '暂无记录'}</td><td><label class="monitor-toggle"><input type="checkbox" data-id="${item.id}" data-monitor-action="toggle" ${schedule.enabled ? 'checked' : ''}><span></span></label><div class="monitor-schedule"><input type="number" min="1" max="1440" value="${schedule.interval_minutes || 300}" data-id="${item.id}" data-monitor-action="interval"><input type="time" value="${esc(schedule.start_time || '00:00')}" data-id="${item.id}" data-monitor-action="start"><button class="btn btn-outline" data-id="${item.id}" data-monitor-action="save-schedule">保存时间</button></div></td><td><button class="btn btn-outline" data-monitor-action="run" data-id="${item.id}" ${item.running ? 'disabled' : ''}>立即检测</button></td></tr>`;
  }
  async function action(kind, id, button) {
    const item = state.items.find(x => x.id === id); if (!item) return;
    if (kind === 'run') await window.fetchDataWithAuth(`/admin/channels/${id}/monitor-run`, { method: 'POST' });
    if (kind === 'toggle') await saveSchedule(item, id, { enabled: button.checked });
    if (kind === 'save-schedule') { const row = button.closest('tr'); await saveSchedule(item, id, { interval_minutes: Number(row.querySelector('[data-monitor-action="interval"]').value), start_time: row.querySelector('[data-monitor-action="start"]').value }); }
    await load();
  }
  async function saveSchedule(item, id, patch) { await window.fetchDataWithAuth(`/admin/channels/${id}/monitor-schedule`, { method: 'PUT', headers: {'Content-Type':'application/json'}, body: JSON.stringify({...item.schedule, ...patch}) }); }
  window.initPageBootstrap({ topbarKey: 'monitor', run: async () => { document.getElementById('refresh').onclick = load; await load(); setInterval(load, 30000); } });
})();
