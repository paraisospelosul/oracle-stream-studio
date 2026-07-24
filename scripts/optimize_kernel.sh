#!/bin/bash
# ============================================================================
# Oracle Stream Studio — Kernel Tuning for Oracle Cloud ARM64
# Optimized for 2 vCPU / 12GB RAM streaming relay (no transcoding)
# ============================================================================

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[kernel-tune]${NC} $1"; }
warn() { echo -e "${YELLOW}[AVISO]${NC} $1"; }

if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}[ERRO]${NC} Execute como root: sudo bash optimize_kernel.sh"
    exit 1
fi

SYSCTL_FILE="/etc/sysctl.d/99-stream-studio.conf"

log "Aplicando otimizações de kernel para streaming..."

cat > "$SYSCTL_FILE" <<'EOF'
# ============================================================================
# Oracle Stream Studio — Sysctl Tuning
# Optimized for low-CPU RTMP/SRT streaming relay on ARM64
# ============================================================================

# --- TCP Congestion Control: BBR ---
# BBR provides better throughput and lower latency than cubic
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr

# --- Network Buffer Sizes ---
# Increase max socket buffers for high-bitrate streams (up to 20Mbps)
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576

# TCP memory tuning (min, default, max in bytes)
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216

# UDP buffer for SRT/SRTLA (large buffers reduce packet loss)
net.core.netdev_max_backlog = 10000

# --- Connection Tuning ---
# Faster TCP connection reuse
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15

# Keep connections alive
net.ipv4.tcp_keepalive_time = 60
net.ipv4.tcp_keepalive_intvl = 10
net.ipv4.tcp_keepalive_probes = 6

# --- Memory Management ---
# Reduce swappiness (prefer keeping processes in RAM)
vm.swappiness = 10

# Allow more dirty pages before flushing (reduces disk I/O spikes)
vm.dirty_ratio = 40
vm.dirty_background_ratio = 10

# --- File Descriptor Limits ---
fs.file-max = 1048576
EOF

sysctl -p "$SYSCTL_FILE"
log "Sysctl aplicado: $SYSCTL_FILE"

# --- Increase file descriptor limits for the irl-studio user ---
LIMITS_FILE="/etc/security/limits.d/99-stream-studio.conf"
cat > "$LIMITS_FILE" <<'EOF'
# Oracle Stream Studio — File descriptor limits
*    soft    nofile    65536
*    hard    nofile    65536
root soft    nofile    65536
root hard    nofile    65536
EOF
log "Limites de FD aplicados: $LIMITS_FILE"

# --- Docker daemon tuning (if present) ---
DOCKER_DAEMON="/etc/docker/daemon.json"
if command -v docker &>/dev/null; then
    if [ ! -f "$DOCKER_DAEMON" ] || ! grep -q "log-opts" "$DOCKER_DAEMON" 2>/dev/null; then
        log "Configurando Docker log rotation..."
        mkdir -p /etc/docker
        cat > "$DOCKER_DAEMON" <<'EOF'
{
    "log-driver": "json-file",
    "log-opts": {
        "max-size": "10m",
        "max-file": "3"
    },
    "storage-driver": "overlay2"
}
EOF
        systemctl restart docker 2>/dev/null || true
        log "Docker daemon configurado com log rotation"
    else
        log "Docker já configurado, pulando..."
    fi
fi

log ""
log "✅ Otimizações de kernel aplicadas com sucesso!"
log ""
log "Resumo:"
log "  • TCP BBR ativado (melhor throughput)"
log "  • Buffers de rede aumentados (16MB)"
log "  • Swappiness reduzido para 10"
log "  • File descriptors aumentados para 65536"
log "  • Docker log rotation configurado"
log ""
log "Reinicie o servidor para aplicar todas as mudanças: sudo reboot"
