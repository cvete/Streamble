# Architecture

This document records how Streamble is built and why. Update it whenever a
design decision changes.

## Goals

- Replace a commercial media server (Nimble Streamer + WMSPanel) for TV,
  radio and event streaming, as open source.
- Ingest RTMP and SRT; deliver HLS and Apple Low-Latency HLS.
- Serve at least 5,000 concurrent viewers per deployment (with edges or a CDN).
- Transcode to adaptive bitrate ladders; restream to YouTube, Twitch,
  Facebook and any RTMP/SRT target.
- Be easy to self-host: one binary, `docker compose up`, sane defaults.

Out of scope for v1: WebRTC, DASH, DRM, server-side ad insertion, NDI/SDI.

## Overview

```
 encoders                     streamble                          viewers
 ─────────        ┌──────────────────────────────────────┐      ─────────
 OBS, vMix   ──►  │ ingest/rtmp ─┐                       │
 FFmpeg      ──►  │ ingest/srt  ─┼─► core.Hub ──► output/hls ──►  HLS / LL-HLS
 hardware    ──►  │              │   (fan-out)  ──► (restream, transcode,
 encoders         │              │                     record: next milestones)
                  │ auth ◄───────┘   api ◄── hub + hls stats  ──►  /api/v1
                  └──────────────────────────────────────┘
```

### core: the stream hub (`internal/core`)

The hub is the only thing ingest and output modules share.

- A **Stream** has one publisher and many **Subscriptions**.
- A **Unit** is one timestamped access unit (video) or batch of audio frames.
  Units are immutable once written and shared by all subscribers.
- `Stream.WriteUnit` never blocks the publisher. Each subscriber has its own
  bounded queue. A subscriber that falls behind drops units and resumes at
  the next keyframe, so it never receives a broken group of pictures and
  can never slow down ingest or other outputs.
- New subscribers start at the next keyframe.
- **Hooks** (`OnPublish` / `OnUnpublish`) let outputs attach to new streams.
- **ParamKeeper** makes every H.264/H.265 keyframe self-contained by
  prepending the last seen SPS/PPS (and VPS). Encoders like OBS send them only
  once per RTMP session; outputs that start later need them.

Timestamps in the hub are `time.Duration` (PTS and DTS). Outputs convert to
their own clock rate.

### Ingest (`internal/ingest/*`)

Each ingest authenticates, converts protocol tracks to hub tracks, publishes,
and writes units from a single goroutine per connection.

- **RTMP** uses `bluenviron/gortmplib`. Stream key `name?key=...`.
- **SRT** uses `datarhei/gosrt` and demuxes MPEG-TS with
  `bluenviron/mediacommon`. Stream ID formats are documented in
  `streamid.go`. 33-bit MPEG-TS timestamps are unwrapped with one shared
  `TimeDecoder` per connection, so all tracks share a timeline.

Unsupported codecs (for example MP2/MP3 audio from broadcast encoders) are
logged and skipped; they will be handled by the transcoder.

### Output: HLS (`internal/output/hls`)

One `gohlslib.Muxer` per enabled variant per stream, each fed by its own hub
subscription, so a problem in one variant cannot affect another.

| Variant | Segments | Codecs | Use |
|---|---|---|---|
| `llhls` | fMP4 + parts | H.264, H.265, AAC, Opus | lowest latency, Apple devices, hls.js |
| `fmp4` | fMP4 | same | CDN friendly |
| `mpegts` | MPEG-TS | H.264, AAC | old smart TVs and set-top boxes |

Segments are kept in memory (`segment_count` per variant). HTTP routing is
`/<app>/<stream>[/<variant>]/<file>`.

### Auth (`internal/auth`) and config (`internal/config`)

Apps group streams and carry publish keys, optional per-stream keys and an
optional SRT passphrase. Names are restricted to `[A-Za-z0-9_.-]`, never
starting with a dot, which keeps paths safe for HTTP routing and future
file storage. Keys are compared in constant time and never logged.

### API (`internal/api`) and monitor (`internal/web`)

Read-only JSON status API plus a single embedded HTML page (monitor and
test player using hls.js from a CDN). Bitrates are sampled every 2 seconds.

## Key decisions

| Decision | Choice | Why |
|---|---|---|
| Language | Go | Fast to build and review, static binaries, best open-source media libraries for RTMP/SRT/HLS. Rust's performance edge is not needed: HLS delivery scales horizontally. |
| Media libraries | bluenviron (MIT), gosrt (MIT) | Mature, maintained, used in production by MediaMTX. MIT is compatible with AGPL. |
| Transcoding (planned) | FFmpeg as a supervised child process | GPU support (NVENC, QSV, VAAPI) for free, crash isolation, keeps codec licensing out of our binary. |
| Scale (planned) | Origin + edge caches or CDN | 5,000 viewers at 3 Mbps is ~15 Gbps; no single server should carry that. |
| Panel (planned) | Built into the same binary, Postgres for config and stats | One install, no separate services to run. |
| Licence | AGPL-3.0-only | Keeps hosted forks open. |

## Testing

- Unit tests per package.
- `internal/e2e` starts the real RTMP, SRT and HLS servers on random ports,
  publishes with FFmpeg test sources and decodes the HLS output with FFmpeg.
  It is skipped when FFmpeg is missing and runs in CI.
