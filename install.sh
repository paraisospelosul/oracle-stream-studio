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

# Create directory structure
echo "[4/6] Setting up directories..."
mkdir -p /opt/oracle-stream-studio
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

# Build and start Oracle Stream Studio
echo "[6/6] Building and configuring Oracle Stream Studio..."

# Check if source code exists locally, otherwise git clone (assuming local copy for now)
if [ -f "main.go" ]; then
    $(command -v go) build -o oracle_stream_studio_final .
    mv oracle_stream_studio_final oracle-stream-studio
    chmod +x oracle-stream-studio
else
    echo "Warning: Source files not found in /opt/oracle-stream-studio. Make sure to copy them before running the service."
fi

# Create systemd service
cat << 'EOF' > /etc/systemd/system/oracle-stream-studio.service
[Unit]
Description=Oracle Stream Studio Server
After=network.target docker.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/oracle-stream-studio
Environment="WEB_USER=${WEB_USER}"
Environment="WEB_PASS=${WEB_PASS}"
ExecStart=/opt/oracle-stream-studio/oracle-stream-studio
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable oracle-stream-studio
# Don't start automatically yet, user might need to copy files first if they are not there
# systemctl start oracle-stream-studio

echo "==========================================="
echo " Installation Complete!"
echo " "
echo " Next steps:"
echo " 1. Make sure all .go files and the 'web' folder are in /opt/oracle-stream-studio"
echo " 2. Compile: cd /opt/oracle-stream-studio && go build -o oracle-stream-studio ."
echo " 3. Start service: sudo systemctl start oracle-stream-studio"
echo " 4. Oracle Stream Studio Web UI will be on port 80"
echo " 5. Bbox receiver Web UI will be on port 8181"
echo "==========================================="
