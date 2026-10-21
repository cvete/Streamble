# Roadmap

Status of planned work. Each milestone ends with a tagged release. Keep this
file current: tick items as they land and add new ones as they come up.

## Milestone 1: core engine ✅

- [x] Stream hub with non-blocking fan-out and keyframe resync
- [x] RTMP ingest (H.264, H.265, AAC, Opus) with publish keys
- [x] SRT ingest (MPEG-TS) with stream ID parsing and encryption
- [x] HLS output: Low-Latency HLS, fMP4 HLS, MPEG-TS HLS
- [x] Audio-only (radio) streams
- [x] Status API, viewer estimates, bitrate sampling
- [x] Built-in monitor and test player page
- [x] End-to-end tests with FFmpeg, CI, Docker image
- [ ] Verify with real encoders: OBS, vMix, a hardware SRT encoder
- [ ] Verify playback on iPhone/Safari, Android, a smart TV

## Milestone 2: transcoding and restreaming

- [ ] FFmpeg process supervisor (start, restart with backoff, stats from `-progress`)
- [ ] Transcode profiles: ABR ladders (e.g. 1080p/720p/480p), audio-only profiles
- [ ] Multi-rendition HLS (one multivariant playlist for the whole ladder)
- [ ] GPU encoding: NVENC, QSV, VAAPI
- [ ] Audio transcoding for MP2/MP3/AC-3 inputs (broadcast encoders)
- [ ] Restream to RTMP/RTMPS/SRT targets (YouTube, Twitch, Facebook) with auto-reconnect
- [ ] RTMP and SRT playback (pull) from the server
- [ ] Pull ingest: fetch a stream from an RTMP/SRT/HLS URL
- [ ] Icecast output for radio (MP3/AAC)

## Milestone 3: control panel

- [ ] Postgres storage; config editable at runtime, hot reload without dropping streams
- [ ] Web UI: apps, streams, keys, transcode profiles, restream targets
- [ ] Users and roles
- [ ] Stats history: viewers, bandwidth, countries (GeoLite2), per stream and app
- [ ] Webhooks: publish start/stop, auth callbacks
- [ ] REST API for everything the UI does

## Milestone 4: recording, VOD and protection

- [ ] Recording to MP4/fMP4 with rotation
- [ ] DVR window (timeshift) in HLS
- [ ] VOD HLS from recorded files
- [ ] Signed playback URLs with expiry
- [ ] Referer/hotlink rules, geo allow/deny lists

## Milestone 5: scale and v0.1 release

- [ ] Edge mode: pull HLS from an origin and cache segments
- [ ] Prometheus metrics
- [ ] Load test at 5,000 concurrent viewers, publish results
- [ ] Packaging: release binaries, Docker image on a registry, install docs
- [ ] Make the repository public

## Later

WebRTC (WHIP/WHEP), DASH, DRM, server-side ad insertion, AI captions, NDI.
