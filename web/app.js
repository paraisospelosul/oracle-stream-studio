document.addEventListener('DOMContentLoaded', () => {
    // ─── Toast ───
    function toast(msg, type = 'info') {
        const t = document.createElement('div');
        t.className = `toast ${type}`;
        t.textContent = msg;
        document.getElementById('toast-container').appendChild(t);
        setTimeout(() => {
            t.classList.add('fadeout');
            setTimeout(() => t.remove(), 300);
        }, 3000);
    }

    function escapeHTML(str) {
        return String(str || '')
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;');
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
            wsText.textContent = 'Conectado';
        };
        ws.onclose = () => {
            wsIndicator.classList.remove('connected');
            wsIndicator.classList.add('pulsing');
            wsText.textContent = 'Reconectando...';
            setTimeout(connectWS, 2000);
        };
        ws.onmessage = (e) => {
            try { handleUpdate(JSON.parse(e.data)); } catch (err) { console.error(err); }
        };
    }

    // ─── Live Update Handler ───
    let lastKnownActiveSceneId = '';
    let manualBrbActive = false;

    function handleUpdate(data) {
        if (data.system) {
            document.getElementById('stat-cpu').textContent = (data.system.cpu_load || 0).toFixed(1) + '%';
            const ramMB = Math.round(data.system.ram_used_mb || 0);
            document.getElementById('sys-ram').textContent = ramMB + ' MB';
        }

        if (data.bbox_status) {
            renderBboxBadge(data.bbox_status);
        }

        if (data.preview) {
            syncPreviewStateFromServer(data.preview);
        }

        if (data.switcher) {
            const sw = data.switcher;
            manualBrbActive = !!sw.manual_fallback;

            const badge = document.getElementById('main-status-badge');
            if (manualBrbActive) {
                badge.dataset.state = 'brb';
                badge.textContent = 'PAUSA / BRB';
            } else {
                badge.dataset.state = sw.state;
                badge.textContent = sw.state === 'live' ? 'AO VIVO' : 'FALLBACK';
            }

            const card = document.getElementById('switcher-card');
            card.style.borderLeftColor = sw.state === 'live' ? 'var(--live)' : (manualBrbActive ? 'var(--brb)' : 'var(--fallback)');
            card.style.borderLeftWidth = '3px';

            // BRB Button & Standby Pill
            const brbBtn = document.getElementById('btn-toggle-brb');
            if (manualBrbActive) {
                brbBtn.classList.add('active-brb');
                brbBtn.textContent = '▶ Voltar pro Ao Vivo Agora';
            } else {
                brbBtn.classList.remove('active-brb');
                brbBtn.textContent = '⏸ Pausar Live (Modo BRB)';
            }

            const standbyPill = document.getElementById('srt-standby-pill');
            standbyPill.style.display = sw.srt_standby ? 'inline-flex' : 'none';

            const mbps = ((sw.input_bitrate_kbps || 0) / 1000).toFixed(2);
            document.getElementById('stat-bitrate').innerHTML = `${mbps} <small>Mbps</small>`;
            document.getElementById('stat-active-scene').textContent = sw.active_scene_name || '—';
            document.getElementById('stat-uptime').textContent = sw.uptime || '—';
            document.getElementById('stat-packets-rx').textContent = formatNum(sw.packets_received);
            document.getElementById('stat-packets-fwd').textContent = formatNum(sw.packets_forwarded);
            document.getElementById('stat-pids').textContent = `${sw.video_pid || '—'} / ${sw.audio_pid || '—'}`;
            document.getElementById('stat-switch-count').textContent = sw.switch_count || 0;
            document.getElementById('stat-last-switch').textContent = sw.last_switch_time ? new Date(sw.last_switch_time).toLocaleTimeString() : '—';

            if (sw.active_scene_id && sw.active_scene_id !== lastKnownActiveSceneId) {
                lastKnownActiveSceneId = sw.active_scene_id;
                highlightActiveSceneCard(sw.active_scene_id);
            }

            const win = document.getElementById('chart-window').value || '5m';
            if (sw.bitrate_history && sw.bitrate_history[win]) {
                drawBitrateChart(sw.bitrate_history[win], win);
            }
            if (sw.recent_events) renderHistory(sw.recent_events);
        }

        if (data.outputs) {
            renderOutputs(data.outputs);
            updateOverlaySelect(data.outputs);
            if (overlaySelect && overlaySelect.value) {
                const activeOut = data.outputs.find(o => o.id === overlaySelect.value);
                if (activeOut) {
                    positionVisualBox(activeOut.overlay_x || 10, activeOut.overlay_y || 10);
                }
            }
        }

        if (data.audio) updateAudioBar(data.audio);
        if (data.recording !== undefined) updateRecordingUI(data.recording);
    }

    function formatNum(n) {
        if (!n) return '0';
        if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
        if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
        return n.toString();
    }

    // ─── Basic Zero-CPU Audio Bar ───
    const audioBarFill = document.getElementById('audio-bar-fill');
    const audioBarText = document.getElementById('audio-bar-text');

    function updateAudioBar(audio) {
        audioBarFill.classList.remove('fallback', 'silent');
        if (audio.source === 'live' && audio.active) {
            const pct = Math.max(15, Math.min(100, Math.round(((audio.bitrate_kbps || 128) / 160) * 100)));
            audioBarFill.style.width = pct + '%';
            audioBarText.textContent = `${audio.bitrate_kbps} kbps • Áudio OK`;
        } else if (audio.source === 'fallback') {
            audioBarFill.classList.add('fallback');
            audioBarFill.style.width = '35%';
            audioBarText.textContent = 'Silêncio (Fallback)';
        } else {
            audioBarFill.classList.add('silent');
            audioBarFill.style.width = '0%';
            audioBarText.textContent = '0 kbps • Sem Áudio';
        }
    }

    // ─── Bitrate Chart (uPlot) ───
    const chartContainer = document.getElementById('bitrate-chart');
    let uplotInstance = null;

    function initUPlot(dataX, dataY) {
        if (uplotInstance) uplotInstance.destroy();
        const rect = chartContainer.getBoundingClientRect();
        const opts = {
            width: rect.width || 400,
            height: 120,
            cursor: { show: true },
            select: { show: false },
            legend: { show: false },
            scales: {
                x: { time: true },
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
                { show: true, stroke: "#64748b", grid: { show: true, stroke: "rgba(255,255,255,0.02)" }, ticks: { show: false } },
                { show: true, stroke: "#64748b", grid: { show: true, stroke: "rgba(255,255,255,0.02)" }, values: (self, splits) => splits.map(v => v.toFixed(1) + "M"), ticks: { show: false } }
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
                    points: { show: false }
                }
            ]
        };
        uplotInstance = new uPlot(opts, [dataX, dataY], chartContainer);
    }

    function drawBitrateChart(history, win = '5m') {
        if (!history || history.length === 0 || typeof uPlot === 'undefined') return;
        const dataY = history.map(v => v / 1000);
        const nowMs = Date.now();
        const durations = { '1m': 60000, '5m': 300000, '30m': 1800000, '1h': 3600000, '2h': 7200000 };
        const totalMs = durations[win] || 300000;
        const interval = totalMs / Math.max(1, history.length - 1);
        const dataX = [];
        for (let i = 0; i < history.length; i++) {
            dataX.push(Math.round((nowMs - totalMs + (i * interval)) / 1000));
        }

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

    // ─── Fallback Scenes & BRB Controls ───
    document.getElementById('btn-toggle-brb').addEventListener('click', async () => {
        try {
            const res = await fetch('/api/actions/toggle-brb', { method: 'POST' });
            const d = await res.json();
            if (res.ok) toast(d.status, 'info');
            else toast(d.error || 'Erro ao alternar modo BRB', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    async function loadScenes() {
        const scenesList = document.getElementById('scenes-list');
        try {
            const res = await fetch('/api/scenes');
            if (!res.ok) throw new Error('Falha ao carregar cenas');
            const data = await res.json();

            if (data.resolution) {
                document.getElementById('fb-resolution').value = data.resolution;
            }
            document.getElementById('fb-return-mode').value = data.wait_for_keyframe ? 'keyframe' : 'instant';

            const scenes = data.scenes || [];
            if (scenes.length === 0) {
                scenesList.innerHTML = '<p class="text-muted">Nenhuma cena cadastrada. Envie uma imagem abaixo.</p>';
                return;
            }

            const canDelete = scenes.length > 1;
            lastKnownActiveSceneId = data.active_id || '';

            scenesList.innerHTML = scenes.map(sc => {
                const isActive = sc.id === data.active_id;
                const safeName = escapeHTML(sc.name);
                const safeId = escapeHTML(sc.id);
                const kb = sc.ram_bytes ? `${Math.max(1, Math.round(sc.ram_bytes / 1024))} KB RAM` : 'Compilando...';
                const thumbURL = `/api/scenes/thumb/${encodeURIComponent(sc.id)}?v=${sc.ram_bytes || 0}`;

                return `<div class="scene-card ${isActive ? 'active' : ''}" data-scene-id="${safeId}">
                    <div class="scene-thumb-wrap">
                        <img src="${thumbURL}" alt="${safeName}" loading="lazy" onerror="this.style.display='none'; this.nextElementSibling.style.display='block';">
                        <span class="scene-thumb-fallback" style="display:none;">🎬 ${safeName}</span>
                        ${isActive ? '<span class="scene-badge-active">Em Uso</span>' : ''}
                        <span class="scene-ram-tag">${kb}</span>
                    </div>
                    <div class="scene-card-info">
                        <span class="scene-card-title" title="${safeName}">${safeName}</span>
                        <div class="scene-card-actions">
                            <button class="btn btn-xs ${isActive ? 'btn-secondary' : 'btn-primary'} btn-activate-scene" data-id="${safeId}" ${isActive ? 'disabled' : ''}>${isActive ? 'Ativa' : 'Usar'}</button>
                            ${canDelete ? `<button class="btn-icon btn-delete btn-delete-scene" data-id="${safeId}" title="Excluir cena" style="font-size:0.9rem; min-width:28px; min-height:28px;">🗑</button>` : ''}
                        </div>
                    </div>
                </div>`;
            }).join('');

            scenesList.querySelectorAll('.btn-activate-scene').forEach(btn => {
                btn.addEventListener('click', () => activateScene(btn.dataset.id));
            });
            scenesList.querySelectorAll('.btn-delete-scene').forEach(btn => {
                btn.addEventListener('click', () => deleteScene(btn.dataset.id));
            });
        } catch (e) {
            scenesList.innerHTML = `<p style="color:var(--error); font-size:0.8rem;">Erro: ${escapeHTML(e.message)}</p>`;
        }
    }

    function highlightActiveSceneCard(activeId) {
        const cards = document.querySelectorAll('.scene-card');
        if (!cards.length) return;
        loadScenes();
    }

    async function activateScene(id) {
        try {
            const res = await fetch('/api/scenes/activate', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id })
            });
            if (res.ok) {
                toast('Cena ativada instantaneamente na RAM!', 'success');
                loadScenes();
            } else {
                const d = await res.json();
                toast(d.error || 'Erro ao ativar cena', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteScene(id) {
        if (!confirm('Excluir esta cena de fallback?')) return;
        try {
            const res = await fetch('/api/scenes/delete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id })
            });
            if (res.ok) {
                toast('Cena excluída!', 'success');
                loadScenes();
            } else {
                const d = await res.json();
                toast(d.error || 'Erro ao excluir', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }

    document.getElementById('btn-add-scene').addEventListener('click', async () => {
        const fileInput = document.getElementById('new-scene-file');
        const nameInput = document.getElementById('new-scene-name');
        const activateInput = document.getElementById('new-scene-activate');
        const btn = document.getElementById('btn-add-scene');

        if (!fileInput.files.length) {
            toast('Selecione uma imagem (.jpg/.png) ou vídeo primeiro', 'error');
            return;
        }

        const fd = new FormData();
        fd.append('file', fileInput.files[0]);
        if (nameInput.value.trim()) fd.append('name', nameInput.value.trim());
        fd.append('activate', activateInput.checked ? 'true' : 'false');

        btn.disabled = true;
        btn.textContent = 'Convertendo p/ RAM...';

        try {
            const res = await fetch('/api/upload/scene', { method: 'POST', body: fd });
            const data = await res.json();
            if (res.ok) {
                toast('Cena pronta na RAM (0% CPU)!', 'success');
                fileInput.value = '';
                nameInput.value = '';
                loadScenes();
            } else {
                toast(data.error || 'Falha ao enviar cena', 'error');
            }
        } catch (e) {
            toast(e.message, 'error');
        } finally {
            btn.disabled = false;
            btn.textContent = '⬆ Enviar p/ RAM';
        }
    });

    document.getElementById('btn-save-fb-settings').addEventListener('click', async () => {
        const resolution = document.getElementById('fb-resolution').value;
        const waitForKeyframe = document.getElementById('fb-return-mode').value === 'keyframe';
        try {
            const res = await fetch('/api/fallback/settings', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ resolution, wait_for_keyframe: waitForKeyframe })
            });
            if (res.ok) {
                toast('Ajustes do Fallback salvos!', 'success');
                setTimeout(loadScenes, 800);
            } else {
                toast('Erro ao salvar ajustes', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Recording ───
    const btnRec = document.getElementById('btn-rec');
    let isRecording = false;

    function updateRecordingUI(rec) {
        isRecording = rec.active;
        const recStatus = document.getElementById('rec-status');
        if (rec.active) {
            btnRec.className = 'btn btn-rec recording';
            btnRec.textContent = '⏹ PARAR REC';
            recStatus.textContent = `🔴 ${rec.duration || ''} — ${rec.size_mb || 0} MB`;
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
            if (res.ok) toast(d.status, 'success'); else toast(d.error || 'Erro', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    document.getElementById('btn-show-recordings').addEventListener('click', () => {
        openModal(document.getElementById('recordings-modal'));
        loadRecordings();
    });
    document.getElementById('btn-refresh-recs').addEventListener('click', loadRecordings);

    async function loadRecordings() {
        const list = document.getElementById('recordings-list');
        list.innerHTML = '<p class="text-muted">Carregando...</p>';
        try {
            const res = await fetch('/api/recordings');
            const data = await res.json();
            const recs = data.recordings;
            if (!recs || recs.length === 0) {
                list.innerHTML = '<p class="text-muted">Nenhuma gravação encontrada.</p>';
                return;
            }
            list.innerHTML = recs.map(r => {
                const safeName = escapeHTML(r.name);
                return `<div class="recording-item">
                    <span class="rec-name" title="${safeName}">🎬 ${safeName}</span>
                    <span class="rec-size">${r.size_mb} MB</span>
                    <span class="rec-date">${new Date(r.mod_time).toLocaleString()}</span>
                    <div class="rec-actions">
                        <button class="btn-icon rec-download" data-name="${safeName}" title="Baixar">⬇</button>
                        <button class="btn-icon rec-rename" data-name="${safeName}" title="Renomear">✏</button>
                        <button class="btn-icon btn-delete rec-delete" data-name="${safeName}" title="Excluir">🗑</button>
                    </div>
                </div>`;
            }).join('');

            list.querySelectorAll('.rec-download').forEach(btn => {
                btn.addEventListener('click', () => { location.href = '/api/recordings/' + encodeURIComponent(btn.dataset.name); });
            });
            list.querySelectorAll('.rec-rename').forEach(btn => {
                btn.addEventListener('click', () => renameRec(btn.dataset.name));
            });
            list.querySelectorAll('.rec-delete').forEach(btn => {
                btn.addEventListener('click', () => deleteRec(btn.dataset.name));
            });
        } catch (e) { list.innerHTML = `<p class="text-muted">Erro: ${escapeHTML(e.message)}</p>`; }
    }

    async function renameRec(name) {
        const newName = prompt('Novo nome do arquivo:', name);
        if (!newName || newName === name) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ new_name: newName })
            });
            if (res.ok) { toast('Renomeado!', 'success'); loadRecordings(); }
            else toast('Falha ao renomear', 'error');
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteRec(name) {
        if (!confirm(`Excluir ${name}?`)) return;
        try {
            const res = await fetch(`/api/recordings/${encodeURIComponent(name)}`, { method: 'DELETE' });
            if (res.ok) { toast('Excluído!', 'success'); loadRecordings(); }
            else toast('Falha ao excluir', 'error');
        } catch (e) { toast(e.message, 'error'); }
    }

    // ─── Switch History ───
    function renderHistory(events) {
        const list = document.getElementById('history-list');
        if (!events || events.length === 0) {
            list.innerHTML = '<p class="text-muted">Nenhum evento registrado ainda.</p>';
            return;
        }
        list.innerHTML = events.slice().reverse().map(ev => {
            const time = ev.time ? new Date(ev.time).toLocaleTimeString() : '';
            return `<div class="history-event">
                <span class="history-dot ${escapeHTML(ev.to)}"></span>
                <span class="history-time">${escapeHTML(time)}</span>
                <span>${escapeHTML(ev.from)} → ${escapeHTML(ev.to)}</span>
                <span class="history-reason">${escapeHTML(ev.reason)}</span>
            </div>`;
        }).join('');
    }

    // ─── Outputs ───
    let editingOutputId = null;
    let currentLogsId = null;
    const template = document.getElementById('output-card-template');

    function renderOutputs(outputs) {
        const container = document.getElementById('outputs-container');
        if (!outputs || outputs.length === 0) {
            container.innerHTML = '<div class="empty-state"><p>Nenhum destino RTMP configurado.</p></div>';
            return;
        }

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
        s.textContent = out.running ? 'Transmitindo' : 'Parado';
        s.className = `detail-value output-status ${out.running ? 'running' : 'stopped'}`;
        card.querySelector('.output-codec').textContent = out.codec === 'h264' ? 'H.264' : 'H.265 Copy';
        const outMbps = out.output_bitrate_kbps ? `${(out.output_bitrate_kbps / 1000).toFixed(2)} Mbps` : (out.bitrate ? out.bitrate + 'k' : 'Copy');
        card.querySelector('.output-bitrate').textContent = outMbps;
        card.querySelector('.output-uptime').textContent = out.uptime || '—';

        const errEl = card.querySelector('.output-error');
        if (out.error) {
            errEl.style.display = 'block';
            errEl.textContent = out.error;
        } else {
            errEl.style.display = 'none';
        }

        const t = card.querySelector('.btn-toggle');
        t.title = out.running ? 'Parar' : 'Iniciar';
        t.textContent = out.running ? '⏸' : '▶';
    }

    async function toggleOutput(id) {
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        const running = card && card.classList.contains('running');
        try {
            const res = await fetch(`/api/outputs/${id}/${running ? 'stop' : 'start'}`, { method: 'POST' });
            if (res.ok) toast(`Destino ${running ? 'parado' : 'iniciado'}!`, 'success');
            else {
                const d = await res.json();
                toast(d.error || 'Erro na saída', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }

    async function deleteOutput(id) {
        if (!confirm('Excluir este destino RTMP?')) return;
        try {
            await fetch(`/api/outputs/${id}`, { method: 'DELETE' });
            toast('Destino excluído', 'success');
        } catch (e) { toast(e.message, 'error'); }
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
            document.getElementById('out-bitrate').value = out.bitrate || 4500;
            document.getElementById('out-preset').value = out.preset || 'ultrafast';
            document.getElementById('out-overlay').checked = out.overlay_enabled || false;
            toggleH264Options();
            document.querySelector('#add-modal .modal-header h3').textContent = 'Editar Destino RTMP';
            openModal(document.getElementById('add-modal'));
        });
    }

    async function showLogs(id) {
        currentLogsId = id;
        const card = document.querySelector(`.output-card[data-id="${id}"]`);
        document.getElementById('logs-output-name').textContent = card?.querySelector('.output-name')?.textContent || id;
        document.getElementById('logs-content').textContent = 'Carregando...';
        openModal(document.getElementById('logs-modal'));
        refreshLogs();
    }

    async function refreshLogs() {
        if (!currentLogsId) return;
        try {
            const r = await fetch(`/api/outputs/${currentLogsId}/logs`);
            const d = await r.json();
            document.getElementById('logs-content').textContent = d.logs?.join('\n') || 'Sem logs.';
        } catch (e) { document.getElementById('logs-content').textContent = e.message; }
    }
    document.getElementById('btn-refresh-logs').addEventListener('click', refreshLogs);

    // ─── Collapsible Cards ───
    const configToggle = document.getElementById('config-toggle');
    const configBody = document.getElementById('config-body');
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
            if (c.fallback_resolution) {
                document.getElementById('fb-resolution').value = c.fallback_resolution;
            }
            document.getElementById('fb-return-mode').value = c.wait_for_keyframe ? 'keyframe' : 'instant';
        } catch (e) { /* ignore */ }
    }

    document.getElementById('btn-save-config').addEventListener('click', async () => {
        const cfg = {
            srt_addr: document.getElementById('cfg-srt-addr').value.trim(),
            srt_mode: document.getElementById('cfg-srt-mode').value,
            srt_timeout: parseInt(document.getElementById('cfg-srt-timeout').value, 10) || 2000,
            stats_url: document.getElementById('cfg-stats-url').value.trim(),
            fallback_resolution: document.getElementById('fb-resolution').value,
            wait_for_keyframe: document.getElementById('fb-return-mode').value === 'keyframe',
            min_bitrate_kbps: parseInt(document.getElementById('cfg-min-bitrate').value, 10) || 0,
            bitrate_hysteresis_seconds: parseInt(document.getElementById('cfg-hysteresis').value, 10) || 5,
        };
        try {
            const r = await fetch('/api/config', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(cfg)
            });
            if (r.ok) toast('Configuração SRT salva!', 'success'); else toast('Falha ao salvar', 'error');
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Add/Edit Output Form ───
    const codecSelect = document.getElementById('out-codec');
    function toggleH264Options() {
        document.getElementById('h264-options').style.display = codecSelect.value === 'h264' ? 'block' : 'none';
    }
    codecSelect.addEventListener('change', toggleH264Options);

    document.getElementById('btn-add-output').addEventListener('click', () => {
        editingOutputId = null;
        document.getElementById('form-add-output').reset();
        toggleH264Options();
        document.querySelector('#add-modal .modal-header h3').textContent = 'Adicionar Destino RTMP';
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
            if (r.ok) {
                toast(editingOutputId ? 'Destino atualizado!' : 'Destino adicionado!', 'success');
                closeModal(document.getElementById('add-modal'));
            } else {
                const d = await r.json();
                toast(d.error || 'Falha ao salvar destino', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Quick Actions ───
    async function quickAction(action) {
        try {
            const r = await fetch(`/api/actions/${action}`, { method: 'POST' });
            const d = await r.json();
            toast(d.status || 'Executado!', 'success');
        } catch (e) { toast(e.message, 'error'); }
    }
    document.getElementById('act-restart-srt').addEventListener('click', () => quickAction('restart-srt'));
    document.getElementById('act-restart-fallback').addEventListener('click', () => quickAction('restart-fallback'));
    document.getElementById('act-restart-outputs').addEventListener('click', () => quickAction('restart-outputs'));
    document.getElementById('act-restart-all').addEventListener('click', () => {
        if (confirm('Reiniciar SRT e todos os destinos RTMP?')) quickAction('restart-all');
    });

    // ─── Watermark Modal ───
    document.getElementById('btn-open-watermark').addEventListener('click', () => {
        openModal(document.getElementById('watermark-modal'));
    });

    document.getElementById('btn-upload-watermark').addEventListener('click', async () => {
        const input = document.getElementById('file-watermark');
        const btn = document.getElementById('btn-upload-watermark');
        if (!input.files.length) { toast('Selecione um arquivo .png', 'error'); return; }
        const fd = new FormData();
        fd.append('file', input.files[0]);
        btn.disabled = true;
        btn.textContent = 'Enviando...';
        try {
            const r = await fetch('/api/upload/watermark', { method: 'POST', body: fd });
            if (r.ok) {
                toast('Marca d\'água atualizada!', 'success');
                closeModal(document.getElementById('watermark-modal'));
            } else {
                toast('Falha ao enviar PNG', 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
        btn.disabled = false;
        btn.textContent = 'Enviar PNG';
    });

    // ─── On-Demand Preview (Off by Default!) ───
    const previewImg = document.getElementById('preview-snapshot');
    const previewPlaceholder = document.getElementById('preview-placeholder');
    let previewInterval = null;
    let previewFps = 0;

    function stopPreviewPolling() {
        if (previewInterval) {
            clearInterval(previewInterval);
            previewInterval = null;
        }
        previewImg.style.display = 'none';
        previewPlaceholder.style.display = 'flex';
    }

    function startPreviewPolling() {
        stopPreviewPolling();
        if (previewFps <= 0) return;
        previewPlaceholder.style.display = 'none';
        previewImg.style.display = 'block';
        const ms = Math.max(250, Math.floor(1000 / previewFps));
        const load = () => {
            const img = new Image();
            img.onload = () => { previewImg.src = img.src; };
            img.src = '/api/preview/frame?t=' + Date.now();
        };
        load();
        previewInterval = setInterval(load, ms);
    }

    function syncPreviewStateFromServer(prv) {
        const serverFps = prv.running ? (prv.fps || 0) : 0;
        if (serverFps !== previewFps) {
            previewFps = serverFps;
            document.getElementById('preview-fps').value = String(serverFps);
            if (serverFps === 0) stopPreviewPolling();
            else startPreviewPolling();
        }
    }

    document.getElementById('btn-apply-preview').addEventListener('click', async () => {
        const fps = parseInt(document.getElementById('preview-fps').value, 10) || 0;
        const [w, h] = document.getElementById('preview-res').value.split('x').map(Number);
        previewFps = fps;
        try {
            await fetch(`/api/preview/settings?fps=${fps}&w=${w}&h=${h}`, { method: 'PUT' });
            if (fps === 0) {
                stopPreviewPolling();
                toast('Preview desligado (0% CPU)', 'info');
            } else {
                startPreviewPolling();
                toast(`Preview ligado: ${fps} fps (${w}×${h})`, 'success');
            }
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Bbox Receiver (Status pushed via WebSocket — zero 3s HTTP polling!) ───
    function renderBboxBadge(status) {
        const badge = document.getElementById('bbox-status-badge');
        const st = (status || 'unknown').toLowerCase();
        badge.dataset.state = st;
        badge.textContent = st.toUpperCase();
    }

    async function bboxAction(action) {
        try {
            const res = await fetch(`/api/bbox/action?action=${action}`, { method: 'POST' });
            const d = await res.json();
            if (res.ok) {
                toast(`Belabox: ${action} concluído!`, 'success');
                if (d.bbox_status) renderBboxBadge(d.bbox_status);
            } else {
                toast(`Erro: ${d.error}`, 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    }
    document.getElementById('btn-bbox-start').addEventListener('click', () => bboxAction('start'));
    document.getElementById('btn-bbox-stop').addEventListener('click', () => bboxAction('stop'));
    document.getElementById('btn-bbox-restart').addEventListener('click', () => bboxAction('restart'));
    document.getElementById('btn-bbox-update').addEventListener('click', () => {
        if (confirm('Baixar a imagem mais recente do Belabox Receiver (docker compose pull) e recriar o container?')) {
            toast('Baixando imagem mais recente do Bbox...', 'info');
            bboxAction('update');
        }
    });

    async function showBboxLogs() {
        openModal(document.getElementById('bbox-logs-modal'));
        document.getElementById('bbox-logs-content').textContent = 'Carregando...';
        try {
            const res = await fetch('/api/bbox/logs');
            const data = await res.json();
            document.getElementById('bbox-logs-content').textContent = data.logs || 'Sem logs.';
        } catch (e) { document.getElementById('bbox-logs-content').textContent = e.message; }
    }
    document.getElementById('btn-bbox-logs').addEventListener('click', showBboxLogs);
    document.getElementById('btn-refresh-bbox-logs').addEventListener('click', showBboxLogs);

    let currentBboxEditorMode = '';
    async function openBboxEditor(mode) {
        currentBboxEditorMode = mode;
        document.getElementById('bbox-editor-title').textContent = mode === 'config' ? 'Editar config.json' : 'Editar docker-compose.yml';
        document.getElementById('bbox-editor-textarea').value = 'Carregando...';
        openModal(document.getElementById('bbox-editor-modal'));

        try {
            const res = await fetch(`/api/bbox/${mode}`);
            const data = await res.json();
            document.getElementById('bbox-editor-textarea').value = data.content || '';
        } catch (e) { document.getElementById('bbox-editor-textarea').value = `Erro: ${e.message}`; }
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
                toast('Salvo! Reinicie o Bbox para aplicar.', 'success');
                closeModal(document.getElementById('bbox-editor-modal'));
            } else {
                const d = await res.json();
                toast(`Erro: ${d.error}`, 'error');
            }
        } catch (e) { toast(e.message, 'error'); }
    });

    // ─── Modal Helpers ───
    function openModal(m) { if (m) m.classList.add('open'); }
    function closeModal(m) { if (m) m.classList.remove('open'); editingOutputId = null; currentLogsId = null; }

    document.querySelectorAll('.btn-close-modal, .btn-cancel-modal').forEach(b => b.addEventListener('click', () => {
        const m = b.closest('.modal-backdrop');
        if (m) closeModal(m);
    }));
    document.querySelectorAll('.modal-backdrop').forEach(b => b.addEventListener('click', e => { if (e.target === b) closeModal(b); }));
    document.addEventListener('keydown', e => {
        if (e.key === 'Escape') document.querySelectorAll('.modal-backdrop.open').forEach(closeModal);
    });

    // ─── Dynamic Overlay (Watermark) Positioner ───
    const overlayBox = document.getElementById('preview-overlay-box');
    const overlayCoords = overlayBox ? overlayBox.querySelector('.overlay-coords') : null;
    const overlaySelect = document.getElementById('preview-overlay-output');
    const previewContainer = document.getElementById('preview-container-box');
    const presetsBar = document.getElementById('layout-presets-bar');

    let isDragging = false;
    let startX = 0, startY = 0, initialLeft = 0, initialTop = 0, lastSentTime = 0;
    const throttleMs = 60;
    const targetW = 1920, targetH = 1080;

    function updateOverlaySelect(outputs) {
        if (!overlaySelect) return;
        const currentVal = overlaySelect.value;
        const overlayOutputs = outputs.filter(o => o.overlay_enabled);
        if (overlayOutputs.length === 0) {
            overlayBox.style.display = 'none';
            if (presetsBar) presetsBar.style.display = 'none';
            overlaySelect.disabled = true;
            overlaySelect.innerHTML = '<option value="">Sem saída H.264 c/ overlay</option>';
            return;
        }

        overlaySelect.disabled = false;
        overlaySelect.innerHTML = '<option value="">Selecionar saída p/ Overlay...</option>';
        overlayOutputs.forEach(out => {
            const opt = document.createElement('option');
            opt.value = out.id;
            opt.textContent = `${out.name || out.id} (${out.running ? 'Ativa' : 'Parada'})`;
            overlaySelect.appendChild(opt);
        });

        if (overlayOutputs.some(o => o.id === currentVal)) {
            overlaySelect.value = currentVal;
            overlayBox.style.display = 'flex';
            if (presetsBar) presetsBar.style.display = 'flex';
        } else {
            overlayBox.style.display = 'none';
            if (presetsBar) presetsBar.style.display = 'none';
        }
    }

    function positionVisualBox(xReal, yReal) {
        if (!previewContainer || !overlayBox || isDragging) return;
        const rect = previewContainer.getBoundingClientRect();
        if (rect.width === 0 || rect.height === 0) return;
        overlayBox.style.left = `${(xReal / targetW) * rect.width}px`;
        overlayBox.style.top = `${(yReal / targetH) * rect.height}px`;
        if (overlayCoords) overlayCoords.textContent = `${xReal}, ${yReal}`;
    }

    if (overlaySelect) {
        overlaySelect.addEventListener('change', () => {
            const outId = overlaySelect.value;
            if (outId) {
                overlayBox.style.display = 'flex';
                if (presetsBar) presetsBar.style.display = 'flex';
                fetch('/api/outputs').then(r => r.json()).then(outputs => {
                    const out = outputs.find(o => o.id === outId);
                    if (out) positionVisualBox(out.overlay_x || 10, out.overlay_y || 10);
                });
            } else {
                overlayBox.style.display = 'none';
                if (presetsBar) presetsBar.style.display = 'none';
            }
        });
    }

    function sendMoveUpdate(x, y) {
        const outId = overlaySelect.value;
        if (!outId || !ws || ws.readyState !== WebSocket.OPEN) return;
        ws.send(JSON.stringify({ type: 'overlay_move', output_id: outId, x, y }));
    }

    function onStart(e) {
        if (!overlaySelect || !overlaySelect.value) return;
        isDragging = true;
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
        const containerRect = previewContainer.getBoundingClientRect();
        const boxRect = overlayBox.getBoundingClientRect();

        let newLeft = Math.max(0, Math.min(initialLeft + (clientX - startX), containerRect.width - boxRect.width));
        let newTop = Math.max(0, Math.min(initialTop + (clientY - startY), containerRect.height - boxRect.height));

        overlayBox.style.left = `${newLeft}px`;
        overlayBox.style.top = `${newTop}px`;

        const xReal = Math.round((newLeft / containerRect.width) * targetW);
        const yReal = Math.round((newTop / containerRect.height) * targetH);
        if (overlayCoords) overlayCoords.textContent = `${xReal}, ${yReal}`;

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
        const containerRect = previewContainer.getBoundingClientRect();
        const xReal = Math.round((overlayBox.offsetLeft / containerRect.width) * targetW);
        const yReal = Math.round((overlayBox.offsetTop / containerRect.height) * targetH);
        sendMoveUpdate(xReal, yReal);
    }

    if (overlayBox) {
        overlayBox.addEventListener('mousedown', onStart);
        window.addEventListener('mousemove', onMove);
        window.addEventListener('mouseup', onEnd);
        overlayBox.addEventListener('touchstart', onStart, { passive: false });
        window.addEventListener('touchmove', onMove, { passive: false });
        window.addEventListener('touchend', onEnd);
    }

    document.querySelectorAll('.btn-layout-preset').forEach(btn => {
        btn.addEventListener('click', () => {
            const x = parseInt(btn.dataset.x, 10);
            const y = parseInt(btn.dataset.y, 10);
            positionVisualBox(x, y);
            sendMoveUpdate(x, y);
        });
    });

    // ─── Initialize ───
    fetchConfig();
    loadScenes();
    connectWS();
});
