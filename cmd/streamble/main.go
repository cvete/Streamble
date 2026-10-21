// Command streamble is an open-source live streaming server:
// RTMP and SRT in, HLS and Low-Latency HLS out, with transcoding,
// restreaming and a web control panel.
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
	"github.com/cvete/streamble/internal/events"
	"github.com/cvete/streamble/internal/ingest/pull"
	"github.com/cvete/streamble/internal/ingest/rtmp"
	"github.com/cvete/streamble/internal/ingest/srt"
	"github.com/cvete/streamble/internal/output/hls"
	"github.com/cvete/streamble/internal/output/httpstream"
	"github.com/cvete/streamble/internal/output/push"
	"github.com/cvete/streamble/internal/panel"
	"github.com/cvete/streamble/internal/settings"
	"github.com/cvete/streamble/internal/stats"
	"github.com/cvete/streamble/internal/store"
	"github.com/cvete/streamble/internal/transcode"
	"github.com/cvete/streamble/internal/users"
	"github.com/cvete/streamble/internal/webhooks"
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

	fileCfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log := newLogger(fileCfg.LogLevel)
	log.Info("starting Streamble", "version", api.Version, "config", *cfgPath)

	ctx := context.Background()
	db, err := store.Open(fileCfg.Storage.Driver, fileCfg.Storage.DSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer db.Close()
	log.Info("storage ready", "driver", db.Driver())

	live, err := settings.Open(ctx, fileCfg, db, log)
	if err != nil {
		return err
	}
	get := live.Getter()
	cfg := get() // server settings never change at runtime

	var closers []func()
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}()

	bus := events.NewBus()
	hub := core.NewHub(log)
	authz := auth.NewDynamic(get)
	authz.Log = log.With("module", "auth") // auth callback decisions

	hlsManager := hls.NewManager(cfg.HLS, cfg.HTTP.AllowOrigin, log)
	hub.AddHook(hlsManager)
	hub.AddHook(events.StreamHook{Bus: bus})

	pusher := push.NewManager(get, log)
	pusher.OnState = func(st push.Status) {
		bus.Publish(events.Event{Type: config.EventPushState, Data: events.Data(st)})
	}
	hub.AddHook(pusher)
	closers = append(closers, pusher.Close)

	transcoder := transcode.NewManager(get, hub, log)
	transcoder.OnState = func(st transcode.Status) {
		bus.Publish(events.Event{Type: config.EventTranscodeState, Data: events.Data(st)})
	}
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
		s := &rtmp.Server{Addr: cfg.RTMP.Listen, Hub: hub, Auth: authz, Log: log,
			AllowPlayFunc: func() bool { return get().Playback.RTMP }}
		if err := s.Start(); err != nil {
			return fmt.Errorf("RTMP: %w", err)
		}
		closers = append(closers, s.Close)
	}
	if cfg.SRT.Enabled {
		s := &srt.Server{
			Addr:          cfg.SRT.Listen,
			Latency:       time.Duration(cfg.SRT.LatencyMS) * time.Millisecond,
			Hub:           hub,
			Auth:          authz,
			Log:           log,
			AllowReadFunc: func() bool { return get().Playback.SRT },
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("SRT: %w", err)
		}
		closers = append(closers, s.Close)
	}

	streams := &httpstream.Handler{
		Hub:          hub,
		Auth:         authz,
		PlaybackFunc: func() config.Playback { return get().Playback },
		AllowOrigin:  cfg.HTTP.AllowOrigin,
		Log:          log,
	}

	accounts := &users.Service{
		DB:            db,
		TTL:           cfg.Panel.SessionTTL.D(),
		SecureCookies: cfg.Panel.SecureCookies,
		LegacyToken:   cfg.HTTP.APIToken,
		Log:           log,
	}
	if err := accounts.EnsureAdmin(ctx); err != nil {
		return fmt.Errorf("creating the first admin: %w", err)
	}

	// Viewer countries need a GeoLite2 Country database (optional).
	var geo *stats.GeoIP
	if cfg.GeoIP.Database != "" {
		geo, err = stats.OpenGeoIP(cfg.GeoIP.Database)
		if err != nil {
			return fmt.Errorf("geoip: %w", err)
		}
		closers = append(closers, func() { geo.Close() })
		hlsManager.Country = geo.Country
		streams.Country = geo.Country
		log.Info("GeoIP country lookups enabled", "database", cfg.GeoIP.Database)
	}

	statusAPI := api.New(api.Options{
		Hub:       hub,
		HLS:       hlsManager,
		Users:     accounts,
		Settings:  live,
		DB:        db,
		Bus:       bus,
		Log:       log,
		Listeners: streams.Listeners,
		Restart:   map[string]func(string) bool{transcode.ProtocolTranscode: transcoder.Restart},
	})
	closers = append(closers, statusAPI.Close)
	statusAPI.AddStatus("transcodes", func() any { return transcoder.Status() })
	statusAPI.AddStatus("pushes", func() any { return pusher.Status() })

	recorder := &stats.Recorder{
		DB:            db,
		Hub:           hub,
		Viewers:       statusAPI.Viewers,
		Bitrates:      statusAPI.Bitrates,
		RetentionDays: cfg.Stats.RetentionDays,
		Log:           log,
	}
	if geo != nil {
		recorder.Countries = func(path string) map[string]int {
			out := hlsManager.ViewersByCountry(path)
			if out == nil {
				out = map[string]int{}
			}
			for c, n := range streams.ListenersByCountry(path) {
				out[c] += n
			}
			return out
		}
	}
	recorder.Start()
	closers = append(closers, recorder.Close) // writes the partial minute
	stats.Register(statusAPI, recorder, geo)

	// Webhooks: stream events are POSTed to the configured URLs.
	hooks := &webhooks.Dispatcher{Bus: bus, Settings: get, DB: db, Log: log.With("module", "webhooks")}
	hooks.Start()
	closers = append(closers, hooks.Close)
	webhooks.Register(statusAPI, hooks)

	// Pull sources last, once every hook is registered, so pulled streams
	// get HLS, transcoding and restreaming like any other.
	puller := &pull.Manager{Sources: cfg.Sources, Hub: hub, Log: log}
	puller.OnState = func(st pull.Status) {
		bus.Publish(events.Event{Type: config.EventSourceState, Data: events.Data(st)})
	}
	puller.Start()
	closers = append(closers, puller.Close)
	statusAPI.AddStatus("sources", func() any { return puller.Status() })

	// Settings edited in the panel apply live, without dropping streams.
	live.Subscribe(func(_, cur *config.Config) {
		pusher.Reconcile()
		transcoder.Reconcile()
		puller.Reconcile(cur.Sources)
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", statusAPI)
	if cfg.Panel.Enabled {
		mux.Handle("/panel/", panel.Handler("/panel/"))
		mux.Handle("GET /{$}", http.RedirectHandler("/panel/", http.StatusFound))
	}
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
		log.Info("HTTP listening (HLS, API, control panel at /panel/)", "addr", cfg.HTTP.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigCtx.Done():
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
