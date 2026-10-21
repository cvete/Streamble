FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/cvete/streamble/internal/api.Version=${VERSION}" \
    -o /out/streamble ./cmd/streamble

# FFmpeg is not needed yet; it will be added when the transcoder lands.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/streamble /usr/local/bin/streamble
EXPOSE 1935/tcp 8890/udp 8080/tcp
USER nonroot
ENTRYPOINT ["/usr/local/bin/streamble"]
