# Streamble

Streamble is an open-source live streaming server for TV channels, radio and
events. Publish with RTMP or SRT from OBS, vMix, hardware encoders or FFmpeg;
watch with HLS and Apple Low-Latency HLS in any browser, phone or smart TV.

It is a single binary with no runtime dependencies, licensed under the
AGPL-3.0.

> **Status: early development (milestone 1 of 5).** Ingest and HLS playback
> work and are covered by end-to-end tests. Transcoding, restreaming to
> YouTube/Twitch, the control panel and recording are next; see
> [ROADMAP.md](ROADMAP.md).

## What works today

| | |
|---|---|
| **Ingest** | RTMP (H.264, H.265, AAC, Opus), SRT with optional encryption (MPEG-TS with H.264, H.265, AAC, Opus) |
| **Playback** | Apple Low-Latency HLS, HLS with fMP4 segments, classic HLS with MPEG-TS segments |
| **Radio** | Audio-only streams (AAC, Opus) over all HLS variants |
| **Security** | Publish keys per app or per stream, SRT passphrases, optional API token |
| **Monitoring** | JSON status API with live bitrates and viewer estimates, built-in monitor and test player |

## Quick start

```sh
make build
./bin/streamble                       # runs with defaults, no config needed
```

Publish a test stream:

```sh
ffmpeg -re -f lavfi -i testsrc2=size=1280x720:rate=30 -f lavfi -i sine \
  -c:v libx264 -preset veryfast -g 60 -pix_fmt yuv420p -c:a aac \
  -f flv rtmp://localhost:1935/live/test
```

Open <http://localhost:8080> to see it on air and play it. The stream is at
`http://localhost:8080/live/test/index.m3u8`.

### Docker

```sh
cp streamble.example.yml streamble.yml   # edit keys
docker compose up -d
```

## Publishing

Streams live under an **app**: `live/tv1` is stream `tv1` in app `live`.
Apps are defined in the config file.

**OBS / vMix (RTMP)**

- Server: `rtmp://your-server:1935/live`
- Stream key: `tv1?key=YOUR_PUBLISH_KEY` (just `tv1` if the app has no key)

**SRT (caller mode)**

```
srt://your-server:8890?streamid=live/tv1?key=YOUR_PUBLISH_KEY
```

The standard SRT access-control syntax also works:
`streamid=#!::r=live/tv1,m=publish,s=YOUR_PUBLISH_KEY`. If the app has an
`srt_passphrase`, add `&passphrase=...` on the encoder.

## Playback URLs

| URL | Format |
|---|---|
| `/live/tv1/index.m3u8` | Primary variant (Low-Latency HLS by default) |
| `/live/tv1/llhls/index.m3u8` | Apple Low-Latency HLS |
| `/live/tv1/fmp4/index.m3u8` | HLS with fMP4 segments (enable in config) |
| `/live/tv1/mpegts/index.m3u8` | Classic HLS with `.ts` segments, H.264 + AAC only |

Use any HLS player: Safari and iOS natively, [hls.js](https://github.com/video-dev/hls.js)
or Video.js elsewhere. CORS is open by default (`http.allow_origin`).

## Configuration

Everything is optional. Copy [`streamble.example.yml`](streamble.example.yml)
and run `streamble -config streamble.yml` (or set `STREAMBLE_CONFIG`).
`${VAR}` in the file is replaced with environment variables, so keys can be
kept out of the file. An unset variable is left as literal text rather than
becoming an empty key, so an app never becomes open by accident.

## API

| Endpoint | |
|---|---|
| `GET /api/v1/health` | Liveness probe, always public |
| `GET /api/v1/streams` | Live streams with tracks, bitrates, viewers and playback URLs |
| `GET /api/v1/streams/{app}/{stream}` | One stream |
| `GET /api/v1/version` | Version, licence and source URL |

Set `http.api_token` to require `Authorization: Bearer <token>`.

Viewer counts are estimated from playlist requests over the last 30 seconds.
Behind a CDN the origin sees only CDN servers, so use CDN statistics there.

## Scaling

Ingest and packaging are light; serving viewers is mostly limited by network
bandwidth (5,000 viewers at 3 Mbps is 15 Gbps). A load test with published
numbers is planned for milestone 5.
For large audiences put a CDN or caching proxies in front of the HLS URLs.
Segments are immutable and cacheable; playlists change every segment.
Built-in edge mode is on the roadmap.

## Development

```sh
make test         # unit + end-to-end tests (needs ffmpeg for e2e)
make test-short   # unit tests only
make lint
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit
together and [ROADMAP.md](ROADMAP.md) for what is being built next.

## Licence

Streamble is free software under the [GNU Affero General Public License v3.0](LICENSE).
If you run a modified version as a network service, the AGPL requires you to
offer your users its source code. The server advertises its source URL in
`/api/v1/version` and on the monitor page; change `api.SourceURL` in a fork.

Streamble builds on the excellent MIT-licensed libraries
[gortmplib](https://github.com/bluenviron/gortmplib),
[gohlslib](https://github.com/bluenviron/gohlslib) and
[mediacommon](https://github.com/bluenviron/mediacommon) by bluenviron, and
[gosrt](https://github.com/datarhei/gosrt) by datarhei.
