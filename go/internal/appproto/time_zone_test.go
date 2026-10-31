package appproto

import "testing"

// memZones keeps a site zone the way the API server does: an offer is taken
// only while none is kept.
type memZones struct {
	kept  string
	calls int
}

func (m *memZones) ProposeTimeZone(zone string) (TimeZoneAnswer, error) {
	m.calls++
	if m.kept != "" {
		return TimeZoneAnswer{Zone: m.kept}, nil
	}
	m.kept = zone
	return TimeZoneAnswer{Zone: zone, Taken: true}, nil
}

func cmdProposeTimeZone(id string, zone any) Cmd {
	return Cmd{
		CmdID:           id,
		Op:              OpSiteTimeZonePropose,
		Args:            map[string]any{"tz": zone},
		NotValidAfterMs: 200_000,
		Expect:          Expect{Rev: 7},
	}
}

func zoneRig(t *testing.T, zones TimeZones) (*Handler, *recorder) {
	t.Helper()
	h, _, rec, _ := newRig(t)
	h.cfg.TimeZones = zones
	subscribe(t, h, rec)
	rec.reset()
	return h, rec
}

func TestATimeZoneOfferIsTakenByABoxWithoutOne(t *testing.T) {
	zones := &memZones{}
	h, rec := zoneRig(t, zones)

	deliver(t, h, MsgCmd, nil, cmdProposeTimeZone("0192f2a0-7c1e-7000-8000-0123456789b0", "Europe/Stockholm"))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || zones.kept != "Europe/Stockholm" {
		t.Fatalf("result %+v kept %q, want applied Europe/Stockholm", res, zones.kept)
	}
}

// The box decides: a kept zone stays, and the answer names it.
func TestATimeZoneOfferMeetsTheZoneTheBoxKeeps(t *testing.T) {
	zones := &memZones{kept: "Europe/Stockholm"}
	h, rec := zoneRig(t, zones)

	deliver(t, h, MsgCmd, nil, cmdProposeTimeZone("0192f2a0-7c1e-7000-8000-0123456789b1", "America/New_York"))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrPrecondition || res.Error.Args["tz"] != "Europe/Stockholm" {
		t.Fatalf("result %+v, want E_PRECONDITION naming Europe/Stockholm", res)
	}
}

func TestATimeZoneNoClientCanLoadIsRefusedBeforeTheBoxSeesIt(t *testing.T) {
	zones := &memZones{}
	h, rec := zoneRig(t, zones)

	deliver(t, h, MsgCmd, nil, cmdProposeTimeZone("0192f2a0-7c1e-7000-8000-0123456789b2", "Local"))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnknownOp || zones.calls != 0 {
		t.Fatalf("result %+v calls %d, want a refused arg and no offer", res, zones.calls)
	}
}

func TestATimeZoneOfferWithoutAPortIsUnavailable(t *testing.T) {
	h, rec := zoneRig(t, nil)

	deliver(t, h, MsgCmd, nil, cmdProposeTimeZone("0192f2a0-7c1e-7000-8000-0123456789b3", "Europe/Stockholm"))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
		t.Fatalf("result %+v, want E_UNAVAILABLE", res)
	}
}
