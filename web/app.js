document.addEventListener('DOMContentLoaded', () => {
    // ─── Toast ───
    function toast(msg, type = 'info') {
        const t = document.createElement('div');
        t.className = `toast ${type}`;
        t.textContent = msg;
        document.getElementById('toast-container').appendChild(t);
        setTimeout(() => { t.classList.add('fadeout'); setTimeout(() => t.remove(), 300); }, 3000);
    }

    // ─── WebSocket ───
    let ws = null;
    const wsStatus = document.getElementById('ws-status');
    const wsIndicator = wsStatus.querySelector('.indicator');
    const wsText = wsStatus.querySelector('.text');

    function connectWS() {
        const proto = location.protocol === 'https:' ? 'wss' : 'ws';
        ws = new WebSocket(`${proto}://${location.host}/ws`);
        ws.onopen = () => {
            wsIndicator.classList.add('connected');
            wsIndicator.classList.remove('pulsing');
            wsText.textContent = 'Connected';
        };
        ws.onclose = () => {
            wsIndicator.classList.remove('connected');
            wsIndicator.classList.add('pulsing');
            wsText.textContent = 'Reconnecting...';
            setTimeout(connectWS, 2000);
        };
        ws.onmessage = (e) => {
            try { handleUpdate(JSON.parse(e.data)); } catch (err) { console.error(err); }
        };
    }

    // ─── Update Handler ───
    function handleUpdate(data) {
        if (data.system) {
            document.getElementById('stat-cpu').textContent = (data.system.cpu_load || 0).toFixed(1) + '%';
            const ramMB = Math.round(data.system.ram_used_mb || 0);
            document.getElementById('sys-ram').textContent = ramMB + ' MB';
        }

        if (data.switcher) {
            const sw = data.switcher;
            const badge = document.getElementById('main-status-badge');
            badge.dataset.state = sw.state;
            badge.textContent = sw.state.toUpperCase().replace('_', ' ');

            const card = document.getElementById('switcher-card');
            card.style.borderLeftColor = sw.state === 'live' ? 'var(--live)' : (sw.state === 'fallback' || sw.state === 'backup') ? 'var(--fallback)' : 'var(--starting)';
            card.style.borderLeftWidth = '3px';

            const mbps = ((sw.input_bitrate_kbps || 0) / 1000).toFixed(2);
            document.getElementById('stat-bitrate').innerHTML = `${mbps} <small>Mbps</small>`;
            document.getElementById('stat-uptime').textContent = sw.uptime || '—';
            document.getElementById('stat-packets-rx').textContent = formatNum(sw.packets_received);
            document.getElementById('stat-packets-fwd').textContent = formatNum(sw.packets_forwarded);
            document.getElementById('stat-pids').textContent = `${sw.video_pid || '—'} / ${sw.audio_pid || '—'}`;
            document.getElementById('stat-switch-count').textContent = sw.switch_count || 0;
            document.getElementById('stat-last-switch').textContent = sw.last_switch_time ? new Date(sw.last_switch_time).toLocaleTimeString() : '—';

            const win = document.getElementById('chart-window').value || '5m';
            if (sw.bitrate_history && sw.bitrate_history[win]) {
                drawBitrateChart(sw.bitrate_history[win], win);
            }
            if (sw.recent_events) renderHistory(sw.recent_events);
        }

        if (data.outputs) {
            renderOutputs(data.outputs);
            updateOverlaySelect(data.outputs);
            
            // Sync box position from active selection
            if (overlaySelect && overlaySelect.value) {
                const activeOut = data.outputs.find(o => o.id === overlaySelect.value);
                if (activeOut) {
                    positionVisualBox(activeOut.overlay_x || 10, activeOut.overlay_y || 10);
                }
            }
        }
        if (data.audio) updateVUMeter(data.audio);
        if (data.recording !== undefined) updateRecordingUI(data.recording);
    }

    function formatNum(n) {
        if (!n) return '0';
        if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
        if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
        return n.toString();
    }

    // ─── Bitrate Chart (uPlot Integration) (Stage 7) ───
    const chartContainer = document.getElementById('bitrate-chart');
    let uplotInstance = null;

    function initUPlot(dataX, dataY) {
        if (uplotInstance) {
            uplotInstance.destroy();
        }
        
        const data = [dataX, dataY];
        const rect = chartContainer.getBoundingClientRect();
        
        const opts = {
            width: rect.width || 400,
            height: 120,
            cursor: {
                show: true
            },
            select: {
                show: false
            },
            legend: {
                show: false
            },
            scales: {
                x: {
                    time: true
                },
                y: {
                    auto: false,
                    range: (self, min, max) => {
                        let scaleMax = max * 1.2;
                        if (scaleMax < 6) scaleMax = 6;
                        if (scaleMax > 15) scaleMax = 15;
                        return [0, scaleMax];
                    }
                }
            },
            axes: [
                {
                    show: true,
                    stroke: "#64748b",
                    grid: {
                        show: true,
                        stroke: "rgba(255, 255, 255, 0.02)"
                    },
                    ticks: {
                        show: false
                    }
                },
                {
                    show: true,
                    stroke: "#64748b",
                    grid: {
                        show: true,
                        stroke: "rgba(255, 255, 255, 0.02)"
                    },
                    values: (self, splits) => splits.map(v => v.toFixed(1) + "M"),
                    ticks: {
                        show: false
                    }
                }
            ],
            series: [
                {},
                {
                    show: true,
                    spanGaps: false,
                    label: "Bitrate",
                    value: (self, rawValue) => rawValue == null ? null : rawValue.toFixed(2) + " Mbps",
                    stroke: "#00f0ff",
                    width: 2,
                    fill: "rgba(0, 240, 255, 0.08)",
                    points: {
                        show: false
                    }
                }
            ]
        };

        uplotInstance = new uPlot(opts, data, chartContainer);
    }

    function drawBitrateChart(history, win = '5m') {
        if (!history || history.length === 0) return;
        
        // Convert history (which is in Kbps) to Mbps
        const dataY = history.map(v => v / 1000);
        
        // Generate historical timestamps
        const nowMs = Date.now();
        let totalMs = 300000; // 5m
        if (win === '1m') totalMs = 60000;
        else if (win === '30m') totalMs = 1800000;
        else if (win === '1h') totalMs = 3600000;
        else if (win === '5h') totalMs = 18000000;
        else if (win === '10h') totalMs = 36000000;

        const interval = totalMs / Math.max(1, history.length - 1);
        const dataX = [];
        for (let i = 0; i < history.length; i++) {
            dataX.push(Math.round((nowMs - totalMs + (i * interval)) / 1000));
        }

        // Initialize or update uPlot
        if (!uplotInstance) {
            initUPlot(dataX, dataY);
        } else {
            const containerWidth = chartContainer.getBoundingClientRect().width;
            if (containerWidth > 0 && uplotInstance.width !== containerWidth) {
                uplotInstance.setSize({ width: containerWidth, height: 120 });
            }
            uplotInstance.setData([dataX, dataY]);
        }
    }

    // ─── VU Meter ───
    const vuLeft = document.getElementById('vu-left');
    const vuRight = document.getElementById('vu-right');
    const vuDbValue = document.getElementById('vu-db-value');

    function updateVUMeter(audio) {
        const toP = (db) => Math.max(0, Math.min(100, ((db + 60) / 60) * 100));
        vuLeft.style.width = toP(audio.peak_left_db) + '%';
        vuRight.style.width = toP(audio.peak_right_db) + '%';
        const avg = (audio.peak_left_db + audio.peak_right_db) / 2;
        vuDbValue.textContent = avg <= -60 ? '-∞ dB' : avg.toFixed(1) + ' dB';
    }

    // ─── Recording ───
    const btnRec = document.getElementById('btn-rec');
    let isRecording = false;

    function updateRecordingUI(rec) {
        isRecording = rec.active;
        const recStatus = document.getElementById('rec-status');
        if (rec.active) {
            btnRec.className = 'btn btn-rec recording';
            btnRec.textContent = '⏹ STOP REC';
            recStatus.textContent = `🔴 REC ${rec.duration || ''} — ${rec.size_mb || 0} MB`;
            recStatus.style.display = 'inline';
        } else {
            btnRec.className = 'btn btn-rec';
            btnRec.textContent = '⏺ REC';
            recStatus.textContent = '';
            recStatus.style.display = 'none';
        }
    }

    btnRec.addEventListener('click', async () => {
        const action = isRecording ? 'stop' : 'start';
        try {
            const res = await fetch(`/api/recording?action=${action}`, { method: 'POST' });
            const d = await res.json();
            if (res.ok) toast(d.status, 'success'); else toast(d.error || 'Error', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // Recordings modal
    document.getElementById('btn-show-recordings').addEventListener('click', () => { openModal(document.getElementById('recordings-modal')); loadRecordings(); });
    document.getElementById('btn-refresh-recs').addEventListener('click', loadRecordings);

    async function loadRecordings() {
        const list = document.getElementById('recordings-list');
        list.innerHTML = '<p class="text-muted">Loading...</p>';
        try {
            const res = await fetch('/api/recordings');
            const data = await res.json();
            const recs = data.recordings;
            if (!recs || recs.length === 0) { list.innerHTML = '<p class="text-muted">No recordings yet.</p>'; return; }
            list.innerHTML = recs.map(r => {
                // Use data attributes instead of inline onclick to prevent XSS
                const safeName = r.name.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
                return `<div class="recording-item">
                    <span class="rec-name" title="${safeName}">🎬 ${safeName}</span>
                    <span class="rec-size">${r.size_mb} MB</span>
                    <span class="rec-date">${new Date(r.mod_time).toLocaleString()}</span>
                    <div class="rec-actions">
                        <button class="btn-icon rec-download" data-name="${safeName}" title="Download">⬇</button>
                        <button class="btn-icon rec-rename" data-name="${safeName}" title="Rename">✏</button>
                        <button class="btn-icon btn-delete rec-delete" data-name="${safeName}" title="Delete">🗑</button>
                    </div>
                </div>`;
            }).join('');
            // Attach event listeners safely (no inline JS)
            list.querySelectorAll('.rec-download').forEach(btn => {
                btn.addEventListener('click', () => { location.href = '/api/recordings/' + encodeURIComponent(btn.dataset.name); });
            });
            list.querySelectorAll('.rec-rename').forEach(btn => {
                btn.addEventListener('click', () => renameRec(btn.dataset.name));
            });
            list.querySelectorAll('.rec-delete').forEach(btn => {
                btn.addEventListener('click', () => deleteRec(btn.dataset.name));
            });
        } catch (e) { list.innerHTML = `<p class="text-muted">Error: ${e.message}</p>`; }
    }

    async function renameRec(name) {
        const newName = prompt('New filename:', name);
        if (!newName || newName === name) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ new_name: newName }) });
            if (res.ok) { toast('Renamed!', 'success'); loadRecordings(); }
            else toast('Rename failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteRec(name) {
        if (!confirm(`Delete ${name}?`)) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, { method: 'DELETE' });
            if (res.ok) { toast('Deleted!', 'success'); loadRecordings(); }
            else toast('Delete failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    }

    // ─── Switch History ───
    function renderHistory(events) {
        const list = document.getElementById('history-list');
        if (!events || events.length === 0) { list.innerHTML = '<p class="text-muted">No events yet.</p>'; return; }
        list.innerHTML = events.slice().reverse().map(ev => {
            const time = ev.time ? new Date(ev.time).toLocaleTimeString() : '';
            return `<div class="history-event">
                <span class="history-dot ${ev.to}"></span>
                <span class="history-time">${time}</span>
                <span>${ev.from} → ${ev.to}</span>
                <span class="history-reason">${ev.reason}</span>
            </div>`;
        }).join('');
    }

    // ─── Outputs ───
    let editingOutputId = null;
    let currentLogsId = null;
    const template = document.getElementById('output-card-template');

    function renderOutputs(outputs) {
        const container = document.getElementById('outputs-container');
        if (!outputs || outputs.length === 0) { container.innerHTML = '<div class="empty-state"><p>No outputs configured.</p></div>'; return; }
        
        // Remove empty state if it exists
        const emptyState = container.querySelector('.empty-state');
        if (emptyState) emptyState.remove();

        const existing = new Map();
        container.querySelectorAll('.output-card').forEach(c => existing.set(c.dataset.id, c));
        outputs.forEach(out => {
            let card = existing.get(out.id);
            if (!card) {
                const clone = template.content.cloneNode(true);
                card = clone.querySelector('.output-card');
                card.dataset.id = out.id;
                setupOutputCard(card, out.id);
                container.appendChild(card);
            }
            updateOutputCard(card, out);
            existing.delete(out.id);
        });
        existing.forEach(c => c.remove());
    }

    function setupOutputCard(card, id) {
        card.querySelector('.btn-toggle').addEventListener('click', () => toggleOutput(id));
        card.querySelector('.btn-delete').addEventListener('click', () => deleteOutput(id));
        card.querySelector('.btn-edit').addEventListener('click', () => editOutput(id));
        card.querySelector('.btn-logs').addEventListener('click', () => showLogs(id));
        card.querySelector('.btn-unlock').addEventListener('click', (e) => {
            const locked = card.querySelector('.locked-controls');
            if (locked.style.display === 'none') {
                locked.style.display = 'flex';
                e.target.textContent = '🔓';
                e.target.style.opacity = '1';
            } else {
                locked.style.display = 'none';
                e.target.textContent = '🔒';
                e.target.style.opacity = '0.7';
            }
        });
    }

    function updateOutputCard(card, out) {
        card.querySelector('.output-name').textContent = out.name || out.id;
        card.classList.toggle('running', out.running);
        const s = card.querySelector('.output-status');
        s.textContent = out.running ? 'Running' : 'Stopped';
        s.className = `detail-value output-status ${out.running ? 'running' : 'stopped'}`;
        card.querySelector('.output-codec').textContent = out.codec === 'h264' ? 'H.264' : 'H.265';
        card.querySelector('.output-bitrate').textContent = out.bitrate ? out.bitrate + 'k' : 'Copy';
        card.querySelector('.output-uptime').textContent = out.uptime || '—';
        const e = card.querySelector('.output-error');
        if (out.last_error) { e.style.display = 'block'; e.textContent = out.last_error; } else e.style.display = 'none';
        const t = card.querySelector('.btn-toggle');
        t.title = out.running ? 'Stop' : 'Start';
        t.textContent = out.running ? '⏸' : '▶';
    }

    async function toggleOutput(id) {
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        const running = card && card.classList.contains('running');
        try {
            const res = await fetch(`/api/outputs/${id}/${running ? 'stop' : 'start'}`, { method: 'POST' });
            if (res.ok) toast(`Output ${running ? 'stopped' : 'started'}`, 'success');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteOutput(id) {
        if (!confirm('Delete this output?')) return;
        try { await fetch(`/api/outputs/${id}`, { method: 'DELETE' }); toast('Deleted', 'success'); } catch (e) { toast(e.message, 'error'); }
    }

    function editOutput(id) {
        editingOutputId = id;
        fetch('/api/outputs').then(r => r.json()).then(outputs => {
            const out = outputs.find(o => o.id === id);
            if (!out) return;
            document.getElementById('out-name').value = out.name || '';
            document.getElementById('out-url').value = out.url || '';
            document.getElementById('out-key').value = out.stream_key || '';
            document.getElementById('out-codec').value = out.codec || 'h265';
            document.getElementById('out-bitrate').value = out.bitrate || 6000;
            document.getElementById('out-preset').value = out.preset || 'ultrafast';
            document.getElementById('out-overlay').checked = out.overlay_enabled || false;
            toggleH264Options();
            document.querySelector('#add-modal .modal-header h3').textContent = 'Edit Output';
            openModal(document.getElementById('add-modal'));
        });
    }

    async function showLogs(id) {
        currentLogsId = id;
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        document.getElementById('logs-output-name').textContent = card?.querySelector('.output-name')?.textContent || id;
        document.getElementById('logs-content').textContent = 'Loading...';
        openModal(document.getElementById('logs-modal'));
        refreshLogs();
    }

    async function refreshLogs() {
        if (!currentLogsId) return;
        try {
            const r = await fetch(`/api/outputs/${currentLogsId}/logs`);
            const d = await r.json();
            document.getElementById('logs-content').textContent = d.logs?.join('\n') || 'No logs.';
        } catch (e) { document.getElementById('logs-content').textContent = e.message; }
    }
    document.getElementById('btn-refresh-logs').addEventListener('click', refreshLogs);

    // ─── Config (Stage 7) ───
    const configToggle = document.getElementById('config-toggle');
    const configBody = document.getElementById('config-body');
    
    // Load config collapse state (Stage 7)
    if (localStorage.getItem('config_expanded') === 'true') {
        configBody.classList.remove('collapsed');
        configToggle.querySelector('.collapse-icon').classList.add('open');
    }
    
    configToggle.addEventListener('click', () => {
        configBody.classList.toggle('collapsed');
        const expanded = !configBody.classList.contains('collapsed');
        configToggle.querySelector('.collapse-icon').classList.toggle('open');
        localStorage.setItem('config_expanded', expanded);
    });

    const historyToggle = document.getElementById('history-toggle');
    const historyBody = document.getElementById('history-body');
    
    // Load history collapse state (Stage 7)
    if (localStorage.getItem('history_expanded') === 'true') {
        historyBody.classList.remove('collapsed');
        historyToggle.querySelector('.collapse-icon').classList.add('open');
    }
    
    historyToggle.addEventListener('click', () => {
        historyBody.classList.toggle('collapsed');
        const expanded = !historyBody.classList.contains('collapsed');
        historyToggle.querySelector('.collapse-icon').classList.toggle('open');
        localStorage.setItem('history_expanded', expanded);
    });

    async function fetchConfig() {
        try {
            const r = await fetch('/api/config');
            if (!r.ok) return;
            const c = await r.json();
            document.getElementById('cfg-srt-addr').value = c.srt_addr || '';
            document.getElementById('cfg-srt-mode').value = c.srt_mode || 'caller';
            document.getElementById('cfg-srt-timeout').value = c.srt_timeout || 2000;
            document.getElementById('cfg-min-bitrate').value = c.min_bitrate_kbps || 0;
            document.getElementById('cfg-hysteresis').value = c.bitrate_hysteresis_seconds || 5;
            document.getElementById('cfg-stats-url').value = c.stats_url || '';
            document.getElementById('cfg-fallback-path').value = c.fallback_path || '';
        } catch (e) { /* ok */ }
    }

    document.getElementById('btn-save-config').addEventListener('click', async () => {
        const cfg = {
            srt_addr: document.getElementById('cfg-srt-addr').value.trim(),
            srt_mode: document.getElementById('cfg-srt-mode').value,
            srt_timeout: parseInt(document.getElementById('cfg-srt-timeout').value, 10),
            stats_url: document.getElementById('cfg-stats-url').value.trim(),
            fallback_path: document.getElementById('cfg-fallback-path').value.trim(),
            min_bitrate_kbps: parseInt(document.getElementById('cfg-min-bitrate').value, 10) || 0,
            bitrate_hysteresis_seconds: parseInt(document.getElementById('cfg-hysteresis').value, 10) || 5,
        };
        try {
            const r = await fetch('/api/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(cfg) });
            if (r.ok) toast('Config saved!', 'success'); else toast('Failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Add/Edit Output ───
    const codecSelect = document.getElementById('out-codec');
    function toggleH264Options() { document.getElementById('h264-options').style.display = codecSelect.value === 'h264' ? 'block' : 'none'; }
    codecSelect.addEventListener('change', toggleH264Options);

    document.getElementById('btn-add-output').addEventListener('click', () => {
        editingOutputId = null;
        document.getElementById('form-add-output').reset();
        toggleH264Options();
        document.querySelector('#add-modal .modal-header h3').textContent = 'Add Output';
        openModal(document.getElementById('add-modal'));
    });

    document.getElementById('form-add-output').addEventListener('submit', async (e) => {
        e.preventDefault();
        const config = {
            name: document.getElementById('out-name').value.trim(),
            url: document.getElementById('out-url').value.trim(),
            stream_key: document.getElementById('out-key').value.trim(),
            codec: document.getElementById('out-codec').value,
            bitrate: parseInt(document.getElementById('out-bitrate').value, 10) || 0,
            preset: document.getElementById('out-preset').value,
            overlay_enabled: document.getElementById('out-overlay').checked,
        };
        const method = editingOutputId ? 'PUT' : 'POST';
        const url = editingOutputId ? `/api/outputs/${editingOutputId}` : '/api/outputs';
        try {
            const r = await fetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(config) });
            if (r.ok) { toast(editingOutputId ? 'Updated!' : 'Added!', 'success'); closeModal(document.getElementById('add-modal')); }
            else toast('Failed', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Quick Actions ───
    async function quickAction(action) {
        try { const r = await fetch(`/api/actions/${action}`, { method: 'POST' }); const d = await r.json(); toast(d.status || 'Done', 'success'); }
        catch (e) { toast(e.message, 'error'); }
    }
    document.getElementById('act-restart-srt').addEventListener('click', () => quickAction('restart-srt'));
    document.getElementById('act-restart-fallback').addEventListener('click', () => quickAction('restart-fallback'));
    document.getElementById('act-restart-outputs').addEventListener('click', () => quickAction('restart-outputs'));
    document.getElementById('act-restart-all').addEventListener('click', () => { if (confirm('Restart ALL?')) quickAction('restart-all'); });

    // ─── Assets & Fallback Scenes (Stage 4) ───
    document.getElementById('btn-open-assets').addEventListener('click', () => {
        openModal(document.getElementById('assets-modal'));
        loadScenes();
    });

    async function loadScenes() {
        const scenesList = document.getElementById('scenes-list');
        scenesList.innerHTML = '<p class="text-muted">Loading fallback scenes...</p>';
        try {
            const res = await fetch('/api/scenes');
            if (!res.ok) throw new Error('Failed to load scenes');
            const data = await res.json();
            
            scenesList.innerHTML = '';
            if (!data.scenes || data.scenes.length === 0) {
                scenesList.innerHTML = '<p class="text-muted">No scenes configured.</p>';
                return;
            }

            data.scenes.forEach(s => {
                const isActive = s.id === data.active_id;
                const row = document.createElement('div');
                row.className = 'scene-row';
                row.style = `display: flex; align-items: center; justify-content: space-between; padding: 0.6rem; background: rgba(255,255,255,0.02); border: 1px solid ${isActive ? 'var(--live)' : 'var(--border-color)'}; border-radius: var(--radius-sm); margin-bottom: 0.4rem;`;
                
                const typeLabel = s.type === 'image' ? '🖼️ IMG' : '🎥 VID';
                
                row.innerHTML = `
                    <div style="display: flex; flex-direction: column;">
                        <span style="font-weight: 600; font-size: 0.85rem; color: ${isActive ? 'var(--live)' : 'var(--text-primary)'};">${s.name} ${isActive ? '🟢 Active' : ''}</span>
                        <span class="text-muted" style="font-size: 0.7rem; color: var(--text-muted); margin-top: 0.15rem;">Type: ${typeLabel} | Path: ${s.file_path.split('/').pop()}</span>
                    </div>
                    <div style="display: flex; gap: 0.4rem; align-items: center;">
                        ${!isActive ? `<button class="btn btn-xs btn-primary btn-activate-scene" data-id="${s.id}" style="min-height:28px;">Activate</button>` : `<span class="status-badge" data-state="live" style="font-size:0.6rem; padding:0.15rem 0.4rem;">Active</span>`}
                        ${s.id !== 'default' ? `<button class="btn btn-xs btn-secondary btn-delete-scene" data-id="${s.id}" style="color:var(--error); border-color:rgba(255,68,68,0.2); min-height:28px;">Delete</button>` : ''}
                    </div>
                `;
                scenesList.appendChild(row);
            });

            // Add event listeners to active/delete buttons
            document.querySelectorAll('.btn-activate-scene').forEach(btn => {
                btn.addEventListener('click', () => activateScene(btn.dataset.id));
            });
            document.querySelectorAll('.btn-delete-scene').forEach(btn => {
                btn.addEventListener('click', () => deleteScene(btn.dataset.id));
            });

        } catch (e) {
            scenesList.innerHTML = `<p style="color:var(--error); font-size:0.8rem;">Error: ${e.message}</p>`;
        }
    }

    async function activateScene(id) {
        try {
            const res = await fetch('/api/scenes/activate', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id })
            });
            if (res.ok) {
                toast('Scene activated!', 'success');
                loadScenes();
                fetchConfig(); // Update main config fallback path on display
            } else {
                toast('Failed to activate scene', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteScene(id) {
        if (!confirm('Are you sure you want to delete this scene?')) return;
        try {
            const res = await fetch('/api/scenes/delete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id })
            });
            if (res.ok) {
                toast('Scene deleted!', 'success');
                loadScenes();
            } else {
                toast('Failed to delete scene', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }

    // Add Scene
    document.getElementById('btn-add-scene').addEventListener('click', async () => {
        const fileInput = document.getElementById('new-scene-file');
        const nameInput = document.getElementById('new-scene-name');
        const btn = document.getElementById('btn-add-scene');

        if (!fileInput.files.length) {
            toast('Select a media file first', 'error');
            return;
        }

        const name = nameInput.value.trim();
        const fd = new FormData();
        fd.append('file', fileInput.files[0]);
        if (name) fd.append('name', name);

        btn.disabled = true;
        btn.textContent = 'Uploading...';
        
        try {
            const res = await fetch('/api/upload/scene', {
                method: 'POST',
                body: fd
            });
            if (res.ok) {
                toast('Scene uploaded! Images will pre-render in background.', 'success');
                fileInput.value = '';
                nameInput.value = '';
                loadScenes();
            } else {
                const data = await res.json();
                toast(data.error || 'Failed to upload scene', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
        
        btn.disabled = false;
        btn.textContent = '➕ Add Scene';
    });

    // Original Watermark upload (Stage 2)
    async function handleWatermarkUpload() {
        const input = document.getElementById('file-watermark');
        const btn = document.getElementById('btn-upload-watermark');
        if (!input.files.length) { toast('Select a watermark file', 'error'); return; }
        const fd = new FormData(); fd.append('file', input.files[0]);
        btn.disabled = true; btn.textContent = 'Uploading...';
        try { 
            const r = await fetch('/api/upload/watermark', { method: 'POST', body: fd }); 
            r.ok ? toast('Watermark uploaded!', 'success') : toast('Upload failed', 'error'); 
        }
        catch (e) { toast(e.message, 'error'); }
        btn.disabled = false; btn.textContent = 'Upload Watermark';
    }
    document.getElementById('btn-upload-watermark').addEventListener('click', handleWatermarkUpload);

    // ─── Preview ───
    const previewImg = document.getElementById('preview-snapshot');
    let previewInterval = null;
    let previewFps = 2;

    function stopPreview() { if (previewInterval) { clearInterval(previewInterval); previewInterval = null; } }
    function startPreview() {
        stopPreview();
        if (previewFps <= 0) return;
        const ms = Math.max(200, Math.floor(1000 / previewFps));
        const load = () => { const img = new Image(); img.onload = () => { previewImg.src = img.src; }; img.src = '/api/preview/frame?t=' + Date.now(); };
        load();
        previewInterval = setInterval(load, ms);
    }

    document.getElementById('btn-apply-preview').addEventListener('click', async () => {
        const fps = parseInt(document.getElementById('preview-fps').value, 10);
        const [w, h] = document.getElementById('preview-res').value.split('x').map(Number);
        previewFps = fps;
        try {
            await fetch(`/api/preview/settings?fps=${fps}&w=${w}&h=${h}`, { method: 'PUT' });
            if (fps === 0) { stopPreview(); toast('Preview off', 'info'); }
            else { startPreview(); toast(`Preview: ${fps}fps ${w}x${h}`, 'success'); }
        } catch (e) { toast(e.message, 'error'); }
    });
    startPreview();

    // ─── Bbox Receiver ───
    async function updateBboxStatus() {
        try {
            const res = await fetch('/api/bbox/status');
            const data = await res.json();
            const badge = document.getElementById('bbox-status-badge');
            badge.textContent = (data.status || 'unknown').toUpperCase();
            if (data.status === 'running') {
                badge.style.background = 'rgba(0,255,136,0.15)';
                badge.style.color = 'var(--live)';
            } else {
                badge.style.background = 'rgba(255,68,68,0.15)';
                badge.style.color = 'var(--error)';
            }
        } catch (e) { /* ignore */ }
    }
    setInterval(updateBboxStatus, 3000);
    updateBboxStatus();

    async function bboxAction(action) {
        try {
            const res = await fetch(`/api/bbox/action?action=${action}`, { method: 'POST' });
            if (res.ok) { toast(`Bbox ${action} successful`, 'success'); setTimeout(updateBboxStatus, 1000); }
            else { const d = await res.json(); toast(`Error: ${d.error}`, 'error'); }
        } catch (e) { toast(e.message, 'error'); }
    }
    document.getElementById('btn-bbox-start').addEventListener('click', () => bboxAction('start'));
    document.getElementById('btn-bbox-stop').addEventListener('click', () => bboxAction('stop'));
    document.getElementById('btn-bbox-restart').addEventListener('click', () => bboxAction('restart'));

    // Bbox Logs
    async function showBboxLogs() {
        openModal(document.getElementById('bbox-logs-modal'));
        document.getElementById('bbox-logs-content').textContent = 'Loading...';
        try {
            const res = await fetch('/api/bbox/logs');
            const data = await res.json();
            document.getElementById('bbox-logs-content').textContent = data.logs || 'No logs.';
        } catch (e) { document.getElementById('bbox-logs-content').textContent = e.message; }
    }
    document.getElementById('btn-bbox-logs').addEventListener('click', showBboxLogs);
    document.getElementById('btn-refresh-bbox-logs').addEventListener('click', showBboxLogs);

    // Bbox Editor
    let currentBboxEditorMode = '';
    async function openBboxEditor(mode) { // 'config' or 'compose'
        currentBboxEditorMode = mode;
        const title = mode === 'config' ? 'Edit config.json' : 'Edit docker-compose.yml';
        document.getElementById('bbox-editor-title').textContent = title;
        document.getElementById('bbox-editor-textarea').value = 'Loading...';
        openModal(document.getElementById('bbox-editor-modal'));
        
        try {
            const res = await fetch(`/api/bbox/${mode}`);
            const data = await res.json();
            document.getElementById('bbox-editor-textarea').value = data.content || '';
        } catch (e) { document.getElementById('bbox-editor-textarea').value = `Error loading: ${e.message}`; }
    }
    
    document.getElementById('btn-bbox-edit-config').addEventListener('click', () => openBboxEditor('config'));
    document.getElementById('btn-bbox-edit-compose').addEventListener('click', () => openBboxEditor('compose'));
    
    document.getElementById('btn-bbox-save-editor').addEventListener('click', async () => {
        const content = document.getElementById('bbox-editor-textarea').value;
        try {
            const res = await fetch(`/api/bbox/${currentBboxEditorMode}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ content })
            });
            if (res.ok) { 
                toast('Saved successfully!', 'success'); 
                closeModal(document.getElementById('bbox-editor-modal')); 
                if (currentBboxEditorMode === 'compose') toast('Restart Bbox to apply compose changes', 'info');
            }
            else { const d = await res.json(); toast(`Error: ${d.error}`, 'error'); }
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Modal Helpers ───
    function openModal(m) { if (m) m.classList.add('open'); }
    function closeModal(m) { if (m) m.classList.remove('open'); editingOutputId = null; currentLogsId = null; }

    document.querySelectorAll('.btn-close-modal, .btn-cancel-modal').forEach(b => b.addEventListener('click', () => { const m = b.closest('.modal-backdrop'); if (m) closeModal(m); }));
    document.querySelectorAll('.modal-backdrop').forEach(b => b.addEventListener('click', e => { if (e.target === b) closeModal(b); }));
    document.addEventListener('keydown', e => { if (e.key === 'Escape') document.querySelectorAll('.modal-backdrop.open').forEach(closeModal); });

    // ─── Dynamic Overlay (Watermark) Positioner ───
    const overlayBox = document.getElementById('preview-overlay-box');
    const overlayCoords = overlayBox ? overlayBox.querySelector('.overlay-coords') : null;
    const overlaySelect = document.getElementById('preview-overlay-output');
    const previewContainer = document.querySelector('.preview-container');
    const presetsBar = document.getElementById('layout-presets-bar');

    let isDragging = false;
    let startX = 0;
    let startY = 0;
    let initialLeft = 0;
    let initialTop = 0;
    let lastSentTime = 0;
    const throttleMs = 60; // 60ms throttling for smooth dragging without overloading the server

    // Mapped logical resolution of FFmpeg output
    const targetW = 1920;
    const targetH = 1080;

    let transitionInterval = null;

    function animateOverlayTo(targetX, targetY) {
        if (!overlaySelect || !overlaySelect.value) return;

        if (transitionInterval) {
            clearInterval(transitionInterval);
            transitionInterval = null;
        }

        const duration = 400; // ms
        const frameRate = 30; // fps
        const totalFrames = (duration / 1000) * frameRate;
        const intervalMs = 1000 / frameRate;

        let curX = 10;
        let curY = 10;
        
        const coordText = overlayCoords ? overlayCoords.textContent : "10, 10";
        const parts = coordText.split(",");
        if (parts.length === 2) {
            curX = parseInt(parts[0].trim(), 10) || 10;
            curY = parseInt(parts[1].trim(), 10) || 10;
        }

        let frame = 0;

        transitionInterval = setInterval(() => {
            frame++;
            if (frame >= totalFrames) {
                clearInterval(transitionInterval);
                transitionInterval = null;
                sendMoveUpdate(targetX, targetY);
                positionVisualBox(targetX, targetY);
                return;
            }

            const t = frame / totalFrames;
            const easeOut = 1 - Math.pow(1 - t, 3); // cubic ease-out

            const nextX = Math.round(curX + (targetX - curX) * easeOut);
            const nextY = Math.round(curY + (targetY - curY) * easeOut);

            if (!previewContainer) return;
            const containerRect = previewContainer.getBoundingClientRect();
            const xDOM = (nextX / targetW) * containerRect.width;
            const yDOM = (nextY / targetH) * containerRect.height;
            
            overlayBox.style.left = `${xDOM}px`;
            overlayBox.style.top = `${yDOM}px`;
            
            if (overlayCoords) {
                overlayCoords.textContent = `${nextX}, ${nextY}`;
            }

            sendMoveUpdate(nextX, nextY);
        }, intervalMs);
    }

    // Update overlay select dropdown with outputs that have overlay enabled (even if stopped)
    function updateOverlaySelect(outputs) {
        if (!overlaySelect) return;
        const currentVal = overlaySelect.value;
        overlaySelect.innerHTML = '<option value="">Select Output for Overlay...</option>';
        
        const overlayOutputs = outputs.filter(o => o.overlay_enabled);
        if (overlayOutputs.length === 0) {
            overlayBox.style.display = 'none';
            if (presetsBar) presetsBar.style.display = 'none';
            overlaySelect.disabled = true;
            overlaySelect.innerHTML = '<option value="">No H.264 outputs with overlay enabled</option>';
            return;
        }

        overlaySelect.disabled = false;
        overlayOutputs.forEach(out => {
            const opt = document.createElement('option');
            opt.value = out.id;
            const statusText = out.running ? 'Running' : 'Stopped';
            opt.textContent = `${out.name || out.id} (H.264 - ${statusText})`;
            overlaySelect.appendChild(opt);
        });

        // Restore selected value if it still exists
        if (overlayOutputs.some(o => o.id === currentVal)) {
            overlaySelect.value = currentVal;
            overlayBox.style.display = 'flex';
            if (presetsBar) presetsBar.style.display = 'flex';
        } else {
            overlayBox.style.display = 'none';
            if (presetsBar) presetsBar.style.display = 'none';
        }
    }

    // Positions the draggable visual box based on coordinates from the output
    function positionVisualBox(xReal, yReal) {
        if (!previewContainer || !overlayBox || isDragging || transitionInterval) return;

        const rect = previewContainer.getBoundingClientRect();
        const containerW = rect.width;
        const containerH = rect.height;

        if (containerW === 0 || containerH === 0) return;

        const xDOM = (xReal / targetW) * containerW;
        const yDOM = (yReal / targetH) * containerH;

        overlayBox.style.left = `${xDOM}px`;
        overlayBox.style.top = `${yDOM}px`;
        
        if (overlayCoords) {
            overlayCoords.textContent = `${xReal}, ${yReal}`;
        }
    }

    // Process select change
    if (overlaySelect) {
        overlaySelect.addEventListener('change', () => {
            const outId = overlaySelect.value;
            if (outId) {
                overlayBox.style.display = 'flex';
                if (presetsBar) presetsBar.style.display = 'flex';
                // Trigger initial positioning from last active state of output
                fetch('/api/outputs').then(r => r.json()).then(outputs => {
                    const out = outputs.find(o => o.id === outId);
                    if (out) {
                        positionVisualBox(out.overlay_x || 10, out.overlay_y || 10);
                    }
                });
            } else {
                overlayBox.style.display = 'none';
                if (presetsBar) presetsBar.style.display = 'none';
            }
        });
    }

    // Drag events
    function onStart(e) {
        if (!overlaySelect || !overlaySelect.value) return;
        isDragging = true;
        
        if (transitionInterval) {
            clearInterval(transitionInterval);
            transitionInterval = null;
        }

        // Touch or mouse coordinates
        const clientX = e.touches ? e.touches[0].clientX : e.clientX;
        const clientY = e.touches ? e.touches[0].clientY : e.clientY;

        startX = clientX;
        startY = clientY;

        initialLeft = overlayBox.offsetLeft;
        initialTop = overlayBox.offsetTop;

        overlayBox.classList.add('grabbing');
        e.preventDefault();
    }

    function onMove(e) {
        if (!isDragging) return;

        const clientX = e.touches ? e.touches[0].clientX : e.clientX;
        const clientY = e.touches ? e.touches[0].clientY : e.clientY;

        const deltaX = clientX - startX;
        const deltaY = clientY - startY;

        const containerRect = previewContainer.getBoundingClientRect();
        const boxRect = overlayBox.getBoundingClientRect();

        let newLeft = initialLeft + deltaX;
        let newTop = initialTop + deltaY;

        // Constraint boundaries
        const maxLeft = containerRect.width - boxRect.width;
        const maxTop = containerRect.height - boxRect.height;

        newLeft = Math.max(0, Math.min(newLeft, maxLeft));
        newTop = Math.max(0, Math.min(newTop, maxTop));

        overlayBox.style.left = `${newLeft}px`;
        overlayBox.style.top = `${newTop}px`;

        // Calculate real coordinates (1920x1080)
        const xReal = Math.round((newLeft / containerRect.width) * targetW);
        const yReal = Math.round((newTop / containerRect.height) * targetH);

        if (overlayCoords) {
            overlayCoords.textContent = `${xReal}, ${yReal}`;
        }

        // Send throttled websocket update
        const now = Date.now();
        if (now - lastSentTime > throttleMs) {
            sendMoveUpdate(xReal, yReal);
            lastSentTime = now;
        }
    }

    function onEnd() {
        if (!isDragging) return;
        isDragging = false;
        overlayBox.classList.remove('grabbing');

        // Send final precise coordinate
        const containerRect = previewContainer.getBoundingClientRect();
        const xReal = Math.round((overlayBox.offsetLeft / containerRect.width) * targetW);
        const yReal = Math.round((overlayBox.offsetTop / containerRect.height) * targetH);

        sendMoveUpdate(xReal, yReal);
    }

    function sendMoveUpdate(x, y) {
        const outId = overlaySelect.value;
        if (!outId || !ws || ws.readyState !== WebSocket.OPEN) return;

        const payload = {
            type: "overlay_move",
            output_id: outId,
            x: x,
            y: y
        };
        ws.send(JSON.stringify(payload));
    }

    if (overlayBox) {
        overlayBox.addEventListener('mousedown', onStart);
        window.addEventListener('mousemove', onMove);
        window.addEventListener('mouseup', onEnd);

        overlayBox.addEventListener('touchstart', onStart, { passive: false });
        window.addEventListener('touchmove', onMove, { passive: false });
        window.addEventListener('touchend', onEnd);
    }

    // Click on preview container to move overlay smoothly
    if (previewContainer) {
        previewContainer.addEventListener('click', (e) => {
            if (!overlaySelect || !overlaySelect.value) return;
            if (e.target === overlayBox || overlayBox.contains(e.target)) return;

            const rect = previewContainer.getBoundingClientRect();
            const clickX = e.clientX - rect.left;
            const clickY = e.clientY - rect.top;

            const boxRect = overlayBox.getBoundingClientRect();
            const targetDOMLeft = clickX - boxRect.width / 2;
            const targetDOMTop = clickY - boxRect.height / 2;

            let xReal = Math.round((targetDOMLeft / rect.width) * targetW);
            let yReal = Math.round((targetDOMTop / rect.height) * targetH);

            xReal = Math.max(0, Math.min(xReal, targetW - 120));
            yReal = Math.max(0, Math.min(yReal, targetH - 50));

            animateOverlayTo(xReal, yReal);
        });
    }

    // Layout presets click listeners
    document.querySelectorAll('.btn-layout-preset').forEach(btn => {
        btn.addEventListener('click', () => {
            const x = parseInt(btn.dataset.x, 10);
            const y = parseInt(btn.dataset.y, 10);
            animateOverlayTo(x, y);
        });
    });

    // ─── Start ───
    fetchConfig();
    connectWS();
});
