package appenroll

import (
	"crypto/elliptic"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/protocol"
)

// agentBox is a box with one owner phone that has registered its approval
// key, and a wall clock the test moves.
type agentBox struct {
	id     *Identity
	owner  Grant
	signer *approvaltest.Signer
	now    time.Time
	synced bool
}

func newAgentBox(t *testing.T) *agentBox {
	t.Helper()
	id, _ := newIdentity(t)
	b := &agentBox{id: id, now: time.UnixMilli(1_792_000_000_000), synced: true}
	id.SetClock(func() (time.Time, bool) { return b.now, b.synced })
	b.owner = pair(t, id, apiauth.RoleOwner)
	b.signer = approvaltest.New()
	if _, err := id.SetApprovalKey(b.owner.DeviceID, b.signer.KeySet(b.signedFor(b.owner.DeviceID), b.ms())); err != nil {
		t.Fatalf("SetApprovalKey: %v", err)
	}
	return b
}

func (b *agentBox) ms() int64 { return b.now.UnixMilli() }

func (b *agentBox) signedFor(device string) protocol.SignedFor {
	return protocol.SignedFor{BoxKey: b.id.StaticKey().Public(), DeviceID: device}
}

// readGrant is an assistant that reads live data and the plan for as long as
// an assistant may have.
func (b *agentBox) readGrant(key []byte) protocol.AgentAdd {
	return protocol.AgentAdd{
		Key: key, Role: apiauth.RoleAgent,
		Scopes:      []string{protocol.ScopeLiveRead, protocol.ScopePlanRead},
		ExpiresAtMs: b.ms() + protocol.AgentMaxMs,
		Principal:   protocol.Principal{Client: "Claude"},
		AtMs:        b.ms(),
	}
}

func (b *agentBox) add(t *testing.T, m protocol.AgentAdd) protocol.AgentRow {
	t.Helper()
	row, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, b.signer.Add(b.signedFor(b.owner.DeviceID), m))
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	return row
}

func refusalCode(err error) string {
	var r *protocol.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestAnAgentNeedsAnOwnersSignature(t *testing.T) {
	b := newAgentBox(t)
	key := appKey(t)
	m := b.readGrant(key)

	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, m); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("unsigned: %v", err)
	}
	other := approvaltest.New()
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, other.Add(b.signedFor(b.owner.DeviceID), m)); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("signed by another key: %v", err)
	}
	signed := b.signer.Add(b.signedFor(b.owner.DeviceID), m)
	wider := signed
	wider.Scopes = []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead, protocol.ScopePlanRead}
	wider.Caps = protocol.Caps{HoldS: 600, MaxW: 3000}
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, wider); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("a signature for reading passed for control: %v", err)
	}
	stale := b.signer.Add(b.signedFor(b.owner.DeviceID), func() protocol.AgentAdd { m := m; m.AtMs -= 11 * 60 * 1000; return m }())
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, stale); refusalCode(err) != protocol.ErrClockSkew {
		t.Errorf("an old signature: %v", err)
	}

	row, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, signed)
	if err != nil {
		t.Fatal(err)
	}
	if row.Role != apiauth.RoleAgent || row.ApprovedBy != "app:"+b.owner.DeviceID || row.Caps.PerMin != protocol.PerMinDefault {
		t.Errorf("row %+v", row)
	}

	// The same signed bytes, sent again after a revoke, add nothing.
	if _, err := b.id.Revoke(row.ID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, signed); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("a replayed approval: %v", err)
	}
}

func TestOnlyAnOwnerWithAKeyApproves(t *testing.T) {
	b := newAgentBox(t)
	viewer := pair(t, b.id, apiauth.RoleViewer)
	if _, err := b.id.SetApprovalKey(viewer.DeviceID, b.signer.KeySet(b.signedFor(viewer.DeviceID), b.ms())); refusalCode(err) != protocol.ErrScopeDenied {
		t.Errorf("a viewer registered a key: %v", err)
	}
	if _, err := b.id.AddAgent(Approver{DeviceID: viewer.DeviceID}, b.signer.Add(b.signedFor(viewer.DeviceID), b.readGrant(appKey(t)))); refusalCode(err) != protocol.ErrScopeDenied {
		t.Errorf("a viewer approved: %v", err)
	}
	second := pair(t, b.id, apiauth.RoleOwner)
	if _, err := b.id.AddAgent(Approver{DeviceID: second.DeviceID}, b.signer.Add(b.signedFor(second.DeviceID), b.readGrant(appKey(t)))); refusalCode(err) != protocol.ErrNoApprovalKey {
		t.Errorf("an owner with no key approved: %v", err)
	}
	if _, err := b.id.AddAgent(AtTheBoxPage, b.readGrant(appKey(t))); err != nil {
		t.Errorf("the box page needs no signature: %v", err)
	}
}

func TestAGrantStaysWithinTheBoxsLimits(t *testing.T) {
	b := newAgentBox(t)
	cases := map[string]struct {
		edit func(*protocol.AgentAdd)
		code string
	}{
		"hold past its cap": {func(m *protocol.AgentAdd) {
			m.Scopes = []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead}
			m.Ops = []string{protocol.OpLoadpointHold}
			m.Caps = protocol.Caps{HoldS: protocol.HoldSMax + 1, MaxW: 3000}
		}, protocol.ErrCapExceeded},
		"a hold scope with no power cap": {func(m *protocol.AgentAdd) {
			m.Scopes = []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead}
			m.Caps = protocol.Caps{HoldS: 600}
		}, protocol.ErrCapExceeded},
		"longer than an assistant may have": {func(m *protocol.AgentAdd) { m.ExpiresAtMs = b.ms() + protocol.AgentMaxMs + 1 }, protocol.ErrCapExceeded},
		"already over":                      {func(m *protocol.AgentAdd) { m.ExpiresAtMs = b.ms() }, protocol.ErrUnknownOp},
		"an op its scopes do not open": {func(m *protocol.AgentAdd) {
			m.Ops = []string{protocol.OpLoadpointHold}
		}, protocol.ErrOpNotGranted},
		"an op no agent may name": {func(m *protocol.AgentAdd) {
			m.Scopes = []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead}
			m.Caps = protocol.Caps{HoldS: 600, MaxW: 3000}
			m.Ops = []string{"members.invite"}
		}, protocol.ErrOpNotGranted},
		"a scope beyond the role": {func(m *protocol.AgentAdd) {
			m.Scopes = []string{protocol.ScopeLiveRead, protocol.ScopeModeWrite}
		}, protocol.ErrScopeDenied},
		"unsorted scopes": {func(m *protocol.AgentAdd) {
			m.Scopes = []string{protocol.ScopePlanRead, protocol.ScopeLiveRead}
		}, protocol.ErrUnknownOp},
		"support without a ticket": {func(m *protocol.AgentAdd) {
			m.Role = apiauth.RoleSupport
			m.ExpiresAtMs = b.ms() + protocol.SupportMaxMs
			m.Principal.ClientID = protocol.SupportClientID
		}, protocol.ErrUnknownOp},
		"a release check that reads more than health": {func(m *protocol.AgentAdd) {
			m.Role = apiauth.RoleRelease
		}, protocol.ErrScopeDenied},
		"a phone's role": {func(m *protocol.AgentAdd) { m.Role = apiauth.RoleOwner }, protocol.ErrUnknownOp},
	}
	for name, c := range cases {
		m := b.readGrant(appKey(t))
		c.edit(&m)
		if _, err := b.id.AddAgent(AtTheBoxPage, m); refusalCode(err) != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}
	if len(b.id.Agents()) != 0 {
		t.Errorf("a refused grant left a row: %+v", b.id.Agents())
	}
}

// An assistant may have a year, renewed for a year from the day it renews,
// and not a millisecond more. The refusal names the latest end the box takes,
// so a client knows what it may ask for.
func TestAnAssistantMayHaveAYearAndNoMore(t *testing.T) {
	b := newAgentBox(t)
	year := (365 * 24 * time.Hour).Milliseconds()
	over := func(err error) bool {
		var r *protocol.Refusal
		return errors.As(err, &r) && r.Code == protocol.ErrCapExceeded &&
			r.Args["cap"] == "expiresAtMs" && r.Args["max"] == b.ms()+year
	}
	m := b.readGrant(appKey(t))
	m.ExpiresAtMs = b.ms() + year + 1
	if _, err := b.id.AddAgent(AtTheBoxPage, m); !over(err) {
		t.Fatalf("a year and a millisecond: %v", err)
	}
	m.ExpiresAtMs = b.ms() + year
	row := b.add(t, m)

	b.now = b.now.Add(200 * 24 * time.Hour)
	by := Approver{DeviceID: b.owner.DeviceID}
	renew := protocol.AgentWiden{ID: row.ID, Epoch: row.Epoch, Scopes: row.Scopes, Ops: row.Ops, Caps: row.Caps,
		ExpiresAtMs: b.ms() + year + 1, AtMs: b.ms()}
	if _, err := b.id.WidenAgent(by, b.signer.Widen(b.signedFor(b.owner.DeviceID), renew, row.Key)); !over(err) {
		t.Fatalf("renewed past a year: %v", err)
	}
	renew.ExpiresAtMs = b.ms() + year
	renewed, err := b.id.WidenAgent(by, b.signer.Widen(b.signedFor(b.owner.DeviceID), renew, row.Key))
	if err != nil || renewed.ExpiresAtMs != b.ms()+year {
		t.Fatalf("renewed for a year: %+v %v", renewed, err)
	}
}

// The limits agent.list tells owners are the ones the box holds a grant to.
func TestTheBoxTellsTheLimitsItHolds(t *testing.T) {
	day := (24 * time.Hour).Milliseconds()
	want := map[string]int64{apiauth.RoleAgent: 365 * day, apiauth.RoleSupport: 14 * day, apiauth.RoleRelease: 90 * day}
	got := protocol.LongestMs()
	if len(got) != len(want) {
		t.Fatalf("LongestMs = %v", got)
	}
	for role, ms := range want {
		if got[role] != ms || longest(role) != ms {
			t.Errorf("%s: told %d, holds %d, want %d", role, got[role], longest(role), ms)
		}
	}
}

func TestNoClockNoAgents(t *testing.T) {
	b := newAgentBox(t)
	key := appKey(t)
	row := b.add(t, b.readGrant(key))

	b.synced = false
	if _, err := b.id.AddAgent(AtTheBoxPage, b.readGrant(appKey(t))); refusalCode(err) != protocol.ErrUnavailable {
		t.Errorf("added without a clock: %v", err)
	}
	if _, _, err := b.id.CheckTunnel(key, nil); !errors.Is(err, ErrNoPairing) {
		t.Errorf("admitted without a clock: %v", err)
	}
	g, ok := b.id.GrantFor(row.ID)
	if !ok || g.Agent == nil || !g.Agent.NoClock {
		t.Errorf("grant without a clock: %+v %v", g, ok)
	}
	if gone := b.id.ExpireAgents(); len(gone) != 0 {
		t.Errorf("swept without a clock: %+v", gone)
	}
}

func TestAnAgentComesOnlyThroughTheTunnel(t *testing.T) {
	b := newAgentBox(t)
	key := appKey(t)
	row := b.add(t, b.readGrant(key))

	if _, _, err := b.id.Check(key, nil); !errors.Is(err, ErrNoPairing) {
		t.Errorf("the relay admitted an agent: %v", err)
	}
	if _, _, err := b.id.CheckWatch(key); !errors.Is(err, ErrNoPairing) {
		t.Errorf("the watch door admitted an agent: %v", err)
	}
	grant, commit, err := b.id.CheckTunnel(key, nil)
	if err != nil || grant.Agent == nil || grant.DeviceID != row.ID {
		t.Fatalf("the tunnel refused an agent: %+v %v", grant, err)
	}
	if _, err := commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAnExpiredAgentIsRefusedAndSwept(t *testing.T) {
	b := newAgentBox(t)
	key := appKey(t)
	m := b.readGrant(key)
	m.ExpiresAtMs = b.ms() + 60_000
	row := b.add(t, m)

	b.now = b.now.Add(time.Minute)
	if _, _, err := b.id.CheckTunnel(key, nil); !errors.Is(err, ErrNoPairing) {
		t.Errorf("admitted after expiry: %v", err)
	}
	if g, ok := b.id.GrantFor(row.ID); !ok || !g.Agent.Expired {
		t.Errorf("grant after expiry: %+v %v", g, ok)
	}
	for _, e := range b.id.Roster() {
		if e.ID == row.ID {
			t.Error("the roster still lists an expired agent")
		}
	}
	gone := b.id.ExpireAgents()
	if len(gone) != 1 || gone[0].ID != row.ID {
		t.Fatalf("swept %+v", gone)
	}
	if _, ok := b.id.GrantFor(row.ID); ok {
		t.Error("an expired agent outlived the sweep")
	}
}

func TestARevokeBitesAtOnce(t *testing.T) {
	b := newAgentBox(t)
	row := b.add(t, b.readGrant(appKey(t)))
	if _, ok := b.id.GrantFor(row.ID); !ok {
		t.Fatal("no grant")
	}
	if _, err := b.id.Revoke(row.ID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.id.GrantFor(row.ID); ok {
		t.Error("a revoked agent still has a grant")
	}
}

func TestAnAgentHoldsOnlyWhatItsApproverHoldsNow(t *testing.T) {
	b := newAgentBox(t)
	second := pair(t, b.id, apiauth.RoleOwner)
	row := b.add(t, b.readGrant(appKey(t)))

	if err := b.id.SetRole(b.owner.DeviceID, apiauth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	g, ok := b.id.GrantFor(row.ID)
	if !ok || g.ParentRole != apiauth.RoleViewer {
		t.Errorf("after the approver was demoted: %+v", g)
	}
	if err := b.id.SetRole(row.ID, apiauth.RoleOwner); !errors.Is(err, ErrAgentRow) {
		t.Errorf("an agent row took a phone's role: %v", err)
	}

	if _, err := b.id.Revoke(b.owner.DeviceID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.id.GrantFor(row.ID); ok {
		t.Error("an agent outlived the phone that approved it")
	}
	_ = second
}

func TestASupportGrantNamesItsTicket(t *testing.T) {
	b := newAgentBox(t)
	m := b.readGrant(appKey(t))
	m.Role = apiauth.RoleSupport
	m.Principal = protocol.Principal{Client: "Sourceful support", Ticket: "FTW-123"}
	m.ExpiresAtMs = b.ms() + protocol.SupportMaxMs
	if _, err := b.id.AddAgent(AtTheBoxPage, m); refusalCode(err) != protocol.ErrUnknownOp {
		t.Errorf("support from another client: %v", err)
	}
	m.Principal.ClientID = protocol.SupportClientID
	m.ExpiresAtMs = b.ms() + protocol.SupportMaxMs + 1
	if _, err := b.id.AddAgent(AtTheBoxPage, m); refusalCode(err) != protocol.ErrCapExceeded {
		t.Errorf("support past 14 days: %v", err)
	}
	m.ExpiresAtMs = b.ms() + protocol.SupportMaxMs
	row := b.add(t, m)
	if row.Principal.Ticket != "FTW-123" {
		t.Errorf("row %+v", row)
	}
}

func TestWideningIsSignedAndFollowsTheEpoch(t *testing.T) {
	b := newAgentBox(t)
	row := b.add(t, b.readGrant(appKey(t)))
	w := protocol.AgentWiden{
		ID: row.ID, Epoch: row.Epoch,
		Scopes:      []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead, protocol.ScopePlanRead},
		Ops:         []string{protocol.OpLoadpointHold},
		Caps:        protocol.Caps{HoldS: 900, MaxW: 7400},
		ExpiresAtMs: row.ExpiresAtMs, AtMs: b.ms(),
	}
	by := Approver{DeviceID: b.owner.DeviceID}
	if _, err := b.id.WidenAgent(by, w); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("unsigned widen: %v", err)
	}
	stale := w
	stale.Epoch++
	if _, err := b.id.WidenAgent(by, b.signer.Widen(b.signedFor(b.owner.DeviceID), stale, row.Key)); refusalCode(err) != protocol.ErrConflict {
		t.Errorf("stale epoch: %v", err)
	}
	other := w
	if _, err := b.id.WidenAgent(by, b.signer.Widen(b.signedFor(b.owner.DeviceID), other, appKey(t))); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("a signature for another row's key: %v", err)
	}
	wider, err := b.id.WidenAgent(by, b.signer.Widen(b.signedFor(b.owner.DeviceID), w, row.Key))
	if err != nil {
		t.Fatal(err)
	}
	if wider.Epoch != row.Epoch+1 || len(wider.Ops) != 1 {
		t.Errorf("widened %+v", wider)
	}
	if _, err := b.id.WidenAgent(by, protocol.AgentWiden{ID: "nobody00"}); refusalCode(err) != protocol.ErrNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

func TestAgentsAndKeysSurviveARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	now := time.UnixMilli(1_792_000_000_000)
	id.SetClock(func() (time.Time, bool) { return now, true })
	owner := pair(t, id, apiauth.RoleOwner)
	signer := approvaltest.New()
	f := protocol.SignedFor{BoxKey: id.StaticKey().Public(), DeviceID: owner.DeviceID}
	if _, err := id.SetApprovalKey(owner.DeviceID, signer.KeySet(f, now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	key := appKey(t)
	m := protocol.AgentAdd{Key: key, Role: apiauth.RoleAgent, Scopes: []string{protocol.ScopeLiveRead},
		ExpiresAtMs: now.UnixMilli() + 1000, AtMs: now.UnixMilli()}
	row, err := id.AddAgent(Approver{DeviceID: owner.DeviceID}, signer.Add(f, m))
	if err != nil {
		t.Fatal(err)
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	again.SetClock(func() (time.Time, bool) { return now, true })
	got, ok := again.Agent(row.ID)
	if !ok || got.ExpiresAtMs != row.ExpiresAtMs || got.ApprovedBy != row.ApprovedBy {
		t.Fatalf("after a restart: %+v %v", got, ok)
	}
	if keys := again.ApprovalKeys(); len(keys) != 1 || keys[0].DeviceID != owner.DeviceID {
		t.Errorf("keys %+v", keys)
	}
	for _, d := range again.Devices() {
		if d.ID == row.ID {
			t.Error("an agent row is listed among the phones")
		}
	}
	// The key still approves after the restart.
	m2 := m
	m2.Key = appKey(t)
	if _, err := again.AddAgent(Approver{DeviceID: owner.DeviceID}, signer.Add(f, m2)); err != nil {
		t.Errorf("approving after a restart: %v", err)
	}
}

// Replacing an approval key needs the old one, or the box page.
func TestReplacingAnApprovalKeyNeedsTheOldOne(t *testing.T) {
	b := newAgentBox(t)
	f := b.signedFor(b.owner.DeviceID)
	thief := approvaltest.New()
	if _, err := b.id.SetApprovalKey(b.owner.DeviceID, thief.KeySet(f, b.ms())); refusalCode(err) != protocol.ErrBadSignature {
		t.Fatalf("a new key without the old one's signature: %v", err)
	}
	if _, err := b.id.SetApprovalKey(b.owner.DeviceID, thief.Replace(f, b.ms(), approvaltest.New())); refusalCode(err) != protocol.ErrBadSignature {
		t.Fatalf("signed by a stranger as the old key: %v", err)
	}
	next := approvaltest.New()
	if _, err := b.id.SetApprovalKey(b.owner.DeviceID, next.Replace(f, b.ms(), b.signer)); err != nil {
		t.Fatalf("a replacement the old key signed: %v", err)
	}
	// The old key approves nothing now.
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, b.signer.Add(f, b.readGrant(appKey(t)))); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("the replaced key still approves: %v", err)
	}
	lost := approvaltest.New()
	if _, err := b.id.ReplaceApprovalKey(b.owner.DeviceID, lost.Pub()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, lost.Add(f, b.readGrant(appKey(t)))); err != nil {
		t.Errorf("the key set on the box page: %v", err)
	}
}

// The replay memory keys on the signed bytes, so the other form of the same
// signature is refused too.
func TestTheSameBytesAreTakenOnce(t *testing.T) {
	b := newAgentBox(t)
	f := b.signedFor(b.owner.DeviceID)
	m := b.signer.Add(f, b.readGrant(appKey(t)))
	row, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.id.Revoke(row.ID, OverASession); err != nil {
		t.Fatal(err)
	}
	// (r, N - s) verifies as well as (r, s).
	flipped := m
	flipped.Sig = highS(m.Sig)
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, flipped); refusalCode(err) != protocol.ErrBadSignature {
		t.Errorf("the other form of the same signature: %v", err)
	}
}

func highS(sig []byte) []byte {
	n := elliptic.P256().Params().N
	s := new(big.Int).SetBytes(sig[32:])
	s.Sub(n, s)
	out := append([]byte(nil), sig[:32]...)
	return append(out, s.FillBytes(make([]byte, 32))...)
}

// An approval signed before the box last started is refused: the memory of
// what it took starts then.
func TestAnApprovalFromBeforeTheStartIsRefused(t *testing.T) {
	b := newAgentBox(t)
	b.now = b.now.Add(time.Hour)
	f := b.signedFor(b.owner.DeviceID)
	m := b.readGrant(appKey(t))
	m.AtMs = b.ms() - 61*60*1000
	if _, err := b.id.AddAgent(Approver{DeviceID: b.owner.DeviceID}, b.signer.Add(f, m)); refusalCode(err) != protocol.ErrClockSkew {
		t.Errorf("from before the start: %v", err)
	}
}

func TestOneIDOneRow(t *testing.T) {
	b := newAgentBox(t)
	row := b.add(t, b.readGrant(appKey(t)))
	twin := append([]byte(nil), row.Key...)
	twin[31] ^= 0xff // the same first eight characters, another key
	if _, err := b.id.AddAgent(AtTheBoxPage, b.readGrant(twin)); refusalCode(err) != protocol.ErrUnknownOp {
		t.Errorf("a second row under one id: %v", err)
	}
}

func TestOwnersHearOfANewOwner(t *testing.T) {
	b := newAgentBox(t)
	heard := 0
	b.id.OnOwnerAdded(func() { heard++ })
	pair(t, b.id, apiauth.RoleViewer)
	pair(t, b.id, apiauth.RoleOwner)
	if heard != 1 {
		t.Errorf("heard %d", heard)
	}
}
