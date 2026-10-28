package cloudlink

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// A box links when its first phone may open a tunnel, with the readable copy
// off: it sends its roster and nothing to read.
func TestTheBoxLinksForItsPhonesAndSendsNoReadings(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key exists before anything needs the link")
	}
	h.setRoster(func(r *fakeRosterState) { r.tun = true })

	r := h.nextRoster(t)
	if len(r.Devices) != 1 || !r.Devices[0].Tun {
		t.Fatalf("roster devices %+v", r.Devices)
	}
	if len(r.Grant.ID) != 0 || len(r.Grant.Scopes) != 0 || len(r.Grant.Purposes) != 0 ||
		r.Grant.Controller != cloudwire.ControllerSourceful {
		t.Fatalf("a roster without a grant names %+v", r.Grant)
	}
	var hello cloudwire.Hello
	svc.mu.Lock()
	payload, _ := cloudwire.DecodeFrame(svc.hello)
	svc.mu.Unlock()
	env, _ := cloudwire.DecodeEnvelope(payload)
	if err := cloudwire.Unmarshal(env.B, &hello); err != nil {
		t.Fatal(err)
	}
	if len(hello.GrantID) != 0 || !slices.Equal(hello.Caps, []string{cloudwire.MsgRoster, cloudwire.MsgNotice, cloudwire.CapTun, cloudwire.CapShard}) {
		t.Fatalf("hello names grant %x and caps %v", hello.GrantID, hello.Caps)
	}

	time.Sleep(2500 * time.Millisecond)
	for _, env := range svc.received() {
		if env.T != cloudwire.MsgRoster {
			t.Fatalf("%s left a box whose readable copy is off", env.T)
		}
	}
	st := h.link.Status()
	if st.Link != "on" || st.State != "off" || st.Home != svc.home {
		t.Fatalf("status %+v", st)
	}
	if s := h.link.SessionInfo(); s == nil || s.Home != svc.home || len(s.Sources) != 0 {
		t.Fatalf("session information %+v", s)
	}
}

// A QR code on the screen links the box and is listed by its hash, never by
// the code.
func TestAPendingCodeIsListedByItsHash(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	code := bytes.Repeat([]byte{0x42}, 16)
	h.setRoster(func(r *fakeRosterState) {
		r.gone, r.code, r.expires = true, code, time.Now().Add(10*time.Minute)
	})
	home := h.link.Status().Home
	if home == "" {
		t.Fatal("a code on the screen did not link the box at once")
	}
	r := h.nextRoster(t)
	if len(r.Pending) != 1 || !bytes.Equal(r.Pending[0].Hash, cloudwire.PairingHash(home, code)) ||
		r.Pending[0].TTLS < 590 || r.Pending[0].TTLS > 600 {
		t.Fatalf("pending %+v", r.Pending)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for _, raw := range svc.raw {
		if bytes.Contains(raw, code) {
			t.Fatal("the pairing code itself left the house")
		}
	}
}

// tun goes both ways unsigned, reaches the Tunnel unread, and counts against
// its own budget: a readable copy past its budget does not stop it, and the
// tunnel past its own does not stop the copy.
func TestTunGoesBothWaysApartFromTheBudget(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgTun, cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 9, D: []byte{1, 2, 3}})
	}
	h := newHarness(t, svc)
	h.link.mu.Lock()
	h.link.budget.add(h.link.uptimeMs(), budgetBytes)
	h.link.mu.Unlock()
	h.setRoster(func(r *fakeRosterState) { r.tun = true })

	select {
	case got := <-h.tunnel.ch:
		if got.Dev != "q5Lw0Zb1" || got.Sid != 9 || !bytes.Equal(got.D, []byte{1, 2, 3}) {
			t.Fatalf("the Tunnel got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tun reached the Tunnel")
	}
	if h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 9, D: []byte{4, 5}}, false) != nil {
		t.Fatal("SendTun refused with the uplink up")
	}
	up := svc.waitFor(cloudwire.MsgTun, 3*time.Second)
	var tun cloudwire.Tun
	if err := cloudwire.Unmarshal(up.B, &tun); err != nil || tun.Sid != 9 || !bytes.Equal(tun.D, []byte{4, 5}) {
		t.Fatalf("the service got %+v: %v", tun, err)
	}
	// The tun went although the copy's budget is spent, and it did not
	// count against that budget: only the roster, sent past the budget on
	// purpose, added to it.
	// hello and the roster go past the budget on purpose; nothing else that
	// left counts but them.
	copyBytes := func() int64 {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		n := int64(len(svc.hello))
		for i, env := range svc.got {
			if env.T != cloudwire.MsgTun {
				n += int64(len(svc.raw[i]))
			}
		}
		return n
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := h.link.Status()
		if st.TunBytesLast24h > 0 && st.BudgetReached && st.BytesLast24h == budgetBytes+copyBytes() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v; the copy's own frames are %d bytes", st, copyBytes())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Past the tunnel's own budget, tun stops.
	h.link.mu.Lock()
	h.link.tunBudget.add(h.link.uptimeMs(), tunBudgetBytes)
	h.link.mu.Unlock()
	if h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 9, D: []byte{6}}, false) == nil {
		t.Fatal("SendTun took a message past the tunnel's budget")
	}
	if h.link.Status().TunDropped == 0 {
		t.Fatal("the drop was not counted")
	}
}

// The readable copy's budget does not count tun, so a phone at work never
// stops hb.
func TestTunDoesNotSpendTheCopysBudget(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	before := h.link.Status().BytesLast24h
	for range 10 {
		if h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 1, D: make([]byte, 4096)}, false) != nil {
			t.Fatal("SendTun refused")
		}
		svc.waitFor(cloudwire.MsgTun, 3*time.Second)
	}
	// The last tun is counted when it leaves, a moment after the service
	// sees it; and the copy's own heartbeat may go meanwhile on a slow
	// runner. What must hold is that the tunnel's 40 KiB never lands on the
	// copy's count.
	deadline := time.Now().Add(3 * time.Second)
	st := h.link.Status()
	for st.TunBytesLast24h < 40_960 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		st = h.link.Status()
	}
	if st.BytesLast24h-before >= 40_960 || st.TunBytesLast24h < 40_960 {
		t.Fatalf("copy %d → %d bytes, tunnel %d", before, st.BytesLast24h, st.TunBytesLast24h)
	}
}

// Without an uplink nothing queues: a session the phone cannot reach is one
// it starts again.
func TestSendTunWithoutAnUplinkIsRefused(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if h.link.SendTun(cloudwire.Tun{Dev: "x", Sid: 1, D: []byte{1}}, false) == nil {
		t.Fatal("SendTun took a message with no uplink")
	}
	if h.link.SendTun(cloudwire.Tun{Dev: "x", Sid: 1, D: make([]byte, cloudwire.TunDataMax+1)}, false) == nil {
		t.Fatal("SendTun took a d over the limit")
	}
}

// Every session the uplink carried ends with it.
func TestTheTunnelDropsWithTheUplink(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, _ string) {
		time.Sleep(300 * time.Millisecond)
		_ = conn.Close()
	}
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	deadline := time.Now().Add(5 * time.Second)
	for h.tunnel.dropped() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the Tunnel was not told the uplink ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// When nothing needs the link any more, the box asks the service to delete
// the home and deletes cloud.key on the receipt. The next link has a new home.
func TestTheLinkEndsWhenNothingNeedsIt(t *testing.T) {
	svc := newFakeService(t)
	svc.purge = true
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	home := h.link.Status().Home

	h.setRoster(func(r *fakeRosterState) { r.gone = true })
	bye := svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	var b cloudwire.Bye
	if cloudwire.Unmarshal(bye.B, &b) != nil || !b.Purge || b.Why != cloudwire.ByeRevoked {
		t.Fatalf("bye %+v", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().Link != "off" {
		if time.Now().After(deadline) {
			t.Fatalf("still %+v after the receipt", h.link.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key survived the end of the link")
	}

	h.setRoster(func(r *fakeRosterState) { r.gone, r.tun = false, true })
	if next := h.link.Status().Home; next == "" || next == home {
		t.Fatalf("the next link's home is %q after %q", next, home)
	}
}

// A grant made before the link existed keeps its key and home id: the link
// takes them over.
func TestAGrantFromBeforeTheLinkKeepsItsHome(t *testing.T) {
	kv := &memKV{}
	keyPath := filepath.Join(t.TempDir(), "cloud.key")
	key, err := createKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	fp, pk, _ := fingerprint(key)
	if err := saveJSON(kv, grantKey, grantRecord{ID: bytes.Repeat([]byte{4}, 16), KeyFp: fp,
		Scopes: []string{protocol.ScopeLiveRead}, Purposes: []string{protocol.CloudPurposeServe},
		GrantKey: bytes.Repeat([]byte{7}, 32), Controller: cloudwire.ControllerSourceful}); err != nil {
		t.Fatal(err)
	}
	svc := newFakeService(t)
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: kv, KeyPath: keyPath,
		ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey}})
	if err != nil {
		t.Fatal(err)
	}
	st := link.Status()
	if st.State != "on" || st.Link != "on" || st.Home != cloudwire.HomeID(pk) {
		t.Fatalf("status %+v", st)
	}
	if rec, _ := loadJSON[linkRecord](kv, linkKey); rec == nil || rec.KeyFp != fp {
		t.Fatalf("link record %+v", rec)
	}
}

// A grant on a box whose link is ending waits for the receipt.
func TestAGrantWaitsForAnEndingLink(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	h.setRoster(func(r *fakeRosterState) { r.gone = true })
	svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); !errors.Is(err, ErrLinkEnding) {
		t.Fatalf("grant while the link ends: %v", err)
	}
}

// A QR that names the home reads it here: empty until the box links, and
// empty again while its link ends.
func TestHomeIsSetOnlyWhileLinked(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if h.link.Home() != "" {
		t.Fatal("a home before the box linked")
	}
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	if home := h.link.Home(); home == "" || home != h.link.Status().Home {
		t.Fatalf("home %q, status %q", home, h.link.Status().Home)
	}
	h.nextRoster(t)
	h.setRoster(func(r *fakeRosterState) { r.gone = true })
	svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	if h.link.Home() != "" {
		t.Fatal("a home while the link ends")
	}
}

// Bulk is paced under the service's limits, and lane 0 never waits behind
// it: a phone's command and its answer go first.
func TestLaneZeroGoesAheadOfPacedBulk(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	// More bulk than the pacer's burst lets through at once.
	big := make([]byte, 16_384)
	n := 0
	for range 240 {
		if h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 1, D: big}, true) == nil {
			n++
		}
	}
	if n < 230 {
		t.Fatalf("only %d of 240 bulk messages queued", n)
	}
	time.Sleep(50 * time.Millisecond)
	if h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 2, D: []byte{1}}, false) != nil {
		t.Fatal("lane 0 was refused")
	}
	start := time.Now()
	for {
		env := svc.waitFor(cloudwire.MsgTun, 5*time.Second)
		var m cloudwire.Tun
		_ = cloudwire.Unmarshal(env.B, &m)
		if m.Sid == 2 {
			break
		}
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("lane 0 waited %s behind bulk", waited)
	}
}

// The pacer lets a burst through and then holds bulk to nine tenths of the
// service's rate.
func TestThePacerKeepsBulkUnderTheServicesLimits(t *testing.T) {
	now := time.Unix(1_791_820_800, 0)
	q := newPacer(now)
	sent, at := 0, now
	for at.Sub(now) < 10*time.Second {
		at = at.Add(q.take(16_384, at))
		sent++
	}
	limit := cloudwire.TunMsgBurst + 10*cloudwire.TunMsgsPerSecond
	byteLimit := (cloudwire.TunByteBurst + 10*cloudwire.TunBytesPerSecond) / 16_384
	if sent > limit || sent > byteLimit {
		t.Fatalf("%d bulk messages of 16 KiB in 10 s, over the service's %d messages or %d by bytes", sent, limit, byteLimit)
	}
}

// A roster built after a revoke landed carries the revoke, whatever snapshot
// the caller held from before.
func TestTheRosterReadsTheLinkAsItIsWhenDue(t *testing.T) {
	svc := newFakeService(t)
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{state: &fakeRosterState{tun: true}}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); err != nil {
		t.Fatal(err)
	}
	stale := link.current()
	if _, ok := link.takeRoster(false); !ok {
		t.Fatal("no roster due after a grant")
	}
	if err := link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	r, ok := link.takeRoster(false)
	if !ok || len(r.Grant.ID) == 0 || len(r.Grant.Purposes) != 0 {
		t.Fatalf("the roster after a revoke is %+v", r.Grant)
	}
	if !stale.copyOn() || stale.allowed(cloudwire.MsgLive) == link.current().allowed(cloudwire.MsgLive) {
		t.Fatal("the stale snapshot is not stale")
	}
}

// A QR code shown and never used: when it runs out, nothing needs the link,
// and the link ends.
func TestAnUnusedCodeEndsTheLinkWhenItRunsOut(t *testing.T) {
	svc := newFakeService(t)
	svc.purge = true
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) {
		r.gone, r.code, r.expires = true, bytes.Repeat([]byte{1}, 16), time.Now().Add(400*time.Millisecond)
	})
	if h.link.Status().Link != "on" {
		t.Fatal("a code on the screen did not link the box")
	}
	h.nextRoster(t)
	svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().Link != "off" {
		if time.Now().After(deadline) {
			t.Fatalf("the link outlived its unused code: %+v", h.link.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A cloud.key already on disk is adopted, never replaced: its home id is
// one phones may dial.
func TestAKeyOnDiskIsAdoptedNeverReplaced(t *testing.T) {
	svc := newFakeService(t)
	keyPath := filepath.Join(t.TempDir(), "cloud.key")
	key, err := createKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	_, pk, _ := fingerprint(key)
	if _, err := createKey(keyPath); err == nil {
		t.Fatal("createKey replaced an existing key")
	}
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{state: &fakeRosterState{tun: true}}, KV: &memKV{},
		KeyPath: keyPath, ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey}})
	if err != nil {
		t.Fatal(err)
	}
	link.RosterChanged()
	if home := link.Home(); home != cloudwire.HomeID(pk) {
		t.Fatalf("linked as %q, the key on disk is %q", home, cloudwire.HomeID(pk))
	}
}

// The readable copy turns on and off while the link stays up: readings start
// with the grant on the same connection and stop with the revoke.
func TestTheCopyTurnsOnAndOffOnALiveConnection(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	time.Sleep(300 * time.Millisecond)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	for len(r.Grant.Purposes) != 0 {
		r = h.nextRoster(t)
	}
	time.Sleep(2500 * time.Millisecond)
	svc.mu.Lock()
	conns := svc.conns
	var order []string
	for _, env := range svc.got {
		order = append(order, env.T)
	}
	svc.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the copy turning on and off took %d connections", conns)
	}
	// off: roster only; on: readings; off again: nothing readable after the
	// withdrawing roster.
	firstLive, withdrawn := -1, -1
	rosters := 0
	for i, typ := range order {
		switch typ {
		case cloudwire.MsgLive:
			if firstLive < 0 {
				firstLive = i
			}
			if withdrawn >= 0 {
				t.Fatalf("live after the withdrawing roster: %v", order)
			}
		case cloudwire.MsgRoster:
			rosters++
			var ro cloudwire.Roster
			_ = cloudwire.Unmarshal(svc.received()[i].B, &ro)
			if len(ro.Grant.ID) > 0 && len(ro.Grant.Purposes) == 0 {
				withdrawn = i
			}
		case cloudwire.MsgHB, cloudwire.MsgLP, cloudwire.MsgPrices:
			if firstLive < 0 || withdrawn >= 0 {
				t.Fatalf("%s outside the copy: %v", typ, order)
			}
		}
	}
	if firstLive <= 0 || withdrawn <= firstLive || rosters < 3 {
		t.Fatalf("order %v", order)
	}
}

// The copy keeps flowing when the tunnel's own budget is spent.
func TestTheCopyFlowsPastTheTunnelBudget(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.link.mu.Lock()
	h.link.tunBudget.add(h.link.uptimeMs(), tunBudgetBytes)
	h.link.mu.Unlock()
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	h.site.setGrid(4321)
	svc.waitFor(cloudwire.MsgLive, 7*time.Second)
	if err := h.link.SendTun(cloudwire.Tun{Dev: "q5Lw0Zb1", Sid: 1, D: []byte{1}}, false); err == nil {
		t.Fatal("tun went past its own budget")
	}
}

// A security notice goes whenever the box is linked, with the readable copy
// off, and carries the kind alone.
func TestANoticeGoesWithTheCopyOff(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	if h.link.Notice("cloud.granted", "") {
		t.Fatal("a kind that is not a notice was queued")
	}
	if !h.link.Notice(protocol.PushAgentAdded, "Claude") {
		t.Fatal("the notice was refused")
	}
	env := svc.waitFor(cloudwire.MsgNotice, 3*time.Second)
	n, err := cloudwire.ReadNotice(env.B)
	if err != nil || n.Kind != protocol.PushAgentAdded || n.Client != "" {
		t.Fatalf("notice %+v %v: with the copy off it carries the kind alone", n, err)
	}
}

// The client's name goes with agent.added only while the copy is on and
// carries events, as an event's arguments would.
func TestANoticeNamesTheClientOnlyWhenTheGrantCarriesEvents(t *testing.T) {
	for _, c := range []struct {
		name   string
		scopes []string
		kind   string
		client string
		want   string
	}{
		{"events granted", []string{protocol.ScopeLiveRead, protocol.ScopeEventsRead}, protocol.PushAgentAdded, "Claude", "Claude"},
		{"the copy without events", []string{protocol.ScopeLiveRead}, protocol.PushAgentAdded, "Claude", ""},
		{"another kind", []string{protocol.ScopeEventsRead}, protocol.PushAgentRequest, "Claude", ""},
		{"a name too long", []string{protocol.ScopeEventsRead}, protocol.PushAgentAdded, strings.Repeat("c", cloudwire.NoticeClientMaxRunes+1), ""},
		{"a name the consent page cut", []string{protocol.ScopeEventsRead}, protocol.PushAgentAdded, strings.Repeat("ö", 64) + "…", strings.Repeat("ö", 64) + "…"},
		{"a format character", []string{protocol.ScopeEventsRead}, protocol.PushAgentAdded, "Claude\u202e", ""},
		{"a line break", []string{protocol.ScopeEventsRead}, protocol.PushAgentAdded, "Claude\nPay here", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newFakeService(t)
			h := newHarness(t, svc)
			h.grant(t, c.scopes...)
			svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
			if !h.link.Notice(c.kind, c.client) {
				t.Fatal("the notice was refused")
			}
			n, err := cloudwire.ReadNotice(svc.waitFor(cloudwire.MsgNotice, 3*time.Second).B)
			if err != nil || n.Kind != c.kind || n.Client != c.want {
				t.Fatalf("notice %+v %v, want client %q", n, err, c.want)
			}
		})
	}
}

// A security notice's kind never goes as an event: the owners get it once,
// as a notice, and a viewer reading events gets nothing of it.
func TestANoticeKindIsNoEvent(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead, protocol.ScopeEventsRead)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	for _, kind := range cloudwire.NoticeKinds {
		if h.link.QueueEvent(EventIn{Kind: kind, Priority: 3}) {
			t.Fatalf("%s was queued as an event", kind)
		}
	}
	if !h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected, Priority: 3}) {
		t.Fatal("an ordinary event was refused")
	}
	svc.waitFor(cloudwire.MsgEvent, 3*time.Second)
}

// Notices wait for the uplink in a queue of 16. A seventeenth pushes the
// oldest out, and the box counts it.
func TestAFullNoticeQueueCountsWhatItDrops(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	// The same linked box, not running: nothing takes its notices.
	idle, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: h.keyPath,
		ServiceKeys: h.link.opts.ServiceKeys})
	if err != nil {
		t.Fatal(err)
	}
	for i := range noticeQueue + 1 {
		if !idle.Notice(protocol.PushAgentRequest, "") {
			t.Fatalf("notice %d was refused", i)
		}
	}
	if st := idle.Status(); st.NoticesDropped != 1 {
		t.Fatalf("dropped %d", st.NoticesDropped)
	}
}

// A notice goes past the spent byte budget, as an event does: it is how an
// owner hears that someone else can reach the box.
func TestANoticeGoesPastTheBudget(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.link.mu.Lock()
	h.link.budget.add(h.link.uptimeMs(), budgetBytes)
	h.link.mu.Unlock()
	h.grant(t)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	if !h.link.Notice(protocol.PushOwnerAdded, "") {
		t.Fatal("the notice was refused")
	}
	svc.waitFor(cloudwire.MsgNotice, 3*time.Second)
}

// A statistics-only grant carries no notice: sendNotices sends none under it.
// Notice says so rather than queue one that would wait unsent, because the
// box's notification history reads the answer as delivered (#1785).
func TestNoNoticeIsTakenUnderAStatisticsOnlyGrant(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	if !h.link.Notice(protocol.PushOwnerAdded, "") {
		t.Fatal("the readable copy refused a notice")
	}
	g := h.record(t)
	g.Policy, g.Scopes, g.Purposes = cloudwire.StatsPolicy, []string{}, []string{protocol.CloudPurposeStats}
	h.link.mu.Lock()
	err := h.link.storeLocked(g, "test")
	h.link.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if h.link.Notice(protocol.PushOwnerAdded, "") {
		t.Fatal("a statistics-only grant took a notice it never sends")
	}
}

// fakeExchange is the box's exchange, as the link sees it.
type fakeExchange struct{ ch chan cloudwire.X }

func (f *fakeExchange) Deliver(x cloudwire.X) { f.ch <- x }

// x goes both ways unsigned and reaches the Exchange unread, and the box says
// x in hello.caps only when it has an Exchange beside its Tunnel.
func TestXGoesBothWaysAndIsSaidOnlyWithAnExchange(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgX, cloudwire.X{Dev: "wAtCh123", ID: 4, D: []byte{1, 2, 3}})
	}
	exchange := &fakeExchange{ch: make(chan cloudwire.X, 4)}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.Exchange = exchange })
	h.setRoster(func(r *fakeRosterState) { r.tun = true })

	select {
	case got := <-exchange.ch:
		if got.Dev != "wAtCh123" || got.ID != 4 || !bytes.Equal(got.D, []byte{1, 2, 3}) {
			t.Fatalf("the Exchange got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no x reached the Exchange")
	}
	if !h.link.TakesX() {
		t.Fatal("a link with an Exchange beside its Tunnel does not take x")
	}
	var hello cloudwire.Hello
	svc.mu.Lock()
	payload, _ := cloudwire.DecodeFrame(svc.hello)
	svc.mu.Unlock()
	env, _ := cloudwire.DecodeEnvelope(payload)
	if err := cloudwire.Unmarshal(env.B, &hello); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hello.Caps, cloudwire.CapX) {
		t.Fatalf("hello caps %v name no x", hello.Caps)
	}

	if err := h.link.SendX(cloudwire.X{Dev: "wAtCh123", ID: 4, D: []byte{4, 5}}); err != nil {
		t.Fatalf("SendX refused with the uplink up: %v", err)
	}
	up := svc.waitFor(cloudwire.MsgX, 3*time.Second)
	var x cloudwire.X
	if err := cloudwire.Unmarshal(up.B, &x); err != nil || x.ID != 4 || x.Dev != "wAtCh123" || !bytes.Equal(x.D, []byte{4, 5}) || len(up.Sig) != 0 {
		t.Fatalf("the service got %+v: %v", x, err)
	}

	// Without an Exchange the box says no x.
	plain := newFakeService(t)
	h2 := newHarness(t, plain)
	h2.setRoster(func(r *fakeRosterState) { r.tun = true })
	h2.nextRoster(t)
	plain.mu.Lock()
	payload, _ = cloudwire.DecodeFrame(plain.hello)
	plain.mu.Unlock()
	env, _ = cloudwire.DecodeEnvelope(payload)
	if err := cloudwire.Unmarshal(env.B, &hello); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(hello.Caps, cloudwire.CapX) || h2.link.TakesX() {
		t.Fatalf("a box with no Exchange says x: %v", hello.Caps)
	}
}

// x keeps to its share: a tenth of the tunnel's budget, and nothing once the
// tunnel has spent nine tenths, so the phones keep the last of it. Past
// either, an answer may not leave, and the exchange runs nothing.
func TestXKeepsToItsShareOfTheTunnelBudget(t *testing.T) {
	svc := newFakeService(t)
	exchange := &fakeExchange{ch: make(chan cloudwire.X, 4)}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.Exchange = exchange })
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.nextRoster(t)
	if !h.link.XAllowed() || h.link.SendX(cloudwire.X{Dev: "w", ID: 1}) != nil {
		t.Fatal("x refused with its share unspent")
	}

	h.link.mu.Lock()
	h.link.xBudget.add(h.link.uptimeMs(), xBudgetBytes)
	h.link.mu.Unlock()
	if h.link.XAllowed() || h.link.SendX(cloudwire.X{Dev: "w", ID: 2}) == nil {
		t.Fatal("x left past its own share")
	}

	h.link.mu.Lock()
	h.link.xBudget = budget{}
	h.link.tunBudget.add(h.link.uptimeMs(), int64(xStopsAt*tunBudgetBytes))
	h.link.mu.Unlock()
	if h.link.XAllowed() {
		t.Fatal("x left with the phones' last share of the tunnel")
	}
	// The phones still have theirs.
	if h.link.SendTun(cloudwire.Tun{Dev: "p", Sid: 1, D: []byte{1}}, false) != nil {
		t.Fatal("a phone's tun was refused at nine tenths")
	}
}
