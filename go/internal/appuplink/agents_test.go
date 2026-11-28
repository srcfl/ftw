package appuplink

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// agentTunRig is the tunnel rig with agents: an enrolment with a clock, and
// handlers that know the box's agent grants.
func agentTunRig(t *testing.T, now *time.Time) (*tunRig, *AgentAccess) {
	t.Helper()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func() (time.Time, bool) { return *now, true }
	enroll.SetClock(clock)
	access := &AgentAccess{Enroll: enroll, Clock: clock}
	state := appproto.NewAgentState()
	r := &tunRig{enroll: enroll, slots: &slots{}, out: &capturedTun{ch: make(chan cloudwire.Tun, 64)},
		modes: &countingModes{}}
	build := func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:  stubSite{}, Info: stubInfo{}, Modes: r.modes, Plans: stubPlans{}, Codec: codec{},
			Sender: s, Caller: caller, Grants: grants, Commands: appproto.NewCmdLog(),
			Access: access, Agents: state,
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
		})
	}
	r.tunnel = newTunnel(build, enroll, r.slots, nil, slog.New(slog.DiscardHandler))
	r.tunnel.Attach(r.out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.tunnel.Run(ctx)
	access.Drop = func(key []byte) { r.tunnel.dropByAppKey(key) }
	return r, access
}

// An agent's session runs through FTW Cloud and ends when its grant is revoked.
func TestAnAgentComesThroughTheTunnel(t *testing.T) {
	now := time.UnixMilli(1_792_000_000_000)
	r, access := agentTunRig(t, &now)
	agent, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.enroll.AddAgent(appenroll.AtTheBoxPage, protocol.AgentAdd{
		Key: agent.staticPublic(), Role: apiauth.RoleAgent, Scopes: []string{protocol.ScopeLiveRead},
		ExpiresAtMs: now.UnixMilli() + time.Hour.Milliseconds()})
	if err != nil {
		t.Fatal(err)
	}

	r.open(t, agent, row.ID, 3, nil)
	r.send(t, agent, row.ID, 3, appproto.MsgHello, appproto.Hello{
		Proto: appproto.ProtoRange{Max: appproto.ProtoMax}, App: appproto.AppInfo{Build: "mcp", UA: "agent"}})
	var ok appproto.HelloOK
	if err := appproto.Unmarshal(r.receive(t, agent, appproto.MsgHelloOK).Envelope.B, &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Role != apiauth.RoleAgent || len(ok.Scopes) != 1 || ok.Scopes[0] != protocol.ScopeLiveRead || ok.Cloud != nil {
		t.Errorf("hello_ok %+v", ok)
	}

	// Revoked: its session ends.
	if err := access.RevokeAgent(row.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.tunnel.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.tunnel.count() != 0 {
		t.Error("a revoked agent's session lives on")
	}
}

// The roster FTW Cloud reads carries an agent row's grant, at the approver's
// scopes now, and leaves out a row past its expiry.
func TestTheRosterCarriesAnAgentsGrant(t *testing.T) {
	now := time.UnixMilli(1_792_000_000_000)
	r, _ := agentTunRig(t, &now)
	agent, _ := newAppClient()
	row, err := r.enroll.AddAgent(appenroll.AtTheBoxPage, protocol.AgentAdd{
		Key: agent.staticPublic(), Role: apiauth.RoleSupport,
		Scopes:      []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead, protocol.ScopeLogsRead},
		Ops:         []string{protocol.OpLoadpointHold},
		Caps:        protocol.Caps{HoldS: 600, MaxW: 3600},
		ExpiresAtMs: now.UnixMilli() + time.Hour.Milliseconds(),
		Principal:   protocol.Principal{ClientID: protocol.SupportClientID, Ticket: "FTW-9"}})
	if err != nil {
		t.Fatal(err)
	}
	devices := CloudRoster{Enroll: r.enroll, Tunnel: true}.Devices()
	if len(devices) != 1 {
		t.Fatalf("devices %+v", devices)
	}
	d := devices[0]
	// No door admits an agent row and it reads no copy: only the keys part
	// opens its session.
	if d.ID != row.ID || d.Tun || len(d.Scopes) != 0 || d.Agent == nil || d.Agent.Principal.Ticket != "FTW-9" ||
		len(d.Agent.Ops) != 1 || d.Agent.Caps.HoldS != 600 || d.Agent.ExpiresAtMs != row.ExpiresAtMs {
		t.Errorf("roster entry %+v %+v", d, d.Agent)
	}
	if !bytes.Equal((CloudRoster{Enroll: r.enroll}).Noise(), r.enroll.StaticKey().Public()) {
		t.Error("the roster's noise is not the box's key")
	}
	now = now.Add(time.Hour)
	if devices := (CloudRoster{Enroll: r.enroll, Tunnel: true}).Devices(); len(devices) != 0 {
		t.Errorf("an expired agent is still listed: %+v", devices)
	}
}
