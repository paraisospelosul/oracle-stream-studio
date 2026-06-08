# ADR-002: SRT Failover Strategy and Advanced Scene Switching

## Context
When the primary SRT live stream drops or has a significant bitrate drop, the relay must switch to a fallback scene instantly to prevent stream death at the RTMP destinations (like YouTube or Twitch). Previous versions waited for H.265 GOP keyframes to transition, causing up to 2 seconds of latency.

## Decision
1. **Pipeline Pre-warming**: Run independent subprocesses (`InputProcess`) for the live SRT feed and fallback streams, caching their PAT/PMT metadata.
2. **Atomic Pointer Swapping**: Distribute stream packets through a `PipelineRouter` which switches the active stream source in the hot path via an atomic pointer swap, achieving ~0ms delay.
3. **Transcoding Bridge (Force IDR)**: When switching from fallback to a new live stream that hasn't sent a keyframe, route the first 100ms through a transient FFmpeg decode-encode bridge to force-generate an IDR keyframe, ensuring immediate alignment.
4. **PCR/PTS Alignment**: Pass all packets through a `PTSRemapper` to rewrite PCR and PTS timestamps in-place, preventing players from freezing due to timeline discontinuities.

## Consequences
- Clean, artifact-free, sub-second failover between inputs.
- Constant PCR timeline prevents RTMP ingest servers from rejecting the feed.
- High-efficiency passthrough mode is maintained during standard operation.
