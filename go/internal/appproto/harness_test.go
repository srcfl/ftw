package appproto

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// testCodec is the frame layout from docs/protocol.md, implemented here so the
// tests drive real bytes rather than passing structs to themselves.
//
// go/internal/appwire owns the production one. Keeping a copy in _test.go means
// a test passing here says the message layer works against the documented
// layout, not that a stub agreed with itself — and it cannot be mistaken for
// the real codec, because it does not ship.
type testCodec struct{}

const frameVersion = 1
const frameHeaderBytes = 6

func (testCodec) EncodeFrame(f Frame) ([]byte, error) {
	payload, err := EncodeEnvelope(f.Envelope)
	if err != nil {
		return nil, err
	}
	if frameHeaderBytes+len(payload) > f.Bucket {
		return nil, fmt.Errorf("frame needs %d bytes, bucket is %d", frameHeaderBytes+len(payload), f.Bucket)
	}
	out := make([]byte, f.Bucket) // zero-filled, which is the padding
	out[0] = frameVersion
	out[1] = f.Lane
	out[2] = f.Flags
	out[3] = 0
	binary.BigEndian.PutUint16(out[4:6], uint16(len(payload)))
	copy(out[frameHeaderBytes:], payload)
	return out, nil
}

func (testCodec) DecodeFrame(b []byte) (Frame, error) {
	if len(b) < frameHeaderBytes {
		return Frame{}, fmt.Errorf("frame is %d bytes", len(b))
	}
	if b[0] != frameVersion {
		return Frame{}, fmt.Errorf("unsupported frame version %d", b[0])
	}
	n := int(binary.BigEndian.Uint16(b[4:6]))
	if frameHeaderBytes+n > len(b) {
		return Frame{}, fmt.Errorf("declared length %d overruns a %d-byte frame", n, len(b))
	}
	env, err := DecodeEnvelope(b[frameHeaderBytes : frameHeaderBytes+n])
	if err != nil {
		return Frame{}, err
	}
	return Frame{Lane: b[1], Flags: b[2], Bucket: len(b), Envelope: env}, nil
}

// sent is one frame the handler put on the wire, kept decoded for assertions
// and raw so a test can measure its size.
type sent struct {
	lane  uint8
	flags uint8
	size  int
	env   Envelope
}

// recorder is guarded because a handler sends from two goroutines: the tick,
// and the passthrough answering a request.
type recorder struct {
	mu     sync.Mutex
	frames []sent
	fail   error
}

func (r *recorder) Send(frame []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	f, err := testCodec{}.DecodeFrame(frame)
	if err != nil {
		return err
	}
	r.frames = append(r.frames, sent{lane: f.Lane, flags: f.Flags, size: len(frame), env: f.Envelope})
	return nil
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = nil
}

// snapshot is how an asynchronous test reads what has been sent so far.
func (r *recorder) snapshot() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sent(nil), r.frames...)
}

// only returns the single frame of a type, failing if there is not exactly one.
func (r *recorder) only(t *testing.T, msgType string) sent {
	t.Helper()
	var found []sent
	for _, f := range r.snapshot() {
		if f.env.T == msgType {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s frame, got %d (%s)", msgType, len(found), r.types())
	}
	return found[0]
}

func (r *recorder) last(t *testing.T, msgType string) sent {
	t.Helper()
	frames := r.snapshot()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].env.T == msgType {
			return frames[i]
		}
	}
	t.Fatalf("no %s frame; got %s", msgType, r.types())
	return sent{}
}

func (r *recorder) has(msgType string) bool {
	for _, f := range r.snapshot() {
		if f.env.T == msgType {
			return true
		}
	}
	return false
}

func (r *recorder) types() string {
	var out []string
	for _, f := range r.snapshot() {
		out = append(out, f.env.T)
	}
	return fmt.Sprint(out)
}

func body[T any](t *testing.T, s sent) T {
	t.Helper()
	var v T
	if err := Unmarshal(s.env.B, &v); err != nil {
		t.Fatalf("decode %s body: %v", s.env.T, err)
	}
	return v
}

// fakeClock is driven by the test, because every age in this protocol is a
// difference of uptimes and a real clock would make those assertions flaky.
type fakeClock struct {
	uptimeMs int64
	now      time.Time
}

func (c *fakeClock) UptimeMs() int64       { return c.uptimeMs }
func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sync() (string, int64) { return "ntp", c.now.UnixMilli() }

// fakeBox stands in for the whole box. Tests mutate it between frames, which
// is how "guards are re-evaluated against fresh state" gets proved rather than
// asserted.
type fakeBox struct {
	snap     Snapshot
	boot     *BootProgress
	identity Identity

	mode         control.Mode
	observedMode control.Mode
	observedOK   bool
	setModeErr   error
	setModeCalls int
	// onSetMode runs inside SetMode, so a test can make the world move
	// between the write and the read-back.
	onSetMode func()

	plan     *mpc.Plan
	planRev  uint64
	ceilingW *int64
}

func (b *fakeBox) Snapshot() Snapshot  { return b.snap }
func (b *fakeBox) Identity() Identity  { return b.identity }
func (b *fakeBox) Boot() *BootProgress { return b.boot }
func (b *fakeBox) Latest() *mpc.Plan   { return b.plan }
func (b *fakeBox) Rev() uint64         { return b.planRev }
func (b *fakeBox) CeilingW() *int64    { return b.ceilingW }

func (b *fakeBox) SetMode(_ context.Context, m control.Mode) error {
	b.setModeCalls++
	if b.setModeErr != nil {
		return b.setModeErr
	}
	b.mode = m
	b.observedMode = m
	b.observedOK = true
	b.snap.Mode = m
	if b.onSetMode != nil {
		b.onSetMode()
	}
	return nil
}

func (b *fakeBox) ObservedMode() (control.Mode, bool) { return b.observedMode, b.observedOK }

// ownerCaller is the grant every test here used to carry implicitly: the
// household's own phone, paired from the code on the box's lid.
func ownerCaller() apiauth.Caller {
	return apiauth.Caller{
		Subject: apiauth.KindApp + ":aBcD1234",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleOwner,
		Scopes:  ScopesForRole(apiauth.RoleOwner),
		Epoch:   1,
	}
}

func viewerCaller() apiauth.Caller {
	return apiauth.Caller{
		Subject: apiauth.KindApp + ":wXyZ9876",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleViewer,
		Scopes:  ScopesForRole(apiauth.RoleViewer),
		Epoch:   1,
	}
}

// fakeGrants is the enrolment record as the box holds it right now. Tests
// move it under a running session, which is the whole point of re-reading it
// per request rather than trusting the handshake.
type fakeGrants struct {
	mu    sync.Mutex
	role  string
	epoch uint64
	gone  bool
}

func newGrants() *fakeGrants { return &fakeGrants{role: apiauth.RoleOwner, epoch: 1} }

func newViewerGrants() *fakeGrants { return &fakeGrants{role: apiauth.RoleViewer, epoch: 1} }

func (g *fakeGrants) Grant() (string, uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.role, g.epoch, !g.gone
}

// revoke is what appenroll.Revoke does to a row: it stops existing.
func (g *fakeGrants) revoke() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gone = true
}

// setRole is what appenroll.SetRole does to a row: the role changes, the epoch
// moves with it, and the row goes on existing. That last part is the whole
// difference between a demotion and a revoke.
func (g *fakeGrants) setRole(role string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.role = role
	g.epoch++
}

// newRig builds a handler over a box with a meter, an inverter and a battery,
// all live.
func newRig(t *testing.T) (*Handler, *fakeBox, *recorder, *fakeClock) {
	t.Helper()
	return newRigWith(t, nil)
}

func newRigWith(t *testing.T, prefs PlannerPrefs) (*Handler, *fakeBox, *recorder, *fakeClock) {
	t.Helper()

	clock := &fakeClock{uptimeMs: 60_000, now: time.UnixMilli(1_760_000_000_000)}
	box := &fakeBox{
		identity: Identity{ID: "ftw-0001", Build: "test", TZ: "Europe/Stockholm"},
		mode:     control.ModePlannerPassiveArbitrage,
		snap: Snapshot{
			Mode:       control.ModePlannerPassiveArbitrage,
			ControlRev: 7,
			// Site convention: positive is into the site, so PV is
			// negative while generating and the battery positive while
			// charging.
			GridW:           1200,
			PVW:             -3400,
			BatteryW:        900,
			LoadW:           3700,
			BatterySoC:      0.62,
			BatterySoCKnown: true,
			Sources: []Source{
				{ID: "meter.p1", Kind: "meter", Name: "P1 meter", LastOkUptimeMs: 60_000, StaleAfterMs: 5_000},
				{ID: "inverter.sungrow", Kind: "inverter", Name: "Sungrow inverter", LastOkUptimeMs: 59_000, StaleAfterMs: 15_000},
				{ID: "battery.sungrow", Kind: "battery", Name: "Sungrow battery", LastOkUptimeMs: 59_000, StaleAfterMs: 15_000},
			},
			DispatchBlockedBy: []string{},
		},
		observedMode: control.ModePlannerPassiveArbitrage,
		observedOK:   true,
		planRev:      3,
	}
	rec := &recorder{}

	h, err := New(Config{
		Clock:        clock,
		Site:         box,
		Info:         box,
		Modes:        box,
		Plans:        box,
		PlannerPrefs: prefs,
		Codec:        testCodec{},
		Sender:       rec,
		Caller:       ownerCaller(),
		Grants:       newGrants(),
		SrcGrid:      "meter.p1",
		SrcPV:        "inverter.sungrow",
		SrcBattery:   "battery.sungrow",
		NewLeaseID:   func() string { return "lease-test" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, box, rec, clock
}

// deliver hands one client message to the handler as bytes.
func deliver(t *testing.T, h *Handler, msgType string, id *uint32, b any) {
	t.Helper()
	env, err := newEnvelope(msgType, id, b)
	if err != nil {
		t.Fatalf("build %s: %v", msgType, err)
	}
	bucket := 512
	raw, err := EncodeEnvelope(env)
	if err != nil {
		t.Fatalf("encode %s: %v", msgType, err)
	}
	if len(raw)+frameHeaderBytes > bucket {
		bucket = bulkBucketFor(len(raw))
	}
	frame, err := testCodec{}.EncodeFrame(Frame{Lane: LaneControl, Bucket: bucket, Envelope: env})
	if err != nil {
		t.Fatalf("frame %s: %v", msgType, err)
	}
	if err := h.Handle(context.Background(), frame); err != nil {
		t.Fatalf("handle %s: %v", msgType, err)
	}
}

func subscribe(t *testing.T, h *Handler, rec *recorder) {
	t.Helper()
	deliver(t, h, MsgHello, nil, Hello{
		Proto:   ProtoRange{Min: ProtoMin, Max: ProtoMax},
		App:     AppInfo{Build: "test", UA: "pwa"},
		Locales: []string{"sv"},
	})
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})
	if !h.Subscribed() {
		t.Fatal("handler did not subscribe")
	}
	rec.reset()
}

func ptrU32(v uint32) *uint32 { return &v }
