# Oracle Stream Studio

**H.265 Stream Failover Relay com Web UI**

Recebe um stream SRT (H.265/HEVC), monitora o fluxo de dados, e troca automaticamente para um vídeo de fallback quando o stream cai. Envia para múltiplos destinos RTMP com passthrough H.265 ou transcoding H.264.

## Arquitetura

```text
Moblin/externo (H.265) → SRTLA://ip-vps:5000/Bbox → SRT:8282 → SRT → [Oracle Stream Studio] → RTMP
                                                                 ↑
                                                            [Fallback .ts]
```

### Como funciona o failover

1. **Oracle Stream Studio** recebe o fluxo SRT via FFmpeg e lê pacotes MPEGTS
2. Monitora continuamente se dados estão chegando
3. Se o SRT parar por mais de 2 segundos (configurável):
   - Troca para o fallback no **próximo keyframe** (zero artefatos)
   - O fallback é um arquivo `.ts` pré-encodado em H.265 com os mesmos parâmetros
4. Quando o SRT volta:
   - Espera um keyframe no fluxo SRT
   - Troca de volta para o stream ao vivo

### Tipos de saída

| Tipo | CPU | Uso |
|------|-----|-----|
| **H.265 Passthrough** | ~0% | YouTube (Enhanced RTMP) |
| **H.264 Transcode** | ~50% (2 cores) | Twitch, Kick, Facebook |

## Instalação rápida (VPS Oracle)

```bash
git clone https://github.com/SEU_USUARIO_GITHUB/oracle-stream-studio.git /opt/oracle-stream-studio
cd /opt/oracle-stream-studio
bash scripts/install.sh
```

O script instala: FFmpeg 7.0+, Go, compila o Oracle Stream Studio, e configura o systemd.

## Uso manual

### 1. Criar o arquivo de fallback

A partir de uma imagem:
```bash
bash scripts/create_fallback.sh brb.png
```

A partir de um vídeo:
```bash
bash scripts/create_fallback.sh -d 120 meu_video.mp4
```

### 2. Compilar

```bash
go mod tidy
go build -o oracle-stream-studio .
```

### 3. Executar

```bash
# Conectar ao SRTLA na porta 8282 (modo caller)
sudo ./oracle-stream-studio --fallback fallback.ts

# Escutar SRT na porta 5000 (modo listener)
sudo ./oracle-stream-studio --srt-mode listener --srt-addr 0.0.0.0:5000 --fallback fallback.ts

# Porta web customizada
sudo ./oracle-stream-studio --fallback fallback.ts --port 8080
```

### 4. Acessar a Web UI

Abra `http://SEU_IP` no navegador.

## Flags

| Flag | Default | Descrição |
|------|---------|-----------|
| `--srt-addr` | `localhost:8282` | Endereço SRT (host:porta) |
| `--srt-mode` | `caller` | Modo SRT: `caller` ou `listener` |
| `--fallback` | *(obrigatório)* | Caminho do arquivo MPEGTS de fallback |
| `--port` | `80` | Porta da Web UI |
| `--srt-timeout` | `2000` | Timeout SRT em ms antes de trocar para fallback |
| `--data-dir` | (dir do binário) | Diretório para configs e dados |

## API REST

| Método | Endpoint | Descrição |
|--------|----------|-----------|
| `GET` | `/api/status` | Status geral (switcher + outputs) |
| `GET` | `/api/outputs` | Lista saídas |
| `POST` | `/api/outputs` | Adiciona saída |
| `PUT` | `/api/outputs/{id}` | Atualiza saída |
| `DELETE` | `/api/outputs/{id}` | Remove saída |
| `POST` | `/api/outputs/{id}/start` | Inicia saída |
| `POST` | `/api/outputs/{id}/stop` | Para saída |

### Exemplo: Adicionar saída YouTube (H.265)

```bash
curl -X POST http://localhost/api/outputs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "YouTube 1",
    "url": "rtmp://a.rtmp.youtube.com/live2",
    "stream_key": "SUA_CHAVE",
    "codec": "h265"
  }'
```

### Exemplo: Adicionar saída Twitch (H.264)

```bash
curl -X POST http://localhost/api/outputs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Twitch",
    "url": "rtmp://live.twitch.tv/app",
    "stream_key": "SUA_CHAVE",
    "codec": "h264",
    "bitrate": 6000,
    "preset": "ultrafast"
  }'
```

## Requisitos do VPS

- **OS:** Ubuntu 22.04+ ou Oracle Linux 9
- **CPU:** 4 cores (ARM Ampere recomendado)
- **RAM:** 4GB+ (24GB ideal)
- **FFmpeg:** 7.0+ (para Enhanced RTMP H.265)
- **Rede:** Porta 80 TCP (Web UI), porta SRT (UDP)

## Requisitos do fallback

O arquivo `.ts` de fallback **deve** ter:
- Codec: H.265 (HEVC)
- Resolução: Mesma do stream (1920x1080)
- FPS: Mesmo do stream (30)
- Keyframe interval: 2 segundos (60 frames)
- Áudio: AAC, 48kHz, stereo
- Container: MPEGTS

Use o script `create_fallback.sh` para garantir compatibilidade.

## Integração Belabox & Agradecimentos

O **Oracle Stream Studio** possui integração nativa de gerenciamento (Start/Stop, Logs e Edição de Configuração) com o [Belabox Receiver](https://github.com/datagutt/belabox-receiver) criado por **Datagutt**. 

Agradecimentos especiais a:
- **Datagutt** pela criação do [Belabox Receiver](https://github.com/datagutt/belabox-receiver), fornecendo uma base excelente e sólida de agregação SRTLA para transmissões IRL (In Real Life) estáveis, e pela colaboração amigável no aprimoramento de regras de segurança contra ataques de negação de serviço (DoS/DDoS) na stack SRTLA.
- Ao projeto oficial de código-fonte aberto [SRTLA](https://github.com/IRLServer/srtla) no qual nossa solução se apoia para o transporte multiplexado.

### Padrões de Entrada/Saída do Bbox

As entradas padrão do Bbox seguem os seguintes formatos de StreamID para autenticação (configurados no `config.json`):

| Direção | Protocolo | Porta | Formato do `streamid` | Exemplo Prático |
|---------|-----------|-------|-----------------------|-----------------|
| **Entrada (Mochila)** | SRTLA | `5000` | `live/stream/nomedostream?srtauth=suachave` | `srtla://ip:5000?streamid=live/stream/belabox?srtauth=belabox` |
| **Saída (Local)** | SRT | `8282` | `play/stream/nomedostream?srtauth=suachave` | `srt://localhost:8282?streamid=play/stream/belabox?srtauth=belabox` |

## Segurança Auditada 🛡️

O Oracle Stream Studio passou por auditoria de segurança e foi otimizado para produção com as seguintes proteções:
- **Autenticação**: Suporte nativo a HTTP Basic Auth para proteger a Web UI.
- **CORS & WebSocket Origin Checks**: Restrição estrita de origens apenas para o mesmo Host de execução e desenvolvimento local (`localhost` / `127.0.0.1`).
- **Proteção contra DoS no WebSocket**: Limitação do tamanho de mensagens para 4KB e limitação de taxa (máximo de 30 msgs/s por conexão com auto-throttling) para mitigar floods.
- **Limites de Upload Inteligentes**: Limite de 10MB para requisições da API REST e limite estrito de 100MB para uploads de arquivos de fallback usando `MaxBytesReader`.
- **Validação de Espaço em Disco**: Bloqueio de novos uploads caso a VPS tenha menos de 500MB de espaço livre em disco, evitando travamentos do sistema operacional.
- **Cabeçalhos de Segurança**: Envio automático de cabeçalhos de proteção moderna, incluindo `Content-Security-Policy` (CSP restritivo), `X-Frame-Options: SAMEORIGIN` (anti-clickjacking) e `X-Content-Type-Options: nosniff`.
- **Sanitização de Caminhos**: Proteção ativa contra Path Traversal no gerenciamento e download de gravações locais.

## Licença

MIT
