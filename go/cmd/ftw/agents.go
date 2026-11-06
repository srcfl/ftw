package main

// Agents: an owner's grants to an assistant, Sourceful support or the release
// check (docs/agents.md). Enrolment holds the grants and checks every
// approval; the app session enforces them on every call; this file wires the
// clock, the audit log, the log lines an agent may read, the configuration
// it may read, the owners' push and the sweep that removes expired rows.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/redact"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// agentWall is the wall clock agents expire by, and whether it can be
// trusted: NTP has set it, and it has not gone back below the highest time
// the box has seen. Setting the clock back must not revive an expired grant.
var agentWall = &wallMark{}

type wallMark struct {
	mu     sync.Mutex
	highMs int64
	saved  int64
}

const wallHighKey = "agents_wall_high_ms"

func agentClock() (time.Time, bool) { return agentWall.now() }

func (w *wallMark) now() (time.Time, bool) {
	now := time.Now()
	ok := cloudlink.ClockSynced()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case ok && now.UnixMilli() < w.highMs:
		return now, false
	case ok:
		w.highMs = now.UnixMilli()
	}
	return now, ok
}

// load reads the mark the box kept, so a clock set back across a restart
// counts as none too.
func (w *wallMark) load(st *state.Store) {
	if st == nil {
		return
	}
	if raw, ok := st.LoadConfig(wallHighKey); ok {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			w.mu.Lock()
			w.highMs = max(w.highMs, v)
			w.saved = v
			w.mu.Unlock()
		}
	}
}

// reset makes now the mark, lower or not, and keeps it.
func (w *wallMark) reset(st *state.Store, nowMs int64) error {
	w.mu.Lock()
	w.highMs, w.saved = nowMs, nowMs
	w.mu.Unlock()
	if st == nil {
		return nil
	}
	return st.SaveConfig(wallHighKey, strconv.FormatInt(nowMs, 10))
}

// mark is the highest wall time the box has seen.
func (w *wallMark) mark() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.highMs
}

// save keeps the mark, at most once a sweep.
func (w *wallMark) save(st *state.Store) {
	if st == nil {
		return
	}
	w.mu.Lock()
	high, saved := w.highMs, w.saved
	w.mu.Unlock()
	if high <= saved {
		return
	}
	if err := st.SaveConfig(wallHighKey, strconv.FormatInt(high, 10)); err == nil {
		w.mu.Lock()
		w.saved = high
		w.mu.Unlock()
	}
}

// agentPorts is what agents need from the rest of the box.
type agentPorts struct {
	logs  *telemetry.LogRing
	cfg   *config.Config
	cfgMu *sync.RWMutex
	st    *state.Store
	// notices is bound to the notification service once it exists.
	notices *cloudNotices
	// home is FTW Cloud's home id for this box, bound once the link exists.
	home func() string
	// notice sends FTW Cloud a security notice for the owners' phones,
	// bound once the link exists. It goes copy or not; the link decides
	// whether client goes with it.
	notice func(kind, client string) bool

	// Filled in by startAppLink.
	access *appuplink.AgentAccess
	state  *appproto.AgentState
	audit  appproto.AuditLog
	// boxPage builds a handler for the box page's own decisions.
	boxPage func(by string) (*appproto.Handler, error)
	// key cuts every id an agent sees, and the redactor's.
	key []byte
	// cars are the owners' cars, whose names, VINs, identifiers and
	// sources the redactor takes out. Nil on a box that keeps none.
	cars *cars.Runtime
	// warned holds the rows already told they expire within a day.
	warnMu sync.Mutex
	warned map[string]bool
}

// push tells every owner about a change to who may reach the box: as a
// notice through FTW Cloud, and through the box's own transports, whose
// history also counts the notice FTW Cloud took. client names the client
// given access, on agent.added.
func (p *agentPorts) push(kind, client string) {
	viaCloud := p.notice != nil && p.notice(kind, client)
	if p.notices == nil {
		return
	}
	if svc := p.notices.svc.Load(); svc != nil {
		svc.NotifyAgents(kind, viaCloud)
	}
}

// agentKey is the key every id an agent sees is cut with, derived from the
// box's own secret, so ids are stable on this box and mean nothing on
// another or in the readable copy.
func agentKey(enroll *appenroll.Identity) []byte { return enroll.DeriveKey("ftw.agent.ids.v1") }

// redactor scrubs with the agent key and every name the configuration
// knows. Built per read, so a renamed driver is scrubbed at once.
func (p *agentPorts) redactor() *redact.Redactor {
	return redact.New(p.key, p.names())
}

// names is everything the configuration calls something.
func (p *agentPorts) names() []string {
	var out []string
	if p.cars != nil {
		// The cars an owner named on the box or in the app: what names
		// them, and the drivers that report them.
		for _, c := range p.cars.Cars() {
			out = append(out, c.Name, c.VIN)
			out = append(out, c.Identifiers...)
			out = append(out, c.Sources...)
		}
	}
	if p.cfg == nil {
		return out
	}
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	out = append(out, p.cfg.Site.Name)
	for _, d := range p.cfg.Drivers {
		out = append(out, d.Name)
	}
	for _, lp := range p.cfg.Loadpoints {
		out = append(out, lp.ID, lp.DriverName)
	}
	for _, v := range p.cfg.Vehicles {
		out = append(out, v.ID, v.Name)
		out = append(out, v.Identifiers...)
	}
	return out
}

// configDoc is the configuration as plain JSON values, for redaction.
func (p *agentPorts) configDoc() (any, error) {
	p.cfgMu.RLock()
	raw, err := json.Marshal(p.cfg)
	p.cfgMu.RUnlock()
	if err != nil {
		return nil, err
	}
	var doc any
	return doc, json.Unmarshal(raw, &doc)
}

// ringLogs is the log ring as agents read it.
type ringLogs struct{ ring *telemetry.LogRing }

func (r ringLogs) Recent(n int) []redact.Line {
	entries := r.ring.RecentGlobal(n)
	out := make([]redact.Line, 0, len(entries))
	for _, e := range entries {
		out = append(out, redact.Line{TsMs: e.TS.UnixMilli(), Level: e.Level, Msg: e.Msg, Driver: e.Driver, Attrs: e.Attrs})
	}
	return out
}

// stateAudit writes audit rows to the state store on a goroutine of its
// own, so a call never waits on the disk. An agent's row may be dropped when
// the queue is full, and is counted; an owner's or the box's never is.
type stateAudit struct {
	st      *state.Store
	rows    chan protocol.AuditRow
	owners  chan protocol.AuditRow
	dropped atomic.Int64
}

func newStateAudit(ctx context.Context, st *state.Store) *stateAudit {
	a := &stateAudit{st: st, rows: make(chan protocol.AuditRow, 256), owners: make(chan protocol.AuditRow, 1024)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-a.owners:
				a.write(r)
			case r := <-a.rows:
				a.write(r)
			}
		}
	}()
	return a
}

func (a *stateAudit) write(r protocol.AuditRow) {
	if err := a.st.AppendAgentAudit(state.AgentAuditRow{AtMs: r.AtMs, Actor: r.Actor, Role: r.Role,
		Principal: r.Principal, By: r.By, What: r.What, Target: r.Target, Result: r.Result,
		Count: int64(r.Count)}); err != nil {
		slog.Warn("could not write the agent audit log", "err", err)
	}
}

func (a *stateAudit) Append(r protocol.AuditRow) {
	if !strings.HasPrefix(r.Actor, apiauth.KindAgent+":") {
		select {
		case a.owners <- r:
		default:
			// Full: write it now rather than lose an owner's row.
			a.write(r)
		}
		return
	}
	select {
	case a.rows <- r:
	default:
		if n := a.dropped.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("agent audit queue full; an agent's rows dropped", "dropped", n)
		}
	}
}

func (a *stateAudit) Page(beforeSeq uint64, n int) ([]protocol.AuditRow, bool, error) {
	rows, more, err := a.st.AgentAudit(int64(beforeSeq), n)
	out := make([]protocol.AuditRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.AuditRow{Seq: uint64(r.Seq), AtMs: r.AtMs, Actor: r.Actor, Role: r.Role,
			Principal: r.Principal, By: r.By, What: r.What, Target: r.Target, Result: r.Result,
			Count: uint64(max(r.Count, 0))})
	}
	return out, more, err
}

// sweepAgents removes expired agent rows every half minute and ends their
// sessions. Each call checks the clock on its own; this keeps the list and
// the roster short.
func sweepAgents(ctx context.Context, p *agentPorts) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sweepOnce(p)
	}
}

// agentGone finishes an agent row a removal took: its holds end, its
// requests and its minute are forgotten, and the audit log says who.
func (p *agentPorts) agentGone(id, by string) {
	if p.access == nil {
		return
	}
	p.access.EndHolds(id)
	p.state.Forget(id)
	if p.audit != nil {
		now, ok := agentClock()
		at := now.UnixMilli()
		if !ok {
			at = 0
		}
		p.audit.Append(protocol.AuditRow{AtMs: at, Actor: by, What: "agent.remove", Target: id, Result: "ok"})
	}
}

// sweepOnce removes expired rows, warns owners of grants that end within a
// day, writes the reads folded into minutes that have ended, and keeps the
// clock's mark.
func sweepOnce(p *agentPorts) {
	now, synced := agentClock()
	for _, gone := range p.access.ExpireAgents() {
		p.state.Forget(gone.ID)
		if p.audit != nil {
			p.audit.Append(protocol.AuditRow{AtMs: now.UnixMilli(), Actor: "box", What: "agent.expire",
				Target: gone.ID, Result: protocol.RequestExpired})
		}
	}
	if synced {
		p.warnExpiring(now.UnixMilli())
	}
	p.state.FlushAudit(p.audit, now.UnixMilli())
	p.state.Sweep(now.UnixMilli())
	agentWall.save(p.st)
}

// warnExpiring pushes agent.expiring once for each grant that ends within a
// day.
func (p *agentPorts) warnExpiring(nowMs int64) {
	p.warnMu.Lock()
	defer p.warnMu.Unlock()
	if p.warned == nil {
		p.warned = map[string]bool{}
	}
	for _, row := range p.access.Agents() {
		key := row.ID + ":" + strconv.FormatInt(row.ExpiresAtMs, 10)
		if row.ExpiresAtMs-nowMs <= 24*60*60*1000 && !p.warned[key] {
			p.warned[key] = true
			p.push(protocol.PushAgentExpiring, "")
		}
	}
}

// views builds the typed reads' bodies from the same source as FTW Cloud's
// copy, with the agent key's ids.
type agentViews struct {
	site *cloudSite
	cars *cars.Runtime
}

func (v agentViews) Live(key []byte) cloudwire.Live {
	return cloudlink.ProjectLive(key, time.Since(v.site.site.started).Milliseconds(), v.site.Live())
}

func (v agentViews) Loadpoints(key []byte) (cloudwire.LP, bool) {
	lp, ok := v.site.Loadpoints()
	if !ok {
		return cloudwire.LP{}, false
	}
	return cloudlink.ProjectLP(key, lp), true
}

func (v agentViews) Cars(key []byte) (cloudwire.Cars, bool) {
	if v.cars == nil {
		return cloudwire.Cars{}, false
	}
	return cloudlink.ProjectCars(key, v.cars.Cars()), true
}

// endAgentHolds releases every hold an agent set, by its subject.
func endAgentHolds(lp appproto.Loadpoints) func(subject string) {
	return func(subject string) {
		if lp == nil {
			return
		}
		now := time.Now()
		for _, r := range lp.Readings() {
			if lp.ClearHoldIfOwn(r.ID, subject, now) {
				slog.Info("released an agent's hold", "loadpoint", r.ID, "agent", subject)
			}
		}
	}
}

// boxPageAgents is the box page's view of agents (api.AgentAdmin).
type boxPageAgents struct{ p *agentPorts }

func (b boxPageAgents) Agents() []protocol.AgentRow { return b.p.access.Agents() }

func (b boxPageAgents) ApprovalKeys() []protocol.ApprovalKeyInfo { return b.p.access.ApprovalKeys() }

func (b boxPageAgents) Requests() []api.AgentRequest {
	h, err := b.p.boxPage("box")
	if err != nil {
		return nil
	}
	defer h.Close()
	reqs := h.Requests()
	out := make([]api.AgentRequest, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, api.AgentRequest{Request: r, Names: h.ArgNames(r)})
	}
	return out
}

func (b boxPageAgents) Home() string {
	if b.p.home == nil {
		return ""
	}
	return b.p.home()
}

func (b boxPageAgents) Wall() (int64, bool) { return b.p.access.Wall() }

// OpenOffer checks a signed offer against the offer keys this build pins.
func (b boxPageAgents) OpenOffer(raw string) (protocol.GrantOffer, error) {
	signed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return protocol.GrantOffer{}, protocol.ErrOfferShape
	}
	var so protocol.SignedOffer
	if err := protocol.UnmarshalSigned(signed, &so); err != nil {
		return protocol.GrantOffer{}, protocol.ErrOfferShape
	}
	now, synced := b.p.access.Wall()
	if !synced {
		return protocol.GrantOffer{}, protocol.ErrOfferExpired
	}
	return protocol.OpenOffer(cloudlink.ParseOfferKeys(cloudlink.OfferKeys), so, now)
}

// Facts tell the box page whether it can take a grant now, before the owner
// spends a code that works once.
func (b boxPageAgents) Facts() api.AgentFacts {
	now, ok := agentClock()
	f := api.AgentFacts{
		Offers:    len(cloudlink.ParseOfferKeys(cloudlink.OfferKeys)) > 0,
		Lookup:    codeLookup(cloudlink.Endpoint),
		NowMs:     now.UnixMilli(),
		MarkMs:    agentWall.mark(),
		LongestMs: protocol.LongestMs(),
	}
	switch {
	case ok:
		f.Clock = api.ClockOK
	case cloudlink.ClockSynced() && f.NowMs < f.MarkMs:
		f.Clock = api.ClockBehind
	default:
		f.Clock = api.ClockUnsynced
	}
	return f
}

// codeLookup is FTW Cloud's code lookup on the host this build links to.
func codeLookup(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return scheme + "://" + u.Host + "/oauth/requests"
}

// ReplaceApprovalKey sets a phone's approval key from the box page.
func (b boxPageAgents) ReplaceApprovalKey(by, deviceID string, pub []byte) error {
	_, err := b.p.access.Enroll.ReplaceApprovalKey(deviceID, pub)
	b.record(by, protocol.MsgApprovalKeySet, deviceID, err)
	if err == nil {
		b.p.push(protocol.PushApprovalKeySet, "")
	}
	return err
}

func (b boxPageAgents) record(by, what, target string, err error) {
	if b.p.audit == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "refused"
		if r, ok := err.(*protocol.Refusal); ok {
			result = r.Code
		}
	}
	now, ok := b.p.access.Wall()
	if !ok {
		now = 0
	}
	b.p.audit.Append(protocol.AuditRow{AtMs: now, Actor: by, Role: apiauth.RoleOwner, What: what, Target: target, Result: result})
}

func (b boxPageAgents) AddAgent(by string, m protocol.AgentAdd) (protocol.AgentRow, error) {
	row, err := b.p.access.Enroll.AddAgent(appenroll.AtTheBoxPage, m)
	b.record(by, protocol.MsgAgentAdd, row.ID, err)
	if err == nil {
		b.p.push(protocol.PushAgentAdded, row.Principal.Client)
	}
	return row, err
}

func (b boxPageAgents) WidenAgent(by string, m protocol.AgentWiden) (protocol.AgentRow, error) {
	row, err := b.p.access.Enroll.WidenAgent(appenroll.AtTheBoxPage, m)
	b.record(by, protocol.MsgAgentWiden, m.ID, err)
	if err == nil {
		if !slices.Contains(row.Ops, protocol.OpLoadpointHold) || !slices.Contains(row.Scopes, protocol.ScopeDispatchHold) {
			b.p.access.EndHolds(row.ID)
		}
		b.p.push(protocol.PushAgentAdded, row.Principal.Client)
	}
	return row, err
}

func (b boxPageAgents) RevokeAgent(by, id string) error {
	if _, ok := b.p.access.Enroll.Agent(id); !ok {
		err := protocol.Refuse(protocol.ErrNotFound, map[string]any{"id": id})
		b.record(by, protocol.MsgAgentRevoke, id, err)
		return err
	}
	err := b.p.access.RevokeAgent(id)
	b.record(by, protocol.MsgAgentRevoke, id, err)
	if err == nil {
		b.p.state.Forget(id)
	}
	return err
}

// ResetClock is for a clock that once ran ahead: until it passes that time
// again, the box trusts no wall time and every agent stays locked out. The
// owner says the clock is right now. Every agent's grant ends with it, since
// a grant made while the clock ran ahead would end years late.
func (b boxPageAgents) ResetClock(by string) error {
	if !cloudlink.ClockSynced() {
		err := protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"})
		b.record(by, "agents.clock.reset", "", err)
		return err
	}
	if err := b.p.access.Enroll.CancelAllAgentInvites(); err != nil {
		return err
	}
	for _, row := range b.p.access.Agents() {
		if err := b.RevokeAgent(by, row.ID); err != nil {
			return err
		}
	}
	err := agentWall.reset(b.p.st, time.Now().UnixMilli())
	b.record(by, "agents.clock.reset", "", err)
	return err
}

func (b boxPageAgents) Decide(ctx context.Context, by, reqID string, approve bool) (string, *protocol.CmdResult, error) {
	h, err := b.p.boxPage(by)
	if err != nil {
		return "", nil, err
	}
	defer h.Close()
	return h.Decide(ctx, reqID, approve)
}

func (b boxPageAgents) Audit(beforeSeq int64, n int) ([]protocol.AuditRow, bool, error) {
	if b.p.audit == nil {
		return nil, false, nil
	}
	return b.p.audit.Page(uint64(max(beforeSeq, 0)), n)
}

// boxPageGrant is the box page's grant: an owner, always, since whoever is
// signed in there may do anything the box can.
type boxPageGrant struct{}

func (boxPageGrant) Grant() (appproto.Enrolment, bool) {
	return appproto.Enrolment{Role: apiauth.RoleOwner, Epoch: 1}, true
}

// discardSender takes frames nobody reads: the box page reads a command's
// outcome from the command log instead.
type discardSender struct{}

func (discardSender) Send([]byte) error { return nil }

// boxPageCaller is the box page's caller for one decision.
func boxPageCaller(by string) apiauth.Caller {
	return apiauth.Caller{Subject: by, Kind: apiauth.KindLAN, Role: apiauth.RoleOwner,
		Scopes: appproto.ScopesForRole(apiauth.RoleOwner), Epoch: 1}
}

// agentsForAPI hands the box page its agents, or nothing when the app link
// is off: an untyped nil, so the routes see no admin rather than a typed nil
// that panics.
func agentsForAPI(p *agentPorts) api.AgentAdmin {
	if p == nil || p.access == nil {
		return nil
	}
	p.access.Enroll.SetAgentOfferKeys(cloudlink.ParseOfferKeys(cloudlink.OfferKeys))
	p.access.Enroll.OnAgentInviteClaimed(func(v protocol.AgentInvite, row protocol.AgentRow) {
		b := boxPageAgents{p: p}
		b.record("invitation", protocol.MsgAgentAdd, row.ID, nil)
		p.push(protocol.PushAgentAdded, row.Principal.Client)
	})
	return boxPageAgents{p: p}
}
