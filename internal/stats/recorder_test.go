package stats

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/store"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a settable clock for driving tick by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func newRecorder(d *store.DB, l *live, clock func() time.Time) *Recorder {
	r := &Recorder{DB: d, Hub: newHub(), Viewers: l.Viewers, Bitrates: l.Bitrates,
		Countries: l.Countries, RetentionDays: 30, Log: discard, now: clock}
	r.init()
	return r
}

func TestMinuteAggregation(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		ctx := context.Background()
		l := newLive()
		clock := &fakeClock{now: t0}
		r := newRecorder(d, l, clock.Now)
		a := publish(t, r.Hub, "live/a", "rtmp")
		b := publish(t, r.Hub, "live/b", "srt")
		tr := publish(t, r.Hub, "live/a_720p", "transcode")
		l.rates["live/a"] = [2]int64{1000, 2000}
		l.rates["live/b"] = [2]int64{500, 0}
		l.rates["live/a_720p"] = [2]int64{800, 100}

		a.BytesOut.Add(100)
		writeBytes(b, 50)
		writeBytes(tr, 30) // transcoded input: not server traffic
		samples := []struct {
			a, b      int
			ac, bc    map[string]int
			addOutToA uint64
		}{
			{1, 1, map[string]int{"FR": 1}, map[string]int{"FR": 1}, 200},
			{2, 1, map[string]int{"FR": 2, "MK": 1}, map[string]int{"FR": 1}, 0},
			{6, 1, map[string]int{"FR": 1}, map[string]int{"FR": 1}, 0},
		}
		for i, s := range samples {
			l.set("live/a", s.a, s.ac)
			l.set("live/b", s.b, s.bc)
			clock.now = t0.Add(time.Duration(i) * 10 * time.Second)
			r.tick(ctx, clock.now)
			a.BytesOut.Add(s.addOutToA)
		}
		if _, ok := readMinute(t, d, t0.UnixMilli(), "live/a"); ok {
			t.Fatal("minute written before it ended")
		}
		// The first tick of the next minute writes the previous one.
		r.tick(ctx, t0.Add(time.Minute))

		ts := t0.UnixMilli()
		got, ok := readMinute(t, d, ts, "live/a")
		want := minuteRow{viewers: 3, viewersMax: 6, in: 1000, out: 2000, bytesIn: 0, bytesOut: 300}
		if !ok || got != want {
			t.Fatalf("live/a = %+v, want %+v", got, want)
		}
		got, _ = readMinute(t, d, ts, "live/b")
		if want := (minuteRow{viewers: 1, viewersMax: 1, in: 500, bytesIn: 50}); got != want {
			t.Fatalf("live/b = %+v, want %+v", got, want)
		}
		got, _ = readMinute(t, d, ts, "live/a_720p")
		if want := (minuteRow{in: 800, out: 100, bytesIn: 30}); got != want {
			t.Fatalf("rendition = %+v, want %+v", got, want)
		}
		// Server: viewer samples 2, 3, 7; input excludes the rendition.
		got, _ = readMinute(t, d, ts, ServerPath)
		if want := (minuteRow{viewers: 4, viewersMax: 7, in: 1500, out: 2100, bytesIn: 50, bytesOut: 300}); got != want {
			t.Fatalf("server = %+v, want %+v", got, want)
		}

		if c := readCountries(t, d, ts, "live/a"); len(c) != 2 || c["FR"] != 2 || c["MK"] != 1 {
			t.Fatalf("live/a countries = %v", c)
		}
		if c := readCountries(t, d, ts, ServerPath); len(c) != 2 || c["FR"] != 3 || c["MK"] != 1 {
			t.Fatalf("server countries = %v", c)
		}
		if c := readCountries(t, d, ts, "live/a_720p"); len(c) != 0 {
			t.Fatalf("rendition countries = %v", c)
		}
	})
}

func TestCounterReset(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		ctx := context.Background()
		l := newLive()
		clock := &fakeClock{now: t0}
		r := newRecorder(d, l, clock.Now)

		// A stream already live when the recorder starts counts from its
		// first sample, not from zero.
		old := publish(t, r.Hub, "live/old", "rtmp")
		old.BytesOut.Add(1_000_000)
		r.started = time.Now()

		a := publish(t, r.Hub, "live/a", "rtmp")
		a.BytesOut.Add(500)
		r.tick(ctx, t0)
		old.BytesOut.Add(10)

		// The publisher reconnects: the old stream's last bytes count, and
		// the new stream's counters start from zero.
		a.BytesOut.Add(200)
		r.Hub.Unpublish(a)
		a2 := publish(t, r.Hub, "live/a", "rtmp")
		a2.BytesOut.Add(50)
		r.tick(ctx, t0.Add(10*time.Second))
		r.tick(ctx, t0.Add(time.Minute))

		ts := t0.UnixMilli()
		if got, _ := readMinute(t, d, ts, "live/a"); got.bytesOut != 750 {
			t.Fatalf("live/a bytes_out = %d, want 750", got.bytesOut)
		}
		if got, _ := readMinute(t, d, ts, "live/old"); got.bytesOut != 10 {
			t.Fatalf("live/old bytes_out = %d, want 10", got.bytesOut)
		}
		if got, _ := readMinute(t, d, ts, ServerPath); got.bytesOut != 760 {
			t.Fatalf("server bytes_out = %d, want 760", got.bytesOut)
		}
	})
}

func TestCounterDelta(t *testing.T) {
	for _, c := range []struct{ prev, cur uint64 }{{10, 25}, {100, 7}, {0, 0}} {
		got := counterDelta(c.prev, c.cur)
		if got < 0 || (c.cur >= c.prev && got != int64(c.cur-c.prev)) || (c.cur < c.prev && got != int64(c.cur)) {
			t.Fatalf("counterDelta(%d, %d) = %d", c.prev, c.cur, got)
		}
	}
}

// TestRestartWithinMinute writes the same minute twice, as after a quick
// restart: the insert must not fail and the values merge.
func TestRestartWithinMinute(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		ctx := context.Background()
		for i, viewers := range []int{3, 1} {
			l := newLive()
			r := newRecorder(d, l, func() time.Time { return t0 })
			s := publish(t, r.Hub, "live/a", "rtmp")
			s.BytesOut.Add(100)
			l.set("live/a", viewers, map[string]int{"FR": viewers})
			r.tick(ctx, t0.Add(time.Duration(i)*20*time.Second))
			r.flush(ctx, r.cur)
		}
		got, _ := readMinute(t, d, t0.UnixMilli(), "live/a")
		if got.bytesOut != 200 || got.viewers != 3 || got.viewersMax != 3 {
			t.Fatalf("merged row = %+v", got)
		}
		if c := readCountries(t, d, t0.UnixMilli(), "live/a"); c["FR"] != 3 {
			t.Fatalf("merged countries = %v", c)
		}
	})
}

func TestNoCountriesWithoutFunc(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		l := newLive()
		r := newRecorder(d, l, func() time.Time { return t0 })
		r.Countries = nil
		publish(t, r.Hub, "live/a", "rtmp")
		l.set("live/a", 2, map[string]int{"FR": 2})
		r.tick(context.Background(), t0)
		r.flush(context.Background(), r.cur)
		if n := count(t, d, "stats_country"); n != 0 {
			t.Fatalf("%d country rows", n)
		}
		if got, _ := readMinute(t, d, t0.UnixMilli(), "live/a"); got.viewers != 2 {
			t.Fatalf("row = %+v", got)
		}
	})
}

func TestRetention(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		ctx := context.Background()
		r := newRecorder(d, newLive(), func() time.Time { return t0 })
		r.RetentionDays = 2
		for _, ts := range []time.Time{t0.Add(-72 * time.Hour), t0.Add(-47 * time.Hour), t0} {
			if _, err := d.Exec(ctx, `INSERT INTO stats_minute VALUES (?, '', 1, 1, 0, 0, 0, 0)`, ts.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(ctx, `INSERT INTO stats_country VALUES (?, '', 'FR', 1)`, ts.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		r.cleanup(ctx)
		if a, b := count(t, d, "stats_minute"), count(t, d, "stats_country"); a != 2 || b != 2 {
			t.Fatalf("after cleanup: %d minute rows, %d country rows", a, b)
		}
	})
}

// TestLoop runs the real background loop with a fast clock: 100 ms
// "minutes" sampled every 10 ms.
func TestLoop(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *store.DB) {
		ctx := context.Background()
		old := time.Now().Add(-3 * 24 * time.Hour).UnixMilli()
		if _, err := d.Exec(ctx, `INSERT INTO stats_minute VALUES (?, '', 1, 1, 0, 0, 0, 0)`, old); err != nil {
			t.Fatal(err)
		}

		l := newLive()
		r := &Recorder{DB: d, Hub: newHub(), Viewers: l.Viewers, Bitrates: l.Bitrates, Countries: l.Countries,
			RetentionDays: 1, Log: discard,
			sampleEvery: 10 * time.Millisecond, minute: 100 * time.Millisecond, cleanupEvery: 50 * time.Millisecond}
		l.set("live/a", 2, map[string]int{"--": 2})
		r.Start()
		r.Start()                                // idempotent
		s := publish(t, r.Hub, "live/a", "rtmp") // after Start: counted from zero
		for i := 0; i < 30; i++ {
			s.BytesOut.Add(1000)
			time.Sleep(10 * time.Millisecond)
		}
		r.Close()
		r.Close() // idempotent

		var rows, viewers, bytesOut int64
		if err := d.QueryRow(ctx, `SELECT COUNT(*), MAX(viewers_max), SUM(bytes_out) FROM stats_minute WHERE path = ?`,
			"live/a").Scan(&rows, &viewers, &bytesOut); err != nil {
			t.Fatal(err)
		}
		if rows < 2 || viewers != 2 || bytesOut != 30000 {
			t.Fatalf("rows=%d viewers_max=%d bytes_out=%d", rows, viewers, bytesOut)
		}
		var n int
		if err := d.QueryRow(ctx, `SELECT COUNT(*) FROM stats_minute WHERE ts = ?`, old).Scan(&n); err != nil || n != 0 {
			t.Fatalf("old row not deleted: n=%d err=%v", n, err)
		}
		if count(t, d, "stats_country") == 0 {
			t.Fatal("no country rows")
		}
	})
}

func TestCloseWithoutStart(t *testing.T) {
	(&Recorder{}).Close()
}
