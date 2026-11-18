#!/bin/bash
# ==============================================================================
# Oracle Stream Studio v2.0 — Instalador Inteligente & Atualizador (2-OCPU ARM64/AMD64)
# ==============================================================================
# PRINCIPAIS MUDANÇAS DESTA VERSÃO DO INSTALADOR:
# 1. Detecção automática de arquitetura (ARM64 Ampere aarch64 vs x86_64 amd64).
# 2. Busca dinâmica da última versão estável do Go direto da API oficial (go.dev/VERSION).
# 3. Instalação da build estática mais recente do FFmpeg + FFprobe (BtbN/FFmpeg-Builds)
#    com suporte completo a Enhanced RTMP (H.265), libx265, libsrt e libzmq
#    (evitando o FFmpeg antigo dos repositórios apt padrão do Ubuntu).
# 4. Container Docker do Belabox Receiver (bbox):
#    - Consulta a API do Docker Hub/GitHub em tempo real para descobrir a última versão;
#    - Permite usar a versão mais recente automaticamente (pull_policy: always) ou escolher a tag.
# 5. Container bbox otimizado para 2 OCPUs (network_mode: "host" sem overhead de NAT UDP,
#    cpus: "0.5", mem_limit: 512m e rotação de logs).
# 6. Opção de atualizar dependências do Go (go get -u) e aplicar tuning de Kernel (BBR + buffers UDP).
# 7. Correção do arquivo systemd (expansão real de WEB_USER / WEB_PASS e LimitNOFILE=65536).
# 8. Sem geração lenta de vídeo de 60s no install: o binário v2.0 gera o RAM Slate de 1.6s em <0.5s!
# ==============================================================================

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[install]${NC} $1"; }
warn() { echo -e "${YELLOW}[aviso]${NC} $1"; }
err()  { echo -e "${RED}[erro]${NC} $1"; }

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
  err "Execute como root: sudo bash scripts/install.sh"
  exit 1
fi

AUTO_MODE=false
UPDATE_MODE=false
for arg in "$@"; do
  case "$arg" in
    --auto|-y) AUTO_MODE=true ;;
    --update|-u) UPDATE_MODE=true ;;
  esac
done

IS_TTY=false
if [ -t 0 ] && [ "$AUTO_MODE" = false ]; then
  IS_TTY=true
fi

ask_choice() {
  local prompt="$1"
  local default="$2"
  if [ "$IS_TTY" = true ]; then
    local ans=""
    read -r -p "$prompt [$default]: " ans
    echo "${ans:-$default}"
  else
    echo "$default"
  fi
}

echo -e "${CYAN}================================================================${NC}"
echo -e "${CYAN}   Oracle Stream Studio v2.0 — Instalador Inteligente (2-OCPU)  ${NC}"
echo -e "${CYAN}================================================================${NC}"
echo ""

# ------------------------------------------------------------------------------
# 0. Detectar Arquitetura da CPU (ARM64 Ampere vs AMD64)
# ------------------------------------------------------------------------------
RAW_ARCH="$(uname -m)"
case "$RAW_ARCH" in
  aarch64|arm64)
    GO_ARCH="arm64"
    BTBN_ARCH="linuxarm64"
    ;;
  x86_64|amd64)
    GO_ARCH="amd64"
    BTBN_ARCH="linux64"
    ;;
  *)
    err "Arquitetura de CPU não suportada: $RAW_ARCH"
    exit 1
    ;;
esac
log "Arquitetura detectada: ${RAW_ARCH} (Go=${GO_ARCH}, FFmpeg=${BTBN_ARCH})"

# ------------------------------------------------------------------------------
# 1. Credenciais do Painel Web (preserva valores atuais em modo --update)
# ------------------------------------------------------------------------------
EXISTING_SERVICE="/etc/systemd/system/oracle-stream-studio.service"
DEFAULT_WEB_USER="belabox"
DEFAULT_WEB_PASS="belabox"

if [ -f "$EXISTING_SERVICE" ]; then
  SAVED_USER=$(grep -E '^Environment="WEB_USER=' "$EXISTING_SERVICE" | sed -E 's/^Environment="WEB_USER=(.*)"/\1/' || true)
  SAVED_PASS=$(grep -E '^Environment="WEB_PASS=' "$EXISTING_SERVICE" | sed -E 's/^Environment="WEB_PASS=(.*)"/\1/' || true)
  [ -n "$SAVED_USER" ] && [ "$SAVED_USER" != '${WEB_USER}' ] && DEFAULT_WEB_USER="$SAVED_USER"
  [ -n "$SAVED_PASS" ] && [ "$SAVED_PASS" != '${WEB_PASS}' ] && DEFAULT_WEB_PASS="$SAVED_PASS"
fi

if [ "$IS_TTY" = true ] && [ "$UPDATE_MODE" = false ]; then
  echo ""
  echo "Configure o login de proteção do Painel Web:"
  read -r -p "Usuário (padrão: ${DEFAULT_WEB_USER}): " INPUT_USER
  WEB_USER="${INPUT_USER:-$DEFAULT_WEB_USER}"
  read -r -s -p "Senha (padrão: manter/belabox): " INPUT_PASS
  echo ""
  WEB_PASS="${INPUT_PASS:-$DEFAULT_WEB_PASS}"
else
  WEB_USER="$DEFAULT_WEB_USER"
  WEB_PASS="$DEFAULT_WEB_PASS"
fi

# ------------------------------------------------------------------------------
# 2. Dependências Base do Sistema
# ------------------------------------------------------------------------------
log "[1/7] Atualizando pacotes básicos do sistema..."
apt-get update -qq
apt-get install -y -qq curl wget git jq xz-utils tar ca-certificates net-tools

# ------------------------------------------------------------------------------
# 3. FFmpeg + FFprobe (Última Build Estática GPL ou APT)
# ------------------------------------------------------------------------------
log "[2/7] Verificando FFmpeg (necessário 6.1+ / 7.0+ para H.265 Enhanced RTMP)..."
CURRENT_FFMPEG="não instalado"
if command -v ffmpeg &>/dev/null; then
  CURRENT_FFMPEG="$(ffmpeg -version | head -n1)"
fi
echo "  -> Atual: ${CURRENT_FFMPEG}"
echo "  Opções de FFmpeg:"
echo "    [1] Baixar FFmpeg + FFprobe Estático Mais Recente (BtbN GitHub Latest - Recomendado)"
echo "    [2] Usar FFmpeg do repositório APT do Ubuntu/Debian"
echo "    [3] Manter FFmpeg atual sem alterar"
DEFAULT_FFMPEG_OPT="1"
if [ "$CURRENT_FFMPEG" != "não instalado" ] && [ "$UPDATE_MODE" = false ]; then
  DEFAULT_FFMPEG_OPT="1"
fi
FFMPEG_CHOICE="$(ask_choice "Escolha a opção de FFmpeg (1/2/3)" "$DEFAULT_FFMPEG_OPT")"

install_static_ffmpeg() {
  local url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${BTBN_ARCH}-gpl.tar.xz"
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  log "Baixando FFmpeg estático mais recente (${BTBN_ARCH})..."
  if curl -fL --retry 3 --connect-timeout 15 "$url" -o "${tmp_dir}/ffmpeg.tar.xz"; then
    tar -xJf "${tmp_dir}/ffmpeg.tar.xz" -C "$tmp_dir"
    local bin_dir
    bin_dir="$(find "$tmp_dir" -type f -name ffmpeg -exec dirname {} \; | head -n1)"
    if [ -n "$bin_dir" ]; then
      install -m 0755 "${bin_dir}/ffmpeg" /usr/local/bin/ffmpeg
      install -m 0755 "${bin_dir}/ffprobe" /usr/local/bin/ffprobe
      rm -rf "$tmp_dir"
      hash -r || true
      log "FFmpeg atualizado em /usr/local/bin: $(/usr/local/bin/ffmpeg -version | head -n1)"
      return 0
    fi
  fi
  rm -rf "$tmp_dir"
  return 1
}

case "$FFMPEG_CHOICE" in
  1)
    if ! install_static_ffmpeg; then
      warn "Falha ao baixar build estática do GitHub; instalando via APT como fallback..."
      apt-get install -y ffmpeg
    fi
    ;;
  2)
    apt-get install -y ffmpeg
    ;;
  3)
    if ! command -v ffmpeg &>/dev/null; then
      apt-get install -y ffmpeg
    fi
    ;;
esac

# ------------------------------------------------------------------------------
# 4. Docker Engine + Compose v2 + Log Rotation
# ------------------------------------------------------------------------------
log "[3/7] Verificando Docker e Docker Compose v2..."
if ! command -v docker &>/dev/null; then
  log "Instalando versão mais recente do Docker..."
  curl -fsSL https://get.docker.com | sh
elif [ "$UPDATE_MODE" = true ]; then
  log "Verificando atualizações do Docker Engine / Compose Plugin..."
  apt-get install -y --only-upgrade docker-ce docker-ce-cli containerd.io docker-compose-plugin 2>/dev/null || true
fi

# Garante rotação de logs do Docker para não lotar o disco de boot da Oracle
mkdir -p /etc/docker
if [ ! -f /etc/docker/daemon.json ]; then
  cat << 'EOF' > /etc/docker/daemon.json
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  }
}
EOF
  systemctl restart docker || true
fi

# ------------------------------------------------------------------------------
# 5. Go Toolchain — Busca Dinâmica da Última Versão Estável em go.dev
# ------------------------------------------------------------------------------
log "[4/7] Consultando última versão estável do Go em go.dev..."
export PATH="/usr/local/go/bin:${PATH}"

LATEST_GO_VERSION="$(curl -fsSL --connect-timeout 10 'https://go.dev/VERSION?m=text' | head -n1 | tr -d '\r\n' || true)"
if [[ ! "$LATEST_GO_VERSION" =~ ^go[0-9]+\.[0-9]+ ]]; then
  LATEST_GO_VERSION="go1.23.6"
  warn "Não foi possível consultar go.dev; usando fallback seguro ${LATEST_GO_VERSION}"
fi

INSTALLED_GO_VERSION="nenhuma"
if [ -x /usr/local/go/bin/go ]; then
  INSTALLED_GO_VERSION="$(/usr/local/go/bin/go version | awk '{print $3}')"
elif command -v go &>/dev/null; then
  INSTALLED_GO_VERSION="$(go version | awk '{print $3}')"
fi

echo "  -> Go instalado: ${INSTALLED_GO_VERSION} | Última oficial: ${LATEST_GO_VERSION}"
INSTALL_GO="s"
if [ "$INSTALLED_GO_VERSION" = "$LATEST_GO_VERSION" ]; then
  INSTALL_GO="n"
  log "Go já está na última versão (${LATEST_GO_VERSION})."
elif [ "$INSTALLED_GO_VERSION" != "nenhuma" ]; then
  INSTALL_GO="$(ask_choice "Atualizar Go de ${INSTALLED_GO_VERSION} para ${LATEST_GO_VERSION}? (s/n)" "s")"
fi

if [[ "$INSTALL_GO" =~ ^[sSyY] ]]; then
  GO_TARBALL="${LATEST_GO_VERSION}.linux-${GO_ARCH}.tar.gz"
  log "Baixando e instalando ${GO_TARBALL}..."
  curl -fL --retry 3 "https://go.dev/dl/${GO_TARBALL}" -o "/tmp/${GO_TARBALL}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm -f "/tmp/${GO_TARBALL}"
  if ! grep -q "/usr/local/go/bin" /etc/profile; then
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
  fi
  log "Go instalado com sucesso: $(/usr/local/go/bin/go version)"
fi

# ------------------------------------------------------------------------------
# 6. Preparar Diretório /opt/oracle-stream-studio
# ------------------------------------------------------------------------------
log "[5/7] Preparando código-fonte em /opt/oracle-stream-studio..."
mkdir -p /opt/oracle-stream-studio

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

if [ "$REPO_DIR" != "/opt/oracle-stream-studio" ] && [ -f "$REPO_DIR/main.go" ]; then
  log "Sincronizando arquivos de ${REPO_DIR} para /opt/oracle-stream-studio..."
  cp -a "$REPO_DIR"/. /opt/oracle-stream-studio/
elif [ "$UPDATE_MODE" = true ] && [ -d "/opt/oracle-stream-studio/.git" ]; then
  log "Baixando últimos commits do GitHub (git pull)..."
  git -C /opt/oracle-stream-studio pull --ff-only || warn "git pull ignorado (alterações locais presentes)."
fi

cd /opt/oracle-stream-studio

# ------------------------------------------------------------------------------
# 7. Belabox SRTLA Receiver (Bbox) — Descoberta da Versão Mais Recente
# ------------------------------------------------------------------------------
log "[6/7] Configurando Belabox SRTLA Receiver (bbox)..."

# Consulta a última release no GitHub (datagutt/bbox-receiver) e tags recentes no Docker Hub
LATEST_BBOX_RELEASE="$(curl -fsSL --connect-timeout 8 'https://api.github.com/repos/datagutt/bbox-receiver/releases/latest' | jq -r '.tag_name // empty' 2>/dev/null | sed 's/^v//' || true)"
RECENT_DOCKER_TAGS="$(curl -fsSL --connect-timeout 8 'https://hub.docker.com/v2/repositories/datagutt/belabox-receiver/tags?page_size=8' | jq -r '.results[].name' 2>/dev/null | tr '\n' ' ' || true)"

RECOMMENDED_BBOX_TAG="latest"
if [ -n "$LATEST_BBOX_RELEASE" ]; then
  # Verifica se a tag numérica da última release existe no Docker Hub
  if echo " $RECENT_DOCKER_TAGS " | grep -q " ${LATEST_BBOX_RELEASE} "; then
    RECOMMENDED_BBOX_TAG="$LATEST_BBOX_RELEASE"
  fi
fi

echo ""
echo "  Versões detectadas do Belabox Receiver (datagutt/belabox-receiver no Docker):"
echo "  -> Última versão oficial: ${RECOMMENDED_BBOX_TAG}"
echo "  -> Tags disponíveis no Docker Hub: ${RECENT_DOCKER_TAGS:-latest}"
echo ""
BBOX_TAG_CHOICE="$(ask_choice "Qual tag da imagem datagutt/belabox-receiver deseja usar no Docker?" "${RECOMMENDED_BBOX_TAG}")"
BBOX_IMAGE="datagutt/belabox-receiver:${BBOX_TAG_CHOICE}"

# Monta o docker-compose.yml do container bbox (otimizado p/ 2 OCPUs com network_mode: host)
cat << EOF > docker-compose.yml
services:
  bbox:
    image: ${BBOX_IMAGE}
    pull_policy: always
    container_name: bbox
    restart: unless-stopped
    network_mode: "host"
    volumes:
      - ./config.json:/app/config.json:ro
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp:size=64m,noexec,nosuid,nodev
      - /var/log:size=64m,noexec,nosuid,nodev
    pids_limit: 256
    mem_limit: 512m
    cpus: "0.5"
    logging:
      driver: json-file
      options:
        max-size: "10m"
EOF

if [ ! -f "config.json" ]; then
  BBOX_USER="belabox"
  BBOX_KEY="belabox"
  if [ "$IS_TTY" = true ]; then
    echo ""
    echo "Configuração inicial de autenticação SRTLA (Moblin -> Bbox):"
    read -r -p "Stream User (padrão: belabox): " IN_BUSER
    BBOX_USER="${IN_BUSER:-belabox}"
    read -r -p "Stream Key/Auth (padrão: belabox): " IN_BKEY
    BBOX_KEY="${IN_BKEY:-belabox}"
  fi
  cat << EOF > config.json
{
  "auth": [
    {
      "user": "${BBOX_USER}",
      "key": "${BBOX_KEY}"
    }
  ]
}
EOF
fi

log "Subindo container Bbox (${BBOX_IMAGE})..."
if docker compose version &>/dev/null; then
  docker compose pull || true
  docker compose up -d --force-recreate
else
  docker-compose pull || true
  docker-compose up -d --force-recreate
fi

# ------------------------------------------------------------------------------
# 8. Atualizar Dependências Go, Compilar Binário Enxuto & Configurar Systemd
# ------------------------------------------------------------------------------
log "[7/7] Compilando Oracle Stream Studio v2.0..."

UPD_GO_DEPS="$(ask_choice "Atualizar bibliotecas Go (go.mod) para as versões mais recentes? (s/n)" "s")"
if [[ "$UPD_GO_DEPS" =~ ^[sSyY] ]]; then
  log "Atualizando dependências Go (gorilla/websocket, go-zeromq/zmq4)..."
  /usr/local/go/bin/go get -u ./... || warn "Aviso ao atualizar dependências; mantendo versões do go.mod."
fi
/usr/local/go/bin/go mod tidy

# Compila com -ldflags="-s -w" para gerar binário mais leve e rápido em RAM
/usr/local/go/bin/go build -ldflags="-s -w" -o oracle_stream_studio_new .
mv oracle_stream_studio_new oracle-stream-studio
chmod +x oracle-stream-studio

# Tuning opcional de Kernel (TCP BBR + Buffers UDP de 16MB para evitar perda no SRTLA)
if [ -f "scripts/optimize_kernel.sh" ]; then
  APPLY_KERNEL="$(ask_choice "Aplicar otimizações de Kernel p/ streaming (TCP BBR + Buffers UDP 16MB)? (s/n)" "s")"
  if [[ "$APPLY_KERNEL" =~ ^[sSyY] ]]; then
    bash scripts/optimize_kernel.sh || warn "Alguns parâmetros sysctl foram ignorados pelo kernel atual."
  fi
fi

# Nota: EOF sem aspas para expandir corretamente WEB_USER e WEB_PASS no systemd!
cat << EOF > /etc/systemd/system/oracle-stream-studio.service
[Unit]
Description=Oracle Stream Studio v2.0 (Ultra-Light 2-OCPU Edition)
After=network.target docker.service
Wants=docker.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/oracle-stream-studio
Environment="PATH=/usr/local/bin:/usr/local/go/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Environment="WEB_USER=${WEB_USER}"
Environment="WEB_PASS=${WEB_PASS}"
LimitNOFILE=65536
ExecStart=/opt/oracle-stream-studio/oracle-stream-studio --port 80
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable oracle-stream-studio
systemctl restart oracle-stream-studio

echo ""
echo -e "${GREEN}================================================================${NC}"
echo -e "${GREEN} ✅ Instalação / Atualização Concluída com Sucesso! (v2.0)      ${NC}"
echo -e "${GREEN}================================================================${NC}"
echo " • Arquitetura:  ${RAW_ARCH} (Otimizado p/ Oracle Free Tier 2 OCPU / 12 GB)"
echo " • Go:           $(/usr/local/go/bin/go version | awk '{print $3}')"
echo " • FFmpeg:       $(ffmpeg -version | head -n1 | awk '{print $1, $2, $3}')"
echo " • Bbox Image:   ${BBOX_IMAGE} (network_mode: host)"
echo " • Painel Web:   http://SEU_IP_ORACLE (Login: ${WEB_USER})"
echo ""
echo " Dicas importantes:"
echo " 1. Suba sua imagem .jpg/.png de fallback direto pelo card 'Cena de Fallback' no Painel!"
echo " 2. Libere na Security List da Oracle Cloud apenas: TCP 80 (ou 443) e UDP 5000 (SRTLA)."
echo " 3. Para atualizar tudo no futuro: sudo bash /opt/oracle-stream-studio/scripts/update.sh"
echo -e "${GREEN}================================================================${NC}"
