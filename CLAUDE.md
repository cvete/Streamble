# Notes for AI agents working on Streamble

Read `docs/ARCHITECTURE.md` and `ROADMAP.md` before changing code. They are
the source of truth for design decisions and current status.

## Ground rules

- Go, standard library first. New dependencies need a reason and must be
  AGPL-compatible (MIT, BSD, Apache-2.0, MPL-2.0, LGPL as a separate process).
- Outputs talk to ingest only through `internal/core`. Never import an ingest
  package from an output package or the reverse.
- The publisher goroutine must never block: outputs consume through their own
  `core.Subscription`.
- Never log publish keys, passphrases or API tokens. Log paths, not URLs.
- Stream and app names are validated by `config.ValidStreamName`; reuse it for
  anything that becomes a URL segment or file name.
- This is clean-room software. Do not copy code, configs or text from
  proprietary products; use public protocol specs and our own design.

## Before you finish

```sh
make lint
make test        # needs ffmpeg; e2e tests publish real streams
```

- Add or extend an e2e test in `internal/e2e` for any new protocol or output.
- Update `ROADMAP.md` (tick what landed) and `docs/ARCHITECTURE.md` if a
  design decision changed.
- Write user-facing docs (README, config comments) in English.

## Handy commands

```sh
make run                                      # default config on :1935 / :8890 / :8080
ffmpeg -re -f lavfi -i testsrc2 -f lavfi -i sine -c:v libx264 -g 50 \
  -pix_fmt yuv420p -c:a aac -f flv rtmp://localhost:1935/live/test
curl localhost:8080/api/v1/streams
```

When stopping the server in scripts, use `pkill -x streamble`; `pkill -f`
matches the shell running the script and kills it too.
