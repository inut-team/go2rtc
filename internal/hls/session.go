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

func (s *Session) Run() {
	ticker := time.NewTicker(time.Duration(s.duration * float64(time.Second)))
	go func() {
		defer ticker.Stop()
		for range ticker.C {
			s.flush()
		}
	}()
	_, _ = s.cons.(io.WriterTo).WriteTo(s)
	ticker.Stop()
}

func (s *Session) Main() []byte {
	type withCodecs interface {
		Codecs() []*core.Codec
	}

	codecs := mp4.MimeCodecs(s.cons.(withCodecs).Codecs())
	codecs = strings.Replace(codecs, mp4.MimeFlac, "fLaC", 1)

	// bandwidth important for Safari, codecs useful for smooth playback
	return []byte(`#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=192000,CODECS="` + codecs + `"
hls/playlist.m3u8?id=` + s.id)
}

func (s *Session) Playlist() []byte {
	return []byte(fmt.Sprintf(s.template, s.seq, s.seq, s.seq+1))
}

func (s *Session) Init() (init []byte) {
	for i := 0; i < 60 && init == nil; i++ {
		if i > 0 {
			time.Sleep(50 * time.Millisecond)
		}

		s.mu.Lock()
		if len(s.buffer) > 0 {
			init = s.init
		}
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
