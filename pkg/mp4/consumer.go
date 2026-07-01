package mp4

import (
	"errors"
	"io"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

type Consumer struct {
	core.Connection
	wr         *core.WriteBuffer
	muxer      *Muxer
	mu         sync.Mutex
	start      bool
	initWriter io.Writer // set by WriteTo; used by hev1 handler to write real init on first keyframe

	// OnKeyframe is called before writing each keyframe after the first.
	// HLS session sets this to flush the current segment before the keyframe starts a new one,
	// ensuring every fMP4 segment begins with an IDR as required by iOS AVFoundation.
	OnKeyframe func()

	Rotate int `json:"-"`
	ScaleX int `json:"-"`
	ScaleY int `json:"-"`
}

func NewConsumer(medias []*core.Media) *Consumer {
	if medias == nil {
		// default local medias
		medias = []*core.Media{
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
					{Name: core.CodecAAC},
				},
			},
		}
	}

	wr := core.NewWriteBuffer(nil)
	return &Consumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "mp4",
			Medias:     medias,
			Transport:  wr,
		},
		muxer: &Muxer{},
		wr:    wr,
	}
}

func (c *Consumer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	trackID := byte(len(c.Senders))

	codec := track.Codec.Clone()
	handler := core.NewSender(media, codec)

	switch track.Codec.Name {
	case core.CodecH264:
		handler.Handler = func(packet *rtp.Packet) {
			isKey := h264.IsKeyframe(packet.Payload)
			if !c.start {
				if !isKey {
					return
				}
				c.start = true
			} else if isKey && c.OnKeyframe != nil {
				c.OnKeyframe()
			}

			// important to use Mutex because right fragment order
			c.mu.Lock()
			b := c.muxer.GetPayload(trackID, packet)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
			c.mu.Unlock()
		}

		if track.Codec.IsRTP() {
			handler.Handler = h264.RTPDepay(track.Codec, handler.Handler)
		} else {
			handler.Handler = h264.RepairAVCC(track.Codec, handler.Handler)
		}

	case core.CodecH265:
		handler.Handler = func(packet *rtp.Packet) {
			isKey := h265.IsKeyframe(packet.Payload)
			if !c.start {
				if !isKey {
					return
				}
				// For hev1 delayed-init: write real init with correct dimensions before any data.
				// c.muxer.DelayedInit is set by GetInit() (called from WriteTo) when no sprop params
				// are in the SDP. initWriter is set by WriteTo just before c.wr.WriteTo blocks.
				if c.muxer.DelayedInit && c.initWriter != nil {
					vps, sps, pps := h265.ExtractParamSets(packet.Payload)
					if len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
						c.muxer.ResolveH265Params(vps, sps, pps)
					}
					realInit, _ := c.muxer.GetInit()
					if c.Rotate != 0 {
						PatchVideoRotate(realInit, c.Rotate)
					}
					if c.ScaleX != 0 && c.ScaleY != 0 {
						PatchVideoScale(realInit, c.ScaleX, c.ScaleY)
					}
					_, _ = c.initWriter.Write(realInit)
				}
				c.start = true
			} else if isKey && c.OnKeyframe != nil {
				c.OnKeyframe()
			}

			// important to use Mutex because right fragment order
			c.mu.Lock()
			b := c.muxer.GetPayload(trackID, packet)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
			c.mu.Unlock()
		}

		if track.Codec.IsRTP() {
			handler.Handler = h265.RTPDepay(track.Codec, handler.Handler)
		} else {
			handler.Handler = h265.RepairAVCC(track.Codec, handler.Handler)
		}

	default:
		handler.Handler = func(packet *rtp.Packet) {
			if !c.start {
				return
			}

			// important to use Mutex because right fragment order
			c.mu.Lock()
			b := c.muxer.GetPayload(trackID, packet)
			if n, err := c.wr.Write(b); err == nil {
				c.Send += n
			}
			c.mu.Unlock()
		}

		switch track.Codec.Name {
		case core.CodecAAC:
			if track.Codec.IsRTP() {
				handler.Handler = aac.RTPDepay(handler.Handler)
			}
		case core.CodecOpus, core.CodecMP3: // no changes
		case core.CodecPCMA, core.CodecPCMU, core.CodecPCM, core.CodecPCML:
			codec.Name = core.CodecFLAC
			if codec.Channels == 2 {
				// hacky way for support two channels audio
				codec.Channels = 1
				codec.ClockRate *= 2
			}
			handler.Handler = pcm.FLACEncoder(track.Codec.Name, codec.ClockRate, handler.Handler)

		default:
			handler.Handler = nil
		}
	}

	if handler.Handler == nil {
		s := "mp4: unsupported codec: " + track.Codec.String()
		println(s)
		return errors.New(s)
	}

	c.muxer.AddTrack(codec)

	handler.HandleRTP(track)
	c.Senders = append(c.Senders, handler)

	return nil
}

func (c *Consumer) WriteTo(wr io.Writer) (int64, error) {
	if len(c.Senders) == 1 && c.Senders[0].Codec.IsAudio() {
		c.start = true
	}

	init, err := c.muxer.GetInit()
	if err != nil {
		return 0, err
	}

	if !c.muxer.DelayedInit {
		// Normal path: write init immediately (camera provided real sprop in SDP).
		if c.Rotate != 0 {
			PatchVideoRotate(init, c.Rotate)
		}
		if c.ScaleX != 0 && c.ScaleY != 0 {
			PatchVideoScale(init, c.ScaleX, c.ScaleY)
		}
		if _, err = wr.Write(init); err != nil {
			return 0, err
		}
	} else {
		// hev1 delayed-init path: real VPS/SPS/PPS not yet known (no sprop in SDP).
		// The H265 handler will write the real init (with correct dimensions) when the
		// first keyframe arrives carrying inline parameter sets.
		c.initWriter = wr
	}

	return c.wr.WriteTo(wr)
}
