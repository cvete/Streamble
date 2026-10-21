package transcode

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cvete/streamble/internal/config"
)

// output is one rendition that FFmpeg writes to a pipe.
type output struct {
	Rendition config.Rendition
	// FD is the file descriptor FFmpeg writes this rendition to (pipe:FD).
	FD    int
	Video bool
	Audio bool
}

// plan decides which renditions can be produced from a source and what
// each contains. Renditions that would be empty (an audio-only rendition
// of a source without audio) are dropped.
func plan(p *config.Profile, srcVideo, srcAudio bool) []output {
	var outs []output
	fd := 3 // 0-2 are stdin, stdout, stderr; ExtraFiles start at 3
	for _, r := range p.Renditions {
		o := output{Rendition: r, Video: srcVideo && !r.AudioOnly, Audio: srcAudio}
		if !o.Video && !o.Audio {
			continue
		}
		o.FD = fd
		fd++
		outs = append(outs, o)
	}
	return outs
}

// buildArgs returns the FFmpeg command line (without the binary) that reads
// MPEG-TS on stdin and writes one MPEG-TS stream per output to its pipe.
func buildArgs(p *config.Profile, outs []output) []string {
	gop := p.GOP.D()
	if gop <= 0 {
		gop = 2 * time.Second
	}
	gopSecs := strconv.FormatFloat(gop.Seconds(), 'f', -1, 64)

	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats"}
	if p.HWAccel == "vaapi" {
		dev := p.VAAPIDevice
		if dev == "" {
			dev = "/dev/dri/renderD128"
		}
		args = append(args, "-vaapi_device", dev)
	}
	args = append(args,
		"-fflags", "+genpts",
		"-f", "mpegts", "-i", "pipe:0",
	)

	for _, o := range outs {
		r := o.Rendition
		if o.Video {
			args = append(args, "-map", "0:v:0")
		}
		if o.Audio {
			args = append(args, "-map", "0:a:0")
		}

		if o.Video {
			args = append(args, "-vf", videoFilter(p, r))
			args = append(args, videoEncoder(p)...)
			br := r.VideoBitrate
			if br == "" {
				br = defaultVideoBitrate(r.Height)
			}
			args = append(args, "-b:v", br, "-maxrate", br, "-bufsize", doubleBitrate(br))
			args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+gopSecs+")")
			switch p.HWAccel {
			case "", "none":
				args = append(args, "-sc_threshold", "0")
			case "nvenc":
				args = append(args, "-forced-idr", "1")
			}
		} else {
			args = append(args, "-vn")
		}

		if o.Audio {
			abr := r.AudioBitrate
			if abr == "" {
				abr = "128k"
			}
			if r.AudioCodec == "mp3" {
				args = append(args, "-c:a", "libmp3lame")
			} else {
				args = append(args, "-c:a", "aac")
			}
			args = append(args, "-b:a", abr, "-ac", "2")
		} else {
			args = append(args, "-an")
		}

		args = append(args,
			"-f", "mpegts",
			"-flush_packets", "1",
			"-muxdelay", "0",
			"-muxpreload", "0",
			fmt.Sprintf("pipe:%d", o.FD),
		)
	}
	return args
}

// videoFilter scales to the rendition height (never upscaling, keeping the
// aspect ratio with an even width) and caps the frame rate.
func videoFilter(p *config.Profile, r config.Rendition) string {
	var f []string
	if r.FPS > 0 {
		f = append(f, fmt.Sprintf("fps=%d", r.FPS))
	}
	if p.HWAccel == "vaapi" {
		f = append(f, "format=nv12", "hwupload")
		if r.Height > 0 {
			f = append(f, fmt.Sprintf("scale_vaapi=w=-2:h='min(%d,ih)'", r.Height))
		}
		return strings.Join(f, ",")
	}
	if r.Height > 0 {
		f = append(f, fmt.Sprintf("scale=-2:'min(%d,ih)'", r.Height))
	}
	f = append(f, "format=yuv420p")
	return strings.Join(f, ",")
}

func videoEncoder(p *config.Profile) []string {
	preset := p.Preset
	switch p.HWAccel {
	case "nvenc":
		if preset == "" {
			preset = "p4"
		}
		return []string{"-c:v", "h264_nvenc", "-preset", preset, "-tune", "ll", "-profile:v", "high", "-bf", "0"}
	case "qsv":
		if preset == "" {
			preset = "veryfast"
		}
		return []string{"-c:v", "h264_qsv", "-preset", preset, "-profile:v", "high", "-bf", "0"}
	case "vaapi":
		return []string{"-c:v", "h264_vaapi", "-profile:v", "high", "-bf", "0"}
	default:
		if preset == "" {
			preset = "veryfast"
		}
		return []string{"-c:v", "libx264", "-preset", preset, "-tune", "zerolatency", "-profile:v", "high"}
	}
}

func defaultVideoBitrate(height int) string {
	switch {
	case height == 0:
		return "4000k"
	case height >= 1080:
		return "5000k"
	case height >= 720:
		return "2800k"
	case height >= 480:
		return "1400k"
	case height >= 360:
		return "800k"
	default:
		return "400k"
	}
}

// doubleBitrate turns "2500k" into "5000k" for the VBV buffer size.
func doubleBitrate(br string) string {
	n, unit := parseBitrate(br)
	if n <= 0 {
		return br
	}
	return strconv.FormatFloat(n*2, 'f', -1, 64) + unit
}

func parseBitrate(br string) (float64, string) {
	br = strings.TrimSpace(br)
	unit := ""
	if br != "" {
		switch last := br[len(br)-1]; last {
		case 'k', 'K', 'm', 'M':
			unit = string(last)
			br = br[:len(br)-1]
		}
	}
	n, err := strconv.ParseFloat(br, 64)
	if err != nil {
		return 0, ""
	}
	return n, unit
}
