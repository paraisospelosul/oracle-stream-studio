# Changelog

All notable changes to this project will be documented in this file.

## [1.5.0] - 2026-06-08

### Added
- Rebranded project to **Oracle Stream Studio v1.5**.
- Added standard `LICENSE` file (MIT License).
- Added GitHub Actions CI pipeline (`.github/workflows/test.yml`) for automated testing and builds.
- Added a security policy documentation (`SECURITY.md`).

### Changed
- Refactored all installation directory paths to `/opt/oracle-stream-studio`.
- Renamed the systemd service to `oracle-stream-studio`.
- Updated Belabox Receiver configs to default credentials (`belabox`/`belabox`).

### Fixed
- **Security**: Added strict CORS and WebSocket Origin validation to allow only same-host and local development connections.
- **Security**: Mitigated DoS attacks on WebSocket endpoint `/ws` by limiting message sizes to 4KB and throttling message rates to 30 msgs/sec.
- **Security**: Configured body size limits to allow up to 100MB for media uploads while retaining 10MB for general API requests.
- **Security**: Implemented a disk space check (`checkDiskSpace`) before file uploads to reject writes if disk space is below 500MB, preventing server exhaustion.
- **Security**: Configured modern security headers in HTTP responses: `Content-Security-Policy`, `X-Frame-Options: SAMEORIGIN` (anti-clickjacking), and `X-Content-Type-Options: nosniff`.
