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

type Session struct {
	cons     core.Consumer
	id       string
	template string
	init     []byte
	buffer   []byte
	segments chan []byte
	duration float64
	seq      int
	alive    *time.Timer
	mu       sync.Mutex
}

func NewSession(cons core.Consumer, duration float64) *Session {
	if duration <= 0 {
		duration = 0.5
	}

	s := &Session{
		id:       core.RandString(8, 62),
		cons:     cons,
		duration: duration,
		segments: make(chan []byte, 4),
	}

	durStr := strconv.FormatFloat(duration, 'f', 3, 64)
	targetDur := int(math.Ceil(duration))
	if targetDur < 1 {
		targetDur = 1
	}
	targetDurStr := strconv.Itoa(targetDur)

	// two segments important for Chromecast
	if _, ok := cons.(*mp4.Consumer); ok {
		s.template = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:` + targetDurStr + `
#EXT-X-MEDIA-SEQUENCE:%d
#EXT-X-MAP:URI="init.mp4?id=` + s.id + `"
#EXTINF:` + durStr + `,
segment.m4s?id=` + s.id + `&n=%d
#EXTINF:` + durStr + `,
segment.m4s?id=` + s.id + `&n=%d`
	} else {
		s.template = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:` + targetDurStr + `
#EXT-X-MEDIA-SEQUENCE:%d
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
	if _, ok := s.cons.(*mp4.Consumer); ok {
		s.buffer = nil
	} else {
		// for TS important to start new segment with PAT/PMT header
		s.buffer = append([]byte(nil), s.init...)
	}
	s.seq++
	s.mu.Unlock()

	select {
	case s.segments <- seg:
	default: // drop oldest if queue full (client too slow)
	}
}

// Flush sends the current buffer as a segment immediately.
// Called by mp4.Consumer.OnKeyframe so fMP4 segments always start on an IDR boundary.
func (s *Session) Flush() {
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
	return []byte(fmt.Sprintf(s.template, s.seq, s.seq, s.seq+1))
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

func (s *Session) Segment() []byte {
	// Wait up to 30s so clients survive producer reconnect (~5-10s) without restarting.
	// During normal operation segments arrive every ~0.5s so this doesn't add latency.
	select {
	case seg := <-s.segments:
		return seg
	case <-time.After(30 * time.Second):
		return nil
	}
}
