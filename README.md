# Streamble

Streamble is an open-source live streaming server for TV channels, radio and
events. Publish with RTMP or SRT from OBS, vMix, hardware encoders or FFmpeg,
pull streams from other servers, transcode them into quality ladders, and
deliver them as HLS, Low-Latency HLS, MPEG-TS, MP3/AAC radio streams, RTMP or
SRT, while restreaming to YouTube, Twitch, Facebook or Icecast.

It is a single binary, licensed under the AGPL-3.0. FFmpeg is only needed
for transcoding.

> **Status: early development (milestone 3 of 5).** Everything listed below
> works and is covered by end-to-end tests with real FFmpeg streams, but has
> not yet been run in production. Recording, DVR and edge mode are next; see
> [ROADMAP.md](ROADMAP.md).

## What it does

| | |
|---|---|
| **Ingest** | RTMP and SRT publishing (optionally encrypted), with publish keys per app or per stream |
| **Pull** | HTTP MPEG-TS (`.ts`), Icecast/Shoutcast MP3 and AAC radio, HLS (`.m3u8`), UDP unicast and multicast, SRT, RTMP |
| **Codecs** | H.264, H.265, AAC, MP3, MP2, AC-3, Opus |
| **Transcode** | FFmpeg quality ladders on CPU or GPU (NVENC, QSV, VAAPI), MP2/AC-3/MP3 to AAC for web players, MP3 renditions for radio, adaptive bitrate playlist |
| **Playback** | Apple Low-Latency HLS, fMP4 HLS, MPEG-TS HLS, continuous `.ts` over HTTP, `.mp3` and `.aac` radio streams, RTMP, SRT |
| **Restream** | RTMP/RTMPS (YouTube, Twitch, Facebook, any server), SRT, Icecast (MP3/AAC), with automatic reconnect |
| **Security** | Publish keys, play keys, SRT passphrases, auth callbacks to your own service, users with roles, API tokens, audit log; keys never appear in logs |
| **Control panel** | Built-in web panel: multiviewer of every channel on air, apps and keys, transcoding profiles, restream targets, pull sources, statistics, webhooks, users. Changes apply live without dropping streams |
| **Statistics** | Viewers and bandwidth per minute for every stream, kept in SQLite or PostgreSQL; viewer countries with a free GeoLite2 database |
| **Integration** | REST API for everything the panel does, webhooks for stream events (signed with HMAC) |

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

Open <http://localhost:8080> for the control panel. On first start the
server prints a password for the `admin` account (or set
`STREAMBLE_ADMIN_PASSWORD`); you choose your own password at first sign-in.

### Docker

```sh
cp streamble.example.yml streamble.yml   # edit keys, profiles, targets
docker compose up -d
```

The image includes FFmpeg. Settings and statistics are stored in the
`streamble-data` volume.

### On a VPS with Coolify

Use [`deploy/docker-compose.coolify.yml`](deploy/docker-compose.coolify.yml):
create a resource from this repository with the **Docker Compose** build
pack and that file as the compose location, then give the `streamble`
service a domain such as `https://stream.example.com:8080` (the `:8080` is
the container port; visitors use normal HTTPS). Open **1935/tcp** (RTMP) and
**8890/udp** (SRT) in the VPS firewall. The admin password is printed in the
deployment log on first start, or set `STREAMBLE_ADMIN_PASSWORD`.

Encoders publish to `rtmp://stream.example.com/live` (stream key
`tv1?key=…`). If the domain goes through Cloudflare's proxy, add a DNS-only
record for encoders, because Cloudflare does not pass RTMP or SRT.

### Behind any reverse proxy

Set `http.trusted_proxies` (or `STREAMBLE_TRUSTED_PROXIES`) to the proxy's
address or network, and don't expose port 8080 directly. Otherwise every
request seems to come from the proxy: one failed-login limit for everyone,
viewers counted as one, and the proxy's country for all of them. Cookies
are marked Secure automatically when the proxy reports HTTPS.

## Publishing

Streams live under an **app**: `live/tv1` is stream `tv1` in app `live`.
Apps are created in the control panel (Apps and keys), through the API, or
in the config file.

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

**Pulling** from another server, a radio station or a satellite receiver:

```yaml
sources:
  - path: live/news
    url: http://example.com/news.ts          # also .m3u8, .mp3, udp://, srt://, rtmp://
```

## Watching and listening

For stream `live/tv1`:

| URL | What |
|---|---|
| `http://server:8080/live/tv1/index.m3u8` | HLS, primary variant (Low-Latency HLS by default) |
| `http://server:8080/live/tv1/abr.m3u8` | Adaptive bitrate playlist, when the app has a transcode profile |
| `http://server:8080/live/tv1/mpegts/index.m3u8` | Classic HLS with `.ts` segments, for older TVs (H.264 + AAC only) |
| `http://server:8080/live/tv1.ts` | Continuous MPEG-TS, for IPTV players, VLC and set-top boxes |
| `http://server:8080/live/tv1.mp3` | MP3 radio stream (needs MP3 audio, e.g. a transcode rendition) |
| `http://server:8080/live/tv1.aac` | AAC radio stream |
| `rtmp://server:1935/live/tv1` | RTMP playback |
| `srt://server:8890?streamid=read:live/tv1` | SRT playback |

Add `?key=PLAY_KEY` (or `&key=` in the SRT stream ID) when the app has a
`play_key`. HLS is not covered by play keys yet; signed HLS URLs are planned.

## Transcoding

Give an app a transcode profile and every stream in it gets renditions,
published as `<stream>_<rendition>` (for example `live/tv1_720p`). Each
rendition can be watched, restreamed or recorded like any other stream, and
`abr.m3u8` lets players switch between them.

```yaml
transcode:
  profiles:
    - name: tv
      hwaccel: none        # none, nvenc, qsv, vaapi
      renditions:
        - { name: 720p, height: 720, video_bitrate: 2800k }
        - { name: 480p, height: 480, video_bitrate: 1400k }
        - { name: mp3, audio_only: true, audio_codec: mp3 }
apps:
  - name: live
    transcode: tv
```

Renditions never upscale and share keyframe positions, so players switch
cleanly. FFmpeg runs as a separate process per stream and is restarted
automatically if it fails; `/api/v1/transcodes` shows its state.

**Broadcast audio:** feeds with MP2 or AC-3 audio cannot be played by
browsers and cannot be sent over RTMP. Transcode them: the renditions get
AAC (or MP3 for radio).

## Restreaming

```yaml
apps:
  - name: live
    push:
      - name: youtube
        url: rtmp://a.rtmp.youtube.com/live2/${YOUTUBE_STREAM_KEY}
        streams: [tv1]
      - name: radio
        url: icecast://source:${ICECAST_PASSWORD}@icecast.example.com:8000/{stream}.mp3
        streams: [fm1_mp3]
```

- `{app}` and `{stream}` in the URL are filled in for each stream.
- `streams` limits a target to certain streams. Transcoded renditions are
  only pushed when listed by name, so a ladder never floods a platform.
- YouTube, Twitch and Facebook accept H.264 + AAC. Push a transcoded
  rendition if the source has other codecs; a push that cannot work stops
  with an explanation instead of retrying forever.
- Icecast targets need MP3 or AAC audio and Icecast 2.4 or newer.
- `/api/v1/pushes` shows the state of every target, with keys redacted.

## Configuration

Everything is optional. Copy [`streamble.example.yml`](streamble.example.yml)
and run `streamble -config streamble.yml` (or set `STREAMBLE_CONFIG`).
`${VAR}` in the file is replaced with environment variables, so keys can be
kept out of the file. An unset variable is left as literal text rather than
becoming an empty key, so an app never becomes open by accident.

Settings come in two kinds:

- **Server settings** (listeners, HLS, storage, panel, GeoIP) are read from
  the file at start and need a restart.
- **Live settings** (apps and keys, transcoding profiles, restream targets,
  pull sources, webhooks, playback switches) live in the database. On the
  very first start they are imported from the file; after that the panel and
  the API are the place to change them, and changes apply immediately
  without dropping streams.

Storage is SQLite by default (`data/streamble.db`). For PostgreSQL:

```yaml
storage:
  driver: postgres
  dsn: ${STREAMBLE_DATABASE_URL}   # postgres://user:pass@host:5432/streamble
```

## API

Everything the panel does is available over a REST API under `/api/v1/`;
see [docs/API.md](docs/API.md). Scripts authenticate with an API token
created under Account in the panel: `Authorization: Bearer stb_…`.
`GET /api/v1/health` is always public.

Webhooks POST a JSON event to your URL when a stream starts or stops, a
restream or pull source changes state, or a transcoder fails, signed with
`X-Streamble-Signature: sha256=<HMAC>`. An app's `auth_url` lets your own
service approve or refuse every publish and play.

Viewer counts combine HLS players (estimated from playlist requests over the
last 30 seconds) and connected `.ts`/`.mp3`/`.aac` listeners. Behind a CDN the
origin sees only CDN servers, so use CDN statistics there.

## Scaling

Ingest and packaging are light; serving viewers is mostly limited by network
bandwidth (5,000 viewers at 3 Mbps is 15 Gbps). Transcoding is the heavy
part: plan roughly one CPU core per 720p rendition with libx264, or use a
GPU. For large audiences put a CDN or caching proxies in front of the HLS
URLs. A load test with published numbers and a built-in edge mode are
planned for milestone 5.

## Development

```sh
make test         # unit + end-to-end tests (needs ffmpeg with libx264, libmp3lame, srt)
make test-short   # unit tests only
make lint
make ui           # rebuild the control panel (needs Node.js 22) after changing web/panel
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
[gosrt](https://github.com/datarhei/gosrt) by datarhei. Transcoding uses
[FFmpeg](https://ffmpeg.org) as an external program.
