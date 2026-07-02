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

	// ── sizes ──
	const mfhdLen, tfhdLen, tfdtLen = 16, 16, 20
	moofSize := 8 + mfhdLen
	dataLen := 0
	for _, tid := range order {
		n := len(samples[tid])
		trunLen := 8 + 4 + 4 + 4 + n*16 // verflags+count+data-offset + (dur,size,flags,cts)*n
		moofSize += 8 + tfhdLen + tfdtLen + trunLen
		for _, s := range samples[tid] {
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
		trunLen := 8 + 12 + n*16
		b4(uint32(8 + tfhdLen + tfdtLen + trunLen))
		tag("traf")
		b4(tfhdLen)
		tag("tfhd")
		b4(0x020000) // default-base-is-moof
		b4(tid)
		b4(tfdtLen)
		tag("tfdt")
		b4(0x01000000) // version 1
		b8(smps[0].dts)
		b4(uint32(trunLen))
		tag("trun")
		b4(0x000F01) // data-offset + per-sample duration/size/flags/cts
		b4(uint32(n))
		b4(dataOffset)
		for _, s := range smps {
			b4(s.dur)
			b4(uint32(len(s.data)))
			b4(s.flags)
			b4(s.cts)
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
