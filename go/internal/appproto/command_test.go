package appproto

import (
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
)

func cmdSetMode(mode string, rev uint64, notValidAfterMs int64, guards []Guard) Cmd {
	return Cmd{
		CmdID:           "0192f2a0-7c1e-7000-8000-0123456789ab",
		Op:              OpSetMode,
		Args:            map[string]any{"mode": mode},
		NotValidAfterMs: notValidAfterMs,
		Expect:          Expect{Rev: rev, Guards: guards},
	}
}

// The ack says the dispatcher took the intent. The result says the driver read
// the value back. Collapsing them would make the app claim knowledge it does
// not have.
func TestSetModeAcksThenConfirmsFromAReadBack(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))

	ack := body[CmdAck](t, rec.only(t, MsgCmdAck))
	if ack.LeaseID == "" {
		t.Fatal("ack carried no lease")
	}
	if ack.ExpiresAtMs <= 60_000 {
		t.Fatalf("lease expires at %d, which is not in the future", ack.ExpiresAtMs)
	}

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q, want applied", res.State)
	}
	if res.Observed == nil {
		t.Fatal("applied without an observed value; the echo of a request is not confirmation")
	}
	if res.Observed.Src != ObservedSrcCore {
		t.Fatalf("observed src = %q", res.Observed.Src)
	}
	want := modeIndex(h.modes, string(control.ModeCharge))
	if int64(res.Observed.Value) != want {
		t.Fatalf("observed value = %v, want catalogue index %d", res.Observed.Value, want)
	}
	if box.mode != control.ModeCharge {
		t.Fatalf("box mode = %q", box.mode)
	}
}

// A mode change alters what the planner does, so the plan is remade and pushed
// unasked. Otherwise the app shows the old intent beside the new mode and
// neither looks wrong.
func TestAppliedModeChangePushesAFreshPlan(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))

	if !rec.has(MsgPlan) {
		t.Fatalf("no plan followed the mode change; frames were %s", rec.types())
	}
}

// The dispatcher took the intent but nothing read back. Not a failure and not
// a success, and the app has to be able to say exactly that.
func TestNoReadBackIsUnconfirmedNotApplied(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.onSetMode = func() { box.observedOK = false }

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdUnconfirmed {
		t.Fatalf("state = %q, want unconfirmed", res.State)
	}
	if res.Observed != nil {
		t.Fatal("unconfirmed carried an observed value")
	}
}

// Something else moved the mode between the write and the read. The result
// reports what is true now, not what was asked for.
func TestAReadBackThatDisagreesIsSupersededNotApplied(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.onSetMode = func() { box.observedMode = control.ModeIdle }

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdSuperseded {
		t.Fatalf("state = %q, want superseded", res.State)
	}
	if res.Observed == nil || int64(res.Observed.Value) != modeIndex(h.modes, string(control.ModeIdle)) {
		t.Fatalf("observed = %+v, want the mode actually running", res.Observed)
	}
}

// A command queued while the phone was in a tunnel must never execute as
// though the user had just pressed the button.
func TestAnExpiredCommandIsRefusedWithoutTouchingTheBox(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, clock.uptimeMs-1, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdExpired {
		t.Fatalf("state = %q, want expired", res.State)
	}
	if res.Error == nil || res.Error.Code != ErrCmdExpired {
		t.Fatalf("error = %+v, want %q", res.Error, ErrCmdExpired)
	}
	if box.setModeCalls != 0 {
		t.Fatal("an expired command reached the dispatcher")
	}
	if rec.has(MsgCmdAck) {
		t.Fatal("an expired command was acked")
	}
}

func TestARevMismatchIsAConflict(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 6, 200_000, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrConflict {
		t.Fatalf("result = %+v, want rejected/%s", res, ErrConflict)
	}
	if res.Error.Args["actual"] == nil {
		t.Fatal("E_CONFLICT carried no machine-readable rev; the app cannot resync from prose it does not get")
	}
	if box.setModeCalls != 0 {
		t.Fatal("a conflicting command reached the dispatcher")
	}
}

// Guards are re-evaluated against state as it is now, not as it was when the
// user pressed the button. The world moves between those two moments and that
// gap is where safety lives.
func TestGuardsAreCheckedAgainstFreshState(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	// The app saw 620 permille and asked to charge only below 900.
	guards := []Guard{{Fid: FidBatterySoc, Op: GuardLT, Value: 900}}

	// By the time the command lands the battery has passed the limit.
	box.snap.BatterySoC = 0.95

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, guards))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrPrecondition {
		t.Fatalf("result = %+v, want rejected/%s", res, ErrPrecondition)
	}
	if box.setModeCalls != 0 {
		t.Fatal("a failed precondition still reached the dispatcher")
	}
}

func TestGuardsThatHoldLetTheCommandThrough(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	guards := []Guard{
		{Fid: FidBatterySoc, Op: GuardLT, Value: 900},
		{Fid: FidGridW, Op: GuardLTE, Value: 5000},
	}
	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, guards))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q, want applied", res.State)
	}
}

// A guard nobody can evaluate is not a guard that passed.
func TestAnUncheckableGuardFails(t *testing.T) {
	cases := []struct {
		name  string
		guard Guard
	}{
		{"field the box does not send", Guard{Fid: Fid(42), Op: GuardLT, Value: 1}},
		{"operator this build does not know", Guard{Fid: FidGridW, Op: "approximately", Value: 1}},
	}
	fields := map[string]int64{fidKey(FidGridW): 1200}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ok, _ := guardsHold([]Guard{c.guard}, fields); ok {
				t.Fatal("guard passed")
			}
		})
	}
}

// A retry must return the original outcome, not act a second time. That is the
// entire reason the command id exists.
func TestARetryReplaysTheOutcomeAndDoesNotActTwice(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	cmd := cmdSetMode(string(control.ModeCharge), 7, 200_000, nil)
	deliver(t, h, MsgCmd, nil, cmd)
	firstAck := body[CmdAck](t, rec.only(t, MsgCmdAck))
	rec.reset()

	// The client never saw the ack and sends the same command again.
	deliver(t, h, MsgCmd, nil, cmd)

	if box.setModeCalls != 1 {
		t.Fatalf("the dispatcher ran %d times for one command id", box.setModeCalls)
	}
	replayAck := body[CmdAck](t, rec.only(t, MsgCmdAck))
	if replayAck.LeaseID != firstAck.LeaseID {
		t.Fatalf("retry issued a new lease %q, original %q", replayAck.LeaseID, firstAck.LeaseID)
	}
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("retry replayed state %q", res.State)
	}
}

// The table is remembered for a day and then forgotten, so a box that has been
// up for months is not carrying every command it has ever seen.
func TestCommandIdsAreForgottenAfterTheRetentionWindow(t *testing.T) {
	log := newCmdLog()
	log.remember("old", CmdAck{CmdID: "old"}, 1_000)
	log.remember("new", CmdAck{CmdID: "new"}, 1_000+CmdRetentionMs+1)

	if _, ok := log.lookup("old"); ok {
		t.Fatal("a command id outlived the retention window")
	}
	if _, ok := log.lookup("new"); !ok {
		t.Fatal("the fresh command id was pruned")
	}
	if log.size() != 1 {
		t.Fatalf("table holds %d entries, want 1", log.size())
	}
}

func TestAModeTheBoxDoesNotHaveIsRefused(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode("planner_telepathy", 7, 200_000, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnknownOp {
		t.Fatalf("result = %+v", res)
	}
	if box.setModeCalls != 0 {
		t.Fatal("an invalid mode reached the dispatcher")
	}
}

func TestAnUnknownOpIsRejectedNotIgnored(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, Cmd{
		CmdID:           "0192f2a0-7c1e-7000-8000-0123456789ab",
		Op:              "battery.hold",
		Args:            map[string]any{"watts": 3000},
		NotValidAfterMs: 200_000,
		Expect:          Expect{Rev: 7},
	})

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnknownOp {
		t.Fatalf("result = %+v", res)
	}
}

func TestControlRefusingTheModeIsReportedNotSwallowed(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.setModeErr = errors.New("no battery online")

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))

	if !rec.has(MsgCmdAck) {
		t.Fatal("the dispatcher took the intent but never acked it")
	}
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
		t.Fatalf("result = %+v", res)
	}
}

// Without an idempotency key a retry cannot be made safe, so the command is
// refused rather than run once and hoped about.
func TestACommandWithNoIdIsRefused(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, Cmd{
		Op:              OpSetMode,
		Args:            map[string]any{"mode": string(control.ModeCharge)},
		NotValidAfterMs: 200_000,
		Expect:          Expect{Rev: 7},
	})

	e := body[ErrorBody](t, rec.only(t, MsgError))
	if e.Code != ErrUnknownOp {
		t.Fatalf("code = %q", e.Code)
	}
	if box.setModeCalls != 0 {
		t.Fatal("a command with no id reached the dispatcher")
	}
}

// The box's own invariant is that stale meter data stops dispatch. An
// operation that moves energy does not get to go around it through this door.
func TestDispatchWritesAreRefusedWhileDispatchIsBlocked(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	// site.mode.set is deliberately not a dispatch write, so this test
	// registers a temporary one rather than pretending it is.
	const op = "test.dispatch.write"
	h.ops[op] = opSpec{scope: ScopeDispatchWrite, dispatchWrite: true}

	box.snap.DispatchBlockedBy = []string{"meter.p1"}

	deliver(t, h, MsgCmd, nil, Cmd{
		CmdID:           "0192f2a0-7c1e-7000-8000-0123456789ac",
		Op:              op,
		NotValidAfterMs: 200_000,
		Expect:          Expect{Rev: 7},
	})

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrUnavailable {
		t.Fatalf("result = %+v, want rejected/%s", res, ErrUnavailable)
	}
	if res.Error.Args["dispatchBlockedBy"] == nil {
		t.Fatal("the refusal did not say which source blocked it")
	}
}

// A stale meter must not lock a user out of choosing a safer strategy. The
// dispatch gate in control already refuses to act on the mode until the meter
// is back.
func TestAStaleMeterDoesNotBlockAModeChange(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.snap.DispatchBlockedBy = []string{"meter.p1"}

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeIdle), 7, 200_000, nil))

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q, want applied", res.State)
	}
}
