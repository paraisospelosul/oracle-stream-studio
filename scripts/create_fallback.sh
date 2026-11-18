#!/bin/bash
set -e

OUTPUT="fallback.ts"
RESOLUTION="1920:1080"
INPUT=""

show_help() {
  echo "Uso: $0 [opções] <imagem_ou_video>"
  echo ""
  echo "Nota: No Oracle Stream Studio v2.0 você pode subir .jpg/.png direto pelo Painel Web!"
  echo "Este script é opcional caso queira pré-gerar um clipe .ts manualmente via terminal."
  echo ""
  echo "Opções:"
  echo "  -r, --res <1080p|720p>       Resolução de saída (padrão: 1080p)"
  echo "  -o, --output <arquivo.ts>    Caminho de saída (padrão: fallback.ts)"
  echo "  -h, --help                   Exibe esta ajuda"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -r|--res)
      if [[ "$2" == "720p" ]]; then
        RESOLUTION="1280:720"
      else
        RESOLUTION="1920:1080"
      fi
      shift 2
      ;;
    -o|--output)
      OUTPUT="$2"
      shift 2
      ;;
    -h|--help)
      show_help
      ;;
    *)
      INPUT="$1"
      shift
      ;;
  esac
done

if [ -z "$INPUT" ] || [ ! -f "$INPUT" ]; then
  echo "Erro: Arquivo de entrada inválido."
  show_help
fi

W="${RESOLUTION%%:*}"
H="${RESOLUTION##*:}"
VF="scale=${W}:${H}:force_original_aspect_ratio=decrease,pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2:black,setsar=1,format=yuv420p"
X265_PARAMS="keyint=48:min-keyint=48:bframes=0:scenecut=0:repeat-headers=1:pools=1:frame-threads=1"

EXT=$(echo "${INPUT##*.}" | tr '[:upper:]' '[:lower:]')
if [[ "$EXT" == "png" || "$EXT" == "jpg" || "$EXT" == "jpeg" || "$EXT" == "webp" || "$EXT" == "bmp" ]]; then
  echo "Gerando RAM Slate de 1.6s (${W}x${H}, 48 quadros exatos) a partir da imagem..."
  ffmpeg -hide_banner -loglevel error -y \
    -loop 1 -framerate 30 -t 1.6 -i "$INPUT" \
    -f lavfi -t 1.6 -i anullsrc=channel_layout=stereo:sample_rate=48000 \
    -map 0:v:0 -map 1:a:0 \
    -vf "$VF" -r 30 -frames:v 48 \
    -c:v libx265 -preset ultrafast -x265-params "$X265_PARAMS" \
    -c:a aac -b:a 128k -ar 48000 -ac 2 \
    -mpegts_pmt_start_pid 0x1000 -mpegts_start_pid 0x0100 -pes_payload_size 0 \
    -f mpegts "$OUTPUT"
else
  echo "Normalizando vídeo curto (até 9.6s) para formato RAM Slate (${W}x${H})..."
  ffmpeg -hide_banner -loglevel error -y \
    -t 9.6 -i "$INPUT" \
    -f lavfi -t 9.6 -i anullsrc=channel_layout=stereo:sample_rate=48000 \
    -map 0:v:0 -map 1:a:0 \
    -vf "$VF" -r 30 -shortest \
    -c:v libx265 -preset ultrafast -x265-params "$X265_PARAMS" \
    -c:a aac -b:a 128k -ar 48000 -ac 2 \
    -mpegts_pmt_start_pid 0x1000 -mpegts_start_pid 0x0100 -pes_payload_size 0 \
    -f mpegts "$OUTPUT"
fi

echo "Pronto! Slate H.265 gerado em: $OUTPUT"
