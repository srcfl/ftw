package appuplink

import (
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appwire"
)

// codec joins the message layer to the wire.
//
// The two packages describe the same frame with two types on purpose:
// appproto is reviewable without a cipher in scope, and appwire is reviewable
// without knowing what a hello_ok is. This is the whole of the join, and it
// lives here rather than in either of them so neither has to import the other.
type codec struct{}

func (codec) EncodeFrame(f appproto.Frame) ([]byte, error) {
	return appwire.EncodeFrame(appwire.Frame{
		Lane:   f.Lane,
		Flags:  f.Flags,
		Bucket: f.Bucket,
		Envelope: appwire.Envelope{
			T:  f.Envelope.T,
			ID: f.Envelope.ID,
			B:  f.Envelope.B,
		},
	})
}

func (codec) DecodeFrame(b []byte) (appproto.Frame, error) {
	f, err := appwire.DecodeFrame(b)
	if err != nil {
		return appproto.Frame{}, err
	}
	return appproto.Frame{
		Lane:   f.Lane,
		Flags:  f.Flags,
		Bucket: f.Bucket,
		Envelope: appproto.Envelope{
			T:  f.Envelope.T,
			ID: f.Envelope.ID,
			B:  f.Envelope.B,
		},
	}, nil
}

// Codec is the join, for a caller that builds handlers of its own. The uplink
// uses the same value, so a handler wired from outside cannot end up on a
// different frame layout from the one this package puts on the wire.
func Codec() appproto.FrameCodec { return codec{} }
