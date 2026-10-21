// Package settings holds the live configuration: server settings from the
// file plus runtime settings from the database, as one immutable snapshot
// that is swapped atomically on every change.
//
// Readers call Get (or use Store.Getter) and never modify the result.
// Writers call Update, which validates, persists, swaps and then notifies
// subscribers (the push, pull and transcode reconcilers), in that order.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/store"
)

// ErrInvalid wraps validation errors, so the API can answer 400.
var ErrInvalid = errors.New("invalid settings")

// Store is the live configuration.
type Store struct {
	db  *store.DB
	log *slog.Logger

	mu   sync.Mutex // serialises updates
	cur  atomic.Pointer[config.Config]
	subs []func(old, cur *config.Config)
}

// Open builds the first snapshot. If the database has runtime settings,
// they replace those of base; otherwise base's runtime settings are
// imported (first start).
func Open(ctx context.Context, base *config.Config, db *store.DB, log *slog.Logger) (*Store, error) {
	s := &Store{db: db, log: log}
	cfg := base.Clone()

	r, seeded, err := db.LoadRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	if seeded {
		r.Apply(cfg)
		if err := cfg.ValidateRuntime(); err != nil {
			// Should not happen (every update is validated), but a manual
			// database edit could cause it. Run anyway and say what is wrong.
			log.Error("stored settings are invalid; fix them in the panel", "err", err)
		}
		if hasRuntime(base) {
			log.Info("apps, profiles, sources and webhooks come from the database; " +
				"those sections of the config file are only used on the first start")
		}
	} else {
		if err := db.SaveRuntime(ctx, store.RuntimeOf(cfg)); err != nil {
			return nil, fmt.Errorf("importing settings: %w", err)
		}
		log.Info("imported apps, profiles, sources and webhooks from the config file into the database",
			"apps", len(cfg.Apps), "profiles", len(cfg.Transcode.Profiles), "sources", len(cfg.Sources))
	}
	s.cur.Store(cfg)
	return s, nil
}

func hasRuntime(c *config.Config) bool {
	d := config.Default()
	return len(c.Transcode.Profiles) > 0 || len(c.Sources) > 0 || len(c.Webhooks) > 0 ||
		len(c.Apps) != len(d.Apps) || (len(c.Apps) == 1 && c.Apps[0].Name != d.Apps[0].Name)
}

// Get returns the current snapshot. Do not modify it.
func (s *Store) Get() *config.Config { return s.cur.Load() }

// Getter returns Get as a config.Getter.
func (s *Store) Getter() config.Getter { return s.Get }

// Subscribe registers fn, called after every successful update with the
// previous and new snapshots. Callbacks run synchronously in registration
// order and must not call Update.
func (s *Store) Subscribe(fn func(old, cur *config.Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// Update applies fn to a copy of the current snapshot. Only runtime
// settings may change; server settings edited by fn are ignored. The result
// is validated (errors wrap ErrInvalid), stored, and then made current.
func (s *Store) Update(ctx context.Context, fn func(c *config.Config) error) (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.cur.Load()
	next := old.Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	// Server settings are fixed for the life of the process.
	r := store.RuntimeOf(next)
	next = old.Clone()
	r.Apply(next)

	if err := next.ValidateRuntime(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := s.db.SaveRuntime(ctx, store.RuntimeOf(next)); err != nil {
		return nil, fmt.Errorf("saving settings: %w", err)
	}
	s.cur.Store(next)
	for _, fn := range s.subs {
		fn(old, next)
	}
	return next, nil
}
