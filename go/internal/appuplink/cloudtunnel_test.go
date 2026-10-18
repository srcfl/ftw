package appuplink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The box's half of the one door, end to end: the real cloud link, the real
// tunnel table and the real message handler, with the relay unreachable. The
// service here is FTW Cloud cut to what the tunnel needs, as srcfl/ftw-cloud
// runs it: the handshake, the roster, and tun stamped with the device id of
// the roster entry whose key the phone presented.

type miniCloud struct {
	t   *testing.T
	key *ecdsa.PrivateKey
	srv *httptest.Server

	mu       sync.Mutex
	conn     *websocket.Conn
	home     string
	roster   *cloudwire.Roster
	got      []string
	toApp    chan cloudwire.Tun
	rostered chan struct{}
	wmu      sync.Mutex
}

func newMiniCloud(t *testing.T) *miniCloud {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := &miniCloud{t: t, key: key, toApp: make(chan cloudwire.Tun, 64), rostered: make(chan struct{}, 8)}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(c.serve))
	c.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	c.srv.StartTLS()
	t.Cleanup(c.srv.Close)
	return c
}

func (c *miniCloud) endpoint() string { return "wss://" + c.srv.Listener.Addr().String() + "/v1/box" }

func (c *miniCloud) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.srv.Certificate())
	return pool
}

func (c *miniCloud) write(conn *websocket.Conn, key *ecdsa.PrivateKey, home, t string, body any) error {
	env, err := cloudwire.Seal(key, home, t, body)
	if err != nil {
		return err
	}
	raw, _ := env.Encode()
	frame, _ := cloudwire.EncodeFrame(raw)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (c *miniCloud) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(cloudwire.MaxTunFrame)
	ekm, _ := r.TLS.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
	nonce := bytes.Repeat([]byte{9}, cloudwire.NonceBytes)
	if c.write(conn, nil, "", cloudwire.MsgChallenge, cloudwire.Challenge{V: 1, Nonce: nonce}) != nil {
		return
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	payload, _ := cloudwire.DecodeFrame(msg)
	env, _ := cloudwire.DecodeEnvelope(payload)
	var hello cloudwire.Hello
	if env.T != cloudwire.MsgHello || cloudwire.Unmarshal(env.B, &hello) != nil {
		c.t.Error("the box opened with no hello")
		return
	}
	pub, err := cloudwire.ParsePublicKey(hello.PK)
	home := cloudwire.HomeID(hello.PK)
	if err != nil || cloudwire.Verify(pub, home, env) != nil || !bytes.Equal(hello.EKM, ekm) {
		c.t.Error("hello did not check out")
		return
	}
	if c.write(conn, c.key, home, cloudwire.MsgHelloOK, cloudwire.HelloOK{
		BoxNonce: hello.BoxNonce, EKM: ekm, Have: map[string]string{}}) != nil {
		return
	}
	c.mu.Lock()
	c.conn, c.home = conn, home
	c.mu.Unlock()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		payload, err := cloudwire.DecodeFrame(msg)
		if err != nil {
			c.t.Errorf("box frame: %v", err)
			return
		}
		env, err := cloudwire.DecodeEnvelope(payload)
		if err != nil {
			c.t.Errorf("box envelope: %v", err)
			return
		}
		c.mu.Lock()
		c.got = append(c.got, env.T)
		c.mu.Unlock()
		switch env.T {
		case cloudwire.MsgTun:
			var t cloudwire.Tun
			if cloudwire.Unmarshal(env.B, &t) == nil {
				c.toApp <- t
			}
		case cloudwire.MsgRoster:
			var ro cloudwire.Roster
			if cloudwire.Verify(pub, home, env) == nil && cloudwire.Unmarshal(env.B, &ro) == nil {
				c.mu.Lock()
				c.roster = &ro
				c.mu.Unlock()
				c.rostered <- struct{}{}
			}
		}
	}
}

// fromApp is a phone's tun: the service finds the roster entry by the key
// the phone's own handshake with it proved, and names the device from it.
// A key the roster does not list with tun gets nothing forwarded.
func (c *miniCloud) fromApp(key []byte, sid uint32, d []byte) bool {
	c.mu.Lock()
	conn, home, ro := c.conn, c.home, c.roster
	c.mu.Unlock()
	if conn == nil || ro == nil {
		return false
	}
	for _, dev := range ro.Devices {
		if dev.Tun && bytes.Equal(dev.Key, key) {
			return c.write(conn, nil, home, cloudwire.MsgTun, cloudwire.Tun{Dev: dev.ID, Sid: sid, D: d}) == nil
		}
	}
	return false
}

// forward sends a tun to the box as it is, with no roster check: a service
// that forwards what it should not.
func (c *miniCloud) forward(dev string, sid uint32, d []byte) {
	c.mu.Lock()
	conn, home := c.conn, c.home
	c.mu.Unlock()
	_ = c.write(conn, nil, home, cloudwire.MsgTun, cloudwire.Tun{Dev: dev, Sid: sid, D: d})
}

func (c *miniCloud) toAppNext(t *testing.T) cloudwire.Tun {
	t.Helper()
	select {
	case m := <-c.toApp:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no tun from the box")
		return cloudwire.Tun{}
	}
}

type quietSite struct{}

func (quietSite) Live() cloudlink.Live             { return cloudlink.Live{Fields: map[string]int64{"2": 1200}} }
func (quietSite) Loadpoints() (cloudwire.LP, bool) { return cloudwire.LP{}, false }
func (quietSite) Prices() (cloudwire.Prices, bool) { return cloudwire.Prices{}, false }

// memState is the state store's two calls the link makes.
type memState struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memState) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

func (k *memState) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

// With the relay unreachable, a phone the roster lists sends a command
// through FTW Cloud and gets the box's answer. The readable copy stays off:
// nothing but the roster and the tunnel leaves the box.
func TestACommandThroughFTWCloudWithTheRelayGone(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	modes := &countingModes{}
	cmds := appproto.NewCmdLog()
	// The link is built after the uplink, as cmd/ftw builds it, and bound
	// late, as there.
	var bound atomic.Pointer[cloudlink.Link]
	build := func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:  stubSite{}, Info: stubInfo{}, Modes: modes, Plans: stubPlans{}, Codec: codec{},
			Sender: s, Caller: caller, Grants: grants, Commands: cmds,
			Cloud: func() *appproto.CloudSession {
				if l := bound.Load(); l != nil {
					return l.SessionInfo()
				}
				return nil
			},
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
		})
	}
	uplink, err := New(Options{Enroll: enroll, Handler: build, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	cloud := newMiniCloud(t)
	link, err := cloudlink.New(cloudlink.Options{
		Site: quietSite{}, Roster: CloudRoster{Enroll: enroll, Tunnel: true}, KV: &memState{m: map[string]string{}},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), Build: "test", Tunnel: uplink.Tunnel(),
		Endpoint: cloud.endpoint(), ServiceKeys: []*ecdsa.PublicKey{&cloud.key.PublicKey}, RootCAs: cloud.roots(),
		Tick: 20 * time.Millisecond, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink.Tunnel().Attach(link)
	bound.Store(link)
	enroll.OnChange(link.RosterChanged)
	enroll.OnPairingCode(link.RosterChanged)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = uplink.Run(ctx) }()
	go func() { _ = link.Run(ctx) }()

	// The phone paired earlier; the box links for it.
	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enroll.Authorise(app.staticPublic(), code); err != nil {
		t.Fatal(err)
	}
	link.RosterChanged()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-cloud.rostered:
		case <-deadline:
			t.Fatal("the box never sent a roster with the phone")
		}
		cloud.mu.Lock()
		ok := cloud.roster != nil && len(cloud.roster.Devices) == 1 && cloud.roster.Devices[0].Tun
		cloud.mu.Unlock()
		if ok {
			break
		}
	}

	dev := cloudwire.DeviceID(app.staticPublic())
	m1, err := app.message1(enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cloud.fromApp(app.staticPublic(), 1, m1) {
		t.Fatal("the service would not forward the phone's handshake")
	}
	reply := cloud.toAppNext(t)
	if reply.Dev != dev || reply.Sid != 1 {
		t.Fatalf("the handshake's answer is %+v", reply)
	}
	if err := app.readMessage2(reply.D); err != nil {
		t.Fatal(err)
	}
	sendFrame := func(msgType string, body any) {
		t.Helper()
		raw, _ := appproto.Marshal(body)
		frame, _ := appwire.EncodeFrame(appwire.Frame{Lane: appwire.LaneControl, Bucket: 512,
			Envelope: appwire.Envelope{T: msgType, B: raw}})
		msg, _ := app.encrypt(frame)
		if !cloud.fromApp(app.staticPublic(), 1, msg) {
			t.Fatalf("the service would not forward %s", msgType)
		}
	}
	receiveFrame := func(want string) appwire.Frame {
		t.Helper()
		for {
			m := cloud.toAppNext(t)
			plain, err := app.decrypt(m.D)
			if err != nil {
				t.Fatal(err)
			}
			f, _ := appwire.DecodeFrame(plain)
			if f.Envelope.T == want {
				return f
			}
		}
	}
	sendFrame(appproto.MsgHello, appproto.Hello{Proto: appproto.ProtoRange{Max: appproto.ProtoMax},
		App: appproto.AppInfo{Build: "test", UA: "ios"}})
	var ok appproto.HelloOK
	if err := appproto.Unmarshal(receiveFrame(appproto.MsgHelloOK).Envelope.B, &ok); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	sendFrame(appproto.MsgCmd, setModeCmd("0192f2a0-7c1e-7000-8000-00000000c10d"))
	receiveFrame(appproto.MsgCmdAck)
	var res appproto.CmdResult
	if err := appproto.Unmarshal(receiveFrame(appproto.MsgCmdResult).Envelope.B, &res); err != nil {
		t.Fatal(err)
	}
	t.Logf("cmd to result through the cloud link: %s", time.Since(start))
	if res.State != appproto.CmdApplied || modes.count() != 1 {
		t.Fatalf("result %q after %d mode changes", res.State, modes.count())
	}

	// A stranger's handshake, forwarded anyway, as a broken or hostile
	// service might: the box itself answers nothing and opens nothing.
	stranger, _ := newAppClient()
	sm1, _ := stranger.message1(enroll.StaticKey().Public(), nil)
	strangerDev := cloudwire.DeviceID(stranger.staticPublic())
	cloud.forward(strangerDev, 1, sm1)
	quiet := time.After(500 * time.Millisecond)
wait:
	for {
		select {
		case m := <-cloud.toApp:
			// The phone's own session goes on beside it.
			if m.Dev == strangerDev {
				t.Fatalf("the box answered a stranger: sid %d, %d bytes", m.Sid, len(m.D))
			}
		case <-quiet:
			break wait
		}
	}
	if uplink.Tunnel().count() != 1 {
		t.Fatal("a stranger's handshake changed the box's sessions")
	}

	// The readable copy is off: the box sent the roster and the tunnel,
	// and nothing to read.
	cloud.mu.Lock()
	got := append([]string(nil), cloud.got...)
	cloud.mu.Unlock()
	for _, typ := range got {
		if typ != cloudwire.MsgRoster && typ != cloudwire.MsgTun {
			t.Fatalf("%s left a box whose readable copy is off", typ)
		}
	}
	if st := link.Status(); st.State != "off" || st.Link != "on" {
		t.Fatalf("status %+v", st)
	}
	if ok.Cloud == nil || ok.Cloud.Home != link.Status().Home || len(ok.Cloud.Sources) != 0 {
		t.Fatalf("hello_ok.cloud %+v", ok.Cloud)
	}
}
