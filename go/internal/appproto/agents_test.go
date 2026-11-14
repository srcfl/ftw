package appproto

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/internal/redact"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The agent tests run against a real enrolment, so a signature, an expiry and
// a revoke are the box's own answers and not a fake's.

type testAccess struct {
	id       *appenroll.Identity
	wall     func() (int64, bool)
	lp       *fakeLoadpoints
	ended    []string
	notified []string
	revoked  []string
}

func (a *testAccess) AddAgent(dev string, m protocol.AgentAdd) (protocol.AgentRow, error) {
	return a.id.AddAgent(appenroll.Approver{DeviceID: dev}, m)
}
func (a *testAccess) WidenAgent(dev string, m protocol.AgentWiden) (protocol.AgentRow, error) {
	return a.id.WidenAgent(appenroll.Approver{DeviceID: dev}, m)
}
func (a *testAccess) RevokeAgent(id string) error {
	a.revoked = append(a.revoked, id)
	_, err := a.id.Revoke(id, appenroll.OverASession)
	if err == nil {
		a.EndHolds(id)
	}
	return err
}
func (a *testAccess) Agents() []protocol.AgentRow              { return a.id.Agents() }
func (a *testAccess) ApprovalKeys() []protocol.ApprovalKeyInfo { return a.id.ApprovalKeys() }
func (a *testAccess) CheckConfirm(d string, m protocol.AgentConfirm, req protocol.Request) error {
	return a.id.CheckConfirm(d, m, req)
}

// EndHolds releases the agent's hold, as the box's wiring does.
func (a *testAccess) EndHolds(agentID string) {
	a.ended = append(a.ended, agentID)
	if a.lp != nil && a.lp.holdSet && a.lp.hold.SetBy == apiauth.KindAgent+":"+agentID {
		a.lp.holdSet = false
	}
}
func (a *testAccess) SetApprovalKey(d string, m protocol.ApprovalKeySet) (int64, error) {
	return a.id.SetApprovalKey(d, m)
}
func (a *testAccess) Wall() (int64, bool)   { return a.wall() }
func (a *testAccess) Notify(kind, _ string) { a.notified = append(a.notified, kind) }

// enrolGrants reads one row the way appuplink's liveGrant does.
type enrolGrants struct {
	id  *appenroll.Identity
	dev string
}

func (g enrolGrants) Grant() (Enrolment, bool) {
	grant, ok := g.id.GrantFor(g.dev)
	if !ok {
		return Enrolment{}, false
	}
	e := Enrolment{Role: grant.Role, Epoch: grant.Epoch, ParentRole: grant.ParentRole}
	if grant.Agent != nil {
		e.Agent = &AgentGrant{Scopes: grant.Agent.Scopes, Ops: grant.Agent.Ops, Caps: grant.Agent.Caps,
			ExpiresAtMs: grant.Agent.ExpiresAtMs, Principal: grant.Agent.Principal,
			Expired: grant.Agent.Expired, NoClock: grant.Agent.NoClock}
	}
	return e, true
}

type memAudit struct {
	mu   sync.Mutex
	rows []protocol.AuditRow
}

func (a *memAudit) Append(r protocol.AuditRow) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.Seq = uint64(len(a.rows) + 1)
	a.rows = append(a.rows, r)
}

func (a *memAudit) Page(before uint64, n int) ([]protocol.AuditRow, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []protocol.AuditRow
	for i := len(a.rows) - 1; i >= 0 && len(out) < n; i-- {
		if before == 0 || a.rows[i].Seq < before {
			out = append(out, a.rows[i])
		}
	}
	return out, false, nil
}

func (a *memAudit) find(what string) []protocol.AuditRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []protocol.AuditRow
	for _, r := range a.rows {
		if r.What == what {
			out = append(out, r)
		}
	}
	return out
}

type agentRig struct {
	t      *testing.T
	id     *appenroll.Identity
	now    time.Time
	synced bool
	access *testAccess
	agents *AgentState
	audit  *memAudit
	box    *fakeBox
	clock  *fakeClock
	lp     *fakeLoadpoints
	owner  appenroll.Grant
	signer *approvaltest.Signer
	logs   []redact.Line
	// journal, when a test sets it, serves events.get.
	journal JournalSource
	// cars and chargers, when a test sets them, are the box's car store
	// and the plan's chargers.
	cars     *fakeCars
	chargers PlanChargers
}

func (r *agentRig) ms() int64 { return r.now.UnixMilli() }

func (r *agentRig) signedFor() protocol.SignedFor {
	return protocol.SignedFor{BoxKey: r.id.StaticKey().Public(), DeviceID: r.owner.DeviceID}
}

func (r *agentRig) Recent(n int) []redact.Line { return r.logs }

func newAgentRig(t *testing.T) *agentRig {
	t.Helper()
	id, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	r := &agentRig{t: t, id: id, now: time.UnixMilli(1_792_000_000_000), synced: true,
		agents: NewAgentState(), audit: &memAudit{}}
	id.SetClock(func() (time.Time, bool) { return r.now, r.synced })
	r.access = &testAccess{id: id, wall: func() (int64, bool) { return r.ms(), r.synced }}

	code, _, err := id.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := appwire.GenerateKeyPair()
	if r.owner, err = id.Authorise(phone.Public(), code); err != nil {
		t.Fatal(err)
	}
	r.signer = approvaltest.New()
	if _, err := id.SetApprovalKey(r.owner.DeviceID, r.signer.KeySet(r.signedFor(), r.ms())); err != nil {
		t.Fatal(err)
	}

	_, r.box, _, r.clock = newRig(t)
	r.lp = &fakeLoadpoints{ids: map[string]bool{"lp1": true}, plugged: true}
	r.access.lp = r.lp
	return r
}

var testAgentKey = []byte("agent key")

// lp1 is the charger's id as an agent reads it.
func lp1() string { return cloudwire.LoadpointID(testAgentKey, "lp1") }

type testViews struct{}

func (testViews) Live(key []byte) cloudwire.Live {
	return cloudwire.Live{Fields: map[string]int64{"2": 1200},
		Sources: map[string]cloudwire.Source{cloudwire.SourceID(key, "meter.p1"): {Kind: "meter"}}}
}

func (testViews) Loadpoints(key []byte) (cloudwire.LP, bool) {
	return cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{{ID: cloudwire.LoadpointID(key, "lp1")}}}, true
}

func (testViews) Cars(key []byte) (cloudwire.Cars, bool) {
	soc := int64(140)
	return cloudwire.Cars{Cars: []cloudwire.Car{{ID: cloudwire.CarID(key, "c1"), SocPermille: &soc, SocSource: protocol.CarSoCVehicle}}}, true
}

// handler builds a session for one enrolled row, as the uplink would.
func (r *agentRig) handler(kind, dev, role string) (*Handler, *recorder) {
	r.t.Helper()
	rec := &recorder{}
	grants := enrolGrants{id: r.id, dev: dev}
	g, _ := grants.Grant()
	caller := apiauth.Caller{Subject: kind + ":" + dev, Kind: kind, Role: role, Scopes: ScopesForEnrolment(g)}
	var carPort Cars
	if r.cars != nil {
		carPort = r.cars
	}
	h, err := New(Config{
		Cars: carPort, PlanChargers: r.chargers,
		Clock: r.clock, Site: r.box, Info: r.box, Modes: r.box, Plans: r.box,
		Codec: testCodec{}, Sender: rec, Caller: caller, Grants: grants, Commands: NewCmdLog(),
		Loadpoints: r.lp, Access: r.access, Agents: r.agents, Audit: r.audit,
		Logs: r, Journal: r.journal, Redactor: func() *redact.Redactor { return redact.New(testAgentKey, []string{"Easee carport"}) },
		Views: testViews{}, AgentKey: testAgentKey,
		SrcGrid: "meter.p1", SrcPV: "inverter.sungrow", SrcBattery: "battery.sungrow",
		NewLeaseID: func() string { return "lease-test" },
	})
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	return h, rec
}

func (r *agentRig) ownerSession() (*Handler, *recorder) {
	return r.handler(apiauth.KindApp, r.owner.DeviceID, apiauth.RoleOwner)
}

// grant adds an agent row with scopes and ops, signed by the owner phone.
func (r *agentRig) grant(role string, scopes, ops []string, caps protocol.Caps, ttl time.Duration, p protocol.Principal) (protocol.AgentRow, []byte) {
	r.t.Helper()
	key, _ := appwire.GenerateKeyPair()
	m := protocol.AgentAdd{Key: key.Public(), Role: role, Scopes: scopes, Ops: ops, Caps: caps,
		ExpiresAtMs: r.ms() + ttl.Milliseconds(), Principal: p, AtMs: r.ms()}
	row, err := r.id.AddAgent(appenroll.Approver{DeviceID: r.owner.DeviceID}, r.signer.Add(r.signedFor(), m))
	if err != nil {
		r.t.Fatalf("AddAgent: %v", err)
	}
	return row, key.Public()
}

func (r *agentRig) controlAgent() protocol.AgentRow {
	row, _ := r.grant(apiauth.RoleAgent,
		[]string{protocol.ScopeDispatchHold, protocol.ScopeGoalsRequest, protocol.ScopeLiveRead, protocol.ScopePlanRead},
		[]string{OpLoadpointHold, OpSetMode},
		protocol.Caps{HoldS: 900, MaxW: 7400, PerMin: 6}, 24*time.Hour, protocol.Principal{Client: "Claude"})
	return row
}

func (r *agentRig) agentSession(row protocol.AgentRow) (*Handler, *recorder) {
	return r.handler(apiauth.KindAgent, row.ID, row.Role)
}

func cmdFor(id, op string, args map[string]any, rev uint64) Cmd {
	return Cmd{CmdID: id, Op: op, Args: args, NotValidAfterMs: 1 << 40, Expect: Expect{Rev: rev}}
}

func errCode(t *testing.T, rec *recorder) string {
	t.Helper()
	return body[ErrorBody](t, rec.last(t, MsgError)).Code
}

func resultCode(t *testing.T, rec *recorder) string {
	t.Helper()
	res := body[CmdResult](t, rec.last(t, MsgCmdResult))
	if res.Error == nil {
		return res.State
	}
	return res.Error.Code
}

func TestAnAgentReadsWhatItsScopesOpenAndNothingElse(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLiveRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{Client: "ChatGPT"})
	h, rec := r.agentSession(row)

	deliver(t, h, MsgPlanGet, ptrU32(1), struct{}{})
	if got := errCode(t, rec); got != ErrScopeDenied {
		t.Errorf("plan without plan.read: %s", got)
	}
	deliver(t, h, MsgAPIReq, ptrU32(2), APIReq{Method: APIGet, Path: "/api/status"})
	if got := errCode(t, rec); got != ErrScopeDenied {
		t.Errorf("the passthrough: %s", got)
	}
	deliver(t, h, protocol.MsgAgentAdd, ptrU32(3), protocol.AgentAdd{})
	if got := errCode(t, rec); got != ErrScopeDenied {
		t.Errorf("an agent adding an agent: %s", got)
	}
	deliver(t, h, MsgSub, ptrU32(4), Sub{Bucket: 512, Hz: 1})
	if got := errCode(t, rec); got != ErrScopeDenied {
		t.Errorf("sub, whose source table names devices: %s", got)
	}
	rec.reset()
	deliver(t, h, protocol.MsgLiveGet, ptrU32(5), struct{}{})
	deliver(t, h, protocol.MsgLiveGet, ptrU32(6), struct{}{})
	live := body[cloudwire.Live](t, rec.last(t, protocol.MsgLive))
	if _, ok := live.Sources[cloudwire.SourceID(testAgentKey, "meter.p1")]; !ok || live.Fields["2"] != 1200 {
		t.Errorf("live %+v", live)
	}
	// Reads fold into one audit row per minute, with a count.
	r.agents.FlushAudit(r.audit, r.clock.now.Add(time.Minute).UnixMilli())
	rows := r.audit.find(protocol.MsgLiveGet)
	if len(rows) != 1 || rows[0].Count != 2 || rows[0].Principal != "ChatGPT" {
		t.Errorf("audit %+v", rows)
	}
}

func TestARevokeFailsTheNextCall(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)
	deliver(t, h, MsgPlanGet, ptrU32(1), struct{}{})
	if rec.has(MsgError) {
		t.Fatalf("before the revoke: %s", rec.types())
	}

	owner, orec := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentRevoke, ptrU32(9), protocol.AgentRevoke{ID: row.ID})
	if orec.has(MsgError) {
		t.Fatalf("revoke: %s", orec.types())
	}

	rec.reset()
	deliver(t, h, MsgPlanGet, ptrU32(2), struct{}{})
	if got := errCode(t, rec); got != ErrGrantRevoked {
		t.Errorf("after the revoke: %s", got)
	}
	if body[SessionTerminate](t, rec.last(t, MsgSessionTerminate)).Reason != TerminateRevoked {
		t.Error("the session did not end")
	}
	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdFor("c1", OpLoadpointHold, map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}, 7))
	if got := resultCode(t, rec); got != ErrGrantRevoked || r.lp.holdCalls != 0 {
		t.Errorf("a command after the revoke: %s, holds %d", got, r.lp.holdCalls)
	}
}

// FTW Cloud may still list the agent or have its session open; the box asks
// its own clock on every call.
func TestAnExpiredAgentFailsAtTheBoxEvenWhenTheCloudAdmitsIt(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)

	r.now = r.now.Add(25 * time.Hour)
	deliver(t, h, MsgPlanGet, ptrU32(1), struct{}{})
	if got := errCode(t, rec); got != protocol.ErrGrantExpired {
		t.Errorf("a read after expiry: %s", got)
	}
	if body[SessionTerminate](t, rec.last(t, MsgSessionTerminate)).Reason != protocol.TerminateExpired {
		t.Error("the session did not end as expired")
	}
	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdFor("c1", OpLoadpointHold, map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}, 7))
	if got := resultCode(t, rec); got != protocol.ErrGrantExpired || r.lp.holdCalls != 0 {
		t.Errorf("a command after expiry: %s, holds %d", got, r.lp.holdCalls)
	}

	// An open stream ends on the next tick, without waiting for a call.
	r.now = r.now.Add(-25 * time.Hour)
	h2, _ := r.agentSession(row)
	r.now = r.now.Add(25 * time.Hour)
	if err := h2.Tick(); !errors.Is(err, errAgentEnded) {
		t.Errorf("tick after expiry: %v", err)
	}

	// No clock: nothing, and nothing ends.
	r.now = r.now.Add(-25 * time.Hour)
	r.synced = false
	h3, rec3 := r.agentSession(row)
	deliver(t, h3, MsgPlanGet, ptrU32(1), struct{}{})
	if e := body[ErrorBody](t, rec3.last(t, MsgError)); e.Code != ErrUnavailable || e.Args["subsystem"] != "clock" {
		t.Errorf("without a clock: %+v", e)
	}
}

func TestAnOpOutsideTheAllowlistIsRefused(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent,
		[]string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead},
		[]string{OpLoadpointHold}, protocol.Caps{HoldS: 900, MaxW: 7400}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)

	deliver(t, h, MsgCmd, nil, cmdFor("c1", OpLoadpointBoost, map[string]any{"id": lp1(), "duration_s": 60}, 7))
	if got := resultCode(t, rec); got != protocol.ErrOpNotGranted {
		t.Errorf("boost, not in the grant: %s", got)
	}
	deliver(t, h, MsgCmd, nil, cmdFor("c2", OpSetMode, map[string]any{"mode": "idle"}, 7))
	if got := resultCode(t, rec); got != protocol.ErrNeedsOwner {
		t.Errorf("a durable change as a command: %s", got)
	}
	if r.box.setModeCalls != 0 {
		t.Error("the mode changed")
	}
	// Refusals fold by agent, message and code into a row a minute.
	r.agents.FlushAudit(r.audit, r.clock.now.Add(time.Minute).UnixMilli())
	rows := r.audit.find(MsgCmd)
	codes := map[string]uint64{}
	for _, row := range rows {
		codes[row.Result] += row.Count
	}
	if len(rows) != 2 || codes[protocol.ErrOpNotGranted] != 1 || codes[protocol.ErrNeedsOwner] != 1 {
		t.Errorf("audit %+v", rows)
	}
}

// An agent cannot flood the owner's log: refusals fold, and what it names
// in a message type or an op is cut to a plain word.
func TestAnAgentCannotWriteIntoTheAuditLog(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLiveRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, _ := r.agentSession(row)
	junk := strings.Repeat("<script>", 1000)
	for i := range 50 {
		deliver(t, h, junk, ptrU32(uint32(i+1)), struct{}{})
	}
	r.agents.FlushAudit(r.audit, r.clock.now.Add(time.Minute).UnixMilli())
	r.audit.mu.Lock()
	defer r.audit.mu.Unlock()
	if len(r.audit.rows) != 1 || r.audit.rows[0].Count != 50 || r.audit.rows[0].What != "other" {
		t.Fatalf("audit %+v", r.audit.rows)
	}
}

func TestAHoldPastItsCapIsRefused(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)

	for name, c := range map[string]struct {
		args map[string]any
		code string
	}{
		"longer than holdS":             {map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 901}, protocol.ErrCapExceeded},
		"more power than maxW":          {map[string]any{"id": lp1(), "power_w": 7401, "hold_s": 600}, protocol.ErrCapExceeded},
		"the persistent hold, hold_s 0": {map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 0}, ErrUnknownOp},
		"no end at all":                 {map[string]any{"id": lp1(), "power_w": 3000}, ErrUnknownOp},
		"32 A a phase past maxW":        {map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600, "max_amps_per_phase": 32}, ErrUnknownOp},
		"another voltage":               {map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600, "voltage": 400}, ErrUnknownOp},
		"the charger's real id":         {map[string]any{"id": "lp1", "power_w": 3000, "hold_s": 600}, ErrUnknownOp},
		"a phase mode no box has":       {map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600, "phase_mode": "2p"}, ErrUnknownOp},
	} {
		rec.reset()
		deliver(t, h, MsgCmd, nil, cmdFor("c-"+name, OpLoadpointHold, c.args, 7))
		res := body[CmdResult](t, rec.last(t, MsgCmdResult))
		if res.Error == nil || res.Error.Code != c.code {
			t.Errorf("%s: %+v %+v", name, res, res.Error)
		}
	}
	if r.lp.holdCalls != 0 {
		t.Fatalf("a refused hold reached the charger %d times", r.lp.holdCalls)
	}
	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdFor("c-ok", OpLoadpointHold, map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}, 7))
	if !rec.has(MsgCmdAck) || r.lp.holdCalls != 1 {
		t.Errorf("a hold within the caps: %s, holds %d", rec.types(), r.lp.holdCalls)
	}
}

func TestTheGrantsRateHoldsAcrossSessions(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead},
		[]string{OpLoadpointHold}, protocol.Caps{HoldS: 900, MaxW: 7400, PerMin: 2}, time.Hour, protocol.Principal{})
	args := map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}
	h, rec := r.agentSession(row)
	deliver(t, h, MsgCmd, nil, cmdFor("a", OpLoadpointHold, args, 7))
	deliver(t, h, MsgCmd, nil, cmdFor("b", OpLoadpointHold, args, 7))
	h2, rec2 := r.agentSession(row)
	deliver(t, h2, MsgCmd, nil, cmdFor("c", OpLoadpointHold, args, 7))
	res := body[CmdResult](t, rec2.last(t, MsgCmdResult))
	if res.Error == nil || res.Error.Code != protocol.ErrRateLimited || res.Error.RetryAfterMs == nil {
		t.Errorf("the third in a minute: %+v", res)
	}
	r.clock.uptimeMs += 60_000
	rec2.reset()
	deliver(t, h2, MsgCmd, nil, cmdFor("d", OpLoadpointHold, args, 7))
	if !rec2.has(MsgCmdAck) {
		t.Errorf("after the minute: %s", rec2.types())
	}
	_ = rec
}

func TestADurableChangeWaitsForTheOwner(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)

	deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{
		Op: OpSetMode, Args: map[string]any{"mode": "self_consumption"}, Why: "Cheaper tonight"})
	ok := body[map[string]any](t, rec.last(t, protocol.MsgAgentOK))
	reqID, _ := ok["reqId"].(string)
	if reqID == "" {
		t.Fatalf("no request id: %+v", ok)
	}
	if r.box.setModeCalls != 0 {
		t.Fatal("the request acted before the owner answered")
	}
	if len(r.access.notified) != 1 || r.access.notified[0] != protocol.PushAgentRequest {
		t.Errorf("owners were not told: %v", r.access.notified)
	}

	owner, orec := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentList, ptrU32(2), struct{}{})
	list := body[protocol.AgentList](t, orec.last(t, protocol.MsgAgentOK))
	if len(list.Requests) != 1 || list.Requests[0].State != protocol.RequestPending || list.Requests[0].Why != "Cheaper tonight" {
		t.Fatalf("list %+v", list.Requests)
	}
	req := list.Requests[0]
	if len(req.ReqID) != 22 {
		t.Errorf("a request id of %d characters, want 128 bits", len(req.ReqID))
	}

	// No signature, a stranger's, and one over another request: refused.
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(3), protocol.AgentConfirm{ReqID: reqID, Approve: true})
	if got := errCode(t, orec); got != protocol.ErrBadSignature {
		t.Errorf("unsigned: %s", got)
	}
	stranger := approvaltest.New()
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(4),
		stranger.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: reqID, Approve: true, AtMs: r.ms()}, req))
	if got := errCode(t, orec); got != protocol.ErrBadSignature {
		t.Errorf("a stranger's key: %s", got)
	}
	alt := req
	alt.Args = map[string]any{"mode": "idle"}
	other := r.signer.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: reqID, Approve: true, AtMs: r.ms()}, alt)
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(5), other)
	if got := errCode(t, orec); got != protocol.ErrBadSignature {
		t.Errorf("a signature over other args: %s", got)
	}
	if r.box.setModeCalls != 0 {
		t.Fatal("a refused confirmation acted")
	}

	// A viewer cannot confirm at all.
	code, _, _ := r.id.MintPairingCode(apiauth.RoleViewer, appenroll.PairingTTL)
	vk, _ := appwire.GenerateKeyPair()
	viewer, err := r.id.Authorise(vk.Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	vh, vrec := r.handler(apiauth.KindApp, viewer.DeviceID, apiauth.RoleViewer)
	deliver(t, vh, protocol.MsgAgentConfirm, ptrU32(6), protocol.AgentConfirm{ReqID: reqID, Approve: true})
	if got := errCode(t, vrec); got != ErrScopeDenied {
		t.Errorf("a viewer: %s", got)
	}

	orec.reset()
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(7),
		r.signer.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: reqID, Approve: true, AtMs: r.ms()}, req))
	if orec.has(MsgError) {
		t.Fatalf("a signed confirmation: %s", orec.types())
	}
	if r.box.setModeCalls != 1 || r.box.mode != "self_consumption" {
		t.Errorf("the confirmed change did not run: %d %s", r.box.setModeCalls, r.box.mode)
	}
	if res := body[CmdResult](t, orec.last(t, MsgCmdResult)); res.CmdID != "req:"+reqID || res.State != CmdApplied {
		t.Errorf("result %+v", res)
	}

	// The same signed confirmation again finds nothing to confirm.
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(8),
		r.signer.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: reqID, Approve: true, AtMs: r.ms()}, req))
	if got := errCode(t, orec); got != protocol.ErrNotFound {
		t.Errorf("a second confirmation: %s", got)
	}

	// The agent reads how it ended.
	rec.reset()
	deliver(t, h, protocol.MsgAgentList, ptrU32(9), struct{}{})
	mine := body[protocol.AgentList](t, rec.last(t, protocol.MsgAgentOK))
	if len(mine.Agents) != 1 || mine.Agents[0].ID != row.ID || len(mine.Requests) != 1 || mine.Requests[0].State != protocol.RequestApproved {
		t.Errorf("the agent's own list: %+v", mine)
	}
	if mine.LongestMs != nil {
		t.Errorf("an agent was told the box's limits: %v", mine.LongestMs)
	}
	if rows := r.audit.find("agent.request.run"); len(rows) != 1 || rows[0].Result != CmdApplied {
		t.Errorf("audit of the run: %+v", rows)
	}
}

// An owner's agent.list says, under longestMs, how long the box lets each
// role have, so the app offers no length the box would refuse.
func TestAgentListTellsAnOwnerTheLongestGrant(t *testing.T) {
	r := newAgentRig(t)
	owner, orec := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentList, ptrU32(1), struct{}{})
	got := body[struct {
		LongestMs map[string]int64 `cbor:"longestMs"`
	}](t, orec.last(t, protocol.MsgAgentOK)).LongestMs
	if got[protocol.RoleAgent] != protocol.AgentMaxMs || got[protocol.RoleSupport] != protocol.SupportMaxMs ||
		got[protocol.RoleRelease] != protocol.ReleaseMaxMs {
		t.Errorf("longestMs = %v", got)
	}
}

func TestARequestRunsOutAndADeclineNeedsNoSignature(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)
	ask := func() string {
		rec.reset()
		deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpSetMode, Args: map[string]any{"mode": "idle"}})
		id, _ := body[map[string]any](t, rec.last(t, protocol.MsgAgentOK))["reqId"].(string)
		return id
	}
	owner, orec := r.ownerSession()

	declined := ask()
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(2), protocol.AgentConfirm{ReqID: declined})
	if got := body[map[string]any](t, orec.last(t, protocol.MsgAgentOK))["state"]; got != protocol.RequestDeclined {
		t.Errorf("decline: %v", got)
	}

	late := ask()
	r.now = r.now.Add(13 * time.Hour)
	r.clock.uptimeMs += 13 * 60 * 60 * 1000
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(3),
		r.signer.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: late, Approve: true, AtMs: r.ms()},
			protocol.Request{ReqID: late}))
	if got := errCode(t, orec); got != protocol.ErrNotFound {
		t.Errorf("confirming after 12 hours: %s", got)
	}
	if r.box.setModeCalls != 0 {
		t.Error("an expired request ran")
	}
}

func TestASupportGrantEndsWhenItsTicketCloses(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleSupport,
		[]string{protocol.ScopeHealthRead, protocol.ScopeLogsRead}, nil, protocol.Caps{}, 14*24*time.Hour,
		protocol.Principal{Client: "Sourceful support", ClientID: protocol.SupportClientID, Ticket: "FTW-123"})
	h, rec := r.agentSession(row)

	// A call that names nobody is refused.
	deliver(t, h, protocol.MsgHealthGet, ptrU32(1), struct{}{})
	if e := body[ErrorBody](t, rec.last(t, MsgError)); e.Code != ErrUnknownOp || e.Args["missing"] != "by" {
		t.Errorf("no staff id: %+v", e)
	}
	rec.reset()
	deliverBy(t, h, protocol.MsgHealthGet, ptrU32(2), struct{}{}, "staff:s7")
	if !rec.has(protocol.MsgHealth) {
		t.Fatalf("health with a staff id: %s", rec.types())
	}
	deliverBy(t, h, protocol.MsgHealthGet, ptrU32(9), struct{}{}, "staff-7")
	if e := body[ErrorBody](t, rec.last(t, MsgError)); e.Args["field"] != "by" {
		t.Errorf("a staff id in the wrong form: %+v", e)
	}
	r.agents.FlushAudit(r.audit, r.clock.now.Add(time.Minute).UnixMilli())
	rows := r.audit.find(protocol.MsgHealthGet)
	// Folded reads have no order within a minute. Check the accepted call,
	// which may precede the rejected calls in the flushed rows.
	found := false
	for _, entry := range rows {
		if entry.Result != "ok" {
			continue
		}
		found = true
		if entry.By != "staff:s7" || entry.Principal != "FTW-123" || entry.Actor != "agent:"+row.ID || entry.Count != 1 {
			t.Errorf("audit %+v", entry)
		}
	}
	if !found {
		t.Errorf("accepted support call absent from audit: %+v", rows)
	}

	// Another agent's row is not its to revoke.
	other := r.controlAgent()
	deliverBy(t, h, protocol.MsgAgentRevoke, ptrU32(3), protocol.AgentRevoke{ID: other.ID}, "staff:s7")
	if got := errCode(t, rec); got != ErrScopeDenied {
		t.Errorf("revoking another: %s", got)
	}

	// The ticket closes: Sourceful's tooling revokes its own row.
	rec.reset()
	deliverBy(t, h, protocol.MsgAgentRevoke, ptrU32(4), protocol.AgentRevoke{ID: row.ID}, "staff:s7")
	if rec.has(MsgError) {
		t.Fatalf("self revoke: %s", rec.types())
	}
	deliverBy(t, h, protocol.MsgHealthGet, ptrU32(5), struct{}{}, "staff:s7")
	if got := errCode(t, rec); got != ErrGrantRevoked {
		t.Errorf("after the ticket closed: %s", got)
	}

	// A second ticket that is never closed runs out.
	row2, _ := r.grant(apiauth.RoleSupport, []string{protocol.ScopeHealthRead}, nil, protocol.Caps{}, time.Hour,
		protocol.Principal{ClientID: protocol.SupportClientID, Ticket: "FTW-124"})
	h2, rec2 := r.agentSession(row2)
	r.now = r.now.Add(time.Hour)
	deliverBy(t, h2, protocol.MsgHealthGet, ptrU32(1), struct{}{}, "staff:s7")
	if got := errCode(t, rec2); got != protocol.ErrGrantExpired {
		t.Errorf("after the ticket's expiry: %s", got)
	}
}

func TestLogsReachAnAgentRedacted(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLogsRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	r.logs = []redact.Line{
		{TsMs: 1, Level: "INFO", Msg: "ignored below warn"},
		{TsMs: 2, Level: "WARN", Driver: "easee", Msg: "Easee carport at 192.168.1.40 refused token=abc123", Attrs: "serial=EH123456 power_w=3000"},
		{TsMs: 3, Level: "ERROR", Msg: "owner fredrik@example.com reached https://api.example.com/x"},
	}
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgLogsGet, ptrU32(1), protocol.LogsGet{})
	logs := body[protocol.Logs](t, rec.last(t, protocol.MsgLogs))
	if len(logs.Lines) != 2 {
		t.Fatalf("lines %+v", logs.Lines)
	}
	all := ""
	for _, l := range logs.Lines {
		all += l.Src + " " + l.Msg + "\n"
	}
	for _, bad := range []string{"Easee", "easee", "192.168", "abc123", "EH123456", "fredrik", "example.com"} {
		if strings.Contains(all, bad) {
			t.Errorf("a log line still holds %q:\n%s", bad, all)
		}
	}
	if !strings.Contains(all, "power_w=3000") {
		t.Errorf("lost what helps:\n%s", all)
	}
}

// Cursors page through the ring: beforeMs back, afterMs forward, and more
// says the asked side has lines left.
func TestLogsPageWithCursors(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLogsRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	for ts := int64(1); ts <= 10; ts++ {
		r.logs = append(r.logs, redact.Line{TsMs: ts * 100, Level: "WARN", Msg: "line"})
	}
	h, rec := r.agentSession(row)
	page := func(m protocol.LogsGet) (first, last int64, n int, more bool) {
		t.Helper()
		rec.reset()
		deliver(t, h, protocol.MsgLogsGet, ptrU32(1), m)
		logs := body[protocol.Logs](t, rec.last(t, protocol.MsgLogs))
		if len(logs.Lines) == 0 {
			return 0, 0, 0, logs.More
		}
		return logs.Lines[0].Ts, logs.Lines[len(logs.Lines)-1].Ts, len(logs.Lines), logs.More
	}
	if first, last, n, more := page(protocol.LogsGet{N: 3}); first != 800 || last != 1000 || n != 3 || !more {
		t.Errorf("newest: %d..%d %d more=%v", first, last, n, more)
	}
	if first, last, n, more := page(protocol.LogsGet{N: 3, BeforeMs: 800}); first != 500 || last != 700 || n != 3 || !more {
		t.Errorf("back from 800: %d..%d %d more=%v", first, last, n, more)
	}
	if first, last, n, more := page(protocol.LogsGet{N: 3, BeforeMs: 300}); first != 100 || last != 200 || n != 2 || more {
		t.Errorf("the oldest: %d..%d %d more=%v", first, last, n, more)
	}
	if first, last, n, more := page(protocol.LogsGet{N: 3, AfterMs: 200}); first != 300 || last != 500 || n != 3 || !more {
		t.Errorf("forward from 200: %d..%d %d more=%v", first, last, n, more)
	}
	if first, last, n, more := page(protocol.LogsGet{N: 3, AfterMs: 400, BeforeMs: 700}); first != 500 || last != 600 || n != 2 || more {
		t.Errorf("between: %d..%d %d more=%v", first, last, n, more)
	}
	deliver(t, h, protocol.MsgLogsGet, ptrU32(2), protocol.LogsGet{AfterMs: 700, BeforeMs: 700})
	if got := errCode(t, rec); got != ErrUnknownOp {
		t.Errorf("a window that is empty by its own bounds: %s", got)
	}
}

// A phone with the scope reads the typed reads too.
func TestAPhoneReadsTheTypedReads(t *testing.T) {
	r := newAgentRig(t)
	owner, rec := r.ownerSession()
	deliver(t, owner, protocol.MsgLoadpointsGet, ptrU32(1), struct{}{})
	if lp := body[cloudwire.LP](t, rec.last(t, protocol.MsgLoadpoints)); len(lp.Loadpoints) != 1 {
		t.Errorf("loadpoints %+v", lp)
	}
	deliver(t, owner, protocol.MsgDriversGet, ptrU32(2), struct{}{})
	if d := body[protocol.Drivers](t, rec.last(t, protocol.MsgDrivers)); len(d.Drivers) != 3 {
		t.Errorf("drivers %+v", d)
	}
	deliver(t, owner, protocol.MsgCarsGet, ptrU32(3), struct{}{})
	if c := body[cloudwire.Cars](t, rec.last(t, protocol.MsgCars)); len(c.Cars) != 1 || c.Cars[0].ID != carOf("c1") {
		t.Errorf("cars %+v", c)
	}
}

func TestAnOwnerPhoneManagesAgents(t *testing.T) {
	r := newAgentRig(t)
	owner, rec := r.ownerSession()
	key, _ := appwire.GenerateKeyPair()
	m := protocol.AgentAdd{Key: key.Public(), Role: apiauth.RoleRelease, Scopes: []string{protocol.ScopeHealthRead},
		ExpiresAtMs: r.ms() + protocol.ReleaseMaxMs, AtMs: r.ms()}
	deliver(t, owner, protocol.MsgAgentAdd, ptrU32(1), m)
	if got := errCode(t, rec); got != protocol.ErrBadSignature {
		t.Errorf("unsigned add: %s", got)
	}
	deliver(t, owner, protocol.MsgAgentAdd, ptrU32(2), r.signer.Add(r.signedFor(), m))
	added := body[map[string]protocol.AgentRow](t, rec.last(t, protocol.MsgAgentOK))["agent"]
	if added.Role != apiauth.RoleRelease {
		t.Fatalf("added %+v", added)
	}

	// The release check reads health and nothing else.
	h, arec := r.agentSession(added)
	deliver(t, h, protocol.MsgHealthGet, ptrU32(1), struct{}{})
	health := body[Health](t, arec.last(t, protocol.MsgHealth))
	if health.Drivers["ok"] != 3 || health.Meter == nil {
		t.Errorf("health %+v", health)
	}
	deliver(t, h, MsgSub, ptrU32(2), Sub{Bucket: 512, Hz: 1})
	if got := errCode(t, arec); got != ErrScopeDenied {
		t.Errorf("release check subscribing: %s", got)
	}

	rec.reset()
	deliver(t, owner, protocol.MsgAuditGet, ptrU32(3), protocol.AuditGet{})
	page := body[protocol.AuditPage](t, rec.last(t, protocol.MsgAgentOK))
	if len(page.Rows) == 0 || page.Rows[len(page.Rows)-1].What != protocol.MsgApprovalKeySet && page.Rows[0].Actor == "" {
		t.Errorf("audit page %+v", page)
	}
	if got := r.audit.find(protocol.MsgAgentAdd); len(got) != 2 || got[0].Result != protocol.ErrBadSignature || got[1].Result != "ok" {
		t.Errorf("add audit %+v", got)
	}
}

// deliverBy is deliver with the staff id a support session puts on each
// envelope.
func deliverBy(t *testing.T, h *Handler, msgType string, id *uint32, b any, by string) {
	t.Helper()
	env, err := newEnvelope(msgType, id, b)
	if err != nil {
		t.Fatal(err)
	}
	env.By = by
	raw, _ := EncodeEnvelope(env)
	frame, err := testCodec{}.EncodeFrame(Frame{Lane: LaneControl, Bucket: bulkBucketFor(len(raw)), Envelope: env})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
}

// An agent spends command ids of its own. The same id from the owner's phone
// and from an agent is two intents, and each acts once.
func TestAnAgentHasACommandLogOfItsOwn(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	cmds := NewCmdLog()
	build := func(kind, dev, role string) (*Handler, *recorder) {
		h, rec := r.handler(kind, dev, role)
		h.cfg.Commands = cmds
		return h, rec
	}
	owner, orec := build(apiauth.KindApp, r.owner.DeviceID, apiauth.RoleOwner)
	agent, arec := build(apiauth.KindAgent, row.ID, row.Role)
	args := map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}

	deliver(t, owner, MsgCmd, nil, cmdFor("same-id", OpSetMode, map[string]any{"mode": "idle"}, 7))
	deliver(t, agent, MsgCmd, nil, cmdFor("same-id", OpLoadpointHold, args, 7))
	if !orec.has(MsgCmdAck) || !arec.has(MsgCmdAck) || r.lp.holdCalls != 1 || r.box.setModeCalls != 1 {
		t.Fatalf("owner %s, agent %s, holds %d", orec.types(), arec.types(), r.lp.holdCalls)
	}
	// A retry from the agent replays its own outcome and acts no more.
	deliver(t, agent, MsgCmd, nil, cmdFor("same-id", OpLoadpointHold, args, 7))
	if r.lp.holdCalls != 1 {
		t.Errorf("a retry acted again: %d", r.lp.holdCalls)
	}
}

// An owner's hold wins: an agent neither replaces nor clears it, and clears
// only its own. Its holds end with its grant.
func TestAnOwnersHoldWinsAndAnAgentsEndsWithItsGrant(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	agent, arec := r.agentSession(row)
	owner, orec := r.ownerSession()

	deliver(t, owner, MsgCmd, nil, cmdFor("o1", OpLoadpointHold, map[string]any{"id": "lp1", "power_w": 7000}, 7))
	if !orec.has(MsgCmdAck) || r.lp.hold.SetBy != "app:"+r.owner.DeviceID {
		t.Fatalf("owner's hold: %s %+v", orec.types(), r.lp.hold)
	}
	for name, args := range map[string]map[string]any{
		"replace": {"id": lp1(), "power_w": 3000, "hold_s": 600},
		"clear":   {"id": lp1(), "clear": true},
	} {
		arec.reset()
		deliver(t, agent, MsgCmd, nil, cmdFor("a-"+name, OpLoadpointHold, args, 7))
		if got := resultCode(t, arec); got != protocol.ErrNeedsOwner {
			t.Errorf("%s the owner's hold: %s", name, got)
		}
	}
	if !r.lp.holdSet || r.lp.hold.PowerW != 7000 {
		t.Fatalf("the owner's hold moved: %+v", r.lp.hold)
	}

	// With the owner's hold gone, the agent's own hold, then its revoke.
	r.lp.holdSet = false
	arec.reset()
	deliver(t, agent, MsgCmd, nil, cmdFor("a-own", OpLoadpointHold, map[string]any{"id": lp1(), "power_w": 3000, "hold_s": 600}, 7))
	if !arec.has(MsgCmdAck) || r.lp.hold.SetBy != "agent:"+row.ID {
		t.Fatalf("the agent's hold: %s %+v", arec.types(), r.lp.hold)
	}
	deliver(t, owner, protocol.MsgAgentRevoke, ptrU32(5), protocol.AgentRevoke{ID: row.ID})
	if r.lp.holdSet {
		t.Error("the agent's hold outlived its grant")
	}
}

// Only a support session names who made a call.
func TestAPhoneNamesNobody(t *testing.T) {
	r := newAgentRig(t)
	owner, orec := r.ownerSession()
	deliverBy(t, owner, protocol.MsgAgentList, ptrU32(1), struct{}{}, "staff:s7")
	if e := body[ErrorBody](t, orec.last(t, MsgError)); e.Code != ErrUnknownOp || e.Args["field"] != "by" {
		t.Errorf("a phone's by: %+v", e)
	}
	agentRow := r.controlAgent()
	agent, arec := r.agentSession(agentRow)
	deliverBy(t, agent, protocol.MsgLiveGet, ptrU32(2), struct{}{}, "staff:s7")
	if e := body[ErrorBody](t, arec.last(t, MsgError)); e.Code != ErrUnknownOp || e.Args["field"] != "by" {
		t.Errorf("an assistant's by: %+v", e)
	}
}

// A request's args meet the registry as they arrive, and the agent hears
// how its request ended.
func TestARequestsArgsAreCheckedAndTheAgentHearsTheEnd(t *testing.T) {
	r := newAgentRig(t)
	row := r.controlAgent()
	h, rec := r.agentSession(row)
	for name, args := range map[string]map[string]any{
		"a mode no box has":  {"mode": "free_money"},
		"a key the op lacks": {"mode": "idle", "force": true},
		"no mode":            {},
	} {
		deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpSetMode, Args: args})
		if got := errCode(t, rec); got != ErrUnknownOp {
			t.Errorf("%s: %s", name, got)
		}
	}
	rec.reset()
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(2), protocol.AgentRequest{Op: OpSetMode, Args: map[string]any{"mode": "idle"}})
	reqID, _ := body[map[string]any](t, rec.last(t, protocol.MsgAgentOK))["reqId"].(string)
	owner, _ := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(3), protocol.AgentConfirm{ReqID: reqID})
	d := body[protocol.AgentDecided](t, rec.last(t, protocol.MsgAgentDecided))
	if d.ReqID != reqID || d.State != protocol.RequestDeclined {
		t.Errorf("decided %+v", d)
	}
}

// A request meets its op's own checks as it arrives, not only when an owner
// confirms it: prefs with nothing in them, or prefs for a box without a
// planner, never reach an owner.
func TestARequestMeetsItsOpsOwnChecksAsItArrives(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeGoalsRequest}, []string{OpPlannerPrefsSet},
		protocol.Caps{PerMin: 6}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpPlannerPrefsSet, Args: map[string]any{"safety_k": 0.3}})
	if got := errCode(t, rec); got != ErrUnavailable {
		t.Errorf("prefs on a box without a planner: %s", got)
	}
	if _, _, got := plannerPrefsArgs(OpPlannerPrefsSet, map[string]any{}); got == nil || got.Code != ErrUnknownOp {
		t.Errorf("empty prefs: %+v", got)
	}
	if got := opArgsOf(t, r, OpLoadpointSoCSet, map[string]any{"id": "lp-x", "soc": 0.5}); got == nil || got.Code != ErrUnknownOp {
		t.Errorf("a charger the box lacks: %+v", got)
	}
	if got := opArgsOf(t, r, OpLoadpointHold, map[string]any{}); got == nil || got.Code != protocol.ErrOpNotGranted {
		t.Errorf("an op with no check ahead: %+v", got)
	}
	owner, orec := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentList, ptrU32(2), struct{}{})
	if got := body[protocol.AgentList](t, orec.last(t, protocol.MsgAgentOK)); len(got.Requests) != 0 {
		t.Errorf("an owner was asked: %+v", got.Requests)
	}
}

func opArgsOf(t *testing.T, r *agentRig, op string, args map[string]any) *ErrorBody {
	t.Helper()
	h, _ := r.agentSession(r.controlAgent())
	return h.opArgs(op, args)
}

// An agent never streams, not even through a sub folded into its hello: a
// snapshot's source table names the devices.
func TestAnAgentGetsNoSubThroughHello(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLiveRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoMax},
		App:   AppInfo{Build: "test", UA: "agent"},
		Sub:   &Sub{Bucket: 512, Hz: 1},
	})
	ok := body[HelloOK](t, rec.last(t, MsgHelloOK))
	if ok.Subscribed || h.Subscribed() || rec.has(MsgSnap) {
		t.Fatal("an agent's hello started a stream")
	}
}

// Approving needs a clock to trust; declining does not.
func TestApprovingNeedsAClock(t *testing.T) {
	r := newAgentRig(t)
	h, rec := r.agentSession(r.controlAgent())
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpSetMode, Args: map[string]any{"mode": "idle"}})
	reqID, _ := body[map[string]any](t, rec.last(t, protocol.MsgAgentOK))["reqId"].(string)
	owner, _ := r.ownerSession()
	r.synced = false
	var refusal *protocol.Refusal
	if _, _, err := owner.Decide(context.Background(), reqID, true); !errors.As(err, &refusal) || refusal.Code != ErrUnavailable {
		t.Fatalf("approving without a clock: %v", err)
	}
	if state, _, err := owner.Decide(context.Background(), reqID, false); err != nil || state != protocol.RequestDeclined {
		t.Errorf("declining without a clock: %s %v", state, err)
	}
}

// The typed reads name nothing: chargers and sources go by agent ids.
func TestTypedReadsCarryAgentIDs(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeHealthRead, protocol.ScopeLiveRead}, nil,
		protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgLoadpointsGet, ptrU32(1), struct{}{})
	lp := body[cloudwire.LP](t, rec.last(t, protocol.MsgLoadpoints))
	if len(lp.Loadpoints) != 1 || lp.Loadpoints[0].ID != lp1() {
		t.Errorf("loadpoints %+v", lp)
	}
	deliver(t, h, protocol.MsgDriversGet, ptrU32(2), struct{}{})
	drivers := body[protocol.Drivers](t, rec.last(t, protocol.MsgDrivers))
	for _, d := range drivers.Drivers {
		if strings.Contains(d.Src, "sungrow") || strings.Contains(d.Src, "meter") {
			t.Errorf("a driver by name: %+v", d)
		}
	}
	if len(drivers.Drivers) != 3 {
		t.Errorf("drivers %+v", drivers)
	}
}
