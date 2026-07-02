package hls

import (
	"encoding/binary"
)

// repackFMP4 merges a stream of single-sample moof+mdat pairs (as produced by
// pkg/mp4.Muxer for MSE streaming) into ONE movie fragment: one moof with one
// traf per track, followed by one mdat. iOS AVFoundation rejects HLS segments
// made of dozens of one-frame movie fragments, while a single fragment per
// segment matches what ffmpeg/Apple tooling produces and every player accepts.
// On any parse anomaly the input is returned unchanged.
func repackFMP4(buf []byte, seq uint32) []byte {
	type sample struct {
		dts   uint64
		dur   uint32
		flags uint32
		cts   uint32
		data  []byte
	}

	var order []uint32
	samples := map[uint32][]sample{}

	u32 := binary.BigEndian.Uint32
	u64 := binary.BigEndian.Uint64

	off := 0
	for off+8 <= len(buf) {
		size := int(u32(buf[off:]))
		if size < 8 || off+size > len(buf) {
			return buf
		}
		if string(buf[off+4:off+8]) != "moof" {
			off += size
			continue
		}

		// ── parse one moof (expects the pkg/mp4.Muxer layout: 1 traf, 1 sample) ──
		var smp sample
		var trackID uint32
		var sampleCount uint32
		moof := buf[off+8 : off+size]
		for p := 0; p+8 <= len(moof); {
			bsize := int(u32(moof[p:]))
			if bsize < 8 || p+bsize > len(moof) {
				return buf
			}
			btyp := string(moof[p+4 : p+8])
			if btyp != "traf" {
				p += bsize
				continue
			}
			traf := moof[p+8 : p+bsize]
			for q := 0; q+8 <= len(traf); {
				csize := int(u32(traf[q:]))
				if csize < 8 || q+csize > len(traf) {
					return buf
				}
				body := traf[q+8 : q+csize]
				switch string(traf[q+4 : q+8]) {
				case "tfhd":
					flags := u32(body) & 0xFFFFFF
					trackID = u32(body[4:])
					r := 8
					if flags&0x000001 != 0 { // base-data-offset
						r += 8
					}
					if flags&0x000002 != 0 { // sample-description-index
						r += 4
					}
					if flags&0x000008 != 0 { // default-sample-duration
						smp.dur = u32(body[r:])
						r += 4
					}
					if flags&0x000010 != 0 { // default-sample-size
						r += 4
					}
					if flags&0x000020 != 0 { // default-sample-flags
						smp.flags = u32(body[r:])
					}
				case "tfdt":
					if body[0] == 1 {
						smp.dts = u64(body[4:])
					} else {
						smp.dts = uint64(u32(body[4:]))
					}
				case "trun":
					flags := u32(body) & 0xFFFFFF
					sampleCount = u32(body[4:])
					r := 8
					if flags&0x000001 != 0 { // data-offset
						r += 4
					}
					if flags&0x000004 != 0 { // first-sample-flags
						smp.flags = u32(body[r:])
						r += 4
					}
					if flags&0x000100 != 0 { // sample-duration
						smp.dur = u32(body[r:])
						r += 4
					}
					if flags&0x000200 != 0 { // sample-size
						r += 4
					}
					if flags&0x000400 != 0 { // sample-flags
						smp.flags = u32(body[r:])
						r += 4
					}
					if flags&0x000800 != 0 { // composition-time-offset
						smp.cts = u32(body[r:])
					}
				}
				q += csize
			}
			p += bsize
		}
		if sampleCount != 1 {
			return buf // not the expected one-sample-per-moof layout
		}

		// ── the following box must be this sample's mdat ──
		mo := off + size
		if mo+8 > len(buf) {
			return buf
		}
		msize := int(u32(buf[mo:]))
		if msize < 8 || mo+msize > len(buf) || string(buf[mo+4:mo+8]) != "mdat" {
			return buf
		}
		smp.data = buf[mo+8 : mo+msize]

		if _, seen := samples[trackID]; !seen {
			order = append(order, trackID)
		}
		samples[trackID] = append(samples[trackID], smp)
		off = mo + msize
	}
	if len(order) == 0 {
		return buf
	}

	// traf order = trackID order (video track 1 first). Ordering by buffer
	// appearance put the audio traf first in most segments (audio samples
	// arrive right after the previous IDR flush) — iOS AVFoundation played
	// those segments audio-only with black video.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && order[j] < order[j-1]; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}

	// tfhd carries default size/flags; the first video sample is marked sync via
	// first-sample-flags. The trun lists per-sample DURATION and size: cameras
	// like the Xiaomi cam3_2nd are variable-frame-rate (pts deltas alternate
	// ~20ms/~40ms), so a single default duration would make the segment's media
	// span (n × default) diverge from the real GOP length that tfdt advances by
	// — opening a ~1s gap in the media timeline every segment and freezing iOS.
	// Emitting the true per-sample durations keeps the timeline gapless (this is
	// what ffmpeg does for VFR). Dahua-style true-CBR streams are unaffected: all
	// their durations are equal, so per-sample == the old default.
	mode := func(smps []sample) (dur, flags uint32) {
		durN := map[uint32]int{}
		for _, s := range smps {
			durN[s.dur]++
		}
		for d, c := range durN {
			if c > durN[dur] {
				dur = d
			}
		}
		return dur, smps[len(smps)-1].flags // last sample: steady-state flags
	}

	// ── sizes ──
	const mfhdLen, tfhdLen, tfdtLen = 16, 28, 20
	trunLen := func(n int, firstFlags bool) int {
		l := 8 + 4 + 4 + 4 + n*8 // verflags+count+data-offset + (duration+size)*n
		if firstFlags {
			l += 4
		}
		return l
	}
	moofSize := 8 + mfhdLen
	dataLen := 0
	for _, tid := range order {
		smps := samples[tid]
		moofSize += 8 + tfhdLen + tfdtLen + trunLen(len(smps), smps[0].flags != smps[len(smps)-1].flags)
		for _, s := range smps {
			dataLen += len(s.data)
		}
	}

	out := make([]byte, 0, moofSize+8+dataLen)
	b4 := func(v uint32) { out = binary.BigEndian.AppendUint32(out, v) }
	b8 := func(v uint64) { out = binary.BigEndian.AppendUint64(out, v) }
	tag := func(s string) { out = append(out, s...) }

	b4(uint32(moofSize))
	tag("moof")
	b4(mfhdLen)
	tag("mfhd")
	b4(0)
	b4(seq)

	dataOffset := uint32(moofSize + 8)
	for _, tid := range order {
		smps := samples[tid]
		n := len(smps)
		defDur, defFlags := mode(smps)
		withFirst := smps[0].flags != smps[n-1].flags
		tl := trunLen(n, withFirst)
		b4(uint32(8 + tfhdLen + tfdtLen + tl))
		tag("traf")
		b4(tfhdLen)
		tag("tfhd")
		b4(0x020038) // base-is-moof | default duration+size+flags present
		b4(tid)
		b4(defDur)
		b4(uint32(len(smps[0].data)))
		b4(defFlags)
		b4(tfdtLen)
		tag("tfdt")
		b4(0x01000000) // version 1
		b8(smps[0].dts)
		b4(uint32(tl))
		tag("trun")
		if withFirst {
			b4(0x000305) // data-offset + first-sample-flags + sample-duration + sample-size
		} else {
			b4(0x000301) // data-offset + sample-duration + sample-size
		}
		b4(uint32(n))
		b4(dataOffset)
		if withFirst {
			b4(smps[0].flags) // e.g. IDR sync flags on the first video sample
		}
		for _, s := range smps {
			b4(s.dur) // real per-sample duration — keeps VFR timeline gapless
			b4(uint32(len(s.data)))
			dataOffset += uint32(len(s.data))
		}
	}

	b4(uint32(8 + dataLen))
	tag("mdat")
	for _, tid := range order {
		for _, s := range samples[tid] {
			out = append(out, s.data...)
		}
	}

	return out
}
