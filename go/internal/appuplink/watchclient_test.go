package appuplink

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// watchClient is a watch, in Go: the initiator side of the exchange, built on
// appwire's initiator the way the Swift one is built on CryptoKit. It holds a
// static key across sessions and nothing else.
type watchClient struct {
	static    appwire.KeyPair
	handshake *appwire.InitiatorState
	transport *appwire.Transport
}

// dev is the device id FTW Cloud names this watch by.
func (w *watchClient) dev() string { return cloudwire.DeviceID(w.static.Public()) }

func newWatchClient() (*watchClient, error) {
	static, err := appwire.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	return &watchClient{static: static}, nil
}

// lane0 frames one message in lane 0's 512 B bucket, as every exchange
// request is.
func lane0(msgType string, id *uint32, body any) ([]byte, error) {
	raw, err := appproto.Marshal(body)
	if err != nil {
		return nil, err
	}
	return appwire.EncodeFrame(appwire.Frame{
		Lane:     appwire.LaneControl,
		Bucket:   exchangeFrameBytes,
		Envelope: appwire.Envelope{T: msgType, ID: id, B: raw},
	})
}

// open is X1's request: handshake message 1 carrying a hello, with the
// pairing code on the first run only.
func (w *watchClient) open(boxStatic, code []byte) ([]byte, error) {
	handshake, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey:    w.static,
		RemoteStatic: boxStatic,
		Prologue:     exchangePrologue(boxStatic),
	})
	if err != nil {
		return nil, err
	}
	hello, err := lane0(appproto.MsgHello, nil, appproto.Hello{
		Proto: appproto.ProtoRange{Min: appproto.ProtoMin, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "test", UA: "watchos"},
		Code:  code,
	})
	if err != nil {
		return nil, err
	}
	w.handshake = handshake
	w.transport = nil
	return handshake.WriteMessage(hello)
}

// opened reads X1's answer: handshake message 2 carrying hello_ok.
func (w *watchClient) opened(message2 []byte) (appproto.ExchangeHelloOK, appwire.Frame, error) {
	var ok appproto.ExchangeHelloOK
	payload, err := w.handshake.ReadMessage(message2)
	if err != nil {
		return ok, appwire.Frame{}, err
	}
	session, err := w.handshake.Split()
	if err != nil {
		return ok, appwire.Frame{}, err
	}
	w.transport = appwire.NewTransport(session)
	frame, err := appwire.DecodeFrame(payload)
	if err != nil {
		return ok, frame, err
	}
	if frame.Envelope.T != appproto.MsgHelloOK {
		return ok, frame, fmt.Errorf("message 2 carries %q", frame.Envelope.T)
	}
	return ok, frame, appproto.Unmarshal(frame.Envelope.B, &ok)
}

// request seals one request in the session.
func (w *watchClient) request(msgType string, body any) ([]byte, error) {
	frame, err := lane0(msgType, nil, body)
	if err != nil {
		return nil, err
	}
	return w.transport.Encrypt(frame)
}

// answer opens an answer: transport messages back to back, one per frame.
func (w *watchClient) answer(reply []byte) ([]appwire.Frame, error) {
	if len(reply)%exchangeRequestBytes != 0 {
		return nil, fmt.Errorf("an answer of %d bytes is not whole transport messages", len(reply))
	}
	var frames []appwire.Frame
	for len(reply) > 0 {
		plain, err := w.transport.Decrypt(reply[:exchangeRequestBytes])
		if err != nil {
			return nil, err
		}
		frame, err := appwire.DecodeFrame(plain)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		reply = reply[exchangeRequestBytes:]
	}
	return frames, nil
}

// --------------------------------------------------------------------------
// A charger, so a watch has something to do
// --------------------------------------------------------------------------

// fakeCharger is one loadpoint, "lp1", plugged in. save is how long the EV
// save takes, which is what an applied charge waits for; slow holds the
// command itself up past what a save may take.
type fakeCharger struct {
	mu    sync.Mutex
	holds int
	hold  loadpoint.ManualHold
	held  bool
	save  time.Duration
	slow  time.Duration
}

func (c *fakeCharger) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holds
}

func (c *fakeCharger) WaitForPersistence(ctx context.Context) error {
	c.mu.Lock()
	save := c.save
	c.mu.Unlock()
	select {
	case <-time.After(save):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeCharger) Exists(id string) bool { return id == "lp1" }
func (c *fakeCharger) Hold(_ string, h loadpoint.ManualHold) {
	c.mu.Lock()
	slow := c.slow
	c.holds++
	c.hold, c.held = h, true
	c.mu.Unlock()
	time.Sleep(slow)
}
func (c *fakeCharger) ClearHold(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = false
}
func (c *fakeCharger) ObservedHold(string, time.Time) (loadpoint.ManualHold, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hold, c.held
}
func (c *fakeCharger) HoldIfOwn(id string, h loadpoint.ManualHold, now time.Time) bool {
	c.Hold(id, h)
	return true
}
func (c *fakeCharger) ClearHoldIfOwn(id, _ string, _ time.Time) bool {
	c.ClearHold(id)
	return true
}
func (c *fakeCharger) Boost(string, loadpoint.BatteryBoostLease, time.Time) error { return nil }
func (c *fakeCharger) CancelBoost(string, time.Time)                              {}
func (c *fakeCharger) ObservedBoost(string, time.Time) loadpoint.BatteryBoostStatus {
	return loadpoint.BatteryBoostStatus{}
}
func (c *fakeCharger) SetSoC(string, float64) bool              { return true }
func (c *fakeCharger) ObservedSoC(string) (float64, bool)       { return 0.62, true }
func (c *fakeCharger) SetSurplusOnly(string, bool) (bool, bool) { return false, true }
func (c *fakeCharger) ObservedSurplusOnly(string) (bool, bool)  { return false, true }
func (c *fakeCharger) Readings() []appproto.LoadpointReading {
	return []appproto.LoadpointReading{{ID: "lp1", Plugged: true, SoC: 0.62, SoCKnown: true, PowerW: 7360}}
}

// chargeNow is the watch's one button: hold lp1 at 7.4 kW until cleared.
func chargeNow(cmdID string) appproto.Cmd {
	return appproto.Cmd{
		CmdID:           cmdID,
		Op:              appproto.OpLoadpointHold,
		Args:            map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 0, "phase_mode": "3p"},
		NotValidAfterMs: 10 * 60 * 1000,
		Expect:          appproto.Expect{Rev: 1},
	}
}
