// Package cloudlink is the box's one link to FTW Cloud. It carries the
// phones' own sessions with the box (the tunnel) and, when an owner has
// turned it on, a signed, readable copy of the box's state.
// docs/protocol-cloud.md is the wire format and docs/adr/0008-ftw-cloud.md
// the decision.
//
// FTW Cloud never chooses an action. This package holds that line in its
// build: it reaches the box only through the read-only ports below and the
// Tunnel and Exchange ports, and may import nothing from the box (CI checks
// it with go list -deps). What the service may send back is a closed set of
// seven messages. Five ask the box for nothing but to send sooner, send again
// or forget a key. tun carries a phone's Noise session, which this package
// hands to the Tunnel unread, and x a watch's exchange with the box, which it
// hands to the Exchange unread.
//
// The box links when it first shows a pairing code, has a phone that may
// open a tunnel, or an owner grants the readable copy. A link record or a
// grant whose key fingerprint does not match cloud.key reads as off.
package cloudlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Compile-time settings. Production builds use the defaults; a local test
// build may set them with -ldflags -X. A box that can be pointed at another
// service at runtime is a box a support call can point at an attacker's.
var (
	// Endpoint is the service's uplink.
	Endpoint = "wss://cloud.ftw.energy/v1/box"
	// ServiceKeys are the service's P-256 public keys, base64url of the
	// 65-byte point, comma separated. Up to two, so the key can rotate
	// across a release (docs/protocol-cloud.md, "Keys and ids"). Empty
	// means no service exists for this build: the box refuses a grant and
	// never dials.
	//
	// The one key is cloud.ftw.energy's, in Secrets Manager as
	// ftw-cloud/service-key since srcfl/ftw-cloud#3. Only its public half is
	// here; a box checks every hello_ok and downlink message against it.
	ServiceKeys = "BG6EvBxFzSHPVsQOES6PYaKFo5vHQmte9lJ6D5OBIT9JgcBrJAOxAucdqKXxS76pToQ7orI1Ivsno0OtxbMuliQ"
	// DevRootCA is a PEM file of extra roots, for a local service with its
	// own certificate. Empty in every release build.
	DevRootCA = ""
	// OfferKeys are FTW Cloud's offer keys (docs/agents.md, The grant
	// offer): kid:base64url of the 65-byte P-256 point, comma separated, as
	// https://cloud.ftw.energy/.well-known/ftw-offer-key publishes them.
	// An entry whose kid is not protocol.OfferKid of its key is dropped.
	// Empty means this build opens no offer on the box page.
	//
	// The one key is cloud.ftw.energy's, alias/ftw-cloud-offer-key in KMS
	// (eu-north-1), made 17 October 2026. Its private half never leaves KMS.
	OfferKeys = "s0xLrCPjFa-AoMMl:BEUBb8ul2T01OWM_f1rX6h8Z0yFf8BkvEtUxdHsc_FXGtKv8arbcDZcPeeVOoX6nIxdP2SKYpHky9KSw-cqH-8g"
)

// ParseOfferKeys reads OfferKeys into kid → SEC1 point. A malformed entry is
// left out, so a typo opens nothing rather than everything.
func ParseOfferKeys(list string) map[string][]byte {
	out := map[string][]byte{}
	for _, entry := range strings.Split(list, ",") {
		kid, b64, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || kid == "" {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if _, err := protocol.ParseApprovalKey(pub); err != nil || kid != protocol.OfferKid(pub) {
			continue
		}
		out[kid] = pub
	}
	return out
}

// ErrNoService is a grant on a build that names no service key.
var ErrNoService = errors.New("cloudlink: this build has no FTW Cloud service")

// ErrPurgePending is a grant asked for while the last one is still being
// deleted at the service.
var ErrPurgePending = errors.New("cloudlink: FTW Cloud is still deleting the previous copy")

// ErrNotGranted is statistics asked for while FTW Cloud is off.
var ErrNotGranted = errors.New("cloudlink: turn FTW Cloud on first")

// ErrLinkEnding is a grant asked for while the service deletes the last
// link's home. The next link comes once the receipt is in.
var ErrLinkEnding = errors.New("cloudlink: FTW Cloud is still deleting the previous link")

// statsIDBytes is the length of a statsId, which the protocol fixes at 16.
const statsIDBytes = 16

// Site is the box's state, read. Every method answers from memory and changes
// nothing. Names in the answers are the box's own driver and loadpoint names;
// the link replaces each with an opaque id before anything leaves the house.
type Site interface {
	// Live is the frozen fields and the source table.
	Live() Live
	// Loadpoints is loadpoint state and control feedback. ok is false on a
	// box without loadpoints, which then sends no lp.
	Loadpoints() (lp cloudwire.LP, ok bool)
	// Prices is the price window from now. ok is false when the box has no
	// prices.
	Prices() (p cloudwire.Prices, ok bool)
}

// CarSite is a Site that keeps cars (docs/cars.md). Cars is every car in
// the box's order, with its names, which the link leaves at home
// (ProjectCars). ok is false while the box keeps no cars. A Site without it
// sends no car class.
type CarSite interface {
	Cars() (cars []protocol.Car, ok bool)
}

// Live is the live class before ids are cut.
type Live struct {
	// Fields uses the app protocol's frozen ids as text keys.
	Fields map[string]int64
	// FieldSources names, per field, the drivers its freshness follows.
	FieldSources map[string][]string
	// Sources is keyed by driver name.
	Sources map[string]Source
	// DispatchBlockedBy names drivers whose staleness stops dispatch.
	DispatchBlockedBy []string
}

// Source is one driver's freshness.
type Source struct {
	Kind  string
	State protocol.SourceState
	// LastOkUptimeMs is box uptime at the last good reading, negative when
	// there was none.
	LastOkUptimeMs int64
	StaleAfterMs   int64
}

// RosterDevice is one paired phone.
type RosterDevice struct {
	ID    string
	Key   []byte
	Role  string
	Epoch uint64
	// Scopes are what the role carries, expanded by the box.
	Scopes []string
	// Tun says the device may open a session to the box through FTW Cloud.
	Tun bool
	// Agent is an agent row's grant, nil for a phone or a watch.
	Agent *cloudwire.Agent
}

// Roster lists the paired phones and the pairing code on the box's screen.
type Roster interface {
	Devices() []RosterDevice
	// Pending is the QR pairing code the box shows now and when it stops
	// working. ok is false when none is shown. A spoken code is never
	// listed: its hash would let the service find it.
	Pending() (code []byte, expires time.Time, ok bool)
	// Noise is the box's Noise static key, for FTW Cloud's keys part.
	Noise() []byte
}

type RosterPending struct {
	Hash    []byte
	Kind    string
	Expires time.Time
}

type RosterSnapshot struct {
	Devices []RosterDevice
	Noise   []byte
	Pending []RosterPending
}

func (l *Link) rosterSnapshot(home string) RosterSnapshot {
	if r, ok := l.opts.Roster.(interface{ Snapshot(string) RosterSnapshot }); ok {
		return r.Snapshot(home)
	}
	s := RosterSnapshot{Devices: l.opts.Roster.Devices(), Noise: l.opts.Roster.Noise()}
	if code, expires, ok := l.opts.Roster.Pending(); ok {
		s.Pending = append(s.Pending, RosterPending{Hash: cloudwire.PairingHash(home, code), Expires: expires})
	}
	return s
}

// Tunnel serves the phones' sessions that FTW Cloud carries. The box's app
// session table implements it; the link reaches it only through this port,
// and hands it each tun from the service unread.
type Tunnel interface {
	// Deliver takes one tun from the service. It must not block: the link
	// reads the service on the same goroutine.
	Deliver(t cloudwire.Tun)
	// Drop ends every session the uplink carried, when the uplink ends.
	Drop()
}

// Exchange answers the watches' requests that FTW Cloud carries (ADR 0009).
// The box's exchange table implements it; the link hands it each x from the
// service unread.
type Exchange interface {
	// Deliver takes one x from the service. It must not block: the link
	// reads the service on the same goroutine.
	Deliver(x cloudwire.X)
}

// WebPushSource renders and encrypts the notices the service may send when the box is away.
type WebPushSource interface {
	WebPushRows() ([]cloudwire.WebPushRow, error)
}

// Options wires a link. Site, Roster, KV and KeyPath are required.
type Options struct {
	Site    Site
	WebPush WebPushSource
	Roster  Roster
	KV      KV
	KeyPath string
	Build   string
	// Started anchors uptime, the clock every age is measured against.
	Started time.Time
	// ClockSynced reports whether NTP has set the wall clock. Nil reads as
	// not synced, and wallMs is then left out.
	ClockSynced func() bool
	// Notify tells owners the grant changed, with protocol.PushCloudGranted
	// or PushCloudRevoked. Nil sends nothing.
	Notify func(kind string)
	// Tunnel serves the phones' sessions. Nil takes no tun: the box does not
	// list tun in hello.caps, and the service sends it none.
	Tunnel Tunnel
	// Exchange answers the watches. Nil, or no Tunnel, takes no x: the box
	// does not list x in hello.caps, and the service sends it none.
	Exchange Exchange
	// Arrivals hears each finished driver poll (telemetry.Store.Arrivals).
	// While a phone watches, live goes when a poll lands rather than on the
	// next check. Nil leaves live on its checks alone.
	Arrivals <-chan struct{}
	Logger   *slog.Logger

	// Seams for tests. Production leaves them zero, and races QUIC against
	// the WebSocket. A test that names its Endpoint gets the WebSocket alone
	// unless it sets QUIC; QUICAddr names the service's UDP address when it
	// is not the Endpoint's host and port.
	QUIC        bool
	QUICAddr    string
	Endpoint    string
	ServiceKeys []*ecdsa.PublicKey
	RootCAs     *x509.CertPool
	Tick        time.Duration
	NetDial     func(ctx context.Context, network, addr string) (net.Conn, error)
	Now         func() time.Time
}

// Link is the box's one uplink to FTW Cloud.
type Link struct {
	invitePreparingUntil time.Time
	needsRevision        uint64
	opts                 Options
	log                  *slog.Logger
	boot                 []byte

	// wake is signalled when the grant or the roster changes, so the
	// sender acts within one tick.
	wake chan struct{}

	mu                 sync.Mutex
	seq                uint64
	key                *ecdsa.PrivateKey
	pk                 []byte
	home               string
	link               *linkRecord
	grant              *grantRecord
	disabled           bool
	notice             string
	connected          bool
	serviceStatsPolicy string
	// transport is how the uplink travels, or last travelled: quic or
	// websocket. winners is the race's memory, the transport that opened
	// first on each network.
	transport string
	quicOn    bool
	winners   map[string]winner
	// tickets and quicTickets hold the service's TLS tickets, one cache for
	// each transport, so a reconnect resumes and, over QUIC, sends its
	// target as early data. A ticket from TLS over TCP cannot carry 0-RTT
	// into QUIC, nor the other way.
	tickets     tls.ClientSessionCache
	quicTickets tls.ClientSessionCache
	// upAt is when the service's hello_ok arrived on this connection.
	upAt     time.Time
	lastSend time.Time
	budget   budget
	// tunOut and tunBulk carry framed tun messages, lane 0 and bulk, to
	// the connection's tun writer. Nil while no uplink is up. tunBudget counts what left, apart from
	// the readable copy's budget, and tunDropped what never did.
	tunOut    chan []byte
	tunBulk   chan []byte
	tunBudget budget
	// xBudget counts the watches' exchange answers apart, as well as in
	// tunBudget once they leave.
	xBudget    budget
	tunDropped int64
	sent       map[string]sentFrame
	srcRev     uint64
	table      map[string]cloudwire.Source
	tableKey   string
	liveSeq    uint64
	lpSeq      uint64
	resyncAt   time.Time
	attempt    int
	// shardHops counts wrong-shard closes since the box was last
	// connected, and hostFails dials in a row that failed at the kept
	// host (linkRecord.Host), since hostFailedSince.
	shardHops       int
	hostFails       int
	hostFailedSince time.Time
	purgedAt        int64
	rosterDue       bool

	// events wait for the writer; eventsDropped counts those that never
	// left. closing is set by Bye, after which the link does not dial
	// again.
	events        []EventIn
	eventsDropped int64
	// notices wait for the uplink; see Notice. noticesDropped counts those
	// a full queue pushed out.
	notices        []notice
	noticesDropped int64

	closing bool
	byeWhy  string
	byeDone chan struct{}
}

// New loads the grant and the key and returns a link that runs once Run is
// called.
func New(opts Options) (*Link, error) {
	if opts.Site == nil || opts.Roster == nil || opts.KV == nil || opts.KeyPath == "" {
		return nil, errors.New("cloudlink: site, roster, state store and key path are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	quicOn := opts.QUIC || opts.Endpoint == ""
	if opts.Endpoint == "" {
		opts.Endpoint = Endpoint
	}
	if opts.ServiceKeys == nil {
		keys, err := parseServiceKeys(ServiceKeys)
		if err != nil {
			return nil, err
		}
		opts.ServiceKeys = keys
	}
	if opts.RootCAs == nil && DevRootCA != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pemBytes, err := os.ReadFile(DevRootCA)
		if err != nil {
			return nil, fmt.Errorf("cloudlink: reading dev root CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("cloudlink: %s holds no certificate Go can use", DevRootCA)
		}
		opts.RootCAs = pool
	}
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick <= 0 {
		opts.Tick = 250 * time.Millisecond
	}
	boot, err := randomBytes(cloudwire.BootBytes)
	if err != nil {
		return nil, err
	}
	l := &Link{
		opts: opts, log: opts.Logger.With("component", "cloudlink"), boot: boot,
		wake: make(chan struct{}, 1), sent: map[string]sentFrame{}, quicOn: quicOn,
		tickets: tls.NewLRUClientSessionCache(4), quicTickets: tls.NewLRUClientSessionCache(4),
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func parseServiceKeys(list string) ([]*ecdsa.PublicKey, error) {
	var keys []*ecdsa.PublicKey
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("cloudlink: service key: %w", err)
		}
		pub, err := cloudwire.ParsePublicKey(raw)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pub)
	}
	if len(keys) > 2 {
		return nil, errors.New("cloudlink: at most two service keys")
	}
	return keys, nil
}

// load reads the link, the grant and the key. A record without its key, or
// with another key, reads as off and leaves a notice.
func (l *Link) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.link, l.grant, l.key, l.pk, l.home, l.notice = nil, nil, nil, nil, "", ""
	disabled, _ := l.opts.KV.LoadConfig(disabledKey)
	l.disabled = disabled == "true"
	if r, err := loadJSON[receipt](l.opts.KV, purgedKey); err == nil && r != nil {
		l.purgedAt = r.AtMs
	}
	link, err := loadJSON[linkRecord](l.opts.KV, linkKey)
	if err != nil {
		l.notice = "link_unreadable"
		l.log.Warn("FTW Cloud link unreadable; treating it as off", "err", err)
		link = nil
	}
	g, err := loadJSON[grantRecord](l.opts.KV, grantKey)
	if err != nil {
		l.notice = "grant_unreadable"
		l.log.Warn("FTW Cloud grant unreadable; treating it as off", "err", err)
		g = nil
	}
	if link == nil && g == nil {
		return nil
	}
	key, err := loadKey(l.opts.KeyPath)
	if err != nil {
		l.notice = "key_missing"
		l.log.Warn("FTW Cloud is off: the link has no key on this box", "err", err)
		return nil
	}
	fp, pk, err := fingerprint(key)
	if err != nil {
		l.notice = "key_mismatch"
		return nil
	}
	if link == nil {
		// A grant made before the link existed holds the key it was made
		// with. That key becomes the link's, so the home id stays.
		if g.KeyFp != fp {
			l.notice = "key_mismatch"
			l.log.Warn("FTW Cloud is off: cloud.key is not the key the grant was made with")
			return nil
		}
		link = &linkRecord{KeyFp: fp}
		if err := saveJSON(l.opts.KV, linkKey, link); err != nil {
			return err
		}
	}
	if link.KeyFp != fp {
		l.notice = "key_mismatch"
		l.log.Warn("FTW Cloud is off: cloud.key is not the key the link was made with")
		return nil
	}
	if l.disabled {
		link.Ending = true
		if g != nil {
			g.Revoked = true
		}
	}
	l.link, l.key, l.pk, l.home = link, key, pk, cloudwire.HomeID(pk)
	switch {
	case g == nil:
	case g.KeyFp != fp:
		l.notice = "key_mismatch"
		l.log.Warn("the FTW Cloud grant was made with another key; the readable copy is off")
	default:
		l.grant = g
	}
	return nil
}

// signal wakes the sender. It never blocks.
func (l *Link) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// RosterChanged is called by enrolment after a pair, role change, revoke or
// a pairing code shown. A box that now has a phone to reach it, or a code on
// its screen, links at once, so the code's home id exists before the code
// is drawn.
func (l *Link) RosterChanged() {
	l.mu.Lock()
	l.needsRevision++
	l.mu.Unlock()
	if l.wantsLink() {
		l.mu.Lock()
		if err := l.linkLocked(); err != nil && !errors.Is(err, ErrNoService) {
			l.log.Warn("could not link to FTW Cloud", "err", err)
		}
		l.mu.Unlock()
	}
	l.mu.Lock()
	l.rosterDue = true
	l.mu.Unlock()
	l.signal()
}

// wantsLink reports whether anything needs the link: a phone that may open a
// tunnel, an agent an owner let in, or a pairing code on the screen. An agent
// row has no tunnel, but it reaches the box only through FTW Cloud, which
// learns of it from the roster; an owner with no phone grants one on the box
// page. A box that takes no tun cannot serve an agent, so its rows want no
// link. A grant needs the link too, and makes it itself.
func (l *Link) wantsLink() bool {
	l.mu.Lock()
	blocked := l.disabled || (l.grant != nil && l.grant.statisticsOnly())
	preparing := time.Now().Before(l.invitePreparingUntil)
	l.mu.Unlock()
	if blocked {
		return false
	}
	if preparing {
		return true
	}
	snapshot := l.rosterSnapshot("")
	if len(snapshot.Pending) > 0 {
		return true
	}
	for _, d := range snapshot.Devices {
		if d.Tun || (d.Agent != nil && l.opts.Tunnel != nil) {
			return true
		}
	}
	return false
}

// linkLocked makes the link record when there is none, around the
// cloud.key on disk if there is one, so its home id stays, or a new key. A
// link waiting for its end to be confirmed stays as it is: the next one
// comes after the receipt, under a new home id. The caller holds l.mu.
func (l *Link) linkLocked() error {
	if l.link != nil {
		return nil
	}
	if len(l.opts.ServiceKeys) == 0 {
		return ErrNoService
	}
	key, err := loadKey(l.opts.KeyPath)
	switch {
	case err == nil:
		l.log.Info("FTW Cloud links with the cloud.key already on this box")
	case errors.Is(err, os.ErrNotExist):
		if key, err = createKey(l.opts.KeyPath); err != nil {
			return err
		}
	default:
		// A file that is not a key is not ours to replace.
		return err
	}
	fp, pk, err := fingerprint(key)
	if err != nil {
		return err
	}
	rec := &linkRecord{KeyFp: fp}
	if err := saveJSON(l.opts.KV, linkKey, rec); err != nil {
		return err
	}
	// A grant on disk made with another key stays off; the grant load
	// left it nil.
	l.link, l.key, l.pk, l.home, l.notice = rec, key, pk, cloudwire.HomeID(pk), ""
	l.table, l.tableKey, l.sent, l.rosterDue = nil, "", map[string]sentFrame{}, true
	l.log.Info("audit: FTW Cloud linked", "home", l.home)
	l.signal()
	return nil
}

// endIfIdle starts the end of a link nothing needs any more: no phone may
// open a tunnel, no code is shown and the readable copy is off. The box asks
// the service to delete the home and keeps the key until the receipt.
func (l *Link) endIfIdle() {
	l.mu.Lock()
	idle := l.link != nil && !l.link.Ending && l.grant == nil
	revision := l.needsRevision
	l.mu.Unlock()
	if !idle || l.wantsLink() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link == nil || l.link.Ending || l.grant != nil || l.needsRevision != revision || time.Now().Before(l.invitePreparingUntil) {
		return
	}
	rec := *l.link
	rec.Ending = true
	if err := saveJSON(l.opts.KV, linkKey, rec); err != nil {
		l.log.Warn("could not store the end of the FTW Cloud link", "err", err)
		return
	}
	l.link = &rec
	l.log.Info("audit: FTW Cloud link ending; nothing on the box needs it", "home", l.home)
}

// GrantRequest is an owner's request to turn FTW Cloud on.
type GrantRequest struct {
	Scopes []string `json:"scopes"`
	// Policy names the consent text the owner read.
	Policy string `json:"policy"`
	// By is the device or door that asked, for the audit line.
	By string `json:"-"`
}

// Grant turns the readable copy on, or changes the scopes of a grant already
// on. It links the box first when it is not linked.
func (l *Link) Grant(req GrantRequest) error {
	if len(l.opts.ServiceKeys) == 0 {
		return ErrNoService
	}
	scopes, err := checkScopes(req.Scopes)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Policy) == "" {
		return fmt.Errorf("%w: name the policy the owner accepted", ErrGrantRequest)
	}

	l.mu.Lock()
	switch {
	case l.grant != nil && l.grant.Revoked:
		l.mu.Unlock()
		return ErrPurgePending
	case l.link != nil && l.link.Ending:
		l.mu.Unlock()
		return ErrLinkEnding
	case l.grant != nil:
		g := *l.grant
		fresh := g.Policy != req.Policy
		g.Scopes, g.Policy, g.By = scopes, req.Policy, req.By
		g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeServe, true)
		if req.Policy == cloudwire.CloudTermsPolicy {
			if err := l.includeStatistics(&g, fresh); err != nil {
				l.mu.Unlock()
				return err
			}
		}
		if err := l.enableLinkChoice(); err != nil {
			l.mu.Unlock()
			return err
		}
		if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
			l.mu.Unlock()
			return err
		}
		l.grant = &g
		l.rosterDue = true
		l.mu.Unlock()
		l.audit("changed", &g)
		l.signal()
		return nil
	}
	if err := l.linkLocked(); err != nil {
		l.mu.Unlock()
		return err
	}
	id, err := randomBytes(cloudwire.GrantIDBytes)
	if err != nil {
		l.mu.Unlock()
		return err
	}
	gk, err := randomBytes(cloudwire.GrantKeyBytes)
	if err != nil {
		l.mu.Unlock()
		return err
	}
	g := grantRecord{
		ID: id, Scopes: scopes, Purposes: []string{protocol.CloudPurposeServe},
		KeyFp: l.link.KeyFp, GrantKey: gk, By: req.By, Policy: req.Policy,
		Controller: cloudwire.ControllerSourceful,
	}
	if l.clockSynced() {
		g.AtMs = time.Now().UnixMilli()
	}
	if req.Policy == cloudwire.CloudTermsPolicy {
		if err := l.includeStatistics(&g, true); err != nil {
			l.mu.Unlock()
			return err
		}
	}
	if err := l.enableLinkChoice(); err != nil {
		l.mu.Unlock()
		return err
	}

	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		l.mu.Unlock()
		return err
	}
	l.grant, l.notice = &g, ""
	l.table, l.tableKey, l.sent, l.rosterDue = nil, "", map[string]sentFrame{}, true
	l.mu.Unlock()

	l.audit("granted", &g)
	l.notify(protocol.PushCloudGranted)
	l.signal()
	return nil
}

// Revoke turns the readable copy off. The box stops sending readings at
// once, and its next roster asks the service to delete the copy. The grant
// record stays until the service's signed receipt arrives. The link, the
// home id and the tunnel stay.
func (l *Link) Revoke(by string) error {
	l.mu.Lock()
	whole := l.grant != nil && (l.grant.Policy == cloudwire.CloudTermsPolicy || l.grant.statisticsOnly())
	l.mu.Unlock()
	if whole {
		return l.End(by)
	}
	l.mu.Lock()
	if l.grant == nil || l.grant.Revoked {
		l.mu.Unlock()
		return nil
	}
	g := *l.grant
	g.Revoked, g.By = true, by
	if l.clockSynced() {
		g.RevokedAtMs = time.Now().UnixMilli()
	}
	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		l.mu.Unlock()
		return err
	}
	l.grant = &g
	l.rosterDue = true
	l.mu.Unlock()

	l.audit("revoked", &g)
	l.notify(protocol.PushCloudRevoked)
	l.signal()
	return nil
}

// StatsRequest is an owner's request to contribute to statistics.
type StatsRequest struct {
	// Policy names the consent text the owner read.
	Policy string `json:"policy"`
	// By is the device or door that asked, for the audit line.
	By string `json:"-"`
}

// Stats turns statistics on. For a box without FTW Cloud that is the one
// answer on the box, and nothing links to Cloud. Under a grant already on it
// also adds the stats purpose; the first time, it draws the statsId that
// names this box's rows at the service, and later calls keep it. It sends no
// statistics itself.
func (l *Link) Stats(req StatsRequest) error {
	if strings.TrimSpace(req.Policy) == "" {
		return fmt.Errorf("%w: name the policy the owner accepted", ErrGrantRequest)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.grant == nil || l.grant.Revoked || l.disabled:
		// The box's own answer, which needs no grant: also while a Cloud
		// that was turned off waits for its deletion receipt.
		if req.Policy != cloudwire.StatsPolicy {
			return ErrNotGranted
		}
		if err := l.opts.KV.SaveConfig(statsShareKey, "on"); err != nil {
			return err
		}
		l.log.Info("audit: statistics on", "by", req.By)
		return nil
	case cloudUnderOlderTerms(l.grant):
		return ErrCloudTermsFirst
	case len(l.opts.ServiceKeys) == 0:
		return ErrNoService
	}
	if err := l.opts.KV.SaveConfig(statsShareKey, "on"); err != nil {
		return err
	}
	g := *l.grant
	if len(g.StatsID) == 0 || g.StatsPolicy != req.Policy {
		id, err := randomBytes(statsIDBytes)
		if err != nil {
			return err
		}
		g.StatsID = id
	}
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, true)
	g.StatsPolicy, g.By = req.Policy, req.By
	if g.StatsAtMs == 0 || l.grant.StatsPolicy != req.Policy {
		g.StatsAtMs = 0
		if l.clockSynced() {
			g.StatsAtMs = l.opts.Now().UnixMilli()
		}
	}
	return l.storeLocked(g, "statistics on")
}

// StopStats turns statistics off for a box without FTW Cloud, removes the
// stats purpose and forgets the statsId. Turning it on again draws a new one.
// With Cloud under the current terms statistics are part of Cloud.
func (l *Link) StopStats(by string) error {
	l.mu.Lock()
	if cloudRequiresStatistics(l.grant) {
		l.mu.Unlock()
		return ErrCloudNeedsStatistics
	}
	only := l.grant != nil && l.grant.statisticsOnly()
	l.mu.Unlock()
	if err := l.opts.KV.SaveConfig(statsShareKey, "off"); err != nil {
		return err
	}
	l.log.Info("audit: statistics off", "by", by)
	if only {
		if err := l.End(by); err != nil {
			return err
		}
		return l.opts.KV.SaveConfig(statsDecisionKey, cloudwire.StatsPolicy)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.opts.KV.SaveConfig(statsDecisionKey, cloudwire.StatsPolicy); err != nil {
		return err
	}
	if l.grant == nil || l.grant.Revoked ||
		(len(l.grant.StatsID) == 0 && !slices.Contains(l.grant.Purposes, protocol.CloudPurposeStats)) {
		return nil
	}
	g := *l.grant
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, false)
	g.StatsID, g.StatsPolicy, g.StatsAtMs, g.By = nil, "", 0, by
	return l.storeLocked(g, "statistics off")
}

// storeLocked saves a changed grant, marks the roster due and writes the
// audit line. The caller holds l.mu.
func (l *Link) storeLocked(g grantRecord, event string) error {
	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		return err
	}
	l.grant = &g
	l.rosterDue = true
	l.audit(event, &g)
	l.signal()
	return nil
}

// withPurpose returns purposes with p present or absent, in registry order.
// It never changes the slice it was given.
func withPurpose(purposes []string, p string, on bool) []string {
	out := []string{}
	for _, q := range protocol.CloudPurposes {
		if (q == p && on) || (q != p && slices.Contains(purposes, q)) {
			out = append(out, q)
		}
	}
	return out
}

// finishCopy forgets a revoked grant once the service has confirmed that its
// copy is gone. It reports whether the receipt was the one the box waits for.
func (l *Link) finishCopy(atMs int64, grantID []byte) bool {
	l.mu.Lock()
	g := l.grant
	if g == nil || !g.Revoked || !bytes.Equal(g.ID, grantID) {
		l.mu.Unlock()
		return false
	}
	if err := saveJSON(l.opts.KV, purgedKey, receipt{What: cloudwire.PurgedCopy, AtMs: atMs, GrantID: g.ID}); err != nil {
		l.log.Warn("could not store the FTW Cloud purge receipt", "err", err)
	}
	if err := saveJSON(l.opts.KV, grantKey, nil); err != nil {
		l.log.Warn("could not clear the FTW Cloud grant", "err", err)
	}
	l.grant, l.purgedAt = nil, atMs
	l.table, l.tableKey, l.sent, l.rosterDue = nil, "", map[string]sentFrame{}, true
	l.mu.Unlock()
	l.audit("purged", g)
	l.signal()
	return true
}

// finishLink deletes the key and the link once the service has confirmed
// that the home is gone.
func (l *Link) finishLink() {
	l.mu.Lock()
	if l.link == nil || !l.link.Ending {
		l.mu.Unlock()
		return
	}
	home := l.home
	for _, key := range []string{linkKey, grantKey} {
		if err := saveJSON(l.opts.KV, key, nil); err != nil {
			l.log.Warn("could not clear the FTW Cloud link", "err", err)
		}
	}
	if err := os.Remove(l.opts.KeyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.log.Warn("could not delete cloud.key", "err", err)
	}
	l.link, l.grant, l.key, l.pk, l.home = nil, nil, nil, nil, ""
	l.table, l.tableKey, l.sent = nil, "", map[string]sentFrame{}
	l.mu.Unlock()
	l.log.Info("audit: FTW Cloud link ended", "home", home)
}

// audit writes the line that records a grant change on the box.
func (l *Link) audit(event string, g *grantRecord) {
	l.log.Info("audit: FTW Cloud grant "+event,
		"grant", hex.EncodeToString(g.ID), "by", g.By, "scopes", g.Scopes,
		"purposes", g.Purposes, "policy", g.Policy, "stats_policy", g.StatsPolicy)
}

func (l *Link) notify(kind string) {
	if l.opts.Notify != nil {
		go l.opts.Notify(kind)
	}
}

func (l *Link) clockSynced() bool {
	return l.opts.ClockSynced != nil && l.opts.ClockSynced()
}

// Status is what GET /api/cloud shows.
type Status struct {
	CloudPolicy                string `json:"cloudPolicy"`
	CloudTermsAccepted         bool   `json:"cloudTermsAccepted"`
	StatisticsOnly             bool   `json:"statisticsOnly"`
	StatisticsDecisionRequired bool   `json:"statisticsDecisionRequired"`
	// Statistics is the one answer for both anonymous daily reports, and
	// StatisticsRequired says it is part of FTW Cloud, with no off switch.
	Statistics         bool `json:"statistics"`
	StatisticsRequired bool `json:"statisticsRequired"`
	// StatisticsNeedTerms is Cloud on under older terms: statistics wait
	// until the owner accepts the current ones.
	StatisticsNeedTerms bool `json:"statisticsNeedTerms"`
	// State is the readable copy's: off, on or revoking.
	State            string `json:"state"`
	StatisticsPolicy string `json:"statisticsPolicy"`
	StatisticsActive bool   `json:"statisticsActive"`
	StatisticsReady  bool   `json:"statisticsReady"`
	// Link is off, on or ending: whether the box is linked, which the
	// phones' tunnels need and the readable copy rides on.
	Link string `json:"link"`
	// Notice says why a link or grant on disk reads as off: key_missing,
	// key_mismatch, link_unreadable or grant_unreadable.
	Notice      string   `json:"notice,omitempty"`
	Available   bool     `json:"available"`
	Home        string   `json:"home,omitempty"`
	Scopes      []string `json:"scopes"`
	Purposes    []string `json:"purposes"`
	GrantedAtMs int64    `json:"grantedAtMs,omitempty"`
	GrantedBy   string   `json:"grantedBy,omitempty"`
	Policy      string   `json:"policy,omitempty"`
	Connected   bool     `json:"connected"`
	// Transport is how the uplink travels, the registry's cloud_transports:
	// quic or websocket. Empty before the first connection.
	Transport string `json:"transport,omitempty"`
	// LastSendAgeMs is how long ago the box last sent anything, -1 never.
	LastSendAgeMs int64 `json:"lastSendAgeMs"`
	// BytesLast24h counts what the box sent in the last 24 hours of uptime.
	BytesLast24h  int64 `json:"bytesLast24h"`
	BudgetBytes   int64 `json:"budgetBytes"`
	BudgetReached bool  `json:"budgetReached"`
	// PurgedAtMs is the time on the service's receipt for the last deletion.
	PurgedAtMs int64 `json:"purgedAtMs,omitempty"`
	// EventsDropped counts notifications that never reached the service:
	// the queue was full, or the uplink was down for longer than a phone
	// would still act on them.
	EventsDropped int64 `json:"eventsDropped"`
	// NoticesDropped counts security notices a full queue pushed out
	// before the uplink took them. The box's own transports carried them.
	NoticesDropped int64 `json:"noticesDropped"`
	// TunBytesLast24h counts the tunnel's bytes, apart from the readable
	// copy's, against TunBudgetBytes. TunDropped counts tun messages that
	// never left: no uplink, a full queue or the budget reached.
	TunBytesLast24h int64 `json:"tunBytesLast24h"`
	TunBudgetBytes  int64 `json:"tunBudgetBytes"`
	TunDropped      int64 `json:"tunDropped"`
}

// Status reports the grant and the link.
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Status{
		State: "off", Notice: l.notice, Available: len(l.opts.ServiceKeys) > 0,
		Scopes: []string{}, Purposes: []string{}, Connected: l.connected, Transport: l.transport,
		LastSendAgeMs: -1, PurgedAtMs: l.purgedAt, EventsDropped: l.eventsDropped, NoticesDropped: l.noticesDropped,
		BytesLast24h: l.budget.total(l.uptimeMs()), BudgetBytes: budgetBytes,
		Link: "off", TunBytesLast24h: l.tunBudget.total(l.uptimeMs()), TunBudgetBytes: tunBudgetBytes,
		TunDropped: l.tunDropped,
	}
	s.CloudPolicy = cloudwire.CloudTermsPolicy
	// Nobody is asked any more: statistics are part of Cloud, and on for a
	// box without it until its owner turns them off. The field stays false
	// for clients that still read it.
	share, _ := l.opts.KV.LoadConfig(statsShareKey)
	g := l.grant
	if l.disabled {
		g = nil
	}
	s.Statistics = sharesStatistics(g, share)
	s.StatisticsRequired = cloudRequiresStatistics(g)
	s.StatisticsNeedTerms = cloudUnderOlderTerms(g)
	s.StatisticsOnly = l.grant.statisticsOnly()
	s.CloudTermsAccepted = l.grant != nil && !l.grant.Revoked && l.grant.Policy == cloudwire.CloudTermsPolicy
	s.StatisticsPolicy = cloudwire.StatsPolicy
	s.StatisticsReady = l.connected && l.serviceStatsPolicy == cloudwire.StatsPolicy
	if g := l.grant; g != nil {
		s.StatisticsActive = s.Statistics && !g.Revoked && g.StatsAtMs > 0 && g.StatsPolicy == cloudwire.StatsPolicy && len(g.StatsID) == 16 && slices.Contains(g.Purposes, protocol.CloudPurposeStats)
	}
	s.BudgetReached = s.BytesLast24h >= budgetBytes
	if !l.lastSend.IsZero() {
		s.LastSendAgeMs = time.Since(l.lastSend).Milliseconds()
	}
	if l.link != nil {
		s.Link, s.Home = "on", l.home
		if l.link.Ending {
			s.Link = "ending"
		}
	}
	if g := l.grant; g != nil {
		s.State, s.Scopes, s.Purposes = "on", g.Scopes, g.Purposes
		s.GrantedAtMs, s.GrantedBy, s.Policy = g.AtMs, g.By, g.Policy
		if g.statisticsOnly() {
			s.State = "statistics"
		}
		if g.Revoked {
			s.State = "revoking"
		}
	}
	return s
}

// Home is the home id at FTW Cloud while the box is linked, and "" when it
// is not or its link is ending. A pairing QR that names the home reads it.
func (l *Link) Home() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link == nil || l.link.Ending || l.disabled || l.grant.statisticsOnly() {
		return ""
	}
	return l.home
}

// Available reports whether this build names a service, and so can run the
// uplink at all. Release builds without service keys cannot.
func (l *Link) Available() bool { return len(l.opts.ServiceKeys) > 0 }

// TakesTun reports whether the link carries the phones' own sessions as tun,
// so a phone that knows the home id can reach the box without the relay.
func (l *Link) TakesTun() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Available() && l.opts.Tunnel != nil && !l.disabled && !l.grant.statisticsOnly()
}

// TakesX reports whether the link carries the watches' exchange as x, which
// it does only beside the tunnel: a watch's roster entry opens its door
// through tun.
func (l *Link) TakesX() bool { return l.TakesTun() && l.opts.Exchange != nil }

// SessionInfo is what a phone needs, over its Noise session with the box, to
// reach this home through FTW Cloud: the home id, the key that signs the
// readable copy, the boot that anchors its times and names for the opaque
// ids. It is nil while the box is not linked or its link is ending, and its
// tables are empty while the readable copy is off.
//
// The tables name every driver and loadpoint the box knows now. One that
// appears later has no name on a phone until its next session.
func (l *Link) SessionInfo() *protocol.CloudSession {
	if l.Home() == "" {
		return nil
	}
	snap := l.current()
	if snap.link == nil || snap.link.Ending || len(snap.pk) != cloudwire.PublicKeyBytes {
		return nil
	}
	s := &protocol.CloudSession{
		PK: bytes.Clone(snap.pk), Home: snap.home, Boot: bytes.Clone(l.boot),
		Sources: map[string]string{}, Loadpoints: map[string]string{},
	}
	if !snap.copyOn() {
		return s
	}
	gk := snap.g.GrantKey
	source := func(name string) {
		if name != "" {
			s.Sources[cloudwire.SourceID(gk, name)] = name
		}
	}
	live := l.opts.Site.Live()
	for name := range live.Sources {
		source(name)
	}
	for _, names := range live.FieldSources {
		for _, name := range names {
			source(name)
		}
	}
	for _, name := range live.DispatchBlockedBy {
		source(name)
	}
	if lp, ok := l.opts.Site.Loadpoints(); ok {
		for _, p := range lp.Loadpoints {
			s.Loadpoints[cloudwire.LoadpointID(gk, p.ID)] = p.ID
			source(p.Src)
		}
		for _, f := range lp.Feedback {
			source(f.Src)
		}
	}
	return s
}

func (l *Link) uptimeMs() int64 { return time.Since(l.opts.Started).Milliseconds() }

// header stamps the next box body. Seq rises per message, per boot.
func (l *Link) header() cloudwire.Header {
	l.seq++
	h := cloudwire.Header{Seq: l.seq, Boot: l.boot, UptimeMs: l.uptimeMs()}
	if l.clockSynced() {
		w := time.Now().UnixMilli()
		h.WallMs = &w
	}
	return h
}

// grantSnapshot is the link, the grant and the key as one consistent read.
type grantSnapshot struct {
	link *linkRecord
	g    *grantRecord
	key  *ecdsa.PrivateKey
	pk   []byte
	home string
}

func (l *Link) current() grantSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return grantSnapshot{link: l.link, g: l.grant, key: l.key, pk: l.pk, home: l.home}
}

// copyOn reports whether the readable copy is on and not being withdrawn.
func (g *grantRecord) statisticsOnly() bool {
	return g != nil && !g.Revoked && slices.Contains(g.Purposes, protocol.CloudPurposeStats) && !slices.Contains(g.Purposes, protocol.CloudPurposeServe)
}
func (snap grantSnapshot) copyOn() bool {
	return snap.g != nil && !snap.g.Revoked && slices.Contains(snap.g.Purposes, protocol.CloudPurposeServe)
}

// grantID is the grant hello names: the readable copy's, or none.
func (snap grantSnapshot) grantID() []byte {
	if snap.g == nil || snap.g.Revoked {
		return nil
	}
	return snap.g.ID
}

// Run keeps the uplink up while the box is linked, until ctx ends.
func (l *Link) Run(ctx context.Context) error {
	for {
		if l.isClosing() {
			return nil
		}
		snap := l.current()
		if snap.link == nil && l.wantsLink() {
			// The first link, or the next one after an end was confirmed.
			l.mu.Lock()
			if err := l.linkLocked(); err != nil && !errors.Is(err, ErrNoService) {
				l.log.Warn("could not link to FTW Cloud", "err", err)
			}
			l.mu.Unlock()
			snap = l.current()
		}
		if snap.link == nil || len(l.opts.ServiceKeys) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.wake:
				continue
			}
		}
		delay, err := l.once(ctx, snap)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			l.log.Warn("FTW Cloud uplink dropped; retrying", "retry_in", delay, "err", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Link) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// allowed reports whether the grant lets a class leave the house. Nothing
// readable leaves while the copy is off or being withdrawn.
func (snap grantSnapshot) allowed(class string) bool {
	scope, forwarded := cloudwire.ClassScope[class]
	if !forwarded || !snap.copyOn() {
		return false
	}
	return scope == "" || snap.g.has(scope)
}

// caps is the classes this box sends under the grant, the roster, and tun
// when the box takes the tunnel.
func (l *Link) caps(snap grantSnapshot) []string {
	var out []string
	_, cars := l.opts.Site.(CarSite)
	_, history := l.opts.Site.(HistorySite)
	for _, c := range []string{cloudwire.MsgLive, cloudwire.MsgLP, cloudwire.MsgCar, cloudwire.MsgPrices, cloudwire.MsgHB, cloudwire.MsgEvent, cloudwire.MsgTiles} {
		if snap.allowed(c) && (c != cloudwire.MsgCar || cars) && (c != cloudwire.MsgTiles || history) {
			out = append(out, c)
		}
	}
	out = append(out, cloudwire.MsgRoster, cloudwire.MsgNotice)
	if l.opts.Tunnel != nil {
		out = append(out, cloudwire.CapTun)
		if l.opts.Exchange != nil {
			out = append(out, cloudwire.CapX)
		}
	}
	return append(out, cloudwire.CapShard)
}

// tlsConfig is TLS 1.3 only, with the system roots unless a test or a dev
// build adds its own.
func (l *Link) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: l.opts.RootCAs, ClientSessionCache: l.tickets}
}

// quicTLSConfig is tlsConfig with QUIC's own ticket cache.
func (l *Link) quicTLSConfig() *tls.Config {
	c := l.tlsConfig()
	c.ClientSessionCache = l.quicTickets
	return c
}

// now is the wall clock, or a test's.
func (l *Link) now() time.Time { return l.opts.Now() }

// PrepareAgentInviteHome allocates the home for an owner who creates an
// invitation before pairing a phone. No readable-copy consent changes.
func (l *Link) PrepareAgentInviteHome() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.Available() || l.opts.Tunnel == nil || l.disabled || l.grant.statisticsOnly() {
		return "", protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "cloud"})
	}
	if err := l.linkLocked(); err != nil {
		return "", err
	}
	if l.link.Ending {
		return "", protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "cloud"})
	}
	l.invitePreparingUntil = time.Now().Add(30 * time.Second)
	l.needsRevision++
	return l.home, nil
}
