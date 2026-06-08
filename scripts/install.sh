#!/bin/bash
set -e

# Make sure the script is run as root
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root (sudo ./install.sh)"
  exit 1
fi

echo "==========================================="
echo " Installing Oracle Stream Studio + Belabox Receiver"
echo "==========================================="
echo ""
echo "Please set up a username and password to protect the Web Panel:"
read -p "Username (default: belabox): " WEB_USER
WEB_USER=${WEB_USER:-belabox}
read -s -p "Password (default: belabox): " WEB_PASS
echo ""
WEB_PASS=${WEB_PASS:-belabox}

# Update and install dependencies
echo "[1/6] Installing system dependencies..."
apt-get update
apt-get install -y ffmpeg curl wget git jq net-tools

# Install Docker if not present
if ! command -v docker &> /dev/null; then
    echo "[2/6] Installing Docker..."
    curl -fsSL https://get.docker.com -o get-docker.sh
    sh get-docker.sh
    rm get-docker.sh
else
    echo "[2/6] Docker already installed, skipping..."
fi

# Install Go if not present
if ! command -v go &> /dev/null; then
    echo "[3/6] Installing Go..."
    wget https://go.dev/dl/go1.22.2.linux-amd64.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf go1.22.2.linux-amd64.tar.gz
    rm go1.22.2.linux-amd64.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
else
    echo "[3/6] Go already installed, skipping..."
fi

# Set path for this session just in case
export PATH=$PATH:/usr/local/go/bin

# Create directory structure
echo "[4/6] Setting up directories..."
mkdir -p /opt/oracle-stream-studio

# Copy repo files to /opt/oracle-stream-studio if run from a different location
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

if [ "$REPO_DIR" != "/opt/oracle-stream-studio" ] && [ -f "$REPO_DIR/main.go" ]; then
    echo "Copying repository files from $REPO_DIR to /opt/oracle-stream-studio..."
    # Copy files excluding hidden ones to avoid copying .git/ if we don't want to, but copying everything is fine
    cp -r "$REPO_DIR"/* /opt/oracle-stream-studio/
fi

cd /opt/oracle-stream-studio

# Setup Belabox Receiver initial files
echo "[5/6] Setting up Belabox Receiver (Docker)..."

cat << 'EOF' > docker-compose.yml
services:
  bbox:
    image: datagutt/belabox-receiver:latest
    container_name: bbox
    restart: unless-stopped
    ports:
      - "5000:5000/udp"
      - "8282:8282/udp"
      - "8181:8181/tcp"
      - "3000:3000/tcp"
    volumes:
      - ./config.json:/app/config.json:ro
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp:size=64m,noexec,nosuid,nodev
      - /var/log:size=64m,noexec,nosuid,nodev
    pids_limit: 256
    mem_limit: 1g
    cpus: "2.0"
    logging:
      driver: json-file
      options:
        max-size: "10m"
EOF

cat << 'EOF' > config.json
{
  "auth": [
    {
      "user": "belabox",
      "key": "belabox"
    }
  ]
}
EOF

# Start Belabox Receiver Docker container
if command -v docker &> /dev/null; then
    echo "Starting Belabox Receiver via Docker..."
    if docker compose version &> /dev/null; then
        docker compose up -d
    else
        docker-compose up -d
    fi
fi

# Build and start Oracle Stream Studio
echo "[6/6] Building and configuring Oracle Stream Studio..."

BUILD_SUCCESS=false
if [ -f "main.go" ]; then
    /usr/local/go/bin/go build -o oracle_stream_studio_final .
    mv oracle_stream_studio_final oracle-stream-studio
    chmod +x oracle-stream-studio
    BUILD_SUCCESS=true
else
    echo "Warning: Source files not found in /opt/oracle-stream-studio. Make sure to copy them before running the service."
fi

# Generate default 60s fallback video in H.265 if it does not exist
if [ ! -f "fallback.ts" ]; then
    echo ""
    echo "⚠️  WARNING: No 'fallback.ts' file found. Generating a default 60s fallback video..."
    echo "⚠️  This encoding process uses H.265 and may take a few minutes on lower-performance servers (e.g. Oracle Cloud Free Tier). Please wait..."
    echo ""
    
    ffmpeg -y -f lavfi -i color=c=black:s=1920x1080:r=30 -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000 \
      -c:v libx265 -pix_fmt yuv420p -preset ultrafast -r 30 -g 60 -keyint_min 60 \
      -x265-params "keyint=60:min-keyint=60:no-scenecut=1" \
      -c:a aac -b:a 128k -ar 48000 -ac 2 \
      -shortest -t 60 -f mpegts fallback.ts
      
    echo "✅ Default 60-second fallback generated at /opt/oracle-stream-studio/fallback.ts"
fi

# Create systemd service
cat << 'EOF' > /etc/systemd/system/oracle-stream-studio.service
[Unit]
Description=Oracle Stream Studio Server
After=network.target docker.service
Wants=docker.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/oracle-stream-studio
Environment="WEB_USER=${WEB_USER}"
Environment="WEB_PASS=${WEB_PASS}"
ExecStart=/opt/oracle-stream-studio/oracle-stream-studio --port 80
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable oracle-stream-studio

if [ "$BUILD_SUCCESS" = true ]; then
    echo "Starting Oracle Stream Studio service..."
    systemctl start oracle-stream-studio
fi

echo "==========================================="
echo " Installation Complete!"
echo " "
echo " Next steps:"
echo " 1. Web UI is now running on port 80."
echo " 2. Belabox Receiver is running in Docker (UDP ports 5000 and 8282)."
echo " 3. You can replace /opt/oracle-stream-studio/fallback.ts with your own fallback video."
echo " 4. Service commands: sudo systemctl status/start/stop oracle-stream-studio"
echo "==========================================="
