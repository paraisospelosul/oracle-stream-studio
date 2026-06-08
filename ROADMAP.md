# Oracle Stream Studio Roadmap

This document outlines the planned releases, features, and enhancements for Oracle Stream Studio.

---

## 🚀 Released Versions

### v1.5.0
- Refactored project name to Oracle Stream Studio.
- Implemented CORS and WebSocket Origin validation.
- Mitigated DoS attacks on WebSocket `/ws` via throttling (30 msgs/s) and message size limits (4KB).
- Enforced file upload limits (100MB) and disk space pre-checks (`checkDiskSpace`).
- Integrated security headers (CSP, X-Frame-Options, X-Content-Type-Options) and audit logs (`[AUDIT]`).

### v1.6.0 (Current)
- Increased test coverage to >60% (focusing on output manager, switcher state machine, and recorder).
- Added visual shields/badges to the documentation.
- Created contributing and roadmap guidelines.
- Configured CI/CD automated test coverage profiling.

---

## 🗺️ Future Roadmap

### v1.7.0 (Q3 2026)
- [ ] **Robust WebSocket Reconnection**: Auto-reconnect client panels on disconnection with exponential backoff.
- [ ] **Advanced Metrics & Observability**: Expose Prometheus endpoints `/metrics` for system health tracking.
- [ ] **Extended API v2**: Standardized JSON REST API endpoints with API token authentication for remote control.

### v1.8.0 (Q4 2026)
- [ ] **HLS & WebRTC Low-Latency Previews**: Replace static jpeg preview polling with a low-latency WebRTC/HLS sub-second live view in the dashboard.
- [ ] **Audio Channel Mapping**: UI routing matrix to map specific audio channels from inputs to distinct RTMP stream output channels.

### v2.0.0 (Q1 2027)
- [ ] **Multi-Region Failover**: Run multiple Oracle Stream Studio nodes and synchronize configuration state for high-availability setups.
- [ ] **Dockerized Monolithic Deployments**: Official Docker Compose configuration for instant deployment on Kubernetes and single-command local setup.
