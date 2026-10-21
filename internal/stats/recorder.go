// Package stats records per-minute statistics (viewers, bitrates, bytes and
// viewer countries) for every live stream and for the whole server, and
// serves the history under /api/v1/stats/.
//
// The recorder samples the live state every 10 seconds and writes one row
// per stream and minute to stats_minute (path "" is the server), plus
// stats_country rows when GeoIP is enabled. Rows older than the retention
// period are deleted once an hour.
package stats

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/store"
)

// Default timing. Tests shrink these so a "minute" lasts milliseconds.
const (
	defaultSampleEvery  = 10 * time.Second
	defaultMinute       = time.Minute
	defaultCleanupEvery = time.Hour
)

// ServerPath is the stats path of the whole-server rows.
const ServerPath = ""

// Recorder samples live statistics and writes them to the database.
type Recorder struct {
	DB  *store.DB
	Hub *core.Hub
	// Viewers returns the current viewers of a stream (api.API.Viewers).
	Viewers func(path string) int
	// Bitrates returns the current input and output bitrates of a stream
	// in kbit/s (api.API.Bitrates).
	Bitrates func(s *core.Stream) (in, out int64)
	// Countries, when set, returns the current viewers of a stream by
	// country code. It enables stats_country rows.
	Countries func(path string) map[string]int
	// RetentionDays is how long rows are kept; 0 or less keeps them forever.
	RetentionDays int
	Log           *slog.Logger

	// Injectable clock for tests.
	now          func() time.Time
	sampleEvery  time.Duration
	minute       time.Duration
	cleanupEvery time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}

	// Owned by the loop goroutine (or the test calling sample/flush).
	started time.Time
	cur     *minuteAcc
	last    map[*core.Stream]counters
}

type counters struct{ in, out uint64 }

// pathAcc accumulates the samples of one path within one minute.
type pathAcc struct {
	viewersSum, viewersMax int64
	inSum, outSum          int64
	rateSamples            int64
	bytesIn, bytesOut      int64
	countries              map[string]int64 // max sample per country
}

type minuteAcc struct {
	start   int64 // Unix ms
	samples int64
	paths   map[string]*pathAcc
}

func (m *minuteAcc) get(path string) *pathAcc {
	a := m.paths[path]
	if a == nil {
		a = &pathAcc{}
		m.paths[path] = a
	}
	return a
}

func (r *Recorder) init() {
	if r.now == nil {
		r.now = time.Now
	}
	if r.sampleEvery <= 0 {
		r.sampleEvery = defaultSampleEvery
	}
	if r.minute < time.Millisecond {
		r.minute = defaultMinute
	}
	if r.cleanupEvery <= 0 {
		r.cleanupEvery = defaultCleanupEvery
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.last == nil {
		r.last = make(map[*core.Stream]counters)
	}
	if r.started.IsZero() {
		r.started = r.now()
	}
	if r.stop == nil {
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
	}
}

// SetIntervals shortens the sampling interval and the length of a
// recorded "minute" so tests run in milliseconds. Call it before Start.
func (r *Recorder) SetIntervals(sample, minute time.Duration) {
	r.sampleEvery, r.minute = sample, minute
}

// Start begins sampling in the background and returns immediately.
func (r *Recorder) Start() {
	r.startOnce.Do(func() {
		r.init()
		go r.loop()
	})
}

// Close stops the recorder, writes the current partial minute and waits.
func (r *Recorder) Close() {
	r.stopOnce.Do(func() {
		if r.stop == nil { // never started
			return
		}
		close(r.stop)
		<-r.done
	})
}

func (r *Recorder) loop() {
	defer close(r.done)
	ctx := context.Background()

	r.cleanup(ctx)
	lastCleanup := r.now()

	r.tick(ctx, r.now())
	for {
		// Align samples to multiples of sampleEvery so minutes get the
		// same number of samples (6 with the defaults).
		now := r.now()
		wait := now.Truncate(r.sampleEvery).Add(r.sampleEvery).Sub(now)
		timer := time.NewTimer(wait)
		select {
		case <-r.stop:
			// A last sample catches the bytes moved since the previous
			// one; then the partial minute is written.
			timer.Stop()
			r.tick(ctx, r.now())
			r.flush(ctx, r.cur)
			r.cur = nil
			return
		case <-timer.C:
		}
		now = r.now()
		r.tick(ctx, now)
		if now.Sub(lastCleanup) >= r.cleanupEvery {
			r.cleanup(ctx)
			lastCleanup = now
		}
	}
}

// tick writes the previous minute when a new one begins, then samples.
func (r *Recorder) tick(ctx context.Context, now time.Time) {
	ms := now.UnixMilli()
	start := ms - ms%r.minute.Milliseconds()
	if r.cur != nil && r.cur.start != start {
		r.flush(ctx, r.cur)
		r.cur = nil
	}
	if r.cur == nil {
		r.cur = &minuteAcc{start: start, paths: make(map[string]*pathAcc)}
	}
	r.sample(r.cur)
}

// sample adds the current state of every live stream to m.
func (r *Recorder) sample(m *minuteAcc) {
	m.samples++
	server := m.get(ServerPath)

	var totViewers, totIn, totOut int64
	var totCountries map[string]int64
	if r.Countries != nil {
		totCountries = make(map[string]int64)
	}

	live := make(map[*core.Stream]bool)
	for _, s := range r.Hub.Streams() {
		live[s] = true
		a := m.get(s.Path)

		var v int64
		if r.Viewers != nil {
			v = int64(r.Viewers(s.Path))
		}
		a.viewersSum += v
		a.viewersMax = max(a.viewersMax, v)
		totViewers += v

		var in, out int64
		if r.Bitrates != nil {
			in, out = r.Bitrates(s)
		}
		a.inSum += in
		a.outSum += out
		a.rateSamples++
		if !transcoded(s) {
			totIn += in
		}
		totOut += out

		din, dout := r.delta(s)
		r.addBytes(server, a, s, din, dout)

		if r.Countries != nil {
			for c, n := range r.Countries(s.Path) {
				if a.countries == nil {
					a.countries = make(map[string]int64)
				}
				a.countries[c] = max(a.countries[c], int64(n))
				totCountries[c] += int64(n)
			}
		}
	}

	// Streams that ended since the last sample: count their final bytes.
	for s := range r.last {
		if live[s] {
			continue
		}
		din, dout := r.delta(s)
		r.addBytes(server, m.get(s.Path), s, din, dout)
		delete(r.last, s)
	}

	server.viewersSum += totViewers
	server.viewersMax = max(server.viewersMax, totViewers)
	server.inSum += totIn
	server.outSum += totOut
	server.rateSamples++
	for c, n := range totCountries {
		if server.countries == nil {
			server.countries = make(map[string]int64)
		}
		server.countries[c] = max(server.countries[c], n)
	}
}

// transcoded streams are produced from another stream on this server, so
// their input is not counted again in the server's incoming traffic.
func transcoded(s *core.Stream) bool { return s.Source.Protocol == "transcode" }

func (r *Recorder) addBytes(server, a *pathAcc, s *core.Stream, din, dout int64) {
	a.bytesIn += din
	a.bytesOut += dout
	if !transcoded(s) {
		server.bytesIn += din
	}
	server.bytesOut += dout
}

// delta returns the bytes a stream moved since its previous sample. A
// stream that started after the recorder counts from zero; one that was
// already live when the recorder started counts from its first sample.
// Counters that went backwards are treated as reset, so deltas are never
// negative.
func (r *Recorder) delta(s *core.Stream) (in, out int64) {
	cur := counters{in: s.BytesIn(), out: s.BytesOut.Load()}
	prev, ok := r.last[s]
	if !ok && s.StartedAt.Before(r.started) {
		prev = cur
	}
	r.last[s] = cur
	return counterDelta(prev.in, cur.in), counterDelta(prev.out, cur.out)
}

func counterDelta(prev, cur uint64) int64 {
	if cur < prev {
		return int64(cur) // reset
	}
	return int64(cur - prev)
}

const upsertMinute = `INSERT INTO stats_minute
	(ts, path, viewers, viewers_max, bitrate_in_kbps, bitrate_out_kbps, bytes_in, bytes_out)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (ts, path) DO UPDATE SET
		viewers = CASE WHEN excluded.viewers > stats_minute.viewers
			THEN excluded.viewers ELSE stats_minute.viewers END,
		viewers_max = CASE WHEN excluded.viewers_max > stats_minute.viewers_max
			THEN excluded.viewers_max ELSE stats_minute.viewers_max END,
		bitrate_in_kbps = excluded.bitrate_in_kbps,
		bitrate_out_kbps = excluded.bitrate_out_kbps,
		bytes_in = stats_minute.bytes_in + excluded.bytes_in,
		bytes_out = stats_minute.bytes_out + excluded.bytes_out`

const upsertCountry = `INSERT INTO stats_country (ts, path, country, viewers)
	VALUES (?, ?, ?, ?)
	ON CONFLICT (ts, path, country) DO UPDATE SET
		viewers = CASE WHEN excluded.viewers > stats_country.viewers
			THEN excluded.viewers ELSE stats_country.viewers END`

// flush writes one minute. A minute written twice (the server restarted
// within it) is merged: bytes add up, viewer figures keep the larger value.
func (r *Recorder) flush(ctx context.Context, m *minuteAcc) {
	if m.samples == 0 {
		return
	}
	if err := r.write(ctx, m); err != nil {
		r.Log.Warn("cannot write statistics", "err", err)
	}
}

func (r *Recorder) write(ctx context.Context, m *minuteAcc) error {
	tx, err := r.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	qMinute, qCountry := r.DB.Q(upsertMinute), r.DB.Q(upsertCountry)
	for path, a := range m.paths {
		var in, out int64
		if a.rateSamples > 0 {
			in, out = a.inSum/a.rateSamples, a.outSum/a.rateSamples
		}
		// Divided by every sample of the minute, so a stream that was live
		// for half of it counts half: the sum of these is viewer-minutes.
		viewers := (a.viewersSum + m.samples/2) / m.samples
		if _, err := tx.ExecContext(ctx, qMinute, m.start, path, viewers, a.viewersMax,
			in, out, a.bytesIn, a.bytesOut); err != nil {
			return err
		}
		for c, n := range a.countries {
			if n <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, qCountry, m.start, path, c, n); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// cleanup deletes rows older than the retention period.
func (r *Recorder) cleanup(ctx context.Context) {
	if r.RetentionDays <= 0 {
		return
	}
	cutoff := r.now().Add(-time.Duration(r.RetentionDays) * 24 * time.Hour).UnixMilli()
	for _, table := range []string{"stats_minute", "stats_country"} {
		if _, err := r.DB.Exec(ctx, `DELETE FROM `+table+` WHERE ts < ?`, cutoff); err != nil {
			r.Log.Warn("cannot delete old statistics", "table", table, "err", err)
		}
	}
}

func init() {
	store.Register(store.Migration{ID: "0100_stats_country_path", SQL: []string{
		`CREATE INDEX stats_country_path ON stats_country (path, ts)`,
	}})
}
