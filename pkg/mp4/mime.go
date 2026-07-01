package mp4

import (
	"fmt"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

const (
	MimeH264    = "avc1.640029"
	MimeH265    = "hvc1.1.6.L150.B0" // representative Main L5.0 (actual level is dynamic)
	MimeH265HEV = "hev1.1.6.L153.B0" // in-band params (no sprop) - level unknown at start
	MimeAAC     = "mp4a.40.2"
	MimeFlac    = "flac"
	MimeOpus    = "opus"
)

// hevcMimeBody builds the profile/compat/level portion of an HEVC MIME codec string
// from a raw SPS NALU (e.g. "1.6.L150.B0"). Falls back to "1.6.L153.B0" if parsing fails.
func hevcMimeBody(sps []byte) string {
	if parsed := h265.DecodeSPS(sps); parsed != nil {
		// compat_flags as hex with trailing zero hex digits stripped (per RFC 7798).
		// 0x60000000 → "60000000" → "6"; 0x40000000 → "40000000" → "4".
		cf := fmt.Sprintf("%x", parsed.CompatFlags())
		cf = strings.TrimRight(cf, "0")
		if cf == "" {
			cf = "0"
		}
		return fmt.Sprintf("%d.%s.L%d.B0", parsed.ProfileIdc(), cf, parsed.LevelIdc())
	}
	return "1.6.L153.B0"
}

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
				s += "hvc1." + hevcMimeBody(sps) // level from real SPS (e.g. L150 for Level 5.0)
			} else {
				s += MimeH265HEV // hev1 with conservative fallback level
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
