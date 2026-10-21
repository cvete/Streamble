// Package transcode runs FFmpeg to turn a live stream into a ladder of
// renditions (different resolutions and bitrates, audio-only, MP3 for
// radio), each published back into the hub as "<app>/<stream>_<name>".
//
// FFmpeg runs as a supervised child process: the source is fed to it as
// MPEG-TS on stdin, and each rendition comes back as MPEG-TS on its own
// pipe. A crash only affects that stream's renditions and is restarted
// with backoff. Hardware encoders (NVENC, QSV, VAAPI) need nothing but the
// matching FFmpeg build and driver.
package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cvete/streamble/internal/config"
	"github.com/cvete/streamble/internal/core"
	"github.com/cvete/streamble/internal/tsconv"
)

// ProtocolTranscode marks streams produced by the transcoder in
// core.SourceInfo. They are never transcoded again.
const ProtocolTranscode = "transcode"

const (
	feedBuffer      = 4096
	maxBackoff      = 30 * time.Second
	stableAfter     = 30 * time.Second
	stderrLineLimit = 20
)

// Status describes one running transcode job.
type Status struct {
	Stream     string    `json:"stream"`
	Profile    string    `json:"profile"`
	State      string    `json:"state"` // "starting", "running", "restarting"
	LastError  string    `json:"last_error,omitempty"`
	Since      time.Time `json:"since"`
	Restarts   int       `json:"restarts"`
	Renditions []string  `json:"renditions"`
}

// Rendition is a stream produced by the transcoder.
type Rendition struct {
	Path      string
	Name      string
	Height    int
	AudioOnly bool
}

// Manager starts a job for every stream published in an app with a
// transcode profile. It implements core.Hook.
type Manager struct {
	cfg *config.Config
	hub *core.Hub
	log *slog.Logger

	mu     sync.Mutex
	jobs   map[*core.Stream]*job
	closed bool
	wg     sync.WaitGroup
}

// NewManager creates a Manager.
func NewManager(cfg *config.Config, hub *core.Hub, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, hub: hub, log: log, jobs: make(map[*core.Stream]*job)}
}

// OnPublish implements core.Hook.
func (m *Manager) OnPublish(s *core.Stream) {
	if s.Source.Protocol == ProtocolTranscode {
		return
	}
	appName, streamName, _ := strings.Cut(s.Path, "/")
	app := m.cfg.App(appName)
	if app == nil || app.Transcode == "" {
		return
	}
	profile := m.cfg.Profile(app.Transcode)
	if profile == nil {
		return
	}

	var srcVideo, srcAudio bool
	for _, t := range s.Tracks {
		if t.Codec.IsVideo() {
			srcVideo = true
		} else {
			srcAudio = true
		}
	}
	outs := plan(profile, srcVideo, srcAudio)
	if len(outs) == 0 {
		m.log.Warn("transcode: no rendition can be produced from this stream", "path", s.Path, "profile", profile.Name)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		m:       m,
		stream:  s,
		profile: profile,
		outs:    outs,
		base:    appName + "/" + streamName,
		ctx:     ctx,
		cancel:  cancel,
		state:   "starting",
		since:   time.Now(),
		log:     m.log.With("path", s.Path, "profile", profile.Name),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return
	}
	m.jobs[s] = j
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		j.run()
		m.mu.Lock()
		if m.jobs[s] == j {
			delete(m.jobs, s)
		}
		m.mu.Unlock()
	}()
}

// OnUnpublish implements core.Hook.
func (m *Manager) OnUnpublish(s *core.Stream) {
	m.mu.Lock()
	j := m.jobs[s]
	m.mu.Unlock()
	if j != nil {
		j.cancel()
	}
}

// Close stops every job and waits for FFmpeg processes to exit.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, j := range m.jobs {
		j.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Status returns the running jobs sorted by stream path.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()

	out := make([]Status, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.status())
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Stream < out[k].Stream })
	return out
}

// Renditions returns the renditions configured for a source stream path,
// in profile order, whether or not they are currently live.
func (m *Manager) Renditions(path string) []Rendition {
	m.mu.Lock()
	defer m.mu.Unlock()
	for s, j := range m.jobs {
		if s.Path != path {
			continue
		}
		out := make([]Rendition, 0, len(j.outs))
		for _, o := range j.outs {
			out = append(out, Rendition{
				Path:      j.renditionPath(o),
				Name:      o.Rendition.Name,
				Height:    o.Rendition.Height,
				AudioOnly: !o.Video,
			})
		}
		return out
	}
	return nil
}

// job supervises FFmpeg for one source stream.
type job struct {
	m       *Manager
	stream  *core.Stream
	profile *config.Profile
	outs    []output
	base    string
	ctx     context.Context
	cancel  context.CancelFunc
	log     *slog.Logger

	mu        sync.Mutex
	state     string
	since     time.Time
	restarts  int
	lastError string
}

func (j *job) renditionPath(o output) string { return j.base + "_" + o.Rendition.Name }

func (j *job) setState(state, lastErr string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != state {
		j.state = state
		j.since = time.Now()
	}
	if lastErr != "" {
		j.lastError = lastErr
	}
}

func (j *job) status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := Status{
		Stream:    j.stream.Path,
		Profile:   j.profile.Name,
		State:     j.state,
		LastError: j.lastError,
		Since:     j.since,
		Restarts:  j.restarts,
	}
	for _, o := range j.outs {
		st.Renditions = append(st.Renditions, j.renditionPath(o))
	}
	return st
}

func (j *job) run() {
	backoff := time.Second
	for {
		started := time.Now()
		err := j.runOnce()
		if j.ctx.Err() != nil {
			return
		}
		if time.Since(started) > stableAfter {
			backoff = time.Second
		}
		msg := "FFmpeg exited"
		if err != nil {
			msg = err.Error()
		}
		j.log.Warn("transcoder stopped, restarting", "err", msg, "in", backoff)
		j.mu.Lock()
		j.restarts++
		j.mu.Unlock()
		j.setState("restarting", msg)

		select {
		case <-j.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runOnce runs FFmpeg until it exits or the job is cancelled.
func (j *job) runOnce() error {
	sub := j.stream.Subscribe("transcode", feedBuffer)
	if sub == nil {
		j.cancel()
		return nil
	}
	defer sub.Close()

	ffmpeg := j.m.cfg.Transcode.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	cmd := exec.CommandContext(j.ctx, ffmpeg, buildArgs(j.profile, j.outs)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: stderrLineLimit}
	cmd.Stderr = stderr

	readers := make([]*os.File, len(j.outs))
	writers := make([]*os.File, len(j.outs))
	for i := range j.outs {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(readers)
			closeAll(writers)
			return err
		}
		readers[i], writers[i] = r, w
	}
	cmd.ExtraFiles = writers

	if err := cmd.Start(); err != nil {
		closeAll(readers)
		closeAll(writers)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("ffmpeg not found at %q (set transcode.ffmpeg)", ffmpeg)
		}
		return err
	}
	// The child has its own copies; ours must close so readers see EOF.
	closeAll(writers)

	j.setState("running", "")
	j.log.Info("transcoder started", "renditions", len(j.outs))

	var wg sync.WaitGroup
	for i, o := range j.outs {
		wg.Add(1)
		go func(r *os.File, path string) {
			defer wg.Done()
			defer r.Close()
			src := core.SourceInfo{Protocol: ProtocolTranscode, RemoteAddr: j.stream.Path}
			err := tsconv.Demux(r, j.m.hub, path, src, j.log)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && j.ctx.Err() == nil {
				j.log.Debug("rendition ended", "rendition", path, "err", err)
			}
			if errors.Is(err, core.ErrAlreadyPublishing) {
				j.log.Warn("rendition path is already in use by another publisher", "rendition", path)
			}
		}(readers[i], j.renditionPath(o))
	}

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer stdin.Close()
		mux, err := tsconv.NewMuxer(stdin, j.stream.Tracks)
		if err != nil {
			j.log.Error("transcoder cannot read this stream", "err", err)
			return
		}
		for u := range sub.C() {
			if err := mux.WriteUnit(u); err != nil {
				return // FFmpeg exited or stalled; Wait reports why
			}
		}
	}()

	waitErr := cmd.Wait()
	sub.Close()
	<-feedDone
	wg.Wait()

	if j.ctx.Err() != nil {
		return nil
	}
	if line := stderr.last(); line != "" {
		return fmt.Errorf("%v: %s", waitErr, line)
	}
	return waitErr
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}

// tailBuffer keeps the last lines FFmpeg wrote to stderr.
type tailBuffer struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial strings.Builder
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			if line := strings.TrimSpace(t.partial.String()); line != "" {
				t.lines = append(t.lines, line)
				if len(t.lines) > t.max {
					t.lines = t.lines[1:]
				}
			}
			t.partial.Reset()
			continue
		}
		t.partial.WriteByte(b)
	}
	return len(p), nil
}

func (t *tailBuffer) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := strings.TrimSpace(t.partial.String()); p != "" {
		return p
	}
	if len(t.lines) == 0 {
		return ""
	}
	return t.lines[len(t.lines)-1]
}
