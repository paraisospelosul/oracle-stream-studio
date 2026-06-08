# Oracle Stream Studio Security Policy

## Deployment Recommendations

Oracle Stream Studio is designed to be a lightweight, zero-dependency failover relay for live video streaming. By default, it exposes a Web UI for management and control.

If you are deploying Oracle Stream Studio on a public VPS (Virtual Private Server) or any network accessible from the open internet, **you must follow these security guidelines**:

### 1. Enable Basic Authentication (Mandatory)
Never expose the Oracle Stream Studio Web UI to the public internet without authentication.
Use the built-in HTTP Basic Auth feature by passing the `--web-user` and `--web-pass` flags when starting the server, or by answering the prompts during the `./install.sh` automated installation.

### 2. Encrypt Traffic with HTTPS or a VPN
Basic Authentication transmits credentials in plain text. To prevent network sniffing and credential theft:
- **VPN / Tailscale (Recommended):** The easiest and most secure method. Install [Tailscale](https://tailscale.com/) on your VPS and your local machine. Access the Web UI exclusively via the secure `100.x.x.x` Tailscale IP. This guarantees end-to-end encryption without needing to configure domain names or SSL certificates.
- **Reverse Proxy (Nginx / Caddy):** If you need public access via a domain name (e.g., `panel.yourdomain.com`), configure a reverse proxy like Nginx or Caddy in front of Oracle Stream Studio (Port 80) and secure it with a free Let's Encrypt SSL certificate.

### 3. Belabox Receiver (Docker) Security
Oracle Stream Studio integrates with the `datagutt/belabox-receiver` Docker container. The automated `install.sh` script applies several security hardnenings by default:
- Drops container privileges (`no-new-privileges: true`).
- Restricts resources (RAM, CPU, PID limits) to prevent DDoS attacks from taking down the host system.
- Mounts `/tmp` and `/var/log` as read-only (`noexec`, `nosuid`) `tmpfs` volumes.

Keep the authentication keys in your `config.json` strong and unique to prevent unauthorized video injection.

## Reporting a Vulnerability

If you discover a security vulnerability within Oracle Stream Studio, please do not open a public issue.
Instead, send a private message or email to the repository maintainer. Security issues will be treated with high priority.
