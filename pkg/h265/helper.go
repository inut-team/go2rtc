package h265

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

const (
	NALUTypePFrame    = 1
	NALUTypeIFrame    = 19
	NALUTypeIFrame2   = 20
	NALUTypeIFrame3   = 21
	NALUTypeVPS       = 32
	NALUTypeSPS       = 33
	NALUTypePPS       = 34
	NALUTypePrefixSEI = 39
	NALUTypeSuffixSEI = 40
	NALUTypeFU        = 49
)

func NALUType(b []byte) byte {
	return (b[4] >> 1) & 0x3F
}

func IsKeyframe(b []byte) bool {
	for {
		switch NALUType(b) {
		case NALUTypePFrame:
			return false
		case NALUTypeIFrame, NALUTypeIFrame2, NALUTypeIFrame3:
			return true
		}

		size := int(binary.BigEndian.Uint32(b)) + 4
		if size < len(b) {
			b = b[size:]
			continue
		} else {
			return false
		}
	}
}

func Types(data []byte) []byte {
	var types []byte
	for {
		types = append(types, NALUType(data))

		size := 4 + int(binary.BigEndian.Uint32(data))
		if size < len(data) {
			data = data[size:]
		} else {
			break
		}
	}
	return types
}

// SplitEmbeddedAnnexB repairs AVCC payloads whose NALUs contain embedded
// Annex-B start codes. Some camera firmwares (e.g. Dahua) pack ALL slices of
// a picture into one RTP FU stream separated by start codes; naive FU
// reassembly then yields one giant "NALU" that is really slice1 + 00000001 +
// slice2. Lenient decoders (ffmpeg) resync on the start code, but strict ones
// (iOS VideoToolbox) reject the sample - black video with working audio.
// Emulation prevention guarantees 00 00 01 cannot occur inside a real NALU,
// so splitting on it is safe. Returns the input unchanged when clean.
func SplitEmbeddedAnnexB(avcc []byte) []byte {
	dirty := false
	for i := 0; i+4 <= len(avcc); {
		size := int(binary.BigEndian.Uint32(avcc[i:]))
		end := i + 4 + size
		if size == 0 || end > len(avcc) {
			return avcc // malformed - leave untouched
		}
		if bytes.Index(avcc[i+4:end], []byte{0, 0, 1}) >= 0 {
			dirty = true
			break
		}
		i = end
	}
	if !dirty {
		return avcc
	}

	out := make([]byte, 0, len(avcc))
	for i := 0; i+4 <= len(avcc); {
		size := int(binary.BigEndian.Uint32(avcc[i:]))
		end := i + 4 + size
		nal := avcc[i+4 : end]
		start := 0
		for {
			idx := bytes.Index(nal[start:], []byte{0, 0, 1})
			if idx < 0 {
				break
			}
			cut := start + idx
			pieceEnd := cut
			if pieceEnd > 0 && nal[pieceEnd-1] == 0 { // 4-byte start code
				pieceEnd--
			}
			if pieceEnd > start {
				out = binary.BigEndian.AppendUint32(out, uint32(pieceEnd-start))
				out = append(out, nal[start:pieceEnd]...)
			}
			start = cut + 3
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(nal)-start))
		out = append(out, nal[start:]...)
		i = end
	}
	return out
}

// ExtractParamSets scans AVCC-format H265 data and returns the raw VPS, SPS, and PPS NAL units.
// Cameras that omit sprop in SDP (e.g. hikvision analog) include these before the IDR frame.
func ExtractParamSets(data []byte) (vps, sps, pps []byte) {
	for len(data) >= 5 {
		size := int(binary.BigEndian.Uint32(data[:4]))
		if size <= 0 || 4+size > len(data) {
			break
		}
		nalu := data[4 : 4+size]
		switch (nalu[0] >> 1) & 0x3F {
		case NALUTypeVPS:
			vps = append([]byte(nil), nalu...)
		case NALUTypeSPS:
			sps = append([]byte(nil), nalu...)
		case NALUTypePPS:
			pps = append([]byte(nil), nalu...)
		case NALUTypeIFrame, NALUTypeIFrame2, NALUTypeIFrame3:
			return // params always precede IDR; stop early
		}
		data = data[4+size:]
	}
	return
}

// StripParamSets removes VPS/SPS/PPS NALUs from an AVCC-format HEVC payload.
// ISO 14496-15 §8.3.2.3.1: for hvc1 sample entries the mdat shall not contain
// VPS/SPS/PPS NALUs — they live in hvcC only. Inline param sets cause iOS
// VideoToolbox to reject the sample, producing black video with audio only.
func StripParamSets(data []byte) []byte {
	var out []byte
	for len(data) >= 4 {
		size := int(binary.BigEndian.Uint32(data[:4]))
		end := 4 + size
		if size == 0 || end > len(data) {
			break
		}
		nalType := (data[4] >> 1) & 0x3F
		if nalType < NALUTypeVPS || nalType > NALUTypePPS {
			out = append(out, data[:end]...)
		}
		data = data[end:]
	}
	return out
}

func GetParameterSet(fmtp string) (vps, sps, pps []byte) {
	if fmtp == "" {
		return
	}

	s := core.Between(fmtp, "sprop-vps=", ";")
	vps, _ = base64.StdEncoding.DecodeString(s)
	vps = stripAnnexB(vps)

	s = core.Between(fmtp, "sprop-sps=", ";")
	sps, _ = base64.StdEncoding.DecodeString(s)
	sps = stripAnnexB(sps)

	s = core.Between(fmtp, "sprop-pps=", ";")
	pps, _ = base64.StdEncoding.DecodeString(s)
	pps = stripAnnexB(pps)

	return
}

// stripAnnexB removes Annex-B start codes (00 00 00 01 or 00 00 01) if present.
// Some cameras incorrectly include them in sprop-vps/sps/pps (non-standard per RFC 7798).
func stripAnnexB(b []byte) []byte {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return b[4:]
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return b[3:]
	}
	return b
}
