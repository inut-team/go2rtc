// Package h265 - MPEG4 format related functions
package h265

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func DecodeConfig(conf []byte) (profile, vps, sps, pps []byte) {
	profile = conf[1:4]

	b := conf[23:]
	if binary.BigEndian.Uint16(b[1:]) != 1 {
		return
	}
	vpsSize := binary.BigEndian.Uint16(b[3:])
	vps = b[5 : 5+vpsSize]

	b = conf[23+5+vpsSize:]
	if binary.BigEndian.Uint16(b[1:]) != 1 {
		return
	}
	spsSize := binary.BigEndian.Uint16(b[3:])
	sps = b[5 : 5+spsSize]

	b = conf[23+5+vpsSize+5+spsSize:]
	if binary.BigEndian.Uint16(b[1:]) != 1 {
		return
	}
	ppsSize := binary.BigEndian.Uint16(b[3:])
	pps = b[5 : 5+ppsSize]

	return
}

// stripStartCode removes Annex B start codes (00 00 00 01 or 00 00 01) if present.
// Some cameras include them in sprop-vps/sps/pps (non-standard per RFC 7798).
func stripStartCode(b []byte) []byte {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return b[4:]
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return b[3:]
	}
	return b
}

func EncodeConfig(vps, sps, pps []byte) []byte {
	vps = stripStartCode(vps)
	sps = stripStartCode(sps)
	pps = stripStartCode(pps)

	vpsSize := uint16(len(vps))
	spsSize := uint16(len(sps))
	ppsSize := uint16(len(pps))

	buf := make([]byte, 23+5+vpsSize+5+spsSize+5+ppsSize)

	buf[0] = 1
	copy(buf[1:], sps[3:6]) // general_profile_space/tier/idc + first 2 compat bytes
	// Fill level and chroma fields from the parsed SPS so iOS VideoToolbox can select the
	// correct hardware decoder path. Without these, level_idc=0 causes iOS to fall back to
	// software decode, which cannot keep up with high-resolution HEVC (e.g. 2880×1620).
	// constantFrameRate(2)=0 | numTemporalLayers(3) | temporalIdNested(1) | lengthSizeMinusOne(2)=3.
	// numTemporalLayers MUST NOT be 0 ("unknown") — Apple tooling always writes >=1 and
	// iOS CoreMedia rejects the video track otherwise (audio-only playback symptom).
	buf[21] = 1<<3 | 1<<2 | 3 // sane default: 1 layer, nested, 4-byte lengths
	if s := DecodeSPS(sps); s != nil {
		buf[12] = s.LevelIdc()                     // general_level_idc (e.g. 150 = Level 5.0)
		buf[13] = 0xF0                             // reserved(4)=1111 | min_spatial_segmentation_idc[11:8]=0
		buf[15] = 0xFC                             // reserved(6)=111111 | parallelismType=0
		buf[16] = 0xFC | byte(s.ChromaFormatIdc()) // reserved(6)=111111 | chroma_format_idc
		buf[17] = 0xF8                             // reserved(5)=11111 | bit_depth_luma_minus8=0
		buf[18] = 0xF8                             // reserved(5)=11111 | bit_depth_chroma_minus8=0
		buf[21] = s.NumTemporalLayers()<<3 | s.TemporalIdNested()<<2 | 3
	}
	buf[22] = 3 // numOfArrays=3 (VPS, SPS, PPS)

	// ISO 14496-15 §8.3.3.1: for hvc1 sample entry, array_completeness (bit 7) MUST be 1.
	// With it 0, iOS AVFoundation interprets the arrays as incomplete and attempts in-band
	// parameter set lookup, finds nothing, and fails to initialize the video decoder.
	b := buf[23:]
	_ = b[5]
	b[0] = 0x80 | ((vps[0] >> 1) & 0x3F) // array_completeness=1 | nal_unit_type=VPS(32)
	binary.BigEndian.PutUint16(b[1:], 1)  // VPS count
	binary.BigEndian.PutUint16(b[3:], vpsSize)
	copy(b[5:], vps)

	b = buf[23+5+vpsSize:]
	_ = b[5]
	b[0] = 0x80 | ((sps[0] >> 1) & 0x3F) // array_completeness=1 | nal_unit_type=SPS(33)
	binary.BigEndian.PutUint16(b[1:], 1)  // SPS count
	binary.BigEndian.PutUint16(b[3:], spsSize)
	copy(b[5:], sps)

	b = buf[23+5+vpsSize+5+spsSize:]
	_ = b[5]
	b[0] = 0x80 | ((pps[0] >> 1) & 0x3F) // array_completeness=1 | nal_unit_type=PPS(34)
	binary.BigEndian.PutUint16(b[1:], 1)  // PPS count
	binary.BigEndian.PutUint16(b[3:], ppsSize)
	copy(b[5:], pps)

	return buf
}

func ConfigToCodec(conf []byte) *core.Codec {
	buf := bytes.NewBufferString("profile-id=1")

	_, vps, sps, pps := DecodeConfig(conf)
	if vps != nil {
		buf.WriteString(";sprop-vps=")
		buf.WriteString(base64.StdEncoding.EncodeToString(vps))
	}
	if sps != nil {
		buf.WriteString(";sprop-sps=")
		buf.WriteString(base64.StdEncoding.EncodeToString(sps))
	}
	if pps != nil {
		buf.WriteString(";sprop-pps=")
		buf.WriteString(base64.StdEncoding.EncodeToString(pps))
	}

	return &core.Codec{
		Name:        core.CodecH265,
		ClockRate:   90000,
		FmtpLine:    buf.String(),
		PayloadType: core.PayloadTypeRAW,
	}
}
