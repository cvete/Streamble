package core

import (
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

// ParamKeeper tracks the latest parameter sets (SPS/PPS, plus VPS for H.265)
// of a video track and makes every keyframe self-contained.
//
// Encoders such as OBS send parameter sets only once at the start of an RTMP
// session. Outputs that start later (a new HLS muxer, a restream target, a
// recorder) need them in front of the first keyframe they see. ParamKeeper
// therefore:
//   - absorbs access units that contain only parameter sets (returns nil),
//   - prepends the remembered parameter sets to keyframes that lack them.
//
// A ParamKeeper is used by a single ingest goroutine and is not thread safe.
type ParamKeeper struct {
	h265 bool
	vps  []byte
	sps  []byte
	pps  []byte
}

// NewParamKeeper creates a keeper for the given video codec, seeded with the
// parameter sets the codec already carries. It returns nil for non-video
// codecs.
func NewParamKeeper(c Codec) *ParamKeeper {
	switch c := c.(type) {
	case *H264:
		return &ParamKeeper{sps: clone(c.SPS), pps: clone(c.PPS)}
	case *H265:
		return &ParamKeeper{h265: true, vps: clone(c.VPS), sps: clone(c.SPS), pps: clone(c.PPS)}
	}
	return nil
}

// Process returns the access unit to forward and whether it is a keyframe.
// A nil access unit means there is nothing to forward.
func (k *ParamKeeper) Process(au [][]byte) ([][]byte, bool) {
	if k.h265 {
		return k.process265(au)
	}
	return k.process264(au)
}

func (k *ParamKeeper) process264(au [][]byte) ([][]byte, bool) {
	var hasSPS, hasPPS, hasIDR, hasVCL bool
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			hasSPS = true
			k.sps = clone(nalu)
		case h264.NALUTypePPS:
			hasPPS = true
			k.pps = clone(nalu)
		case h264.NALUTypeIDR:
			hasIDR, hasVCL = true, true
		case h264.NALUTypeNonIDR, h264.NALUTypeDataPartitionA:
			hasVCL = true
		}
	}
	if !hasVCL {
		return nil, false
	}
	if !hasIDR {
		return au, false
	}
	var prefix [][]byte
	if !hasSPS && k.sps != nil {
		prefix = append(prefix, k.sps)
	}
	if !hasPPS && k.pps != nil {
		prefix = append(prefix, k.pps)
	}
	if prefix == nil {
		return au, true
	}
	return append(prefix, au...), true
}

func (k *ParamKeeper) process265(au [][]byte) ([][]byte, bool) {
	var hasVPS, hasSPS, hasPPS, hasIRAP, hasVCL bool
	for _, nalu := range au {
		if len(nalu) < 2 {
			continue
		}
		typ := h265.NALUType((nalu[0] >> 1) & 0x3f)
		switch {
		case typ == h265.NALUType_VPS_NUT:
			hasVPS = true
			k.vps = clone(nalu)
		case typ == h265.NALUType_SPS_NUT:
			hasSPS = true
			k.sps = clone(nalu)
		case typ == h265.NALUType_PPS_NUT:
			hasPPS = true
			k.pps = clone(nalu)
		case typ >= h265.NALUType_BLA_W_LP && typ <= h265.NALUType_CRA_NUT:
			hasIRAP, hasVCL = true, true
		case typ < h265.NALUType_BLA_W_LP:
			hasVCL = true
		}
	}
	if !hasVCL {
		return nil, false
	}
	if !hasIRAP {
		return au, false
	}
	var prefix [][]byte
	if !hasVPS && k.vps != nil {
		prefix = append(prefix, k.vps)
	}
	if !hasSPS && k.sps != nil {
		prefix = append(prefix, k.sps)
	}
	if !hasPPS && k.pps != nil {
		prefix = append(prefix, k.pps)
	}
	if prefix == nil {
		return au, true
	}
	return append(prefix, au...), true
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
