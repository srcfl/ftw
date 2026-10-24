package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// FTW Cloud's uplink, wired to the box.
//
// Everything here reads, but for the tunnel. cloudlink may import nothing
// from the box, so these adapters answer its questions from the same sources
// the app session and the HTTP API use: the live snapshot, the loadpoint view
// GET /api/loadpoints composes, and the price store. None of them is handed
// anything that can change state. The tunnel is the app uplink's session
// table, which cloudlink reaches through its Tunnel port and hands the
// phones' Noise sessions unread.

// cloudSite is the box's state as the uplink wants it.
type cloudSite struct {
	site   *appSite
	api    *lateAPI
	prices *appPrices
	// cars keeps the box's cars, nil on a box without the car runtime.
	// With it, the link sends the car class, and a charger's level and
	// goal fields are views of its session's car.
	cars *cars.Runtime
	// history is the energy ledger, read as the app's history over Noise
	// reads it, so a closed tile is the same bytes either way.
	history *appHistory

	sessMu   sync.Mutex
	sessions map[string]*plugSession
}

// plugSession numbers plug-ins per loadpoint for the life of this process.
// The phone keys what it holds by boot, so a number per boot is enough to
// tell one charging session from the next.
type plugSession struct {
	plugged bool
	n       int
}

var _ cloudlink.HistorySite = (*cloudSite)(nil)

// HistoryWindow serves the tiles class.
func (c *cloudSite) HistoryWindow(ctx context.Context, names []string, stepMs, fromMs, toMs int64) (map[string][]protocol.HistorySample, error) {
	if c.history == nil {
		return nil, errors.New("no energy ledger")
	}
	return c.history.Window(ctx, names, stepMs, fromMs, toMs)
}

func (c *cloudSite) Statistics(ctx context.Context, now time.Time) (cloudwire.Stats, bool) {
	if srv := c.api.srv.Load(); srv != nil {
		return srv.CloudStatistics(ctx, now)
	}
	return cloudwire.Stats{}, false
}

func (c *cloudSite) Live() cloudlink.Live {
	snap := c.site.Snapshot()
	uptime := time.Since(c.site.started).Milliseconds()
	fields, sources := appproto.LiveView(snap, uptime)

	out := cloudlink.Live{
		Fields:            fields,
		Sources:           make(map[string]cloudlink.Source, len(sources)),
		FieldSources:      map[string][]string{},
		DispatchBlockedBy: snap.DispatchBlockedBy,
	}
	for name, s := range sources {
		lastOk := s.LastOkMs
		if s.State == appproto.SourceNever {
			lastOk = -1
		}
		out.Sources[name] = cloudlink.Source{Kind: s.Kind, State: s.State, LastOkUptimeMs: lastOk, StaleAfterMs: s.StaleAfterMs}
	}

	// Each field follows the drivers that produce it. Load is what is left
	// of the grid after every other flow, so it follows all of them. The
	// fuse is config and follows none.
	meter := siteMeterName(c.site.ctrl, c.site.ctrlMu)
	byType := func(t telemetry.DerType) []string {
		var names []string
		for _, r := range c.site.tel.ReadingsByType(t) {
			names = append(names, r.Driver)
		}
		sort.Strings(names)
		return names
	}
	pv, battery, ev := byType(telemetry.DerPV), byType(telemetry.DerBattery), byType(telemetry.DerEV)
	set := func(fid appproto.Fid, names []string) {
		key := strconv.Itoa(int(fid))
		if _, sent := fields[key]; sent && len(names) > 0 {
			out.FieldSources[key] = names
		}
	}
	var meterOnly []string
	if meter != "" {
		meterOnly = []string{meter}
	}
	set(appproto.FidGridW, meterOnly)
	set(appproto.FidGridL1Da, meterOnly)
	set(appproto.FidGridL2Da, meterOnly)
	set(appproto.FidGridL3Da, meterOnly)
	set(appproto.FidPvW, pv)
	set(appproto.FidBatteryW, battery)
	set(appproto.FidBatterySoc, battery)
	set(appproto.FidEvW, ev)
	set(appproto.FidLoadW, uniqueSorted(meterOnly, pv, battery, ev))
	return out
}

func uniqueSorted(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (c *cloudSite) Loadpoints() (cloudwire.LP, bool) {
	srv := c.api.srv.Load()
	if srv == nil {
		return cloudwire.LP{}, false
	}
	now := time.Now()
	states, feedback, ok := srv.CloudLoadpoints(now)
	if !ok || len(states) == 0 {
		return cloudwire.LP{}, false
	}
	c.site.ctrlMu.Lock()
	rev := c.site.revision.Observe(c.site.ctrl)
	c.site.ctrlMu.Unlock()

	out := cloudwire.LP{ControlRev: rev}
	for _, st := range states {
		out.Loadpoints = append(out.Loadpoints, c.loadpoint(st, now))
	}
	c.joinCars(out.Loadpoints)
	for _, f := range feedback {
		out.Feedback = append(out.Feedback, cloudFeedback(f, now))
	}
	return out, true
}

// Cars is every car the box keeps, for the car class. The link cuts the
// ids and leaves the names at home (cloudlink.ProjectCars).
func (c *cloudSite) Cars() ([]protocol.Car, bool) {
	if c.cars == nil {
		return nil, false
	}
	return c.cars.Cars(), true
}

// joinCars makes each charger's level and goal fields views of its
// session's car, read with the car class's own view.
func (c *cloudSite) joinCars(lps []cloudwire.Loadpoint) {
	if c.cars == nil {
		return
	}
	byID := map[string]protocol.Car{}
	for _, car := range c.cars.Cars() {
		byID[car.ID] = car
	}
	for i := range lps {
		if car, ok := byID[lps[i].CarID]; ok && lps[i].CarID != "" {
			joinCar(&lps[i], car)
		}
	}
}

// A charger's socSource on the wire (docs/protocol-cloud.md, lp).
const (
	lpSoCVehicle  = "vehicle"
	lpSoCInferred = "inferred"
	lpSoCAssumed  = "assumed"
	lpSoCUnknown  = "unknown"
)

// joinCar makes a charger's level and goal fields views of its session's
// car (docs/protocol-cloud.md, lp): a phone that joins carId to the car
// class finds the two agree, and an app that knows only chargers reads the
// car's own level and goal. readyByMs stays the box's next ready time for
// that goal.
func joinCar(lp *cloudwire.Loadpoint, car protocol.Car) {
	lp.SocPermille, lp.SocAgeS = car.SocPermille, nil
	switch {
	case car.SocPermille == nil:
		lp.SocSource = lpSoCUnknown
	case car.SocSource == protocol.CarSoCAssumed:
		lp.SocSource = lpSoCAssumed
	case car.SocEstimate || car.SocSource != protocol.CarSoCVehicle:
		lp.SocSource = lpSoCInferred
	default:
		lp.SocSource, lp.SocAgeS = lpSoCVehicle, car.SocAgeS
	}
	lp.TargetPermille, lp.Recurring = nil, false
	if g := car.Goal; g != nil {
		if g.TargetPermille > 0 {
			lp.TargetPermille = ptr(g.TargetPermille)
		}
		lp.Recurring = g.Days != 0
	} else {
		lp.ReadyByMs = nil
	}
	lp.SurplusOnly = car.SurplusOnly
}

func (c *cloudSite) session(id string, plugged bool) string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	if c.sessions == nil {
		c.sessions = map[string]*plugSession{}
	}
	s := c.sessions[id]
	if s == nil {
		s = &plugSession{}
		c.sessions[id] = s
	}
	if plugged && !s.plugged {
		s.n++
	}
	s.plugged = plugged
	if !plugged {
		return ""
	}
	return strconv.Itoa(s.n)
}

// loadpoint maps the API's view onto the wire. Capabilities (charge limits,
// phases, battery size) and the vehicle's name are not in the shape.
func (c *cloudSite) loadpoint(st loadpoint.State, now time.Time) cloudwire.Loadpoint {
	lp := cloudwire.Loadpoint{
		ID: st.ID, Src: st.DriverName, Plugged: st.PluggedIn,
		SocSource: st.SoCSource, Recurring: !st.Schedule.Empty(),
		PowerW: roundW(st.CurrentPowerW), SessionWh: roundW(st.DeliveredWhSession),
		Session:         st.Session,
		CommandedReason: st.CommandedReason, SurplusOnly: st.SurplusOnly,
		GridDeferred: st.GridDeferred, PlanPending: st.PlanPending, Boost: st.BatteryBoost.Active,
	}
	if lp.Session == "" {
		// No car runtime has named this plug-in: the count of plug-ins
		// since the box started, as before cars.
		lp.Session = c.session(st.ID, st.PluggedIn)
	}
	if st.PluggedIn {
		// The session's car by the box's id; the link cuts it per grant
		// (cloudlink.ProjectLP), and an agent's read under the agent key.
		lp.CarID, lp.CarIdentifiedBy = st.CarID, st.CarIdentifiedBy
		lp.CarCorrected, lp.CarCorrectedFrom, lp.Choose = st.CarCorrected, st.CarCorrectedFrom, st.Choose
	}
	if st.PluggedIn && !st.SoCUnknown {
		// A visitor's car has no level; its delivered energy is not one.
		lp.SocPermille = ptr(units.PermilleFromFraction(st.CurrentSoC))
		if st.SoCSource == "vehicle" {
			lp.SocAgeS = ptr(st.VehicleSoCAgeS)
		}
	}
	if st.TargetSoC > 0 {
		lp.TargetPermille = ptr(units.PermilleFromFraction(st.TargetSoC))
	}
	if !st.TargetTime.IsZero() {
		lp.ReadyByMs = ptr(st.TargetTime.UnixMilli())
	}
	if st.CommandedKnown {
		lp.CommandedW = ptr(roundW(st.CommandedW))
	}
	if m := st.Manual; m.Active {
		manual := &cloudwire.Manual{State: m.State, Reason: m.LimitReason, RequestedW: ptr(roundW(m.RequestedW))}
		if m.SinceMs > 0 {
			manual.SinceAgeMs = max(0, now.UnixMilli()-m.SinceMs)
		}
		if m.CommandedW > 0 {
			manual.CommandedW = ptr(roundW(m.CommandedW))
		}
		lp.Manual = manual
	}
	if st.PlanNextStartMs > 0 {
		lp.NextStartMs, lp.NextEndMs = ptr(st.PlanNextStartMs), ptr(st.PlanNextEndMs)
		lp.NextWh = ptr(roundW(st.PlanNextWh))
	}
	return lp
}

func cloudFeedback(f api.ControlFeedback, now time.Time) cloudwire.Feedback {
	out := cloudwire.Feedback{
		Kind: f.Kind, Src: f.Driver, Status: f.Status, Reason: f.Reason,
		Severity: f.Severity, Evidence: f.Evidence, Fresh: f.ReadingsFresh,
	}
	if f.ActualW != nil {
		out.ActualW = ptr(roundW(*f.ActualW))
	}
	if f.ObservedAtMs > 0 {
		out.AgeMs = ptr(max(0, now.UnixMilli()-f.ObservedAtMs))
	}
	return out
}

// Prices is the window from the start of today, on the box's own clock, to
// the last stored slot. Starting at midnight rather than now keeps the hash,
// and so the message, still until new prices land.
func (c *cloudSite) Prices() (cloudwire.Prices, bool) {
	if c.prices == nil {
		return cloudwire.Prices{}, false
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	slots, err := c.prices.Slots(context.Background(), midnight.UnixMilli(), midnight.AddDate(0, 0, 2).UnixMilli())
	if err != nil || len(slots) == 0 {
		return cloudwire.Prices{}, false
	}
	return priceColumns(c.prices.Zone(), c.prices.Currency(), slots), true
}

// priceColumns lays slots out at the shortest slot length. A longer slot's
// price repeats across the columns it covers; a gap is null.
func priceColumns(zone, currency string, slots []appproto.PricePoint) cloudwire.Prices {
	sort.Slice(slots, func(a, b int) bool { return slots[a].StartMs < slots[b].StartMs })
	slotMs := int64(math.MaxInt64)
	for _, s := range slots {
		slotMs = min(slotMs, int64(s.LenMin)*60_000)
	}
	p := cloudwire.Prices{Zone: zone, Currency: currency, T0Ms: slots[0].StartMs, SlotMs: slotMs}
	for _, s := range slots {
		for at := s.StartMs; at < s.StartMs+int64(s.LenMin)*60_000; at += slotMs {
			i := int((at - p.T0Ms) / slotMs)
			for len(p.SpotMinor) < i {
				p.SpotMinor, p.TotalMinor = append(p.SpotMinor, nil), append(p.TotalMinor, nil)
			}
			if len(p.SpotMinor) == i {
				p.SpotMinor = append(p.SpotMinor, ptr(roundW(s.SpotMinor)))
				p.TotalMinor = append(p.TotalMinor, ptr(roundW(s.TotalMinor)))
			}
		}
	}
	return p
}

func roundW(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.Round(v))
}

func ptr[T any](v T) *T { return &v }

// cloudNotices reaches the notifications service once it exists; it is built
// after the link starts.
type cloudNotices struct {
	svc atomic.Pointer[notifications.Service]
}

func (n *cloudNotices) notify(kind string) {
	if svc := n.svc.Load(); svc != nil {
		svc.NotifyCloudGrant(kind)
	}
}

// startCloudLink loads the FTW Cloud link and grant and keeps the uplink up
// while the box is linked: from its first pairing code, or an owner's grant.
// It needs the state store, which holds the link and the grant, and
// enrolment, which holds the phones the roster lists; without either it
// returns nil and the routes say FTW Cloud is unavailable. tunnel is the app
// uplink's session table, nil when the app link is off, and then the box
// takes no tun and links only for a grant. exchange is the watches' door, nil
// with the app link off; it rides beside the tunnel.
func startCloudLink(
	ctx context.Context, statePath, build string, st *state.Store, enroll *appenroll.Identity,
	tel *telemetry.Store, ctrl *control.State, ctrlMu *sync.Mutex, revision *control.Revision,
	siteMeterStale time.Duration, measurementOptions func() telemetry.ForecastOptions, priceSvc *prices.Service, gateway *lateAPI, notices *cloudNotices,
	tunnel *appuplink.Tunnel, exchange *appuplink.Exchange, carsRT *cars.Runtime, webPush *notifications.WebPush,
) *cloudlink.Link {
	if st == nil || enroll == nil {
		return nil
	}
	site := &cloudSite{api: gateway, cars: carsRT, history: &appHistory{st: st},
		site: newAppSite(tel, ctrl, ctrlMu, revision, siteMeterStale, measurementOptions)}
	if priceSvc != nil && priceSvc.Zone != "" && priceSvc.Store != nil {
		site.prices = &appPrices{svc: priceSvc}
	}
	opts := cloudlink.Options{
		Site:        site,
		Roster:      appuplink.CloudRoster{Enroll: enroll, Tunnel: tunnel != nil},
		KV:          st,
		KeyPath:     filepath.Join(filepath.Dir(statePath), "cloud.key"),
		Build:       build,
		Started:     processStarted,
		ClockSynced: cloudlink.ClockSynced,
		Notify:      notices.notify,
		Arrivals:    arrivalsFor(ctx, tel),
	}
	if webPush != nil {
		opts.WebPush = webPushFromState{st: st, wp: webPush, enroll: enroll}
	}
	// Set only with a table: a typed nil would read as a tunnel.
	if tunnel != nil {
		opts.Tunnel = tunnel
	}
	if exchange != nil {
		opts.Exchange = exchange
	}
	link, err := cloudlink.New(opts)
	if err != nil {
		slog.Warn("FTW Cloud unavailable", "err", err)
		return nil
	}
	if tunnel != nil {
		tunnel.Attach(link)
	}
	if exchange != nil {
		exchange.Attach(link)
	}
	enroll.OnChange(link.RosterChanged)
	// A code shown links the box at once, so a QR that names the home can.
	enroll.OnPairingCode(link.RosterChanged)
	enroll.SetHome(link.Home)
	enroll.SetAgentInviteHome(link.PrepareAgentInviteHome)
	go func() {
		if err := link.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("FTW Cloud uplink stopped", "err", err)
		}
	}()
	return link
}

// cloudSession is FTW Cloud as an app session sees it. The app link starts
// first, because the cloud link's roster needs its enrolment, so the link is
// bound here once it exists. A session that opens before then hears no
// cloud.link and no session information, which is what a box without FTW
// Cloud says.
type cloudSession struct {
	link atomic.Pointer[cloudlink.Link]
}

func (c *cloudSession) bind(link *cloudlink.Link) { c.link.Store(link) }

func (c *cloudSession) load() *cloudlink.Link {
	if c == nil {
		return nil
	}
	return c.link.Load()
}

// caps adds cloud.link to an app session's capabilities when this build can
// run the uplink, and cloud.tun when the link also carries the phones' own
// sessions. A build that names no service says nothing.
func (c *cloudSession) caps(caps []string) []string {
	link := c.load()
	if link == nil || !link.Available() {
		return caps
	}
	caps = append(slices.Clip(caps), appproto.CapCloudLink)
	if link.TakesTun() {
		caps = append(caps, appproto.CapCloudTun)
	}
	// A watch reaches the box through FTW Cloud's exchange, so the phone
	// offers to add one only while the link carries it.
	if link.TakesX() {
		caps = append(caps, appproto.CapCmdExchange)
	}
	return caps
}

// info is hello_ok's FTW Cloud session information, nil while FTW Cloud is
// off.
func (c *cloudSession) info() *appproto.CloudSession {
	if link := c.load(); link != nil {
		return link.SessionInfo()
	}
	return nil
}

// cloudByeWait bounds how long shutdown waits for the bye to leave.
const cloudByeWait = 2 * time.Second

// cloudEventQueue turns a rendered notification into an event for the
// uplink, which cuts the names out before it signs.
func cloudEventQueue(link *cloudlink.Link) func(notifications.Message) bool {
	return func(m notifications.Message) bool {
		return link.QueueEvent(cloudlink.EventIn{
			Kind: m.Kind, Priority: m.Priority, Args: m.Args,
			Driver: m.Device, Loadpoint: m.LoadpointID, At: m.At,
		})
	}
}

// cloudForAPI hands the link to the API, or nothing: a typed nil in an
// interface would pass the routes' nil check and panic on first use.
func cloudForAPI(link *cloudlink.Link) api.CloudGrant {
	if link == nil {
		return nil
	}
	return link
}
