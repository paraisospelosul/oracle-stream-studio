# ADR-003: Strict HTTP Security Headers and CORS Policies

## Context
The application exposes a web interface to configure RTMP destinations, upload fallbacks/watermarks, and manage stream controls. Because it runs self-hosted on a VPS, it is exposed to potential clickjacking, cross-site scripting (XSS), and cross-origin request forgery (CSRF) attempts.

## Decision
1. **Security Headers Middleware**: Apply a global middleware setting:
   - `Content-Security-Policy (CSP)`: Restricted to `'self'` and specific whitelisted CDNs for Google Fonts and jsDelivr assets. Inline scripts/styles are minimized.
   - `X-Frame-Options`: Set to `SAMEORIGIN` to completely block clickjacking inside iframe containers.
   - `X-Content-Type-Options`: Set to `nosniff` to prevent MIME-type sniffing exploits.
2. **CORS Restrictions**: Check origins on the API and WebSocket handlers. Only requests originating from the same host, `localhost`, or `127.0.0.1` are allowed. All foreign origins are rejected.
3. **Upload Security**: Protect writes with a disk space check (`checkDiskSpace`), rejecting file uploads if free space on the destination partition is under 500 MB.

## Consequences
- The control panel is locked down against common web vulnerability vectors.
- Disables access from malicious third-party portals trying to interact with the backend API.
- Safe execution in public and private self-hosted environments.
