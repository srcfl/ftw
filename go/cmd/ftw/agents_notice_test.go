package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/protocol"
)

// An agent added or widened at the box page reaches FTW Cloud as an
// agent.added notice that names its client; the link decides whether the
// name leaves the box.
func TestAnAgentAddedAtTheBoxPageNamesItsClient(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "box.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_792_000_000_000)
	clock := func() (time.Time, bool) { return now, true }
	enroll.SetClock(clock)
	type sent struct{ kind, client string }
	var notices []sent
	p := &agentPorts{access: &appuplink.AgentAccess{Enroll: enroll, Clock: clock},
		notice: func(kind, client string) bool { notices = append(notices, sent{kind, client}); return true }}

	row, err := boxPageAgents{p}.AddAgent("owner", protocol.AgentAdd{
		Key: bytes.Repeat([]byte{7}, 32), Role: apiauth.RoleAgent, Scopes: []string{protocol.ScopeLiveRead},
		ExpiresAtMs: now.UnixMilli() + time.Hour.Milliseconds(),
		Principal:   protocol.Principal{Client: "Claude", ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0] != (sent{protocol.PushAgentAdded, "Claude"}) {
		t.Fatalf("notices %+v", notices)
	}
	if _, err := (boxPageAgents{p}).WidenAgent("owner", protocol.AgentWiden{ID: row.ID, Epoch: row.Epoch,
		Scopes: []string{protocol.ScopeHistoryRead, protocol.ScopeLiveRead}, ExpiresAtMs: row.ExpiresAtMs}); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || notices[1] != (sent{protocol.PushAgentAdded, "Claude"}) {
		t.Fatalf("a widened grant: notices %+v", notices)
	}
}

// noSubscriptions is web push before a phone has stored a subscription.
type noSubscriptions struct{}

func (noSubscriptions) Publish(context.Context, notifications.Message) error {
	return notifications.ErrNothingToSend
}

// A phone paired as the first owner has no web push subscription yet, so the
// box's own transports have nobody to tell. FTW Cloud took the owner.added
// notice for the owners' phones, and the history says sent, not failed
// (#1785). When FTW Cloud did not take it, nobody was told.
func TestTheHistoryCountsTheNoticeFTWCloudTook(t *testing.T) {
	for _, c := range []struct {
		queued bool
		want   string
	}{{true, "sent"}, {false, "failed"}} {
		svc := notifications.New(nil, nil, nil)
		svc.AddPublisher(noSubscriptions{})
		bus := events.NewBus()
		svc.Subscribe(bus)
		got := make(chan events.NotificationDispatched, 1)
		bus.Subscribe(events.KindNotificationDispatched, func(e events.Event) {
			if ev, ok := e.(events.NotificationDispatched); ok {
				got <- ev
			}
		})
		notices := &cloudNotices{}
		notices.svc.Store(svc)
		p := &agentPorts{notices: notices, notice: func(string, string) bool { return c.queued }}

		p.push(protocol.PushOwnerAdded, "")
		select {
		case ev := <-got:
			if ev.EventType != protocol.PushOwnerAdded || ev.Status != c.want {
				t.Fatalf("FTW Cloud took it: %v; history %s %q (%s)", c.queued, ev.EventType, ev.Status, ev.Error)
			}
		case <-time.After(time.Second):
			t.Fatal("no history row")
		}
	}
}

// A grant of a year warns the owners once, in its last day, as a short one
// does: never on the day it was given or months before its end.
func TestAYearLongGrantWarnsInItsLastDay(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "box.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_792_000_000_000)
	clock := func() (time.Time, bool) { return now, true }
	enroll.SetClock(clock)
	expiring := 0
	p := &agentPorts{access: &appuplink.AgentAccess{Enroll: enroll, Clock: clock},
		notice: func(kind, _ string) bool {
			if kind == protocol.PushAgentExpiring {
				expiring++
			}
			return true
		}}
	year := 365 * 24 * time.Hour
	if _, err := (boxPageAgents{p}).AddAgent("owner", protocol.AgentAdd{
		Key: bytes.Repeat([]byte{7}, 32), Role: apiauth.RoleAgent, Scopes: []string{protocol.ScopeLiveRead},
		ExpiresAtMs: now.Add(year).UnixMilli(), Principal: protocol.Principal{Client: "Claude"}}); err != nil {
		t.Fatal(err)
	}
	end := now.Add(year)
	for _, at := range []time.Time{now, now.Add(300 * 24 * time.Hour), end.Add(-25 * time.Hour)} {
		if p.warnExpiring(at.UnixMilli()); expiring != 0 {
			t.Fatalf("warned %s before the end", end.Sub(at))
		}
	}
	p.warnExpiring(end.Add(-23 * time.Hour).UnixMilli())
	p.warnExpiring(end.Add(-time.Hour).UnixMilli())
	if expiring != 1 {
		t.Fatalf("%d warnings in the last day, want 1", expiring)
	}
}

// The box page learns from the box's facts how long it may let each kind of
// access last, so it offers nothing the box would refuse.
func TestTheBoxPageIsToldTheLongestGrant(t *testing.T) {
	got := boxPageAgents{&agentPorts{}}.Facts().LongestMs
	if got[protocol.RoleAgent] != protocol.AgentMaxMs || got[protocol.RoleSupport] != protocol.SupportMaxMs {
		t.Fatalf("longestMs = %v", got)
	}
}
