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

Everything below is clean-room work from public protocol specifications and
documentation; no proprietary code or binaries were used.

## Overview

```
 sources                         streamble                              viewers / targets
 ───────          ┌──────────────────────────────────────────────┐      ─────────────────
 OBS, vMix   ──►  │ ingest/rtmp ─┐                 ┌► output/hls ──────► HLS, LL-HLS, ABR
 encoders    ──►  │ ingest/srt  ─┤                 ├► output/httpstream ► .ts .mp3 .aac
 .ts .m3u8   ──►  │ ingest/pull ─┼─► core.Hub ─────┼► ingest/rtmp play ─► RTMP
 Icecast,UDP      │              │   (fan-out)     ├► ingest/srt read ──► SRT
                  │              │      ▲          ├► output/push ──────► YouTube, Twitch,
                  │              │      │          │                      SRT, Icecast
                  │              │      └─renditions── transcode ◄┘ (FFmpeg per stream)
                  │ auth ◄───────┘   api ◄── status of every module ──────► /api/v1
                  └──────────────────────────────────────────────┘
```

Every arrow into the hub publishes a stream; every arrow out of it is a
subscription. The transcoder is both: it subscribes to a source and
publishes its renditions back as new streams, which then get every output
like any other stream.

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

- **Pull** (`internal/ingest/pull`) fetches configured `sources` and
  publishes them, reconnecting with backoff (1 s doubling to 30 s, reset
  after 30 s live). HTTP format detection uses Content-Type, then the URL
  extension, then the first bytes. MP3 and ADTS AAC radio streams are parsed
  frame by frame (ID3 skip, resync after garbage) with timestamps from
  sample counts. HLS uses gohlslib's client. UDP joins multicast groups.
  10 s of silence counts as a failure on every transport.

### Conversion helpers (`internal/tsconv`, `internal/rtmpconv`)

Shared so each protocol is implemented once.

- `tsconv.Demux` reads MPEG-TS from any reader (SRT, UDP, HTTP, FFmpeg
  pipes) into the hub; `tsconv.Muxer` writes hub units as MPEG-TS. MPEG-TS
  carries every hub codec. `PacketWriter` cuts output into 1316-byte
  packets for SRT.
- `rtmpconv.Send` sends a hub stream over any RTMP connection (server-side
  playback or client-side push). Streams from MPEG-TS sources carry
  SPS/PPS only in-band, so it takes them from the first keyframe, which
  ParamKeeper guarantees has them. FLV cannot carry MP1/MP2 audio; that is
  reported as a permanent `ErrMPEGAudioLayer`.

### Transcode (`internal/transcode`)

One FFmpeg process per source stream in an app with a `transcode` profile.

- The source is fed as MPEG-TS on stdin. Each rendition is written by FFmpeg
  to its own pipe (`ExtraFiles`, `pipe:3`, `pipe:4`, ...) and demuxed back
  into the hub as `<app>/<stream>_<rendition>` with protocol `transcode`.
- Keyframes are forced on the same time grid in every rendition, and the
  scale filter never upscales.
- FFmpeg is restarted with backoff if it exits while the source is live;
  the last stderr line becomes `last_error`.
- Streams with protocol `transcode` are never transcoded again, and are only
  pushed when listed by name.

### Output: push (`internal/output/push`)

One goroutine per (stream, target). Each attempt takes a fresh subscription
so it starts at a keyframe. RTMP uses gortmplib's client (the last URL path
segment plus query becomes the stream key), SRT uses gosrt in caller mode,
Icecast uses the 2.4 PUT source protocol on a raw connection. Failures that
retrying cannot fix (bad URL, no codec the target can carry) set the state
to `failed`; everything else retries with backoff until the stream ends.
URLs and error messages are scrubbed of keys before logging.

### Output: HTTP streams, RTMP and SRT playback

`internal/output/httpstream` serves `/<app>/<stream>.ts` (any codec),
`.mp3` (MPEG audio frames) and `.aac` (ADTS) as endless responses with a
per-write deadline. RTMP playback and SRT read mode live in the ingest
servers because they share the listeners. All of them check the app's
`play_key` and count bytes into the stream's outgoing counter.

### Output: HLS (`internal/output/hls`)

One `gohlslib.Muxer` per enabled variant per stream, each fed by its own hub
subscription, so a problem in one variant cannot affect another.

`/<app>/<stream>/abr.m3u8` combines the multivariant playlists of a
transcoded stream's renditions (rewritten to relative URIs, audio groups
renamed per rendition) into one adaptive playlist.

| Variant | Segments | Codecs | Use |
|---|---|---|---|
| `llhls` | fMP4 + parts | H.264, H.265, AAC, Opus | lowest latency, Apple devices, hls.js |
| `fmp4` | fMP4 | same | CDN friendly |
| `mpegts` | MPEG-TS | H.264, AAC | old smart TVs and set-top boxes |

Segments are kept in memory (`segment_count` per variant). HTTP routing is
`/<app>/<stream>[/<variant>]/<file>`.

### Auth (`internal/auth`) and config (`internal/config`)

Apps group streams and carry publish keys, optional per-stream keys, an
optional play key, an optional SRT passphrase, a transcode profile and push
targets. Names are restricted to `[A-Za-z0-9_.-]`, never
starting with a dot, which keeps paths safe for HTTP routing and future
file storage. Keys are compared in constant time and never logged.

### API (`internal/api`) and monitor (`internal/web`)

Read-only JSON status API plus a single embedded HTML page (monitor and
test player using hls.js from a CDN). Bitrates are sampled every 2 seconds.
Modules register their status with `API.AddStatus` (`/api/v1/transcodes`,
`/pushes`, `/sources`).

## Key decisions

| Decision | Choice | Why |
|---|---|---|
| Language | Go | Fast to build and review, static binaries, best open-source media libraries for RTMP/SRT/HLS. Rust's performance edge is not needed: HLS delivery scales horizontally. |
| Media libraries | bluenviron (MIT), gosrt (MIT) | Mature, maintained, used in production by MediaMTX. MIT is compatible with AGPL. |
| Transcoding | FFmpeg as a supervised child process, MPEG-TS over pipes | GPU support (NVENC, QSV, VAAPI) for free, crash isolation, keeps codec licensing out of our binary. |
| Scale (planned) | Origin + edge caches or CDN | 5,000 viewers at 3 Mbps is ~15 Gbps; no single server should carry that. |
| Panel (planned) | Built into the same binary, Postgres for config and stats | One install, no separate services to run. |
| Licence | AGPL-3.0-only | Keeps hosted forks open. |

## Testing

- Unit tests per package.
- `internal/e2e` starts the real servers on random ports, publishes with
  FFmpeg test sources (RTMP, SRT, HTTP, UDP, HLS, Icecast-style servers) and
  decodes every output with FFmpeg: HLS variants, ABR, `.ts`, `.mp3`, `.aac`,
  RTMP and SRT playback, pushes between two in-process servers, and
  transcoder renditions. Skipped when FFmpeg is missing; runs in CI.

## Known library behaviour

- gosrt v0.12 ignores `SetReadDeadline` and `SetWriteDeadline`. Silence and
  stalled peers are detected with watchdogs, its peer idle timeout, and its
  non-blocking send queue.
- mediacommon's `mpegts.NewBufferedReader` requires packet-aligned reads and
  is deprecated; `tsconv.Demux` uses a 64 KB `bufio.Reader` instead.
