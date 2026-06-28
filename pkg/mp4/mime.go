package mp4

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

const (
	MimeH264    = "avc1.640029"
	MimeH265    = "hvc1.1.6.L153.B0" // out-of-band params (sprop present)
	MimeH265HEV = "hev1.1.6.L153.B0" // in-band params (no sprop in SDP)
	MimeAAC     = "mp4a.40.2"
	MimeFlac    = "flac"
	MimeOpus    = "opus"
)

func MimeCodecs(codecs []*core.Codec) string {
	var s string

	for i, codec := range codecs {
		if i > 0 {
			s += ","
		}

		switch codec.Name {
		case core.CodecH264:
			s += "avc1." + h264.GetProfileLevelID(codec.FmtpLine)
		case core.CodecH265:
			// Use hvc1 (out-of-band, needs real sprop) or hev1 (in-band, no sprop).
			// MIME must match the actual box type used in the init segment.
			vps, sps, pps := h265.GetParameterSet(codec.FmtpLine)
			if len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
				s += MimeH265    // hvc1.1.6.L153.B0
			} else {
				s += MimeH265HEV // hev1.1.6.L153.B0
			}
		case core.CodecAAC:
			s += MimeAAC
		case core.CodecOpus:
			s += MimeOpus
		case core.CodecFLAC:
			s += MimeFlac
		}
	}

	return s
}

func ContentType(codecs []*core.Codec) string {
	return `video/mp4; codecs="` + MimeCodecs(codecs) + `"`
}
