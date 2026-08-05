package appproto

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
)

func TestHelloNegotiatesDownToTheClient(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoMax},
		App:   AppInfo{Build: "test", UA: "pwa"},
	})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoMax)
	}
	if ok.Mode != BoxModeFull {
		t.Fatalf("mode = %q, want full", ok.Mode)
	}
	if ok.Hint != "" {
		t.Fatalf("hint = %q, want none", ok.Hint)
	}
}

// An app too old for the box degrades instead of dying. A hard version wall is
// a white screen for everyone whose service worker pinned an old bundle.
func TestHelloFromTooOldAnAppGetsFloorNotAnError(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoFloor},
		App:   AppInfo{Build: "ancient", UA: "pwa"},
	})

	if rec.has(MsgError) {
		t.Fatal("box answered an old app with an error")
	}

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoFloor {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoFloor)
	}
	if ok.Mode != BoxModeFloor {
		t.Fatalf("mode = %q, want floor", ok.Mode)
	}
	if ok.Hint != HintAppUpdate {
		t.Fatalf("hint = %q, want %q", ok.Hint, HintAppUpdate)
	}
	if len(ok.Caps) != 1 || ok.Caps[0] != CapStatusCore {
		t.Fatalf("floor caps = %v, want only %q", ok.Caps, CapStatusCore)
	}
	// The catalogue still goes out: field 1 is an index into it, so a floor
	// session that omitted it could not name the mode it is running.
	if len(ok.Modes) != len(control.ModeCatalog()) {
		t.Fatalf("floor mode catalogue has %d entries, want %d", len(ok.Modes), len(control.ModeCatalog()))
	}
}

// hello_ok carries a capability list and a mode catalogue, both of which vary
// in size with what the box supports. Lane 0 exists so that its frames do not
// vary in size with anything, so this reply belongs on bulk.
func TestHelloOKTravelsOnTheBulkLane(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})

	f := rec.only(t, MsgHelloOK)
	if f.lane != LaneBulk {
		t.Fatalf("hello_ok went out on lane %d, want %d (bulk)", f.lane, LaneBulk)
	}
	if f.size <= 512 {
		t.Fatalf("hello_ok is %d bytes; if it fits lane 0 this test proves nothing", f.size)
	}
}

// The catalogue is control's, verbatim. A hand-written list here would let the
// app offer a mode the box does not have, or hide one it does.
func TestHelloOKModesComeFromTheControlCatalogue(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	cat := control.ModeCatalog()
	if len(ok.Modes) != len(cat) {
		t.Fatalf("catalogue has %d entries, wire has %d", len(cat), len(ok.Modes))
	}
	for i, m := range cat {
		got := ok.Modes[i]
		if got.Key != string(m.Key) || got.Label != m.Label ||
			got.Tooltip != m.Tooltip || got.Tier != string(m.Tier) {
			t.Fatalf("mode %d: wire %+v, catalogue %+v", i, got, m)
		}
	}
}

func TestHelloOKReportsBootProgress(t *testing.T) {
	h, box, rec, _ := newRig(t)
	eta := int64(90_000)
	box.boot = &BootProgress{Phase: BootPhaseVacuum, Pct: 40, EtaMs: &eta}

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	if ok.Mode != BoxModeBooting {
		t.Fatalf("mode = %q, want booting", ok.Mode)
	}
	if ok.Boot == nil || ok.Boot.Phase != BootPhaseVacuum || ok.Boot.Pct != 40 {
		t.Fatalf("boot = %+v", ok.Boot)
	}
}

// Ages are uptime deltas. A Pi reads 1970 until NTP answers, so a wall clock
// on the wire without its provenance would make every freshness claim wrong by
// decades exactly when someone looks after a power cut.
func TestHelloOKCarriesUptimeAndClockProvenance(t *testing.T) {
	h, _, rec, clock := newRig(t)
	clock.uptimeMs = 123_456

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	if ok.Clock.UptimeMs != 123_456 {
		t.Fatalf("uptimeMs = %d, want 123456", ok.Clock.UptimeMs)
	}
	if ok.Clock.Source != "ntp" {
		t.Fatalf("clock source = %q", ok.Clock.Source)
	}
}

func TestCapsHashIsOrderIndependentAndSetSensitive(t *testing.T) {
	a := capsHash([]string{CapStatusCore, CapCmdLease})
	b := capsHash([]string{CapCmdLease, CapStatusCore})
	if a != b {
		t.Fatalf("the same set in a different order hashed differently: %q vs %q", a, b)
	}
	c := capsHash([]string{CapStatusCore})
	if a == c {
		t.Fatal("a different capability set hashed the same")
	}
}

func TestNewRefusesACapabilityNotInTheRegistry(t *testing.T) {
	_, box, rec, clock := newRig(t)
	_, err := New(Config{
		Clock: clock, Site: box, Info: box, Modes: box, Plans: box,
		Codec: testCodec{}, Sender: rec,
		Caps: []string{"status.core", "status.kore"},
	})
	if err == nil {
		t.Fatal("a hand-typed capability name was accepted")
	}
}

// Both directions ignore what they do not recognise. That single rule is what
// lets a newer app talk to an older box and the reverse.
func TestUnknownMessageTypeIsIgnoredWithoutARequestId(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, "der.enumerate", nil, map[string]any{"x": 1})
	if len(rec.frames) != 0 {
		t.Fatalf("unsolicited unknown type produced %s", rec.types())
	}
}

func TestUnknownMessageTypeWithARequestIdIsAnswered(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, "der.enumerate", ptrU32(9), map[string]any{"x": 1})

	f := rec.only(t, MsgError)
	if f.env.ID == nil || *f.env.ID != 9 {
		t.Fatalf("error did not echo the request id: %+v", f.env.ID)
	}
	e := body[ErrorBody](t, f)
	if e.Code != ErrUnknownOp {
		t.Fatalf("code = %q, want %q", e.Code, ErrUnknownOp)
	}
}

// A newer app sends keys this build has never heard of. Dropping them is the
// point; refusing the message would be the version wall by another route.
func TestUnknownBodyKeysAreIgnored(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, map[string]any{
		"proto":     map[string]any{"min": 0, "max": ProtoMax, "preferred": 3},
		"app":       map[string]any{"build": "future", "ua": "pwa", "channel": "beta"},
		"locales":   []string{"sv"},
		"telepathy": true,
	})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoMax)
	}
}

func TestSubWhileBootingIsRefusedWithARetryableCode(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.boot = &BootProgress{Phase: BootPhaseMigrate, Pct: 10}

	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	e := body[ErrorBody](t, rec.only(t, MsgError))
	if e.Code != ErrBooting {
		t.Fatalf("code = %q, want %q", e.Code, ErrBooting)
	}
	if !e.Retryable {
		t.Fatal("E_BOOTING must be retryable; the box is about to be ready")
	}
	if h.Subscribed() {
		t.Fatal("a booting box accepted a subscription")
	}
}
