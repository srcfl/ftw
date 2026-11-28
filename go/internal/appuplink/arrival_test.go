package appuplink

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// movingSite is stubSite with a grid reading the test moves.
type movingSite struct{ gridW atomic.Int64 }

func (m *movingSite) Snapshot() appproto.Snapshot {
	s := stubSite{}.Snapshot()
	s.GridW = float64(m.gridW.Load())
	return s
}

// arrivalRig is a tunnel table that hears arrivals, with one phone
// subscribed through it.
type arrivalRig struct {
	*tunRig
	site     *movingSite
	arrivals chan struct{}
	app      *appClient
}

func newArrivalRig(t *testing.T) *arrivalRig {
	t.Helper()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	r := &arrivalRig{site: &movingSite{}, arrivals: make(chan struct{}, 1),
		tunRig: &tunRig{enroll: enroll, slots: &slots{}, out: &capturedTun{ch: make(chan cloudwire.Tun, 64)}, modes: &countingModes{}}}
	r.site.gridW.Store(1200)
	cmds := appproto.NewCmdLog()
	build := func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:  r.site, Info: stubInfo{}, Modes: r.modes, Plans: stubPlans{}, Codec: codec{},
			Sender: s, Caller: caller, Grants: grants, Commands: cmds,
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
		})
	}
	r.tunnel = newTunnel(build, enroll, r.slots, r.arrivals, slog.New(slog.DiscardHandler))
	r.tunnel.Attach(r.out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.tunnel.Run(ctx)

	r.app = r.pair(t)
	dev := cloudwire.DeviceID(r.app.staticPublic())
	r.open(t, r.app, dev, 7, nil)
	r.hello(t, r.app, dev, 7)
	r.send(t, r.app, dev, 7, appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1})
	r.receive(t, r.app, appproto.MsgSnap)
	return r
}

// laneZero is the next lane 0 reading the phone gets, when it got it, and
// its padded size.
func (r *arrivalRig) laneZero(t *testing.T, within time.Duration) (appwire.Frame, time.Time, int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		m := r.next(t, time.Until(deadline))
		at := time.Now()
		plain, err := r.app.decrypt(m.D)
		if err != nil {
			t.Fatal(err)
		}
		f, err := appwire.DecodeFrame(plain)
		if err != nil {
			t.Fatal(err)
		}
		if f.Envelope.T == appproto.MsgDelta || f.Envelope.T == appproto.MsgTick {
			return f, at, len(plain)
		}
	}
}

// Through FTW Cloud a reading leaves the box as it lands, not on the next
// pulse of a one-second clock, and still in its fixed-size bucket. Five
// readings at different phases of the second all go within 200 ms; with
// the pulse, all five doing so would be a one-in-3,125 fluke (0.2 to the
// fifth).
func TestAReadingLeavesThroughTheTunnelAsItArrives(t *testing.T) {
	r := newArrivalRig(t)
	for i := range 5 {
		time.Sleep(time.Duration(130+97*i) * time.Millisecond)
		want := int64(2000 + i)
		r.site.gridW.Store(want)
		at := time.Now()
		r.arrivals <- struct{}{}
		for {
			f, got, size := r.laneZero(t, 2*time.Second)
			if size != 512 {
				t.Fatalf("a %s left in %d bytes, want the 512-byte bucket", f.Envelope.T, size)
			}
			var d appproto.Delta
			if f.Envelope.T != appproto.MsgDelta || appproto.Unmarshal(f.Envelope.B, &d) != nil ||
				d.Fields["2"] != want {
				continue // a beat that crossed the arrival
			}
			if took := got.Sub(at); took > 200*time.Millisecond {
				t.Fatalf("reading %d took %s to leave the box", i, took)
			}
			break
		}
	}
}

// With nothing arriving, the session still hears from the box once a
// second, so the phone never reads a quiet house as a lost carrier.
func TestAQuietTunnelSessionStillBeats(t *testing.T) {
	r := newArrivalRig(t)
	_, first, _ := r.laneZero(t, 2*time.Second)
	f, second, size := r.laneZero(t, 2*time.Second)
	if f.Envelope.T != appproto.MsgTick || size != 512 {
		t.Fatalf("a quiet beat sent a %s in %d bytes, want a 512-byte tick", f.Envelope.T, size)
	}
	if gap := second.Sub(first); gap < 900*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("beats %s apart, want about a second", gap)
	}
}

// Drivers that each read once a second never wait for the pace; only a
// stream faster than five frames a second does.
func TestThePaceTakesThreeAtOnceAndFiveASecond(t *testing.T) {
	t0 := time.Unix(1_760_000_000, 0)
	p := pace{tokens: tunBurst, at: t0}
	for i := range 3 {
		if w := p.wait(t0); w != 0 {
			t.Fatalf("frame %d of a burst waits %s", i, w)
		}
		p.take(t0)
	}
	if w := p.wait(t0); w != 200*time.Millisecond {
		t.Fatalf("a fourth frame at once waits %s, want 200ms", w)
	}

	// A poll every 100 ms for ten seconds gets five frames a second.
	sent := 0
	next := t0
	for at := t0; at.Before(t0.Add(10 * time.Second)); at = at.Add(100 * time.Millisecond) {
		if at.Before(next) {
			continue
		}
		if w := p.wait(at); w > 0 {
			next = at.Add(w)
			continue
		}
		p.take(at)
		sent++
	}
	if sent < 49 || sent > 52 {
		t.Fatalf("%d frames in ten seconds of polls every 100 ms, want about 50", sent)
	}
}

// gather collects for d what the box sends the phone app under dev, lane 0
// frame types only, in order.
func (r *arrivalRig) gather(t *testing.T, app *appClient, dev string, d time.Duration) []string {
	t.Helper()
	var got []string
	deadline := time.After(d)
	for {
		select {
		case m := <-r.out.ch:
			if m.Dev != dev || m.Ended() {
				continue
			}
			plain, err := app.decrypt(m.D)
			if err != nil {
				t.Fatal(err)
			}
			f, err := appwire.DecodeFrame(plain)
			if err != nil {
				t.Fatal(err)
			}
			if f.Envelope.T == appproto.MsgDelta || f.Envelope.T == appproto.MsgTick {
				got = append(got, f.Envelope.T)
			}
		case <-deadline:
			return got
		}
	}
}

// arrive signals a poll every gap for d.
func (r *arrivalRig) arrive(gap, d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(gap) {
		select {
		case r.arrivals <- struct{}{}:
		default:
		}
	}
}

// A driver reading fifty times a second costs the phone five frames a
// second and the three of a burst, no more.
func TestFastPollsArePacedToFiveFramesASecond(t *testing.T) {
	r := newArrivalRig(t)
	dev := cloudwire.DeviceID(r.app.staticPublic())
	r.gather(t, r.app, dev, 300*time.Millisecond)
	go r.arrive(20*time.Millisecond, 2*time.Second)
	if n := len(r.gather(t, r.app, dev, 2*time.Second)); n < 10 || n > 16 {
		t.Fatalf("%d frames in two seconds of polls every 20 ms, want about 13", n)
	}
}

// Every phone that streams hears each poll.
func TestEveryLiveSessionHearsAPoll(t *testing.T) {
	r := newArrivalRig(t)
	other := r.pair(t)
	dev2 := cloudwire.DeviceID(other.staticPublic())
	r.open(t, other, dev2, 8, nil)
	r.send(t, other, dev2, 8, appproto.MsgHello, appproto.Hello{
		Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "test", UA: "ios"},
	})
	r.send(t, other, dev2, 8, appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1})
	time.Sleep(300 * time.Millisecond)

	r.site.gridW.Store(3333)
	r.arrivals <- struct{}{}
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case m := <-r.out.ch:
			app := r.app
			if m.Dev == dev2 {
				app = other
			}
			plain, err := app.decrypt(m.D)
			if err != nil {
				t.Fatal(err)
			}
			f, _ := appwire.DecodeFrame(plain)
			var d appproto.Delta
			if f.Envelope.T == appproto.MsgDelta && appproto.Unmarshal(f.Envelope.B, &d) == nil && d.Fields["2"] == 3333 {
				seen[m.Dev] = true
			}
		case <-deadline:
			t.Fatalf("the poll reached %d of 2 phones", len(seen))
		}
	}
}

// dropLaneZero loses lane 0 tun while drop is set, as a full queue would,
// and says how much of the tunnel's budget is spent.
type dropLaneZero struct {
	inner *capturedTun
	drop  atomic.Bool
	used  atomic.Int64 // thousandths
}

func (d *dropLaneZero) SendTun(m cloudwire.Tun, bulk bool) error {
	if d.drop.Load() && !bulk {
		return countedDrop{}
	}
	return d.inner.SendTun(m, bulk)
}

func (d *dropLaneZero) TunBudgetUsed() float64 { return float64(d.used.Load()) / 1000 }

// A reading the link dropped is not one the phone has: the next frame
// carries it, and the session goes on.
func TestAReadingTheLinkDroppedGoesWithTheNextFrame(t *testing.T) {
	r := newArrivalRig(t)
	dev := cloudwire.DeviceID(r.app.staticPublic())
	link := &dropLaneZero{inner: r.out}
	r.tunnel.Attach(link)
	r.gather(t, r.app, dev, 300*time.Millisecond)

	link.drop.Store(true)
	r.site.gridW.Store(4444)
	r.arrivals <- struct{}{}
	time.Sleep(300 * time.Millisecond)
	link.drop.Store(false)
	r.arrivals <- struct{}{}
	for {
		f, _, _ := r.laneZero(t, 2*time.Second)
		var d appproto.Delta
		if f.Envelope.T == appproto.MsgDelta && appproto.Unmarshal(f.Envelope.B, &d) == nil {
			if d.Fields["2"] != 4444 {
				t.Fatalf("the next delta carries %v, want grid_w 4444 again", d.Fields)
			}
			return
		}
	}
}

// Past 60 % of the tunnel's daily budget a session gets the beat alone;
// past 90 % no readings at all, so handshakes, answers and ends still go.
func TestTheBudgetStopsPushesThenBeats(t *testing.T) {
	r := newArrivalRig(t)
	dev := cloudwire.DeviceID(r.app.staticPublic())
	link := &dropLaneZero{inner: r.out}
	r.tunnel.Attach(link)
	r.gather(t, r.app, dev, 300*time.Millisecond)

	link.used.Store(700)
	go r.arrive(50*time.Millisecond, 2500*time.Millisecond)
	if n := len(r.gather(t, r.app, dev, 2500*time.Millisecond)); n < 1 || n > 3 {
		t.Fatalf("at 70 %% of the budget %d frames in 2.5 s of polls, want the beat's 2", n)
	}

	link.used.Store(950)
	go r.arrive(50*time.Millisecond, 2500*time.Millisecond)
	if got := r.gather(t, r.app, dev, 2500*time.Millisecond); len(got) != 0 {
		t.Fatalf("at 95 %% of the budget the box still sent %v", got)
	}
}

// Hidden, a poll sends nothing; back in front, the stream picks up within
// a beat and each poll goes again.
func TestHiddenThenVisible(t *testing.T) {
	r := newArrivalRig(t)
	dev := cloudwire.DeviceID(r.app.staticPublic())
	r.send(t, r.app, dev, 7, appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 0.2})
	r.gather(t, r.app, dev, 300*time.Millisecond)

	go r.arrive(100*time.Millisecond, time.Second)
	if got := r.gather(t, r.app, dev, time.Second); len(got) > 1 {
		t.Fatalf("hidden, a second of polls sent %v", got)
	}

	r.send(t, r.app, dev, 7, appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1})
	r.laneZero(t, 1500*time.Millisecond)
	r.site.gridW.Store(5555)
	at := time.Now()
	r.arrivals <- struct{}{}
	for {
		f, got, _ := r.laneZero(t, 2*time.Second)
		var d appproto.Delta
		if f.Envelope.T == appproto.MsgDelta && appproto.Unmarshal(f.Envelope.B, &d) == nil && d.Fields["2"] == 5555 {
			if took := got.Sub(at); took > 200*time.Millisecond {
				t.Fatalf("back in front, a poll took %s", took)
			}
			return
		}
	}
}

// A bulk message the link paces must not fall out of the phone's Noise
// replay window behind the lane 0 frames that overtake it.
func TestLaneZeroCannotPushABulkFrameOutOfTheReplayWindow(t *testing.T) {
	wait := cloudlink.MaxBulkWait()
	// Commands and their answers ride lane 0 too; a person sends a few.
	const commands = 8
	if n := tunBurst + tunRate*wait.Seconds() + commands; n >= appwire.ReplayWindow {
		t.Fatalf("%.0f lane 0 frames can pass a bulk frame held %s; the window is %d", n, wait, appwire.ReplayWindow)
	}
}

// What one push costs the box for one session: the site read, the delta,
// its padding, Noise and the tun frame, as Run does it. Multiply by frames a
// second and sessions for the share of a core.
func BenchmarkAPushThroughTheTunnel(b *testing.B) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(b.TempDir(), "nova.key"))
	if err != nil {
		b.Fatal(err)
	}
	app, _ := newAppClient()
	m1, _ := app.message1(enroll.StaticKey().Public(), nil)
	hs, _ := appwire.NewResponder(appwire.HandshakeOptions{StaticKey: enroll.StaticKey(), Prologue: prologue(enroll.StaticKey().Public())})
	if _, err := hs.ReadMessage(m1); err != nil {
		b.Fatal(err)
	}
	reply, _ := hs.WriteMessage(nil)
	noise, _ := hs.Split()
	if err := app.readMessage2(reply); err != nil {
		b.Fatal(err)
	}
	site := &movingSite{}
	var out []byte
	send := func(msg []byte, _ bool) error {
		body, err := cloudwire.Marshal(cloudwire.Tun{Dev: "AbCdEfGh", Sid: 7, D: msg})
		if err != nil {
			return err
		}
		raw, err := cloudwire.Envelope{T: cloudwire.MsgTun, B: body}.Encode()
		if err != nil {
			return err
		}
		out, err = cloudwire.EncodeFrame(raw)
		return err
	}
	h, err := appproto.New(appproto.Config{
		Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:  site, Info: stubInfo{}, Modes: stubModes{}, Plans: stubPlans{}, Codec: codec{},
		Sender: tunSender{transport: appwire.NewTransport(noise), send: send},
		Caller: apiauth.Caller{Subject: "app:AbCdEfGh", Kind: apiauth.KindApp, Role: apiauth.RoleOwner,
			Scopes: appproto.ScopesForRole(apiauth.RoleOwner), Epoch: 1},
		Grants: ownerGrant{}, Commands: appproto.NewCmdLog(),
		SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, m := range []struct {
		t    string
		body any
	}{
		{appproto.MsgHello, appproto.Hello{Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax}}},
		{appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1}},
	} {
		raw, _ := appproto.Marshal(m.body)
		frame, _ := appwire.EncodeFrame(appwire.Frame{Lane: appwire.LaneControl, Bucket: 512, Envelope: appwire.Envelope{T: m.t, B: raw}})
		if err := h.Handle(context.Background(), frame); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		site.gridW.Add(1)
		if err := h.Push(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(out)), "B/frame")
}

// ownerGrant is a paired owner phone whose grant never moves.
type ownerGrant struct{}

func (ownerGrant) Grant() (appproto.Enrolment, bool) {
	return appproto.Enrolment{Role: apiauth.RoleOwner, Epoch: 1}, true
}
