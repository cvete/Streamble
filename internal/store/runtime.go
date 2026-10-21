package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cvete/streamble/internal/config"
)

// Item kinds in runtime_items.
const (
	kindApp      = "app"
	kindProfile  = "profile"
	kindSource   = "source"
	kindWebhook  = "webhook"
	kindSetting  = "setting"
	settingPlay  = "playback"
	metaSeededAt = "runtime_seeded_at"
)

// Runtime is the part of the configuration that lives in the database.
type Runtime struct {
	Playback config.Playback
	Profiles []config.Profile
	Apps     []config.App
	Sources  []config.Source
	Webhooks []config.Webhook
}

// RuntimeOf extracts the runtime settings of a configuration.
func RuntimeOf(c *config.Config) Runtime {
	return Runtime{
		Playback: c.Playback,
		Profiles: c.Transcode.Profiles,
		Apps:     c.Apps,
		Sources:  c.Sources,
		Webhooks: c.Webhooks,
	}
}

// Apply copies runtime settings into a configuration.
func (r Runtime) Apply(c *config.Config) {
	c.Playback = r.Playback
	c.Transcode.Profiles = r.Profiles
	c.Apps = r.Apps
	c.Sources = r.Sources
	c.Webhooks = r.Webhooks
}

// LoadRuntime returns the stored runtime settings, and false when the
// database has never been seeded.
func (d *DB) LoadRuntime(ctx context.Context) (Runtime, bool, error) {
	var seeded string
	err := d.QueryRow(ctx, `SELECT value FROM meta WHERE key = ?`, metaSeededAt).Scan(&seeded)
	if errors.Is(err, sql.ErrNoRows) {
		return Runtime{}, false, nil
	}
	if err != nil {
		return Runtime{}, false, err
	}

	rows, err := d.Query(ctx, `SELECT kind, name, data FROM runtime_items ORDER BY kind, name`)
	if err != nil {
		return Runtime{}, false, err
	}
	defer rows.Close()

	var r Runtime
	for rows.Next() {
		var kind, name, data string
		if err := rows.Scan(&kind, &name, &data); err != nil {
			return Runtime{}, false, err
		}
		var target any
		switch kind {
		case kindApp:
			r.Apps = append(r.Apps, config.App{})
			target = &r.Apps[len(r.Apps)-1]
		case kindProfile:
			r.Profiles = append(r.Profiles, config.Profile{})
			target = &r.Profiles[len(r.Profiles)-1]
		case kindSource:
			r.Sources = append(r.Sources, config.Source{})
			target = &r.Sources[len(r.Sources)-1]
		case kindWebhook:
			r.Webhooks = append(r.Webhooks, config.Webhook{})
			target = &r.Webhooks[len(r.Webhooks)-1]
		case kindSetting:
			if name != settingPlay {
				continue
			}
			target = &r.Playback
		default:
			continue
		}
		if err := json.Unmarshal([]byte(data), target); err != nil {
			return Runtime{}, false, fmt.Errorf("decoding %s %q: %w", kind, name, err)
		}
	}
	return r, true, rows.Err()
}

// SaveRuntime stores runtime settings, writing only what changed, in one
// transaction. It also marks the database as seeded.
func (d *DB) SaveRuntime(ctx context.Context, r Runtime) error {
	want := map[[2]string]string{}
	put := func(kind, name string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		want[[2]string{kind, name}] = string(b)
		return nil
	}
	for _, a := range r.Apps {
		if err := put(kindApp, a.Name, a); err != nil {
			return err
		}
	}
	for _, p := range r.Profiles {
		if err := put(kindProfile, p.Name, p); err != nil {
			return err
		}
	}
	for _, s := range r.Sources {
		if err := put(kindSource, s.Path, s); err != nil {
			return err
		}
	}
	for _, w := range r.Webhooks {
		if err := put(kindWebhook, w.Name, w); err != nil {
			return err
		}
	}
	if err := put(kindSetting, settingPlay, r.Playback); err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `SELECT kind, name, data FROM runtime_items`)
	if err != nil {
		return err
	}
	have := map[[2]string]string{}
	for rows.Next() {
		var kind, name, data string
		if err := rows.Scan(&kind, &name, &data); err != nil {
			rows.Close()
			return err
		}
		have[[2]string{kind, name}] = data
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := Now()
	for key, data := range want {
		if have[key] == data {
			continue
		}
		if _, err := tx.ExecContext(ctx, d.Q(`INSERT INTO runtime_items (kind, name, data, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (kind, name) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`),
			key[0], key[1], data, now); err != nil {
			return err
		}
	}
	for key := range have {
		if _, ok := want[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, d.Q(`DELETE FROM runtime_items WHERE kind = ? AND name = ?`),
			key[0], key[1]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, d.Q(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO NOTHING`), metaSeededAt, fmt.Sprint(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// Meta reads a value from the meta table; "" when absent.
func (d *DB) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.QueryRow(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a value to the meta table.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.Exec(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
