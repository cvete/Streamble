// Command streamble is an open-source live streaming server:
// RTMP and SRT in, HLS and Low-Latency HLS out.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cvete/streamble/internal/api"
	"github.com/cvete/streamble/internal/auth"
	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/ingest/pull"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/ingest/srt"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/output/httpstream"
	"github.com/cvete/streamble/internal/output/push"
	"github.com/cvete/streamble/internal/transcode"
	"github.com/cvete/streamble/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "streamble:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", os.Getenv("STREAMBLE_CONFIG"), "path to streamble.yml (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("streamble", api.Version)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("starting Streamble", "version", api.Version, "config", *cfgPath)

	hub := core.NewHub(log)
	authz := auth.New(cfg)

	hlsManager := hls.NewManager(cfg.HLS, cfg.HTTP.AllowOrigin, log)
	hub.AddHook(hlsManager)

	var closers []func()
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}()

	pusher := push.NewManager(cfg, log)
	hub.AddHook(pusher)
	closers = append(closers, pusher.Close)

	transcoder := transcode.NewManager(cfg, hub, log)
	hub.AddHook(transcoder)
	closers = append(closers, transcoder.Close)
	hlsManager.ABR = func(path string) []string {
		var paths []string
		for _, r := range transcoder.Renditions(path) {
			if !r.AudioOnly {
				paths = append(paths, r.Path)
			}
		}
		return paths
	}

	if cfg.RTMP.Enabled {
		s := &rtmp.Server{Addr: cfg.RTMP.Listen, Hub: hub, Auth: authz, Log: log, AllowPlay: cfg.Playback.RTMP}
		if err := s.Start(); err != nil {
			return fmt.Errorf("RTMP: %w", err)
		}
		closers = append(closers, s.Close)
	}
	if cfg.SRT.Enabled {
		s := &srt.Server{
			Addr:      cfg.SRT.Listen,
			Latency:   time.Duration(cfg.SRT.LatencyMS) * time.Millisecond,
			Hub:       hub,
			Auth:      authz,
			Log:       log,
			AllowRead: cfg.Playback.SRT,
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("SRT: %w", err)
		}
		closers = append(closers, s.Close)
	}

	statusAPI := api.New(hub, hlsManager, cfg.HTTP.APIToken, cfg.HLS.Primary)
	closers = append(closers, statusAPI.Close)
	statusAPI.AddStatus("transcodes", func() any { return transcoder.Status() })
	statusAPI.AddStatus("pushes", func() any { return pusher.Status() })

	streams := &httpstream.Handler{
		Hub:         hub,
		Auth:        authz,
		Playback:    cfg.Playback,
		AllowOrigin: cfg.HTTP.AllowOrigin,
		Log:         log,
	}
	statusAPI.Listeners = streams.Listeners

	// Pull sources last, once every hook is registered, so pulled streams
	// get HLS, transcoding and restreaming like any other.
	puller := &pull.Manager{Sources: cfg.Sources, Hub: hub, Log: log}
	puller.Start()
	closers = append(closers, puller.Close)
	statusAPI.AddStatus("sources", func() any { return puller.Status() })

	mux := http.NewServeMux()
	mux.Handle("/api/", statusAPI)
	mux.Handle("/{$}", web.Handler())
	// /<app>/<stream>.ts|.mp3|.aac are continuous streams; everything else
	// under / is HLS.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if streams.Match(r.URL.Path) {
			streams.ServeHTTP(w, r)
			return
		}
		hlsManager.ServeHTTP(w, r)
	}))

	httpServer := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	httpErr := make(chan error, 1)
	go func() {
		log.Info("HTTP listening (HLS, API, monitor)", "addr", cfg.HTTP.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-httpErr:
		return fmt.Errorf("HTTP: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Continuous stream clients never finish on their own; after the grace
	// period, close their connections.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		httpServer.Close() //nolint:errcheck
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
