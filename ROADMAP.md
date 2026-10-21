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

## Milestone 2: transcoding, restreaming, more formats ✅

- [x] MP3, MP2 and AC-3 audio carried end to end
- [x] FFmpeg process supervisor (restart with backoff, last error in status)
- [x] Transcode profiles: quality ladders, audio-only AAC or MP3 renditions
- [x] Adaptive bitrate master playlist (`abr.m3u8`)
- [x] GPU encoding options: NVENC, QSV, VAAPI (argument generation tested; needs real hardware testing)
- [x] MP2/MP3/AC-3 sources made web-playable through AAC renditions
- [x] Restream to RTMP/RTMPS/SRT targets (YouTube, Twitch, Facebook) with auto-reconnect
- [x] Restream to Icecast (MP3/AAC)
- [x] RTMP and SRT playback from the server
- [x] Continuous HTTP outputs: `.ts`, `.mp3`, `.aac`
- [x] Pull ingest: HTTP `.ts`, Icecast/Shoutcast MP3/AAC, HLS, UDP unicast/multicast, SRT, RTMP
- [x] Play keys for non-HLS playback
- [ ] Verify with real platforms: YouTube, Twitch, Facebook, a real Icecast server
- [ ] Verify GPU transcoding on NVIDIA and Intel hardware
- [ ] FFmpeg in the Docker image (currently host install)
- [ ] Transcode progress (fps, speed) from FFmpeg `-progress`
- [ ] Icecast metadata (now playing title) for radio
- [ ] Per-platform codec checks for RTMP push (warn before connecting)

## Milestone 3: control panel

- [ ] Postgres storage; config editable at runtime, hot reload without dropping streams
- [ ] Web UI: apps, streams, keys, transcode profiles, restream targets, pull sources
- [ ] Monitor page: show renditions, restream and pull status
- [ ] Users and roles
- [ ] Stats history: viewers, bandwidth, countries (GeoLite2), per stream and app
- [ ] Webhooks: publish start/stop, auth callbacks
- [ ] REST API for everything the UI does

## Milestone 4: recording, VOD and protection

- [ ] Recording to MP4/fMP4 with rotation
- [ ] DVR window (timeshift) in HLS
- [ ] VOD HLS from recorded files
- [ ] Signed playback URLs with expiry (including HLS)
- [ ] Referer/hotlink rules, geo allow/deny lists

## Milestone 5: scale and v0.1 release

- [ ] Edge mode: pull HLS from an origin and cache segments
- [ ] Prometheus metrics
- [ ] Load test at 5,000 concurrent viewers, publish results
- [ ] Packaging: release binaries, Docker image on a registry, install docs
- [ ] Make the repository public

## Later

WebRTC (WHIP/WHEP), DASH, DRM, server-side ad insertion, AI captions, NDI.
