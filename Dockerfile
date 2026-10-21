FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/cvete/streamble/internal/api.Version=${VERSION}" \
    -o /out/streamble ./cmd/streamble

# The control panel (internal/panel/dist) is committed and embedded, so no
# Node.js stage is needed. FFmpeg is included for the transcoder.
FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
    && adduser -D -H -u 10001 streamble \
    && mkdir -p /var/lib/streamble && chown streamble /var/lib/streamble
COPY --from=build /out/streamble /usr/local/bin/streamble
# The SQLite database lives in data/ under the working directory.
WORKDIR /var/lib/streamble
VOLUME /var/lib/streamble
EXPOSE 1935/tcp 8890/udp 8080/tcp
USER streamble
ENTRYPOINT ["/usr/local/bin/streamble"]
