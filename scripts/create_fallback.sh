#!/bin/bash
set -e

# Default values
DURATION=60
OUTPUT="fallback.ts"
INPUT=""

# Help message
show_help() {
  echo "Uso: $0 [opções] <arquivo_de_entrada>"
  echo ""
  echo "Opções:"
  echo "  -d, --duration <segundos>   Duração do fallback em segundos (padrão: 60)"
  echo "  -o, --output <arquivo.ts>    Caminho do arquivo de saída (padrão: fallback.ts)"
  echo "  -h, --help                  Exibe esta ajuda"
  echo ""
  echo "Exemplos:"
  echo "  $0 minha_imagem.png"
  echo "  $0 -d 120 meu_video.mp4"
  exit 0
}

# Parse options
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d|--duration)
      DURATION="$2"
      shift 2
      ;;
    -o|--output)
      OUTPUT="$2"
      shift 2
      ;;
    -h|--help)
      show_help
      ;;
    -*)
      echo "Opção inválida: $1"
      show_help
      ;;
    *)
      INPUT="$1"
      shift
      ;;
  esac
done

if [ -z "$INPUT" ]; then
  echo "Erro: Nenhum arquivo de entrada especificado."
  show_help
fi

if [ ! -f "$INPUT" ]; then
  echo "Erro: Arquivo '$INPUT' não encontrado."
  exit 1
fi

# Verify if ffmpeg is installed
if ! command -v ffmpeg &> /dev/null; then
  echo "Erro: ffmpeg não está instalado. Por favor, instale o ffmpeg primeiro."
  exit 1
fi

echo "========================================================="
echo " Gerando arquivo de fallback H.265 compatível"
echo " Entrada:  $INPUT"
echo " Saída:    $OUTPUT"
echo " Duração:  $DURATION segundos"
echo "========================================================="

# Detect input type (image or video) based on extension
IS_IMAGE=false
EXT="${INPUT##*.}"
EXT=$(echo "$EXT" | tr '[:upper:]' '[:lower:]')

if [[ "$EXT" == "png" || "$EXT" == "jpg" || "$EXT" == "jpeg" || "$EXT" == "bmp" || "$EXT" == "webp" ]]; then
  IS_IMAGE=true
fi

# Generate the H.265 MPEG-TS fallback
if [ "$IS_IMAGE" = true ]; then
  echo "Processando entrada como IMAGEM estática..."
  ffmpeg -y -loop 1 -i "$INPUT" -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000 \
    -c:v libx265 -pix_fmt yuv420p -preset medium -r 30 -g 60 -keyint_min 60 \
    -x265-params "keyint=60:min-keyint=60:no-scenecut=1" \
    -c:a aac -b:a 128k -ar 48000 -ac 2 \
    -vf "scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:(ow-iw)/2:(oh-ih)/2" \
    -shortest -t "$DURATION" -f mpegts "$OUTPUT"
else
  echo "Processando entrada como VÍDEO..."
  # Check if the video has audio streams using ffprobe
  HAS_AUDIO=$(ffprobe -v error -select_streams a -show_entries stream=codec_name -of default=noprint_wrappers=1:nokey=1 "$INPUT" || true)
  
  if [ -z "$HAS_AUDIO" ]; then
    echo "Vídeo de entrada não possui áudio. Injetando áudio silencioso..."
    ffmpeg -y -i "$INPUT" -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000 \
      -c:v libx265 -pix_fmt yuv420p -preset medium -r 30 -g 60 -keyint_min 60 \
      -x265-params "keyint=60:min-keyint=60:no-scenecut=1" \
      -c:a aac -b:a 128k -ar 48000 -ac 2 \
      -vf "scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:(ow-iw)/2:(oh-ih)/2" \
      -t "$DURATION" -f mpegts "$OUTPUT"
  else
    ffmpeg -y -i "$INPUT" \
      -c:v libx265 -pix_fmt yuv420p -preset medium -r 30 -g 60 -keyint_min 60 \
      -x265-params "keyint=60:min-keyint=60:no-scenecut=1" \
      -c:a aac -b:a 128k -ar 48000 -ac 2 \
      -vf "scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:(ow-iw)/2:(oh-ih)/2" \
      -t "$DURATION" -f mpegts "$OUTPUT"
  fi
fi

echo ""
echo "Sucesso! Fallback H.265 gerado em: $OUTPUT"
echo "Especificações do arquivo gerado:"
echo " - Codec: H.265 (HEVC)"
echo " - Resolução: 1920x1080"
echo " - FPS: 30 (GOP 60 / 2 segundos de intervalo de keyframe)"
echo " - Áudio: AAC 48kHz Stereo"
