#!/bin/bash
# ==============================================================================
# Oracle Stream Studio v2.0 — Script Rápido de Atualização
# Atualiza código-fonte (git pull), Go, FFmpeg, Bbox, dependências e reinicia!
# Uso:
#   Interativo (escolhendo opções): sudo bash scripts/update.sh
#   Automático (tudo mais recente): sudo bash scripts/update.sh --auto
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "${SCRIPT_DIR}/install.sh" --update "$@"
