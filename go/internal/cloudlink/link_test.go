package cloudlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

func (k *memKV) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	return nil
}

// The fake house uses these names, so a test can search every frame for them.
const (
	meterName   = "p1meter-in-the-hall"
	chargerName = "easee-garage"
	lpName      = "garage-loadpoint"
)

type fakeSite struct {
	mu        sync.Mutex
	grid      int64
	state     protocol.SourceState
	lastOk    int64
	liveCalls int
	lpCalls   int
}

func (s *fakeSite) setGrid(w int64) {
	s.mu.Lock()
	s.grid = w
	s.mu.Unlock()
}

func (s *fakeSite) Live() Live {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveCalls++
	return Live{
		Fields:       map[string]int64{"1": 0, "2": s.grid, "6": 900, "10": 2100},
		FieldSources: map[string][]string{"2": {meterName}, "6": {meterName, chargerName}, "10": {chargerName}},
		Sources: map[string]Source{
			meterName:   {Kind: "driver", State: s.state, LastOkUptimeMs: s.lastOk, StaleAfterMs: 4_000},
			chargerName: {Kind: "driver", State: protocol.SourceLive, LastOkUptimeMs: s.lastOk, StaleAfterMs: 30_000},
		},
		DispatchBlockedBy: nil,
	}
}

func (s *fakeSite) Loadpoints() (cloudwire.LP, bool) {
	s.mu.Lock()
	s.lpCalls++
	s.mu.Unlock()
	soc := int64(430)
	return cloudwire.LP{ControlRev: 9, Loadpoints: []cloudwire.Loadpoint{{
		ID: lpName, Src: chargerName, Plugged: true, SocPermille: &soc, SocSource: "inferred", PowerW: 2100,
	}}, Feedback: []cloudwire.Feedback{{Kind: "ev", Src: chargerName, Status: "following"}}}, true
}

func (s *fakeSite) Prices() (cloudwire.Prices, bool) {
	p := int64(42)
	return cloudwire.Prices{Zone: "SE3", Currency: "SEK", T0Ms: 1_791_820_800_000, SlotMs: 900_000,
		SpotMinor: []*int64{&p}, TotalMinor: []*int64{&p}}, true
}

// fakeRoster is one owner phone. Its zero value is a box from before the
// tunnel, whose phone may not open one, so nothing links it but a grant.
type fakeRoster struct {
	state *fakeRosterState
}

type fakeRosterState struct {
	mu   sync.Mutex
	tun  bool
	gone bool
	// agent adds an assistant's row, which has no tunnel.
	agent   bool
	code    []byte
	expires time.Time
}

func (r fakeRoster) Devices() []RosterDevice {
	tun := false
	if r.state != nil {
		r.state.mu.Lock()
		defer r.state.mu.Unlock()
		if r.state.gone {
			return nil
		}
		tun = r.state.tun
	}
	out := []RosterDevice{{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{5}, 32), Role: "owner", Epoch: 1,
		Scopes: []string{protocol.ScopeLiveRead, protocol.ScopeDispatchWrite, protocol.ScopeEventsRead}, Tun: tun}}
	if r.state != nil && r.state.agent {
		out = append(out, RosterDevice{ID: "Ag3nt0001", Key: bytes.Repeat([]byte{7}, 32), Role: protocol.RoleAgent,
			Epoch: 1, Scopes: []string{}, Agent: &cloudwire.Agent{Scopes: []string{protocol.ScopeLiveRead},
				Ops: []string{}, ExpiresAtMs: 1_791_821_000_000}})
	}
	return out
}

func (r fakeRoster) Noise() []byte { return make([]byte, 32) }

func (r fakeRoster) Pending() ([]byte, time.Time, bool) {
	if r.state == nil {
		return nil, time.Time{}, false
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	// As enrolment does, a code that ran out is none.
	return r.state.code, r.state.expires, r.state.code != nil && time.Now().Before(r.state.expires)
}

// fakeTunnel is the box's session table, as the link sees it.
type fakeTunnel struct {
	ch    chan cloudwire.Tun
	mu    sync.Mutex
	drops int
}

func (f *fakeTunnel) Deliver(t cloudwire.Tun) {
	select {
	case f.ch <- t:
	default:
	}
}

func (f *fakeTunnel) Drop() {
	f.mu.Lock()
	f.drops++
	f.mu.Unlock()
}

func (f *fakeTunnel) dropped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drops
}

// fakeService is ftwcloudd's half of the handshake, enough to test the box.
type fakeService struct {
	t   *testing.T
	key *ecdsa.PrivateKey
	srv *httptest.Server

	// Knobs, set before the box connects.
	badEKM      bool
	wrongKey    bool
	purge       bool
	afterHello  func(conn uplinkConn, home string)
	rosterSeqOK uint64
	// wrongShard, when set, closes after hello with CloseWrongShard and
	// this host, as an instance that does not hold the home;
	// restartBeforeOK closes after hello with CloseRestart, as a service
	// that is stopping.
	wrongShard      string
	restartBeforeOK bool

	wmu   sync.Mutex
	mu    sync.Mutex
	conns int
	hello []byte
	got   []cloudwire.Envelope
	raw   [][]byte
	ch    chan cloudwire.Envelope
	home  string
	pub   *ecdsa.PublicKey
	// kinds is how each message from the box came: by the control
	// stream, the bulk stream or as a datagram over QUIC.
	kinds map[string][]int
	// transports counts the connections each transport carried.
	transports map[string]int
	// kick drops this many connections right after hello_ok, so the box
	// dials again.
	kick        int
	statsPolicy string
	// have is the closed tiles hello_ok names as held.
	have map[string]string
	// usedEarly says, per QUIC connection, whether it took early data.
	usedEarly []bool
}

func newFakeService(t *testing.T) *fakeService {
	return newFakeServiceCert(t, nil)
}

// newFakeServiceCert serves with cert, or httptest's own when nil.
func newFakeServiceCert(t *testing.T, cert *tls.Certificate) *fakeService {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &fakeService{t: t, key: key, ch: make(chan cloudwire.Envelope, 256), kinds: map[string][]int{},
		transports: map[string]int{}}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	if cert != nil {
		s.srv.TLS.Certificates = []tls.Certificate{*cert}
	}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeService) endpoint() string {
	return "wss://" + s.srv.Listener.Addr().String() + "/v1/box"
}

func (s *fakeService) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	return pool
}

func (s *fakeService) write(conn uplinkConn, key *ecdsa.PrivateKey, home, t string, body any) {
	env, err := cloudwire.Seal(key, home, t, body)
	if err != nil {
		s.t.Error(err)
		return
	}
	raw, _ := env.Encode()
	frame, _ := cloudwire.EncodeFrame(raw)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (s *fakeService) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ekm, _ := r.TLS.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
	s.serve(conn, ekm)
}

// serve is the service's side of one uplink, over either transport.
func (s *fakeService) serve(conn uplinkConn, ekm []byte) {
	defer conn.Close()
	s.mu.Lock()
	s.conns++
	s.transports[transportOf(conn)]++
	s.mu.Unlock()
	nonce := bytes.Repeat([]byte{7}, cloudwire.NonceBytes)
	s.write(conn, nil, "", cloudwire.MsgChallenge, cloudwire.Challenge{V: 1, Nonce: nonce})

	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	payload, _ := cloudwire.DecodeFrame(msg)
	env, err := cloudwire.DecodeEnvelope(payload)
	var hello cloudwire.Hello
	if err != nil || env.T != cloudwire.MsgHello || cloudwire.Unmarshal(env.B, &hello) != nil {
		s.t.Errorf("first box message was not a hello: %v", err)
		return
	}
	pub, err := cloudwire.ParsePublicKey(hello.PK)
	home := cloudwire.HomeID(hello.PK)
	if err != nil || cloudwire.Verify(pub, home, env) != nil {
		s.t.Errorf("hello does not verify: %v", err)
		return
	}
	if !bytes.Equal(hello.Nonce, nonce) || !bytes.Equal(hello.EKM, ekm) {
		s.t.Errorf("hello is not bound to this challenge and TLS session")
		return
	}
	s.mu.Lock()
	s.home, s.pub, s.hello = home, pub, msg
	shard := s.wrongShard
	s.mu.Unlock()
	if shard != "" {
		closeWith(conn, cloudwire.CloseWrongShard, shard)
		_, _, _ = conn.ReadMessage()
		return
	}
	if s.restartBeforeOK {
		closeWith(conn, cloudwire.CloseRestart, "0")
		_, _, _ = conn.ReadMessage()
		return
	}

	okEKM := ekm
	if s.badEKM {
		okEKM = bytes.Repeat([]byte{1}, cloudwire.EKMBytes)
	}
	signer := s.key
	if s.wrongKey {
		signer, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	have := map[string]string{}
	s.mu.Lock()
	for id, etag := range s.have {
		have[id] = etag
	}
	s.mu.Unlock()
	s.write(conn, signer, home, cloudwire.MsgHelloOK, cloudwire.HelloOK{
		BoxNonce: hello.BoxNonce, EKM: okEKM, Have: have, RosterSeq: s.rosterSeqOK, StatsPolicy: s.statsPolicy})
	if s.afterHello != nil {
		go s.afterHello(conn, home)
	}
	s.mu.Lock()
	kick := s.kick > 0
	if kick {
		s.kick--
	}
	s.mu.Unlock()
	if kick {
		return
	}

	for {
		kind, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage && kind != msgBulk && kind != msgDatagram {
			s.t.Errorf("the box sent a text message")
			return
		}
		payload, err := cloudwire.DecodeFrame(msg)
		if err != nil {
			s.t.Errorf("box frame: %v", err)
			return
		}
		env, err := cloudwire.DecodeEnvelope(payload)
		switch {
		case err != nil:
			s.t.Errorf("box frame: %v", err)
			return
		case env.T == cloudwire.MsgTun, env.T == cloudwire.MsgX:
			// d is Noise ciphertext; the box signs no tun and no x.
			if len(env.Sig) != 0 {
				s.t.Errorf("the box signed a %s", env.T)
			}
		case cloudwire.Verify(pub, home, env) != nil:
			s.t.Errorf("box envelope %q does not verify", env.T)
			return
		}
		s.mu.Lock()
		s.got = append(s.got, env)
		s.raw = append(s.raw, msg)
		s.kinds[env.T] = append(s.kinds[env.T], kind)
		s.mu.Unlock()
		s.ch <- env
		if !s.purge {
			continue
		}
		switch env.T {
		case cloudwire.MsgBye:
			var bye cloudwire.Bye
			if cloudwire.Unmarshal(env.B, &bye) == nil && bye.Purge {
				s.write(conn, s.key, home, cloudwire.MsgPurged, cloudwire.Purged{
					AtMs: 1_791_821_000_000, What: cloudwire.PurgedHome})
			}
		case cloudwire.MsgRoster:
			// A grant with an id and no purposes asks for its copy to go.
			var r cloudwire.Roster
			if cloudwire.Unmarshal(env.B, &r) == nil && len(r.Grant.ID) > 0 && len(r.Grant.Purposes) == 0 {
				s.write(conn, s.key, home, cloudwire.MsgPurged, cloudwire.Purged{
					AtMs: 1_791_821_000_000, What: cloudwire.PurgedCopy, GrantID: r.Grant.ID})
			}
		}
	}
}

func (s *fakeService) received() []cloudwire.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cloudwire.Envelope(nil), s.got...)
}

// waitFor returns the next envelope of type t, failing after d.
func (s *fakeService) waitFor(t string, d time.Duration) cloudwire.Envelope {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case env := <-s.ch:
			if env.T == t {
				return env
			}
		case <-deadline:
			s.t.Fatalf("no %s within %s", t, d)
			return cloudwire.Envelope{}
		}
	}
}

type harness struct {
	link    *Link
	svc     *fakeService
	site    *fakeSite
	roster  *fakeRosterState
	tunnel  *fakeTunnel
	kv      *memKV
	keyPath string
	cancel  context.CancelFunc
	done    chan error
	pushed  chan string
	// arrivals is the driver registry's signal that a poll finished.
	arrivals chan struct{}
}

func newHarness(t *testing.T, svc *fakeService) *harness {
	t.Helper()
	return newHarnessKeys(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey})
}

// newHarnessKeys is a box that trusts keys; nil means the keys this build
// carries.
func newHarnessKeys(t *testing.T, svc *fakeService, keys []*ecdsa.PublicKey) *harness {
	return newHarnessWith(t, svc, keys, nil, nil)
}

// newHarnessWith also takes the box's state store, and tweak may change the
// options before the link starts.
func newHarnessWith(t *testing.T, svc *fakeService, keys []*ecdsa.PublicKey, kv *memKV, tweak func(*Options)) *harness {
	t.Helper()
	if kv == nil {
		kv = &memKV{}
	}
	h := &harness{svc: svc, site: &fakeSite{grid: 1200, state: protocol.SourceLive}, kv: kv,
		keyPath: filepath.Join(t.TempDir(), "cloud.key"), pushed: make(chan string, 8),
		roster: &fakeRosterState{}, tunnel: &fakeTunnel{ch: make(chan cloudwire.Tun, 64)},
		arrivals: make(chan struct{}, 1)}
	opts := Options{
		Site: h.site, Roster: fakeRoster{state: h.roster}, KV: h.kv, KeyPath: h.keyPath, Build: "test",
		Started: time.Now().Add(-time.Minute), ClockSynced: func() bool { return true },
		Notify: func(kind string) { h.pushed <- kind },
		Tunnel: h.tunnel, Arrivals: h.arrivals,
		Endpoint: svc.endpoint(), ServiceKeys: keys,
		RootCAs: svc.roots(), Tick: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	link, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.link = link
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- link.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

// setRoster changes the phones as enrolment would, and tells the link.
func (h *harness) setRoster(change func(r *fakeRosterState)) {
	h.roster.mu.Lock()
	change(h.roster)
	h.roster.mu.Unlock()
	h.link.RosterChanged()
}

func (h *harness) grant(t *testing.T, scopes ...string) {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{protocol.ScopeLiveRead}
	}
	if err := h.link.Grant(GrantRequest{Scopes: scopes, Policy: "2026-10", By: "test"}); err != nil {
		t.Fatal(err)
	}
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

func TestOffUntilAnOwnerGrants(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	time.Sleep(300 * time.Millisecond)
	svc.mu.Lock()
	conns := svc.conns
	svc.mu.Unlock()
	if conns != 0 {
		t.Fatalf("the box dialled FTW Cloud %d times with no grant", conns)
	}
	if st := h.link.Status(); st.State != "off" || st.Home != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key exists before any grant")
	}
}

// An owner with no phone lets an assistant in on the box page. The agent row
// has no tunnel, but FTW Cloud learns of it only from the roster, so the box
// links for it, and an idle link does not end while it stays.
func TestAnAgentRowLinksTheBox(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.agent = true })
	r := h.nextRoster(t)
	agents := 0
	for _, d := range r.Devices {
		if d.Agent != nil && !d.Tun {
			agents++
		}
	}
	if len(r.Devices) != 2 || agents != 1 {
		t.Fatalf("roster devices %+v", r.Devices)
	}
	if st := h.link.Status(); st.Link != "on" || st.Home == "" {
		t.Fatalf("status %+v", st)
	}
	h.link.endIfIdle()
	h.link.mu.Lock()
	ending := h.link.link != nil && h.link.link.Ending
	h.link.mu.Unlock()
	if ending {
		t.Fatal("the link began to end while an agent still has access")
	}
}

// With the app link off the box takes no tun, so it cannot serve an agent.
// A row left from before must not dial FTW Cloud, make a home id or send a
// roster that names its client.
func TestAnAgentRowLinksNoBoxThatTakesNoTun(t *testing.T) {
	svc := newFakeService(t)
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.Tunnel = nil })
	h.setRoster(func(r *fakeRosterState) { r.agent = true })
	time.Sleep(300 * time.Millisecond)
	svc.mu.Lock()
	conns := svc.conns
	svc.mu.Unlock()
	if conns != 0 {
		t.Fatalf("the box dialled FTW Cloud %d times for an agent it cannot serve", conns)
	}
	if st := h.link.Status(); st.Link == "on" || st.Home != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key exists for an agent the box cannot serve")
	}
}

func TestGrantStreamsSignedStateWithoutNames(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	if kind := <-h.pushed; kind != protocol.PushCloudGranted {
		t.Fatalf("owners were told %q", kind)
	}

	roster := svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	live := svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	svc.waitFor(cloudwire.MsgLP, 5*time.Second)
	svc.waitFor(cloudwire.MsgPrices, 5*time.Second)

	var r cloudwire.Roster
	if err := cloudwire.Unmarshal(roster.B, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Devices) != 1 || strings.Join(r.Devices[0].Scopes, ",") != "ftw.live.read,ftw.events.read" {
		t.Fatalf("roster devices %+v: write scopes must not leave the house", r.Devices)
	}
	var l cloudwire.Live
	if err := cloudwire.Unmarshal(live.B, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Sources) != 2 || l.SrcRev != 1 || l.Fields["2"] != 1200 || len(l.Ages) != 2 {
		t.Fatalf("first live %+v", l)
	}

	// An hb follows within H (S = 4 s, so H = 2 s) when nothing changes.
	svc.waitFor(cloudwire.MsgHB, 4*time.Second)

	for _, raw := range svc.raw {
		for _, name := range []string{meterName, chargerName, lpName} {
			if bytes.Contains(raw, []byte(name)) {
				t.Fatalf("%q left the house in %x", name, raw)
			}
		}
	}
	st := h.link.Status()
	if st.State != "on" || !st.Connected || st.Home != svc.home || st.BytesLast24h == 0 {
		t.Fatalf("status %+v", st)
	}
}

// What the service holds is exactly what the preview shows.
func TestPreviewIsTheBytesThatLeft(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	svc.waitFor(cloudwire.MsgPrices, 5*time.Second)
	svc.waitFor(cloudwire.MsgHB, 4*time.Second)
	h.cancel() // freeze: nothing more is sent
	<-h.done
	h.done <- nil

	latest := map[string][]byte{}
	svc.mu.Lock()
	latest[cloudwire.MsgHello] = svc.hello
	for i, env := range svc.got {
		latest[env.T] = svc.raw[i]
	}
	svc.mu.Unlock()

	p := h.link.Preview()
	if p.Sample || len(p.Frames) == 0 {
		t.Fatalf("preview %+v", p)
	}
	for _, f := range p.Frames {
		if !bytes.Equal(f.Frame, latest[f.T]) {
			t.Fatalf("%s: preview and service differ\npreview %x\nservice %x", f.T, f.Frame, latest[f.T])
		}
		if f.Body == nil {
			t.Fatalf("%s: no decoded body", f.T)
		}
	}
}

func TestPreviewBeforeAGrantIsAnUnsignedSample(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	p := h.link.Preview()
	if !p.Sample || p.State != "off" || len(p.Frames) != 3 {
		t.Fatalf("preview %+v", p)
	}
	for _, f := range p.Frames {
		payload, _ := cloudwire.DecodeFrame(f.Frame)
		env, err := cloudwire.DecodeEnvelope(payload)
		if err != nil || len(env.Sig) != 0 {
			t.Fatalf("%s: sample is signed or broken: %v", f.T, err)
		}
		if bytes.Contains(f.Frame, []byte(meterName)) {
			t.Fatal("a driver name is in the sample")
		}
	}
}

func TestAServiceThatFailsTheBindingGetsOnlyHello(t *testing.T) {
	for name, tweak := range map[string]func(*fakeService){
		"another TLS session": func(s *fakeService) { s.badEKM = true },
		"another key":         func(s *fakeService) { s.wrongKey = true },
	} {
		t.Run(name, func(t *testing.T) {
			svc := newFakeService(t)
			tweak(svc)
			h := newHarness(t, svc)
			h.grant(t)
			time.Sleep(time.Second)
			if got := svc.received(); len(got) != 0 {
				t.Fatalf("the box sent %d messages after a hello_ok that failed", len(got))
			}
			if h.link.Status().Connected {
				t.Fatal("the link reports connected")
			}
		})
	}
}

// A release build knows FTW Cloud at cloud.ftw.energy by one key. A
// service that answers with any other key hears the box's hello and
// nothing after it, and the box does not call itself connected.
func TestAReleaseBuildTrustsOnlyTheServicesKey(t *testing.T) {
	if Endpoint != "wss://cloud.ftw.energy/v1/box" {
		t.Fatalf("release endpoint %q", Endpoint)
	}
	keys, err := parseServiceKeys(ServiceKeys)
	if err != nil || len(keys) != 1 {
		t.Fatalf("release keys: %d, %v", len(keys), err)
	}

	svc := newFakeService(t) // its own key, not the release key
	h := newHarnessKeys(t, svc, nil)
	if !h.link.Available() {
		t.Fatal("a release build says FTW Cloud is unavailable")
	}
	h.grant(t)
	time.Sleep(time.Second)
	svc.mu.Lock()
	dialled := svc.hello != nil
	svc.mu.Unlock()
	if !dialled {
		t.Fatal("the box never said hello, so the key was not what refused it")
	}
	if got := svc.received(); len(got) != 0 {
		t.Fatalf("the box sent %d messages to a service with another key", len(got))
	}
	if h.link.Status().Connected {
		t.Fatal("the link reports connected to a service with another key")
	}
}

func TestUnknownDownlinkClosesTheUplink(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		time.Sleep(200 * time.Millisecond)
		svc.write(conn, nil, home, "cmd", map[string]any{"op": "site.mode.set"})
	}
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatal("the box kept the uplink after a message the service may not send")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchRaisesTheCadence(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})
	}
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	h.site.setGrid(1500)
	start := time.Now()
	svc.waitFor(cloudwire.MsgLive, 3*time.Second)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("a watched change took %s", d)
	}
}

// While a phone watches, a reading goes when its poll lands rather than on
// the next look at the box, and never sooner than a period after the last.
func TestAWatchedReadingGoesAsItsPollLands(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})
	}
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	for i := range 3 {
		time.Sleep(time.Duration(1100+150*i) * time.Millisecond)
		h.site.setGrid(int64(2000 + i))
		start := time.Now()
		h.arrivals <- struct{}{}
		svc.waitFor(cloudwire.MsgLive, 2*time.Second)
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("a watched reading took %s after its poll", d)
		}
	}

	// A second poll right after: its live waits out the period.
	h.site.setGrid(3000)
	start := time.Now()
	h.arrivals <- struct{}{}
	svc.waitFor(cloudwire.MsgLive, 3*time.Second)
	if d := time.Since(start); d < 900*time.Millisecond || d > 1300*time.Millisecond {
		t.Fatalf("the next live went %s after the last, want one period", d)
	}
}

func TestATableChangeGoesAtOnceWithANewRevision(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	h.site.mu.Lock()
	h.site.state = protocol.SourceLagging
	h.site.mu.Unlock()
	// Within the one-second look at the table, well inside the 5 s period.
	env := svc.waitFor(cloudwire.MsgLive, 2*time.Second)
	var l cloudwire.Live
	_ = cloudwire.Unmarshal(env.B, &l)
	if l.SrcRev != 2 || len(l.Sources) != 2 {
		t.Fatalf("table change sent as %+v", l)
	}
}

// Turning the readable copy off stops readings at once, asks the service to
// delete the copy, and forgets the grant on the receipt. The link stays, with
// its key and home id, for the phones' tunnels.
func TestRevokeDeletesTheCopyAndKeepsTheLink(t *testing.T) {
	svc := newFakeService(t)
	svc.purge = true
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	h.grant(t)
	<-h.pushed
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	home := h.link.Status().Home

	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	if kind := <-h.pushed; kind != protocol.PushCloudRevoked {
		t.Fatalf("owners were told %q", kind)
	}
	r := h.nextRoster(t)
	for len(r.Grant.Purposes) != 0 {
		r = h.nextRoster(t)
	}
	if len(r.Grant.ID) != cloudwire.GrantIDBytes || len(r.Grant.Scopes) != 0 {
		t.Fatalf("the revoking roster's grant is %+v", r.Grant)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().State != "off" {
		if time.Now().After(deadline) {
			t.Fatalf("still %+v after the receipt", h.link.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := h.link.Status()
	if st.PurgedAtMs != 1_791_821_000_000 || st.Link != "on" || st.Home != home {
		t.Fatalf("after the receipt: %+v", st)
	}
	if _, err := os.Stat(h.keyPath); err != nil {
		t.Fatal("cloud.key went with the copy")
	}
	if g, _ := loadJSON[grantRecord](h.kv, grantKey); g != nil {
		t.Fatalf("the grant record survived its receipt: %+v", g)
	}
	// After the revoking roster, no reading left the house.
	after := false
	for _, env := range svc.received() {
		switch {
		case env.T == cloudwire.MsgRoster:
			var r cloudwire.Roster
			_ = cloudwire.Unmarshal(env.B, &r)
			after = after || (len(r.Grant.ID) > 0 && len(r.Grant.Purposes) == 0)
		case after && env.T != cloudwire.MsgTun:
			t.Fatalf("the box sent %s after the revoke", env.T)
		}
	}
	// The next roster has no grant at all.
	if r := h.nextRoster(t); len(r.Grant.ID) != 0 || len(r.Grant.Purposes) != 0 {
		t.Fatalf("the roster after the receipt still names a grant: %+v", r.Grant)
	}
}

func TestRevokeWithoutAReceiptKeepsTheGrantRevoking(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if g, _ := loadJSON[grantRecord](h.kv, grantKey); g == nil || !g.Revoked {
		t.Fatalf("grant record before the receipt: %+v", g)
	}
	if st := h.link.Status(); st.State != "revoking" {
		t.Fatalf("state %s", st.State)
	}
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); !errors.Is(err, ErrPurgePending) {
		t.Fatalf("re-grant during a pending purge: %v", err)
	}
}

func TestPastTheBudgetOnlyRosterAndByeGo(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.link.mu.Lock()
	h.link.budget.add(h.link.uptimeMs(), budgetBytes)
	h.link.mu.Unlock()
	h.grant(t)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	time.Sleep(2500 * time.Millisecond)
	for _, env := range svc.received() {
		if env.T != cloudwire.MsgRoster {
			t.Fatalf("%s went past the byte budget", env.T)
		}
	}
	if st := h.link.Status(); !st.BudgetReached {
		t.Fatalf("status %+v", st)
	}
}

func TestAGrantWithoutItsKeyReadsAsOff(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	reopen := func(keyPath string) Status {
		link, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: keyPath,
			ServiceKeys: h.link.opts.ServiceKeys})
		if err != nil {
			t.Fatal(err)
		}
		return link.Status()
	}

	// A restore that brought the grant back without its key.
	if st := reopen(filepath.Join(t.TempDir(), "cloud.key")); st.State != "off" || st.Notice != "key_missing" {
		t.Fatalf("missing key: %+v", st)
	}
	// Or with a key that is not the one the grant was made with.
	other := filepath.Join(t.TempDir(), "cloud.key")
	if _, err := createKey(other); err != nil {
		t.Fatal(err)
	}
	if st := reopen(other); st.State != "off" || st.Notice != "key_mismatch" {
		t.Fatalf("other key: %+v", st)
	}
	if st := reopen(h.keyPath); st.State != "on" {
		t.Fatalf("its own key: %+v", st)
	}
}

func TestGrantRefusesWhatFTWCloudMayNotHold(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	for _, scopes := range [][]string{nil, {protocol.ScopeDispatchWrite}, {protocol.ScopeAssetsRead},
		{protocol.ScopeLiveRead, "ftw.everything"}} {
		if err := h.link.Grant(GrantRequest{Scopes: scopes, Policy: "p"}); !errors.Is(err, ErrGrantRequest) {
			t.Errorf("scopes %v: %v", scopes, err)
		}
	}
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}}); !errors.Is(err, ErrGrantRequest) {
		t.Errorf("no policy: %v", err)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("a refused grant made a key")
	}
}

func TestABuildWithoutAServiceRefusesAGrant(t *testing.T) {
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); !errors.Is(err, ErrNoService) {
		t.Fatalf("grant: %v", err)
	}
	if link.Status().Available {
		t.Fatal("status says FTW Cloud is available")
	}
}

func TestBudgetRollsOver24HoursOfUptime(t *testing.T) {
	var b budget
	b.add(0, 100)
	b.add(60_000*60, 50)
	if got := b.total(60_000 * 60); got != 150 {
		t.Fatalf("total %d", got)
	}
	if got := b.total(60_000 * (budgetMinutes + 1)); got != 50 {
		t.Fatalf("after a day: %d", got)
	}
}

func TestChangeTestsIgnoreAgesAndNoise(t *testing.T) {
	age := int64(5)
	a := cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{{ID: "x", SocAgeS: &age, Manual: &cloudwire.Manual{SinceAgeMs: 1}}},
		Feedback: []cloudwire.Feedback{{Kind: "ev", AgeMs: &age}}}
	older := int64(500)
	b := cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{{ID: "x", SocAgeS: &older, Manual: &cloudwire.Manual{SinceAgeMs: 900}}},
		Feedback: []cloudwire.Feedback{{Kind: "ev", AgeMs: &older}}}
	if lpChanged(a, b) {
		t.Fatal("lp differs only in ages and still counts as changed")
	}
	b.Loadpoints[0].PowerW = 90
	if lpChanged(a, b) {
		t.Fatal("90 W of noise counts as a change")
	}
	b.Loadpoints[0].PowerW = 101
	if !lpChanged(a, b) {
		t.Fatal("a power change past the band does not count")
	}
	b.Loadpoints[0].PowerW = 0
	b.Loadpoints[0].Plugged = true
	if !lpChanged(a, b) {
		t.Fatal("a plug-in does not count")
	}
	s1 := cloudwire.Live{Sources: map[string]cloudwire.Source{"a": {State: "live", LastOkMs: 1}}}
	s2 := cloudwire.Live{Sources: map[string]cloudwire.Source{"a": {State: "live", LastOkMs: 9}}}
	if tableKey(s1) != tableKey(s2) {
		t.Fatal("lastOkMs moves the table")
	}
}

// Composing reads the box. The link looks at the source table once a second
// and at the loadpoints once a period, however fast it ticks.
func TestTheLinkReadsTheBoxNoFasterThanItCanSend(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc) // ticks every 20 ms
	h.grant(t)
	svc.waitFor(cloudwire.MsgLP, 5*time.Second)
	h.site.mu.Lock()
	h.site.liveCalls, h.site.lpCalls = 0, 0
	h.site.mu.Unlock()
	time.Sleep(3 * time.Second)
	h.site.mu.Lock()
	live, lp := h.site.liveCalls, h.site.lpCalls
	h.site.mu.Unlock()
	if live > 5 || lp > 1 {
		t.Fatalf("in 3 s the link read live %d times and loadpoints %d times", live, lp)
	}
}

// --------------------------------------------------------------------------
// Session information for phones
// --------------------------------------------------------------------------

// A phone learns the box's key, home id, boot and names over Noise. With them
// it verifies what the service forwards, anchors its times and names its
// sources.
func TestSessionInfoLetsAPhoneCheckAndNameWhatTheServiceForwards(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if s := h.link.SessionInfo(); s != nil {
		t.Fatalf("session information with FTW Cloud off: %+v", s)
	}
	h.grant(t)
	env := svc.waitFor(cloudwire.MsgLive, 5*time.Second)

	s := h.link.SessionInfo()
	if s == nil {
		t.Fatal("no session information with FTW Cloud on")
	}
	if len(s.PK) != cloudwire.PublicKeyBytes || s.Home != cloudwire.HomeID(s.PK) || s.Home != svc.home {
		t.Fatalf("key %x and home %q; the service saw home %q", s.PK, s.Home, svc.home)
	}
	pub, err := cloudwire.ParsePublicKey(s.PK)
	if err != nil || cloudwire.Verify(pub, s.Home, env) != nil {
		t.Fatalf("a forwarded live does not verify with the session key: %v", err)
	}
	var live cloudwire.Live
	if err := cloudwire.Unmarshal(env.B, &live); err != nil {
		t.Fatal(err)
	}
	if len(s.Boot) != cloudwire.BootBytes || !bytes.Equal(s.Boot, live.Boot) {
		t.Fatalf("session boot %x, live boot %x", s.Boot, live.Boot)
	}

	gk := h.link.current().g.GrantKey
	wantSources := map[string]string{
		cloudwire.SourceID(gk, meterName):   meterName,
		cloudwire.SourceID(gk, chargerName): chargerName,
	}
	if !maps.Equal(s.Sources, wantSources) {
		t.Fatalf("sources %v, want %v", s.Sources, wantSources)
	}
	for id := range live.Sources {
		if s.Sources[id] == "" {
			t.Fatalf("live names source %q, which the session cannot name", id)
		}
	}
	if want := map[string]string{cloudwire.LoadpointID(gk, lpName): lpName}; !maps.Equal(s.Loadpoints, want) {
		t.Fatalf("loadpoints %v, want %v", s.Loadpoints, want)
	}
}

// Turning the readable copy off withdraws the names at once, before the
// service confirms the deletion. The home id stays, for the tunnel.
func TestSessionInfoKeepsTheHomeWithoutTheGrant(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	if s := h.link.SessionInfo(); s == nil || len(s.Sources) == 0 {
		t.Fatalf("session information with FTW Cloud on: %+v", s)
	}
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	s := h.link.SessionInfo()
	if s == nil || s.Home != h.link.Status().Home || len(s.Sources) != 0 || len(s.Loadpoints) != 0 ||
		h.link.Status().State != "revoking" {
		t.Fatalf("while revoking: %+v", s)
	}
}

func TestAvailableFollowsTheServiceKeys(t *testing.T) {
	svc := newFakeService(t)
	if !newHarness(t, svc).link.Available() {
		t.Fatal("a build with a service key says FTW Cloud is unavailable")
	}
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if link.Available() {
		t.Fatal("a build without a service key says FTW Cloud is available")
	}
}

// --------------------------------------------------------------------------
// Statistics
// --------------------------------------------------------------------------

func (h *harness) record(t *testing.T) grantRecord {
	t.Helper()
	g, err := loadJSON[grantRecord](h.kv, grantKey)
	if err != nil || g == nil {
		t.Fatalf("grant record: %v", err)
	}
	return *g
}

func (h *harness) nextRoster(t *testing.T) cloudwire.Roster {
	t.Helper()
	var r cloudwire.Roster
	if err := cloudwire.Unmarshal(h.svc.waitFor(cloudwire.MsgRoster, 3*time.Second).B, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Under Cloud, statistics come with the current terms. Under older terms they
// wait: asking for them is refused rather than added beside the old terms.
func TestStatisticsComeWithTheCurrentCloudTerms(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Stats(StatsRequest{}); !errors.Is(err, ErrGrantRequest) {
		t.Fatalf("statistics without a policy: %v", err)
	}
	h.grant(t)
	first := h.nextRoster(t)
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy, By: "app:aaaa1111"}); !errors.Is(err, ErrCloudTermsFirst) {
		t.Fatalf("statistics under older terms: %v", err)
	}
	if g := h.record(t); len(g.StatsID) != 0 || strings.Join(g.Purposes, ",") != "serve" {
		t.Fatalf("older terms gained statistics: %+v", g)
	}
	if st := h.link.Status(); !st.StatisticsNeedTerms || st.Statistics || h.link.SharesStatistics() {
		t.Fatalf("status under older terms: %+v", st)
	}

	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if strings.Join(r.Grant.Purposes, ",") != "serve,stats" || r.RosterSeq <= first.RosterSeq || len(r.Grant.StatsID) != statsIDBytes {
		t.Fatalf("roster under the current terms: %+v", r.Grant)
	}
	if st := h.link.Status(); !st.Statistics || !st.StatisticsRequired || st.StatisticsNeedTerms || !h.link.SaidYesToStatistics() {
		t.Fatalf("status under the current terms: %+v", st)
	}

	// A restart reads it back.
	reopened, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: h.keyPath,
		ServiceKeys: h.link.opts.ServiceKeys})
	if err != nil {
		t.Fatal(err)
	}
	if st := reopened.Status(); strings.Join(st.Purposes, ",") != "serve,stats" || !st.Statistics {
		t.Fatalf("after a restart: %+v", st)
	}
	for _, env := range svc.received() {
		if env.T == "stats" {
			t.Fatal("the box sent statistics before the service approved them")
		}
	}
}

// While a Cloud that was turned off waits for its deletion receipt, the box's
// own answer can still change. It touches nothing in the grant being deleted.
func TestTheLocalAnswerChangesWhileTheCopyIsBeingDeleted(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	if err := h.link.Stats(StatsRequest{Policy: "p"}); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("an unknown policy: %v", err)
	}
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy}); err != nil {
		t.Fatalf("statistics while revoking: %v", err)
	}
	if g := h.record(t); !g.Revoked || len(g.StatsID) != 0 || !h.link.SharesStatistics() {
		t.Fatalf("grant while revoking: %+v", g)
	}
	if err := h.link.StopStats("lan"); err != nil || h.link.SharesStatistics() {
		t.Fatalf("off while revoking: %v", err)
	}
}

// A stored grant counts only when the link honours it. With a cloud.key that
// is not the one the grant was made with, the box's own answer stands, and a
// box that only shares by default has said no yes.
func TestOnlyAGrantTheLinkHonoursCounts(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	if err := h.kv.SaveConfig(statsShareKey, "default"); err != nil {
		t.Fatal(err)
	}
	if !h.link.SaidYesToStatistics() {
		t.Fatal("the current terms are not a yes")
	}
	if err := os.Remove(h.keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := createKey(h.keyPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: h.keyPath, ServiceKeys: h.link.opts.ServiceKeys})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Status().State != "off" || reopened.SaidYesToStatistics() || !reopened.SharesStatistics() {
		t.Fatalf("a grant made with another key counted: %+v", reopened.Status())
	}
}

// Without Cloud the statistics answer is the box's own, so a build with no
// service still records it. It still names the policy the owner read.
func TestABuildWithoutAServiceStillRecordsTheLocalAnswer(t *testing.T) {
	kv := &memKV{}
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: kv,
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Stats(StatsRequest{Policy: "p"}); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("an unknown policy: %v", err)
	}
	if err := link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy}); err != nil || !SharesStatistics(kv) {
		t.Fatalf("statistics: %v", err)
	}
}

// The release pins FTW Cloud's offer key, and the pin parses under its kid.
func TestReleasePinsTheCloudOfferKey(t *testing.T) {
	got := ParseOfferKeys(OfferKeys)
	pub, ok := got["s0xLrCPjFa-AoMMl"]
	if len(got) != 1 || !ok || protocol.OfferKid(pub) != "s0xLrCPjFa-AoMMl" {
		t.Fatalf("OfferKeys pins %d keys: %v", len(got), got)
	}
}

// A pinned offer key counts only under the kid its key gives it.
func TestOfferKeysPinOnlyUnderTheirOwnKid(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := elliptic.Marshal(elliptic.P256(), key.X, key.Y) //nolint:staticcheck // SEC1 is the pinned form.
	b64 := base64.RawURLEncoding.EncodeToString(pub)
	kid := protocol.OfferKid(pub)
	got := ParseOfferKeys(kid + ":" + b64 + ", wrongkid:" + b64 + ", " + kid + ":not-base64!")
	if len(got) != 1 || !bytes.Equal(got[kid], pub) {
		t.Fatalf("pinned %v", got)
	}
}
