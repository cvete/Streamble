package transcode

import (
	"strings"
	"testing"
	"time"

	"github.com/cvete/streamble/internal/config"
)

func profile(hw string, rends ...config.Rendition) *config.Profile {
	return &config.Profile{Name: "p", HWAccel: hw, GOP: 2 * time.Second, Renditions: rends}
}

func TestPlanDropsEmptyRenditions(t *testing.T) {
	p := profile("",
		config.Rendition{Name: "720p", Height: 720},
		config.Rendition{Name: "radio", AudioOnly: true},
	)

	outs := plan(p, true, true)
	if len(outs) != 2 || outs[0].FD != 3 || outs[1].FD != 4 || outs[1].Video {
		t.Fatalf("unexpected plan: %+v", outs)
	}

	// Video-only source: the audio-only rendition has nothing to carry.
	outs = plan(p, true, false)
	if len(outs) != 1 || outs[0].Rendition.Name != "720p" || outs[0].Audio {
		t.Fatalf("unexpected plan: %+v", outs)
	}

	// Radio source: the video rendition becomes audio-only.
	outs = plan(p, false, true)
	if len(outs) != 2 || outs[0].Video {
		t.Fatalf("unexpected plan: %+v", outs)
	}
}

func TestBuildArgsSoftware(t *testing.T) {
	p := profile("",
		config.Rendition{Name: "720p", Height: 720, FPS: 30, VideoBitrate: "2500k", AudioBitrate: "128k"},
		config.Rendition{Name: "mp3", AudioOnly: true, AudioCodec: "mp3", AudioBitrate: "96k"},
	)
	args := strings.Join(buildArgs(p, plan(p, true, true)), " ")

	for _, want := range []string{
		"-f mpegts -i pipe:0",
		"-map 0:v:0 -map 0:a:0 -vf fps=30,scale=-2:'min(720,ih)',format=yuv420p -c:v libx264 -preset veryfast -tune zerolatency",
		"-b:v 2500k -maxrate 2500k -bufsize 5000k",
		"-force_key_frames expr:gte(t,n_forced*2) -sc_threshold 0",
		"-c:a aac -b:a 128k -ac 2 -f mpegts -flush_packets 1 -muxdelay 0 -muxpreload 0 pipe:3",
		"-map 0:a:0 -vn -c:a libmp3lame -b:a 96k -ac 2",
		"pipe:4",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in:\n%s", want, args)
		}
	}
}

func TestBuildArgsHardware(t *testing.T) {
	r := config.Rendition{Name: "1080p", Height: 1080}

	nv := strings.Join(buildArgs(profile("nvenc", r), plan(profile("nvenc", r), true, false)), " ")
	if !strings.Contains(nv, "-c:v h264_nvenc -preset p4") || !strings.Contains(nv, "-forced-idr 1") ||
		!strings.Contains(nv, "-b:v 5000k") || !strings.Contains(nv, "-an") {
		t.Errorf("nvenc args wrong:\n%s", nv)
	}

	va := profile("vaapi", r)
	va.VAAPIDevice = "/dev/dri/renderD129"
	vaArgs := strings.Join(buildArgs(va, plan(va, true, true)), " ")
	if !strings.HasPrefix(vaArgs, "-hide_banner -loglevel warning -nostats -vaapi_device /dev/dri/renderD129") ||
		!strings.Contains(vaArgs, "format=nv12,hwupload,scale_vaapi=w=-2:h='min(1080,ih)'") ||
		!strings.Contains(vaArgs, "-c:v h264_vaapi") {
		t.Errorf("vaapi args wrong:\n%s", vaArgs)
	}

	qsv := strings.Join(buildArgs(profile("qsv", r), plan(profile("qsv", r), true, true)), " ")
	if !strings.Contains(qsv, "-c:v h264_qsv") {
		t.Errorf("qsv args wrong:\n%s", qsv)
	}
}

func TestBitrateHelpers(t *testing.T) {
	cases := map[string]string{"2500k": "5000k", "5M": "10M", "800000": "1600000", "1.5M": "3M", "bad": "bad"}
	for in, want := range cases {
		if got := doubleBitrate(in); got != want {
			t.Errorf("doubleBitrate(%q) = %q, want %q", in, got, want)
		}
	}
	if defaultVideoBitrate(1080) != "5000k" || defaultVideoBitrate(360) != "800k" || defaultVideoBitrate(0) != "4000k" {
		t.Error("unexpected default bitrates")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 2}
	tb.Write([]byte("one\ntwo\nthr")) //nolint:errcheck
	tb.Write([]byte("ee\n\n"))        //nolint:errcheck
	if tb.last() != "three" || len(tb.lines) != 2 || tb.lines[0] != "two" {
		t.Fatalf("got %q %v", tb.last(), tb.lines)
	}
}
