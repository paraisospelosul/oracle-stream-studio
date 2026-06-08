# ADR-001: WebSocket Message Rate Limiting and Size Restrictions

## Context
The application exposes a WebSocket endpoint `/ws` for real-time status updates and watermark overlay positioning from the frontend dashboard. Without restrictions, this endpoint is susceptible to Denial of Service (DoS) attacks via payload flooding or memory exhaustion (sending large buffers).

## Decision
1. **Message Size Cap**: Enforce a strict physical cap on incoming WebSocket message sizes at **4 KB** (4,096 bytes). Status updates and drag overlay coordinates are small JSON structures (< 1 KB), making this threshold safe and effective.
2. **Rate Throttling**: Restrict incoming message frequency to a maximum of **30 messages per second** per client connection.
3. **Graceful Dropping**: If a client sends messages exceeding the threshold (e.g. rapid watermark coordinate adjustments), excess packets are discarded to protect thread safety and CPU utilization without disconnecting the user.

## Consequences
- Prevents resource exhaustion from malicious or bugged clients.
- Protects the switcher loop from CPU starvation.
- Retains smooth UI interactions since coordinate updates fit comfortably within 30 Hz.
