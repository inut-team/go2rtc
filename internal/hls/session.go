package hls

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
)

// segWindow is how many flushed segments stay addressable by their sequence
// number. Players (iOS Safari native, hls.js) may re-fetch or parallel-fetch a
// segment; serving by absolute number keeps those requests consistent.
const segWindow = 6

type Session struct {
	cons      core.Consumer
	id        string
	template  string
	init      []byte
	buffer    []byte
	segs      map[int][]byte  // seq -> segment (last segWindow entries)
	segDurs   map[int]float64 // seq -> measured real duration in seconds (fMP4)
	updated   chan struct{}   // closed+recreated on each flush to wake Segment waiters
	duration  float64
	seq       int
	isMP4     bool
	lastFlush time.Time // wall-clock of the previous flush, to measure GOP length
	lastDur   float64   // last measured segment duration (estimate for pending segment)
	alive     *time.Timer
	mu        sync.Mutex
}

func NewSession(cons core.Consumer, duration float64) *Session {
	if duration <= 0 {
		duration = 0.5
	}

	s := &Session{
		id:       core.RandString(8, 62),
		cons:     cons,
		duration: duration,
		segs:     map[int][]byte{},
		segDurs:  map[int]float64{},
		updated:  make(chan struct{}),
	}

	durStr := strconv.FormatFloat(duration, 'f', 3, 64)
	targetDur := int(math.Ceil(duration))
	if targetDur < 1 {
		targetDur = 1
	}
	targetDurStr := strconv.Itoa(targetDur)

	// At least 3 segments per Apple HLS authoring rules (live playlists with
	// fewer may be rejected by iOS AVFoundation); also important for Chromecast.
	if _, ok := cons.(*mp4.Consumer); ok {
		// fMP4 segments are cut on IDR boundaries so their real length is the
		// camera GOP, not `duration`. The playlist is built dynamically in
		// Playlist() with measured per-segment EXTINF — no static template.
		s.isMP4 = true
	} else {
		s.template = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:` + targetDurStr + `
#EXT-X-MEDIA-SEQUENCE:%d
#EXTINF:` + durStr + `,
segment.ts?id=` + s.id + `&n=%d
#EXTINF:` + durStr + `,
segment.ts?id=` + s.id + `&n=%d
#EXTINF:` + durStr + `,
segment.ts?id=` + s.id + `&n=%d`
	}

	return s
}

func (s *Session) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	if s.init == nil {
		s.init = p
		// For TS: prime buffer with PAT+PMT so the FIRST flushed segment also starts with it.
		// (For fMP4 the buffer stays nil until actual moof/mdat data arrives.)
		if _, ok := s.cons.(*mp4.Consumer); !ok {
			s.buffer = append([]byte(nil), p...)
		}
	} else {
		s.buffer = append(s.buffer, p...)
	}
	s.mu.Unlock()
	return len(p), nil
}

func (s *Session) flush() {
	s.mu.Lock()
	if len(s.buffer) == 0 {
		s.mu.Unlock()
		return
	}
	seg := s.buffer
	if s.isMP4 {
		// Merge the per-sample moof/mdat pairs into a single movie fragment —
		// iOS AVFoundation rejects segments made of ~100 one-frame fragments.
		seg = repackFMP4(seg, uint32(s.seq)+1)
		s.buffer = nil
		// Measure the real segment length from the wall-clock gap between IDR
		// flushes (≈ camera GOP). EXTINF must reflect this or iOS schedules
		// fetches on the wrong cadence and stalls every GOP. Clamp so a
		// producer reconnect gap can't inflate EXTINF/TARGETDURATION.
		now := time.Now()
		if !s.lastFlush.IsZero() {
			d := now.Sub(s.lastFlush).Seconds()
			if d < 0.5 {
				d = 0.5
			} else if d > 8.0 {
				d = 8.0
			}
			s.segDurs[s.seq] = d
			s.lastDur = d
			delete(s.segDurs, s.seq-segWindow)
		}
		s.lastFlush = now
	} else {
		// for TS important to start new segment with PAT/PMT header
		s.buffer = append([]byte(nil), s.init...)
	}
	s.segs[s.seq] = seg
	delete(s.segs, s.seq-segWindow)
	s.seq++
	close(s.updated) // wake all Segment(n) waiters
	s.updated = make(chan struct{})
	s.mu.Unlock()
}

// FlushSegment sends the current buffer as a segment immediately.
// Called by mp4.Consumer.OnKeyframe so fMP4 segments always start on an IDR boundary.
//
// IMPORTANT: must NOT be named "Flush" — core.WriteBuffer.Write does an
// http.Flusher assertion on its destination writer and would then call it
// after EVERY sample write, producing one segment per frame (~40 segments/s)
// and making the HLS playlist slide faster than any player can follow.
func (s *Session) FlushSegment() {
	s.flush()
}

func (s *Session) Run() {
	if _, ok := s.cons.(*mp4.Consumer); !ok {
		// TS: use timer-based flushing (HLS.js handles non-IDR segments fine).
		ticker := time.NewTicker(time.Duration(s.duration * float64(time.Second)))
		go func() {
			defer ticker.Stop()
			for range ticker.C {
				s.flush()
			}
		}()
		defer ticker.Stop()
	}
	// fMP4: flushing done by OnKeyframe callback, so every segment starts on an IDR boundary
	// as required by iOS AVFoundation / Apple HLS Authoring Specification for HEVC.
	_, _ = s.cons.(io.WriterTo).WriteTo(s)
}

func (s *Session) Main() []byte {
	type withCodecs interface {
		Codecs() []*core.Codec
	}

	allCodecs := s.cons.(withCodecs).Codecs()

	var streamInf string

	if _, isFMP4 := s.cons.(*mp4.Consumer); isFMP4 {
		// fMP4: include CODECS so Safari/AVFoundation can select the right decoder.
		// Apple HLS Authoring Spec requires VIDEO-RANGE and RESOLUTION for HEVC.
		codecs := mp4.MimeCodecs(allCodecs)
		codecs = strings.Replace(codecs, mp4.MimeFlac, "fLaC", 1)
		streamInf = `BANDWIDTH=4000000,CODECS="` + codecs + `"`
		for _, codec := range allCodecs {
			if codec.Name == core.CodecH265 {
				streamInf += `,VIDEO-RANGE=SDR,FRAME-RATE=25.000`
				if _, sps, _ := h265.GetParameterSet(codec.FmtpLine); len(sps) > 0 {
					if parsed := h265.DecodeSPS(sps); parsed != nil {
						streamInf += fmt.Sprintf(`,RESOLUTION=%dx%d`, parsed.Width(), parsed.Height())
					}
				}
				break
			}
		}
	} else {
		// TS: omit CODECS so HLS.js skips MediaSource.isTypeSupported() for HEVC.
		// Without CODECS the player downloads segments and lets the video element decide,
		// matching how FFmpeg-based HLS servers serve HEVC TS streams.
		streamInf = `BANDWIDTH=4000000`
	}

	return []byte(`#EXTM3U
#EXT-X-STREAM-INF:` + streamInf + `
hls/playlist.m3u8?id=` + s.id)
}

func (s *Session) Playlist() []byte {
	// List a 3-segment window ending at the newest flushed segment plus one
	// pending — requests for the pending one long-poll in Segment(n), which
	// keeps latency low without listing far-future segments.
	s.mu.Lock()
	defer s.mu.Unlock()

	start := s.seq - 2
	if start < 0 {
		start = 0
	}

	if !s.isMP4 {
		// TS uses timer-based flushing at exactly s.duration, so the declared
		// EXTINF is accurate — keep the precomputed static template.
		return []byte(fmt.Sprintf(s.template, start, start, start+1, start+2))
	}

	// fMP4: emit the real measured per-segment EXTINF. The not-yet-flushed
	// pending segment (start+2) has no measurement yet, so estimate it from the
	// last measured GOP (falling back to the declared duration at startup).
	dur := func(n int) float64 {
		if d, ok := s.segDurs[n]; ok {
			return d
		}
		if s.lastDur > 0 {
			return s.lastDur
		}
		return s.duration
	}
	d0, d1, d2 := dur(start), dur(start+1), dur(start+2)

	// TARGETDURATION must be >= every listed EXTINF (Apple HLS spec).
	target := s.duration
	for _, d := range []float64{d0, d1, d2} {
		if d > target {
			target = d
		}
	}
	targetDur := int(math.Ceil(target))
	if targetDur < 1 {
		targetDur = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:")
	b.WriteString(strconv.Itoa(targetDur))
	b.WriteString("\n#EXT-X-MEDIA-SEQUENCE:")
	b.WriteString(strconv.Itoa(start))
	b.WriteString("\n#EXT-X-MAP:URI=\"init.mp4?id=")
	b.WriteString(s.id)
	b.WriteString("\"\n")
	writeSeg := func(n int, d float64) {
		b.WriteString("#EXTINF:")
		b.WriteString(strconv.FormatFloat(d, 'f', 3, 64))
		b.WriteString(",\nsegment.m4s?id=")
		b.WriteString(s.id)
		b.WriteString("&n=")
		b.WriteString(strconv.Itoa(n))
		b.WriteByte('\n')
	}
	writeSeg(start, d0)
	writeSeg(start+1, d1)
	writeSeg(start+2, d2)
	return []byte(b.String())
}

func (s *Session) Init() (init []byte) {
	for i := 0; i < 300 && init == nil; i++ {
		if i > 0 {
			time.Sleep(50 * time.Millisecond)
		}

		s.mu.Lock()
		init = s.init
		s.mu.Unlock()
	}

	return
}

// Segment returns the segment with absolute sequence number n, waiting up to
// 30s for it to be produced (long-poll; also survives producer reconnects).
// Re-fetches of a recent n return the same bytes — required for iOS Safari
// native HLS and hls.js, which may request a segment more than once.
func (s *Session) Segment(n int) []byte {
	deadline := time.After(30 * time.Second)
	for {
		s.mu.Lock()
		seg, ok := s.segs[n]
		tooOld := n < s.seq-segWindow
		ch := s.updated
		s.mu.Unlock()

		if ok {
			return seg
		}
		if tooOld || n < 0 {
			return nil // already evicted — client fell too far behind
		}
		select {
		case <-ch: // new segment flushed, re-check
		case <-deadline:
			return nil
		}
	}
}
