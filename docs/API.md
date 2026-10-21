# Streamble API

JSON over HTTP at `/api/v1/`. The control panel uses exactly this API, so
anything the panel does can be scripted.

## Authentication

Two ways to authenticate:

- **Session cookie** (the panel): `POST /api/v1/auth/login` sets an
  HttpOnly cookie. Requests that change something (POST, PUT, DELETE) must
  also send the header `X-Streamble-Request: 1`; browsers cannot add it to
  cross-site requests, which blocks CSRF.
- **API token** (scripts): `Authorization: Bearer stb_...`. Create tokens
  in the panel (Account) or with `POST /api/v1/auth/tokens`. No extra header
  needed. The legacy `http.api_token` from the config file also works and
  acts as an admin.

On the first start, an `admin` user is created. Its password is taken from
`STREAMBLE_ADMIN_PASSWORD`, or generated and printed once in the log. Users
whose password was set by someone else must change it before anything else
(`403 change your password first`).

## Roles

| Role | Can |
|---|---|
| `viewer` | See streams, statistics, settings and status. Secrets (keys, passphrases, stream keys in target URLs) are shown as `********`. |
| `operator` | Everything a viewer can, plus see secrets, manage apps, transcode profiles, restream targets and pull sources, and kick streams. |
| `admin` | Everything, plus users, webhooks, playback switches and the audit log. |

## Conventions

- Errors: `{"error": "message"}`. Invalid settings: status 400 with
  `{"error": "the settings are not valid", "details": ["app \"x\": ...", ...]}`.
- Status codes: 400 invalid request, 401 not logged in, 403 role or CSRF,
  404 not found, 409 already exists or not possible, 429 too many logins.
- Times are RFC 3339 in responses. Query parameters `from`, `to`, `before`
  accept RFC 3339 or Unix milliseconds.
- Settings changes apply immediately to the running server (see
  [Hot reload](#hot-reload)) and are recorded in the audit log.
- Updates use `PUT` with the full object. Renaming is not supported. Secrets
  sent back as `********` keep their stored value.

## Public

| | |
|---|---|
| `GET /api/v1/health` | `{"status":"ok"}` |
| `GET /api/v1/version` | name, version, license, source URL, uptime |

## Account

| | Body / response |
|---|---|
| `POST /api/v1/auth/login` | `{"username","password"}` → `{"user"}` + cookie |
| `POST /api/v1/auth/logout` | |
| `GET /api/v1/auth/me` | `{"user", "csrf_header", "roles"}` |
| `POST /api/v1/auth/password` | `{"current_password","new_password"}` (min. 10 characters); signs out other sessions |
| `GET /api/v1/auth/tokens` | `{"tokens":[{"id","name","prefix","created_at","last_used_at"}]}` |
| `POST /api/v1/auth/tokens` | `{"name"}` → `{"token": "stb_…" (shown once), "info"}` |
| `DELETE /api/v1/auth/tokens/{id}` | |

User object: `{"id","username","role","disabled","must_change_password","created_at","updated_at","last_login_at"}`.

## Live state (viewer)

| | |
|---|---|
| `GET /api/v1/server` | version, uptime, storage driver, listeners, HLS settings, playback switches, GeoIP on/off, stats retention, `overview` |
| `GET /api/v1/streams` | `{"streams": [Stream]}` |
| `GET /api/v1/streams/{app}/{stream}` | `Stream` |
| `POST /api/v1/streams/{app}/{stream}/kick` | operator. Disconnects the publisher; a pulled source reconnects; a rendition restarts its transcode job. 409 if not possible. |
| `GET /api/v1/transcodes` | `{"transcodes":[{"stream","profile","state","last_error","since","restarts","renditions"}]}` |
| `GET /api/v1/pushes` | `{"pushes":[{"stream","name","url" (redacted),"state","last_error","since","bytes_sent","reconnects"}]}` |
| `GET /api/v1/sources` | `{"sources":[{"path","url" (redacted),"state","last_error","since","reconnects"}]}` |
| `GET /api/v1/events` | Server-Sent Events, see below |

States: transcode `starting`, `running`, `restarting`; push `connecting`,
`live`, `retrying`, `failed`; source `connecting`, `live`, `retrying`.

`Stream`:

```json
{
  "path": "live/tv1", "app": "live", "name": "tv1",
  "protocol": "rtmp", "remote_addr": "203.0.113.7:51234",
  "started_at": "2026-10-21T14:00:00Z", "uptime_seconds": 3600,
  "tracks": [{"codec": "H264", "type": "video"}, {"codec": "AAC", "type": "audio"}],
  "bytes_in": 1300000000, "bytes_out": 52000000000,
  "bitrate_in_kbps": 3100, "bitrate_out_kbps": 120000,
  "viewers": 410, "kickable": true,
  "parent": "",                       // source stream, for transcoder renditions
  "renditions": ["live/tv1_720p"],    // for transcoded sources
  "hls": {"primary": "/live/tv1/index.m3u8",
          "variants": {"llhls": "/live/tv1/llhls/index.m3u8"},
          "abr": "/live/tv1/abr.m3u8"},
  "http": {"ts": "/live/tv1.ts", "aac": "/live/tv1.aac"}
}
```

### Events stream

`GET /api/v1/events` (`text/event-stream`) sends:

- `event: snapshot` every 2 seconds:
  `{"streams": [Stream], "overview": {"streams","sources","viewers","bitrate_in_kbps","bitrate_out_kbps"}, "transcodes": [...], "pushes": [...], "sources": [...]}`
- `event: event` when something happens:
  `{"type": "stream.published" | "stream.ended" | "push.state" | "source.state" | "transcode.state", "at", "data"}`

## Settings

All return the stored object; viewers get secrets masked.

| | Role | |
|---|---|---|
| `GET /api/v1/config/apps` | viewer | `{"apps":[App]}` |
| `POST /api/v1/config/apps` | operator | `App` → 201 |
| `GET /api/v1/config/apps/{name}` | viewer | `{"app"}` |
| `PUT /api/v1/config/apps/{name}` | operator | `App` |
| `DELETE /api/v1/config/apps/{name}` | operator | 400 if a pull source still uses it |
| `…/config/profiles[/{name}]` | viewer / operator | `Profile`; delete fails while an app uses it |
| `…/config/sources[/{app}/{stream}]` | viewer / operator | `Source` |
| `…/config/webhooks[/{name}]` | admin | `Webhook`; the list also returns `"events"` (all event names) |
| `GET /api/v1/config/playback` | viewer | `{"playback"}` |
| `PUT /api/v1/config/playback` | admin | `{"http_ts","http_audio","rtmp","srt"}` |

Objects (same fields as the YAML config):

```json
App:     {"name","description","publish_key","stream_keys":{"tv1":"key"},"srt_passphrase",
          "play_key","auth_url","transcode":"<profile>","push":[Push]}
Push:    {"name","url","streams":["tv1"],"disabled":false}
Profile: {"name","hwaccel":"none|nvenc|qsv|vaapi","vaapi_device","preset","gop":"2s",
          "renditions":[{"name","height","fps","video_bitrate":"2500k","audio_bitrate":"128k",
                          "audio_only":false,"audio_codec":"aac|mp3"}]}
Source:  {"path":"app/stream","url","disabled":false}
Webhook: {"name","url","secret","events":["stream.published"],"disabled":false}
```

### Hot reload

- Keys, play keys, passphrases and auth URLs: next connection.
- Push targets: new targets start on live streams; removed or disabled
  targets stop; unchanged targets keep streaming.
- Transcode profiles: live streams whose profile changed restart their
  transcoder; streams that gained a profile start one.
- Pull sources: added sources start; changed, disabled or removed ones stop.
- Playback switches: next request.
- Server settings (listeners, HLS, FFmpeg path, storage) need a restart and
  are read-only in the API.

## Users (admin)

| | |
|---|---|
| `GET /api/v1/users` | `{"users":[User]}` |
| `POST /api/v1/users` | `{"username","password","role"}`; the user must change the password at first login |
| `PUT /api/v1/users/{id}` | any of `{"role","disabled","password"}`; disabling or a password reset signs the user out |
| `DELETE /api/v1/users/{id}` | |

You cannot delete, disable or demote yourself, and at least one active
admin always remains.

## Audit log (admin)

`GET /api/v1/audit?before=` → `{"entries":[{"id","at","username","action","target","detail"}]}`,
newest first, 200 per page. Actions: `auth.login`, `user.*`, `token.*`,
`app.*`, `profile.*`, `source.*`, `webhook.*`, `playback.update`,
`stream.kick`, `stream.restart`.

## Statistics (viewer)

Recorded once a minute per stream and for the whole server, kept for
`stats.retention_days`.

| | |
|---|---|
| `GET /api/v1/stats/series?path=&from=&to=` | `{"path","step_seconds","points":[{"ts","viewers","viewers_max","bitrate_in_kbps","bitrate_out_kbps","bytes_in","bytes_out"}]}`. `path` empty = whole server. Default range: last hour. Step: 1 min up to 6 h, 15 min up to 7 days, 1 h beyond. |
| `GET /api/v1/stats/summary?from=&to=` | `{"peak_viewers","viewer_minutes","bytes_in","bytes_out","streams":[{"path","viewer_minutes","peak_viewers","bytes_out"}]}` (streams sorted by viewer minutes) |
| `GET /api/v1/stats/countries?path=&from=&to=` | `{"enabled","countries":[{"country":"FR","viewer_minutes"}]}` |
| `GET /api/v1/stats/countries/live?path=` | `{"enabled","countries":[{"country":"FR","viewers"}]}` |

Countries need `geoip.database` (a GeoLite2-Country `.mmdb` file);
otherwise `enabled` is false. `country` is an ISO 3166-1 alpha-2 code, or
`"--"` when unknown.

## Webhooks (admin)

Each enabled webhook receives matching events as
`POST <url>` with body `{"id","type","at","data"}` and headers
`X-Streamble-Event`, `X-Streamble-Delivery` and, when a secret is set,
`X-Streamble-Signature: sha256=<hex HMAC-SHA256 of the body>`.
Failures (network errors, 5xx) are retried after 1 s, 10 s and 60 s.

| | |
|---|---|
| `GET /api/v1/config/webhooks/{name}/deliveries` | `{"deliveries":[{"id","event","at","status","attempts","error","duration_ms"}]}` newest first |
| `POST /api/v1/config/webhooks/{name}/test` | sends a `ping` event now and returns the delivery |

### Auth callbacks

When an app has `auth_url`, every publish and play request (after key
checks) is sent as `POST <auth_url>`:

```json
{"action": "publish" | "play", "app": "live", "stream": "tv1", "path": "live/tv1",
 "key": "<key given by the client>", "protocol": "rtmp" | "srt" | "http",
 "remote_addr": "203.0.113.7:51234"}
```

Any 2xx response allows the request; anything else, or no answer within
3 seconds, refuses it.
