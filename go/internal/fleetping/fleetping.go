// Package fleetping adds anonymous reports to FTW's daily fleet totals without
// telling the relay which box sent which report.
//
// It answers one question, and it is the owner's: how many households run FTW,
// on which version, with which drivers, so engineering effort lands where the
// fleet actually is. It runs against the constraint in docs/architecture.md:
// the daily totals must not give the relay a stable household record.
// It also says, coarsely, how the box ran over the last day: how slow its
// slowest plan was, whether plans fell back, how often driver polls failed and
// whether an update failed, so the fleet's trouble is found where it is.
//
// So one ping is a picture of one box's shape and carries nothing that joins
// two pings together:
//
//   - no gateway ID, no site identity key, no Noise static key, no MAC, no
//     site name, no driver instance name, no serial and no hostname. Not
//     hashed, not truncated — absent. A hash of a stable value is a stable
//     value, and a salted one still matches itself tomorrow;
//   - no counter, no sequence number and no "last sent" time, because any of
//     them would let two pings be ordered into one box's series;
//   - buckets, not values. 12.4 kWh of battery and 197 days since install are
//     close to unique across a fleet this size; "5-15" and "6-12m" are not;
//   - a send time that moves. A box that pings at 03:17 every night has told
//     the endpoint its name in a field nobody thought to look at.
//
// Sent straight to an endpoint, the source IP is what this cannot fix: the
// endpoint sees it, and a household often keeps one for months. A release
// that names a relay (package statsrelay) sends the report to FTW Cloud
// through it instead, sealed with Oblivious HTTP, so FTW Cloud reads the
// report without the address and the relay sees the address without the
// report. The sealed request also carries a random number drawn new for each
// report, and the day it was sent (package reportwire); neither joins two
// pings. Without a relay, this design makes the *payload* useless as an
// identifier; it does not make the connection anonymous. The drifting send time is what stops arrival time
// becoming a second selector alongside it — though the drift is a random walk
// in six-hour steps, so it takes about four days to spread across the clock,
// and until then one box's arrival times are still loosely related.
//
// The residual risk worth naming: the payload is still a quasi-identifier. A
// beta box in DK2 with 30+ kWh and an unusual driver pair may be the only one
// like it, and two such pings could be guessed to be the same box. Coarse
// buckets, a capped driver list and a small field set are what keep that
// population large; nothing here pretends it is a proof.
package fleetping

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/protocol/fleetwire"
)

// Schema names the payload shape so the endpoint can read old pings after the
// fields change. It is the one constant string in the message.
const Schema = fleetwire.Schema

const (
	// period is the nominal gap between pings.
	period = 24 * time.Hour

	// spread is how far either side of period a gap may land. Half a day is
	// wide enough that the time of day performs a random walk across the
	// clock within a week rather than settling into a slot, and the mean
	// stays at one ping a day.
	spread = 6 * time.Hour

	// sendTimeout bounds one attempt. Generous, because there is no retry:
	// a slow endpoint should still be given a chance to answer.
	sendTimeout = 30 * time.Second

	// maxDrivers caps the reported driver list. Nobody runs sixteen driver
	// types; the cap is here so a config that does cannot become a signature
	// by its length alone.
	maxDrivers = 16

	// maxResponseBytes is how much of the reply is drained before the body is
	// closed. The box takes no instruction from this endpoint, so the reply is
	// read only far enough to let the connection be reused and discarded.
	maxResponseBytes = 4 << 10
)

// Facts is what one box knows about itself, before any of it is coarsened.
// The caller fills it; this package decides what may leave the house.
type Facts struct {
	// Version is the running binary's version, e.g. "v1.4.0" or "v1.4.0-beta.2".
	// Anything that is not a release tag — a dev build, a local hack — reports
	// as unknown rather than as itself, because a one-off build string names
	// the person who made it.
	Version string

	// Drivers are the driver types this site has configured, as the catalogue
	// names them ("sungrow", "easee_cloud"). Never the operator's own name for
	// a driver: that field is free text and reads "sungrow-vasagatan" on real
	// sites.
	Drivers []string

	// Catalogue is the driver names FTW published: ShippedCatalogue, which is
	// generated from drivers/BUNDLED_SOURCE.json when the binary is built, plus
	// the files the box's install history says arrived under FTW's signature,
	// asked one file at a time. A driver the config names that is not in it
	// reports as "other", because a driver file the household put there can be
	// called anything at all, including the household's own name.
	//
	// Neither half is the contents of a directory, and that is the point: a
	// config field says where installed drivers are kept, so any list
	// discovered by walking a path is a list the household can add to.
	//
	// Filled by fleetCatalogue in go/cmd/ftw, which is where the argument for
	// what belongs in this list lives.
	Catalogue []string

	// BatteryWh is the installed battery capacity across the site.
	BatteryWh float64

	// PriceZone is the configured bidding zone. Kept only when it is a zone
	// the price catalogue knows, so a hand-edited value cannot ride out.
	PriceZone string

	// InstalledAt is when this box first ran FTW.
	InstalledAt time.Time

	// Health is how the box ran lately.
	Health Health
}

// Health is how the box ran lately, before it is coarsened. Every field is a
// sum over the whole box: never a driver's name, a plan's time or an
// update's version.
type Health struct {
	// Plans is how many plans the planner kept in the last 24 hours,
	// SlowestSolve the longest any of them took, and Fallbacks how many were
	// a fallback plan.
	Plans        int
	SlowestSolve time.Duration
	Fallbacks    int

	// Polls is how many driver polls ran in the last 24 hours, all drivers
	// together, and PollErrors how many failed. Both stay zero while the
	// count covers too little of the day to say anything.
	Polls, PollErrors uint64

	// Update is the outcome of the newest update in the last 7 days: "ok",
	// "failed", or empty without one.
	Update string
}

// Payload is the whole ping. Every field is a bucket or a label shared with
// thousands of other boxes; there is deliberately no field a household could
// be looked up by, and no field that says which ping came first.
type Payload = fleetwire.Report

// Build coarsens one box's facts into the message that may leave it.
//
// Every field is narrowed here rather than at the call site, so there is one
// place to read when the question is "what does a ping actually contain".
func Build(f Facts, now time.Time) Payload {
	version, channel := releaseVersion(f.Version)
	return Payload{
		Schema:    Schema,
		Version:   version,
		Channel:   channel,
		Drivers:   driverTypes(f.Drivers, f.Catalogue),
		Battery:   batteryBucket(f.BatteryWh),
		Zone:      knownZone(f.PriceZone),
		Age:       ageBucket(f.InstalledAt, now),
		Solve:     solveBucket(f.Health),
		Fallbacks: fallbackBucket(f.Health),
		Errors:    errorBucket(f.Health),
		Update:    updateBucket(f.Health.Update),
	}
}

// The health buckets answer where engineering time goes: is the planner fast
// enough on the hardware boxes run, does it fall back, do drivers fail, do
// updates. Each is three or four words wide and describes the last day, or
// for updates the last week, so it changes as the box runs instead of
// becoming one more fixed trait of the household. Most boxes share the
// common answer, which is what keeps the rarer ones in a crowd.

// solveBucket is the slowest plan of the last 24 hours. Five seconds is the
// edge that matters: past it a small box's planner runs out of its budget.
func solveBucket(h Health) string {
	switch s := h.SlowestSolve; {
	case h.Plans <= 0:
		return "none"
	case s < time.Second:
		return "0-1s"
	case s < 5*time.Second:
		return "1-5s"
	default:
		return "5s+"
	}
}

// fallbackBucket is how many of the last 24 hours' plans fell back.
func fallbackBucket(h Health) string {
	switch {
	case h.Plans <= 0:
		return "none"
	case h.Fallbacks <= 0:
		return "0"
	case h.Fallbacks <= 3:
		return "1-3"
	default:
		return "4+"
	}
}

// errorBucket is the share of the last 24 hours' driver polls that failed.
// A share, not a count, so a driver that polls every second and one that
// polls every minute land in the same bucket for the same trouble. None is
// no polls, or too few hours of them, as just after a restart.
func errorBucket(h Health) string {
	switch {
	case h.Polls == 0:
		return "none"
	case h.PollErrors == 0:
		return "0"
	case h.PollErrors*100 < h.Polls:
		return "0-1%"
	default:
		return "1%+"
	}
}

// updateBucket is the newest update of the last 7 days. A box that does not
// update itself says none, like a box that had no update, so how a box was
// installed does not become one more field.
func updateBucket(outcome string) string {
	switch outcome {
	case "ok", "failed":
		return outcome
	}
	return "none"
}

// releaseTag matches the two shapes this project publishes: vX.Y.Z and
// vX.Y.Z-beta.N, and nothing looser.
//
// The distinction is the whole point. The default build stamps
// `git describe --tags --always --dirty`, so any tagged tree with an uncommitted
// file becomes "v1.4.0-dirty", and a developer's tag becomes "v1.4.0-fredrik".
// Both look like a release and neither is one; a stable near-unique
// string in the payload is exactly the value that lets two pings be joined,
// which is the one thing this package exists to prevent.
var releaseTag = regexp.MustCompile(`^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-beta\.[0-9]{1,4})?$`)

func releaseVersion(v string) (version, channel string) {
	v = strings.TrimSpace(v)
	if !releaseTag.MatchString(v) {
		return "unknown", "unknown"
	}
	_, pre, hasPre := strings.Cut(v, "-")
	switch {
	case !hasPre:
		return v, "stable"
	case strings.HasPrefix(pre, "beta"):
		return v, "beta"
	}
	// Unreachable against the pattern above, and deliberately not a branch that
	// returns v. There is no default that lets an unrecognised string travel:
	// widening the pattern must be a decision somebody makes here, not a
	// fall-through it inherits.
	return "unknown", "unknown"
}

// driverTypes returns the sorted, deduplicated set of driver types, with
// anything the catalogue does not name collapsed into one "other".
//
// Sorted because the order drivers appear in a config is itself a detail of
// that config; deduplicated because two Sungrow inverters are one answer to
// "which drivers are in use" and the count would narrow the household.
func driverTypes(configured, catalogue []string) []string {
	known := make(map[string]bool, len(catalogue))
	for _, name := range catalogue {
		known[name] = true
	}

	seen := make(map[string]bool, len(configured))
	out := make([]string, 0, len(configured))
	for _, name := range configured {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			name = "other"
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > maxDrivers {
		out = out[:maxDrivers]
	}
	return out
}

// batteryBucket answers "roughly how much storage", which is what sizing an
// engineering bet needs. The edges are the ones the residential market lands
// on: nothing, one module, a normal install, a big one, and everything above.
func batteryBucket(wh float64) string {
	kwh := wh / 1000
	switch {
	case !(kwh > 0):
		return "none"
	case kwh < 5:
		return "0-5"
	case kwh < 15:
		return "5-15"
	case kwh < 30:
		return "15-30"
	default:
		return "30+"
	}
}

// ageBucket answers "how long has this site been running", coarsely enough
// that the answer cannot be read backwards into an install date. A month of
// resolution at the near end and a year at the far end is what separates the
// cohorts the owner asks about: brand new, bedded in, and long-lived.
func ageBucket(installedAt, now time.Time) string {
	if installedAt.IsZero() {
		return "unknown"
	}
	days := now.Sub(installedAt).Hours() / 24
	switch {
	case days < 0:
		// A clock that went backwards, usually an RTC-less box before NTP.
		return "unknown"
	case days < 30:
		return "0-1m"
	case days < 180:
		return "1-6m"
	case days < 365:
		return "6-12m"
	default:
		return "1y+"
	}
}

// knownZone keeps the configured bidding zone only when the price catalogue
// knows it. A zone is a region shared with a whole country's worth of
// households, but only if it is a real one — free text in that field would
// travel verbatim.
func knownZone(zone string) string {
	z, ok := prices.LookupZone(zone)
	if !ok {
		return "unknown"
	}
	return z.Code
}

// Provider delivers one ping. It is an interface so the tests never open a
// socket, and so the transport can be replaced without touching the schedule.
type Provider interface {
	// Send delivers the payload. A non-nil error is an ordinary outcome, not
	// a fault: the endpoint may be unavailable.
	Send(ctx context.Context, p Payload) error
}

// HTTPProvider posts the report to the relay's separate HTTPS endpoint. It
// never enters the encrypted WebSocket path: the endpoint validates the six
// fields, adds them to daily totals and keeps neither the request nor an id.
type HTTPProvider struct {
	Endpoint string
	Client   *http.Client
}

// NewHTTPProvider validates the endpoint up front so a bad one fails at
// startup rather than once a day in a log nobody reads. The rule lives in
// config next to the field it guards, so a hand-edited YAML and this
// constructor cannot disagree about what a usable endpoint is.
func NewHTTPProvider(endpoint string, client *http.Client) (*HTTPProvider, error) {
	// Trimmed before both the check and the store, or a stray space in YAML
	// passes validation and then fails every request for the life of the
	// process, at debug level, where nobody sees it.
	endpoint = strings.TrimSpace(endpoint)
	if err := config.ValidateFleetPingEndpoint(endpoint); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	} else {
		// Do not alter a client the caller may use elsewhere.
		copy := *client
		client = &copy
	}
	// A 307 or 308 resends this body. Refuse every redirect so an HTTPS
	// collector cannot move the household-shaped report to plain HTTP or an
	// address the box owner never chose.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &HTTPProvider{Endpoint: endpoint, Client: client}, nil
}

// EndpointURL is the address this provider actually uses. The Settings API
// reads it from the running pinger because a saved endpoint change needs a
// restart before it replaces this provider.
func (h *HTTPProvider) EndpointURL() string { return h.Endpoint }

func (h *HTTPProvider) Send(ctx context.Context, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Pinned rather than left to Go's default, which carries the runtime
	// version. Everything worth knowing is already in the body; a header that
	// grows a build detail later would add a field nobody reviewed.
	req.Header.Set("User-Agent", "ftw-fleet-ping")

	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drained, never parsed. The box takes no instruction from this endpoint,
	// so there is nothing in the reply worth reading and no path for it to
	// become a control channel.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("fleet ping: endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// RelayProvider sends the report through the relay (package statsrelay), as
// its own message at its own time: never beside the energy report, whose
// sums a driver set and a price zone could otherwise tie to one home.
type RelayProvider struct {
	c *statsrelay.Client
}

func NewRelayProvider(c *statsrelay.Client) *RelayProvider { return &RelayProvider{c: c} }

func (p *RelayProvider) Send(ctx context.Context, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return p.c.Send(ctx, body)
}

// EndpointURL is the relay, which is where the box sends.
func (p *RelayProvider) EndpointURL() string { return p.c.Relay() }

// Options wires the pinger. Everything except the injectable seams is required.
type Options struct {
	// Enabled is read at each send rather than captured, so turning the ping
	// off in Settings takes effect without a restart.
	Enabled func() bool

	// Facts reports the box's current shape. Called once per ping and once per
	// preview, so what Settings shows is built by the same code that sends.
	Facts func() Facts

	Provider Provider
	Logger   *slog.Logger

	// Injectable seams. Production leaves them nil.
	Now    func() time.Time
	Random func() float64
}

// Pinger owns the schedule.
type Pinger struct {
	enabled  func() bool
	facts    func() Facts
	provider Provider
	log      *slog.Logger
	now      func() time.Time
	random   func() float64

	// first distinguishes the delay after boot from the ones after a send.
	first bool
}

func New(opts Options) (*Pinger, error) {
	switch {
	case opts.Facts == nil:
		return nil, errors.New("fleetping: Facts is required")
	case opts.Provider == nil:
		return nil, errors.New("fleetping: Provider is required")
	}
	if opts.Enabled == nil {
		opts.Enabled = func() bool { return true }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = randomFloat
	}
	return &Pinger{
		enabled:  opts.Enabled,
		facts:    opts.Facts,
		provider: opts.Provider,
		log:      opts.Logger.With("component", "fleetping"),
		now:      opts.Now,
		random:   opts.Random,
		first:    true,
	}, nil
}

// Payload is exactly what this box would send right now. Settings renders it,
// which is the point: the claim about what leaves the house is checkable
// against the same function that sends it, not against a paragraph of prose.
func (p *Pinger) Payload() Payload {
	return Build(p.facts(), p.now())
}

// Endpoint is the address used by the running provider, when it has one.
func (p *Pinger) Endpoint() string {
	type endpointProvider interface {
		EndpointURL() string
	}
	if provider, ok := p.provider.(endpointProvider); ok {
		return provider.EndpointURL()
	}
	return ""
}

// Enabled is whether the next report would go, read the way the sender reads
// it, so Settings says what the box does.
func (p *Pinger) Enabled() bool { return p.enabled() }

// ThroughRelay reports whether the running provider sends through the relay.
func (p *Pinger) ThroughRelay() bool {
	_, ok := p.provider.(*RelayProvider)
	return ok
}

// Run pings once a day until ctx ends.
func (p *Pinger) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(p.delay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		p.sendOnce(ctx)
	}
}

// delay is how long to wait before the next ping.
//
// The first one after boot lands anywhere in the day, so the send time is not
// derived from when the box started — a box rebooted by an update at 04:00
// would otherwise announce that, and the whole fleet would announce it
// together. It also makes a box stuck in a reboot loop ping about once a day
// anyway, since only the boots that outlive their drawn delay ever send.
//
// Later ones are drawn fresh each time, so the time of day drifts instead of
// settling. Waiting a fixed 24 h with a few minutes of jitter would keep a
// box in the same slot for life, and a slot is an identifier.
func (p *Pinger) delay() time.Duration {
	if p.first {
		p.first = false
		return time.Duration(p.random() * float64(period))
	}
	return period - spread + time.Duration(p.random()*float64(2*spread))
}

// sendOnce makes one attempt and forgets about it.
//
// There is no retry, here or anywhere: one household's ping is a rounding
// error in an aggregate, the endpoint may be unavailable, and a fleet that
// retries against a dead endpoint is an outage the fleet inflicted on itself.
// The next attempt is tomorrow, and the failure is a debug line rather than a
// warning because nothing about the house is wrong.
func (p *Pinger) sendOnce(ctx context.Context) {
	if !p.enabled() {
		// Checked here rather than around the wait, so the schedule does not
		// restart — and so the next send time does not encode the moment
		// somebody flipped the switch back on.
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := p.provider.Send(sendCtx, p.Payload()); err != nil {
		p.log.Debug("fleet ping did not go through", "err", err)
	}
}

// randomFloat draws from the system source, the same way appuplink's jitter
// does. math/rand would do for a delay, but one source per binary is one
// fewer thing to reason about when the next use is not a delay.
func randomFloat() float64 {
	const precision = 1 << 53
	n, err := rand.Int(rand.Reader, big.NewInt(precision))
	if err != nil {
		// Entropy failing is not something a once-a-day timer should turn
		// into a decision. The middle of the range is a fine delay.
		return 0.5
	}
	return float64(n.Int64()) / precision
}
