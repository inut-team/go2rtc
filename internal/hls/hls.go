package hls

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/rs/zerolog"
)

var defaultDuration float64

func Init() {
	var conf struct {
		Mod struct {
			Duration float64 `yaml:"duration"`
		} `yaml:"hls"`
	}
	conf.Mod.Duration = 0.5
	app.LoadConfig(&conf)
	defaultDuration = conf.Mod.Duration

	log = app.GetLogger("hls")

	api.HandleFunc("api/stream.m3u8", handlerStream)
	api.HandleFunc("api/hls/playlist.m3u8", handlerPlaylist)

	// HLS (TS)
	api.HandleFunc("api/hls/segment.ts", handlerSegmentTS)

	// HLS (fMP4)
	api.HandleFunc("api/hls/init.mp4", handlerInit)
	api.HandleFunc("api/hls/segment.m4s", handlerSegmentMP4)

	ws.HandleFunc("hls", handlerWSHLS)
}

var log zerolog.Logger

const keepalive = 60 * time.Second // extended to survive producer reconnect gaps

// once I saw 404 on MP4 segment, so better to use mutex
var sessions = map[string]*Session{}
var sessionsMu sync.RWMutex

// hlsDefaultMedias returns the default media list for HLS fMP4 output.
// Audio sample rates below 16kHz are excluded per Apple HLS Authoring Specification,
// which prevents newer iOS versions from rejecting streams with 8kHz AAC (e.g. newer Dahua firmware).
func hlsDefaultMedias() []*core.Media {
	return []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecH264},
				{Name: core.CodecH265},
			},
		},
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecAAC, ClockRate: 48000},
				{Name: core.CodecAAC, ClockRate: 44100},
				{Name: core.CodecAAC, ClockRate: 32000},
				{Name: core.CodecAAC, ClockRate: 22050},
				{Name: core.CodecAAC, ClockRate: 16000},
			},
		},
	}
}

func handlerStream(w http.ResponseWriter, r *http.Request) {
	// CORS important for Chromecast
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")

	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		return
	}

	src := r.URL.Query().Get("src")
	stream := streams.Get(src)
	if stream == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}

	query := r.URL.Query()

	var cons core.Consumer

	if query.Has("mp4") || query.Get("format") == "mp4" {
		// fMP4 — for iOS Safari native HLS / Chromecast. Triggered by &mp4 or &format=mp4.
		medias := mp4.ParseQuery(query)
		if medias == nil {
			medias = hlsDefaultMedias()
		}
		c := mp4.NewConsumer(medias)
		c.FormatName = "hls/fmp4"
		c.WithRequest(r)
		cons = c
	} else {
		// Default: MPEG-TS — HLS.js-compatible for H.264 and H.265.
		// HLS.js's built-in TS demuxer handles HEVC without needing browser MSE HEVC support,
		// matching how FFmpeg-based HLS servers behave. fMP4+HEVC only works in Chrome 107+/macOS.
		c := mpegts.NewConsumer()
		c.FormatName = "hls/mpegts"
		c.WithRequest(r)
		cons = c
	}

	if err := stream.AddConsumer(cons); err != nil {
		log.Error().Err(err).Caller().Send()
		return
	}

	duration := defaultDuration
	if d := query.Get("duration"); d != "" {
		if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
			duration = v
		}
	}

	session := NewSession(cons, duration)

	// For fMP4: trigger a segment flush before each keyframe so every segment starts on
	// an IDR boundary. Required by iOS AVFoundation / Apple HLS Authoring Spec for HEVC.
	if mp4c, ok := cons.(*mp4.Consumer); ok {
		mp4c.OnKeyframe = session.FlushSegment
	}

	session.alive = time.AfterFunc(keepalive, func() {
		sessionsMu.Lock()
		delete(sessions, session.id)
		sessionsMu.Unlock()

		stream.RemoveConsumer(cons)
	})

	sessionsMu.Lock()
	sessions[session.id] = session
	sessionsMu.Unlock()

	go session.Run()

	if _, err := w.Write(session.Main()); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func handlerPlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")

	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		return
	}

	sid := r.URL.Query().Get("id")
	sessionsMu.RLock()
	session := sessions[sid]
	sessionsMu.RUnlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}

	if _, err := w.Write(session.Playlist()); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func handlerSegmentTS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "video/mp2t")

	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		return
	}

	query := r.URL.Query()

	sid := query.Get("id")
	sessionsMu.RLock()
	session := sessions[sid]
	sessionsMu.RUnlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}

	session.alive.Reset(keepalive)

	n, err := strconv.Atoi(query.Get("n"))
	if err != nil {
		n = -1
	}
	data := session.Segment(n)
	if data == nil {
		log.Warn().Msgf("[hls] can't get segment %s", r.URL.RawQuery)
		http.NotFound(w, r)
		return
	}

	if _, err := w.Write(data); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func handlerInit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Add("Content-Type", "video/mp4")

	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		return
	}

	sid := r.URL.Query().Get("id")
	sessionsMu.RLock()
	session := sessions[sid]
	sessionsMu.RUnlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}

	data := session.Init()
	if data == nil {
		log.Warn().Msgf("[hls] can't get init %s", r.URL.RawQuery)
		http.NotFound(w, r)
		return
	}

	if _, err := w.Write(data); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func handlerSegmentMP4(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Add("Content-Type", "video/iso.segment")

	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		return
	}

	query := r.URL.Query()

	sid := query.Get("id")
	sessionsMu.RLock()
	session := sessions[sid]
	sessionsMu.RUnlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}

	session.alive.Reset(keepalive)

	n, err := strconv.Atoi(query.Get("n"))
	if err != nil {
		n = -1
	}
	data := session.Segment(n)
	if data == nil {
		log.Warn().Msgf("[hls] can't get segment %s", r.URL.RawQuery)
		http.NotFound(w, r)
		return
	}

	if _, err := w.Write(data); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}
