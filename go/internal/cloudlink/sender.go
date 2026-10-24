package cloudlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Cadence and limits, from the protocol's "Numbers in one place".
const (
	periodIdle    = 5 * time.Second
	periodWatched = time.Second
	// lpPeriodMin holds lp at 2 s until a Pi 4 bench shows what composing
	// it costs.
	lpPeriodMin   = 2 * time.Second
	watchFor      = 60 * time.Second
	fullEvery     = 15 * time.Minute
	resyncEvery   = 5 * time.Minute
	pricesEvery   = time.Minute
	rosterRenew   = 6 * time.Hour
	rosterTTLS    = 86_400
	budgetBytes   = 20_000_000
	budgetMinutes = 24 * 60
	// tunBudgetBytes bounds the tunnel apart from the readable copy. A phone
	// holding a session open all day at 1 Hz sends about 50 MB; this is five
	// of them, and stops a runaway before it eats a household's uplink.
	tunBudgetBytes = 250_000_000
	// xBudgetBytes is the watches' share of it: their exchange answers, a
	// tenth. An answer is about a kilobyte, so this is some twenty thousand
	// a day. xStopsAt is the share of the tunnel's budget past which x
	// stops, so the phones keep the last of it.
	xBudgetBytes = tunBudgetBytes / 10
	xStopsAt     = 0.9
)

// sentFrame is the last frame of a class that left the house, exactly.
type sentFrame struct {
	T     string
	Frame []byte
	AtMs  int64
}

// budget counts bytes sent over a rolling 24 hours of uptime, per minute.
// Past budgetBytes the box sends only bye and roster, and no hb, so data on
// phones ages to stale rather than looking current.
type budget struct {
	bytes  [budgetMinutes]int64
	minute [budgetMinutes]int64
}

func (b *budget) add(uptimeMs, n int64) {
	m := uptimeMs / 60_000
	i := m % budgetMinutes
	if b.minute[i] != m {
		b.minute[i], b.bytes[i] = m, 0
	}
	b.bytes[i] += n
}

func (b *budget) total(uptimeMs int64) int64 {
	m := uptimeMs / 60_000
	var sum int64
	for i := range b.bytes {
		if age := m - b.minute[i]; age >= 0 && age < budgetMinutes {
			sum += b.bytes[i]
		}
	}
	return sum
}

// cadence is one connection's view of what it last sent.
type cadence struct {
	statsHour      int64
	statsCheckedAt time.Time
	full           bool
	lastFull       time.Time
	liveKey        string
	liveAt         time.Time
	lpLast         *cloudwire.LP
	lpAt           time.Time
	carLast        *cloudwire.Cars
	carAt          time.Time
	pricesKey      string
	pricesAt       time.Time
	anyAt          time.Time // last live or hb
	rosterAt       time.Time
	watchUntil     time.Time
	// pendingUntil is when the pairing code the last roster listed runs
	// out, zero when it listed none.
	pendingUntil time.Time

	// When each class was last composed, and whether a change it found is
	// still waiting for its period. Composing reads the box, and on a Pi
	// the loadpoint view is not free, so it happens no faster than a
	// message could go.
	liveCheckedAt, lpCheckedAt, carCheckedAt time.Time
	liveWaiting, lpWaiting, carWaiting       bool
	// arrived says a driver finished a poll since live was last composed,
	// while a phone watches.
	arrived bool

	// tiles is what this connection sent of the tiles class.
	tiles tileState
}

// liveCheckEvery is how often the source table is looked at, so a table
// change goes within a second whatever the period.
const liveCheckEvery = time.Second

// run sends until the connection or the link ends.
func (w *writer) run(ctx context.Context, events <-chan cloudwire.Envelope) error {
	l := w.link
	tick := time.NewTicker(l.opts.Tick)
	defer tick.Stop()
	connHome := l.current().home
	// A roster goes at every connect, which also renews its ttlS: the
	// service counts that from when it received the last one.
	st := &cadence{full: true, tiles: newTileState(w.have)}
	var purgeSince time.Time
	// slot fires when an arrival waits for the watched live's period.
	var slot <-chan time.Time

	for {
		if err := l.startStatisticsClock(); err != nil {
			return err
		}
		snap := l.current()
		if snap.link == nil || snap.home != connHome {
			return errors.New("cloudlink: the link changed under the connection")
		}
		now := time.Now()
		if snap.link.Ending {
			if purgeSince.IsZero() {
				if err := w.bye(snap, cloudwire.ByeRevoked, true); err != nil {
					return err
				}
				purgeSince = now
			} else if now.Sub(purgeSince) > purgeWait {
				return errors.New("cloudlink: no purge receipt yet")
			}
		} else {
			if err := w.closeIfAsked(snap); err != nil {
				return err
			}
			if err := w.sendEvents(snap, now); err != nil {
				return err
			}
			if err := w.sendNotices(snap); err != nil {
				return err
			}
			if err := w.tick(snap, st, now); err != nil {
				return err
			}
			if wait := st.liveAt.Add(periodWatched).Sub(now); st.arrived && slot == nil && wait > 0 {
				slot = time.After(wait)
			}
		}

		// Polls are heard only while a phone watches; otherwise the copy
		// keeps its period and a poll needs no wakeup.
		var arrivals <-chan struct{}
		if time.Now().Before(st.watchUntil) {
			arrivals = l.opts.Arrivals
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-l.wake:
		case <-arrivals:
			st.arrived = true
		case <-slot:
			slot = nil
		case env := <-events:
			if err := w.downlink(snap, st, env); err != nil {
				return err
			}
		}
	}
}

// downlink acts on watch, resync and purged, and on nothing else.
func (w *writer) downlink(snap grantSnapshot, st *cadence, env cloudwire.Envelope) error {
	l := w.link
	switch env.T {
	case cloudwire.MsgWatch:
		var b cloudwire.Watch
		if cloudwire.Unmarshal(env.B, &b) == nil && b.Hz == 1 {
			st.watchUntil = time.Now().Add(watchFor)
		}
	case cloudwire.MsgResync:
		var b cloudwire.Resync
		if cloudwire.Unmarshal(env.B, &b) != nil {
			return nil
		}
		l.mu.Lock()
		honoured := l.resyncAt.IsZero() || time.Since(l.resyncAt) >= resyncEvery
		if honoured {
			l.resyncAt = time.Now()
		}
		l.mu.Unlock()
		if !honoured {
			return nil
		}
		if slices.Contains(b.What, cloudwire.ResyncFull) {
			st.full = true
		}
		if slices.Contains(b.What, cloudwire.ResyncRoster) {
			st.rosterAt = time.Time{}
		}
		if slices.Contains(b.What, cloudwire.ResyncTiles) {
			st.tiles.resync(b.Tiles)
		}
	}
	return nil
}

// acceptPurged takes a valid receipt: signed by the service for this home.
// A copy receipt for the grant being revoked forgets that grant; a home
// receipt while the link ends ends it, and true says so, so the connection
// closes.
func (l *Link) acceptPurged(snap grantSnapshot, env cloudwire.Envelope) bool {
	var b cloudwire.Purged
	cur := l.current()
	if cur.link == nil || cur.home != snap.home || !l.verifiedByService(snap.home, env) ||
		cloudwire.Unmarshal(env.B, &b) != nil {
		return false
	}
	switch b.What {
	case cloudwire.PurgedCopy:
		l.finishCopy(b.AtMs, b.GrantID)
	case cloudwire.PurgedHome:
		if cur.link.Ending {
			l.finishLink()
			return true
		}
	}
	return false
}

// tick sends whatever is due.
func (w *writer) tick(snap grantSnapshot, st *cadence, now time.Time) error {
	l := w.link

	// A pairing code that ran out unused changes the roster too, and may
	// leave nothing that needs the link.
	if !st.pendingUntil.IsZero() && !now.Before(st.pendingUntil) {
		st.pendingUntil = time.Time{}
		l.mu.Lock()
		l.rosterDue = true
		l.mu.Unlock()
	}
	l.mu.Lock()
	h := time.Duration(cloudwire.Heartbeat(l.table)) * time.Millisecond
	l.mu.Unlock()
	force := st.rosterAt.IsZero() || now.Sub(st.rosterAt) >= rosterRenew
	if roster, ok := l.takeRoster(force); ok {
		if cur := l.current(); cur.link == nil || cur.link.Ending {
			// The end goes on the next pass, as bye.
			return nil
		}
		// What follows reads the state the roster was built from, so a
		// revoke that just landed sends no reading after it.
		snap = l.current()
		if _, err := w.sendBody(snap, cloudwire.MsgRoster, &roster, true); err != nil {
			return err
		}
		st.rosterAt = now
		for _, p := range roster.Pending {
			expires := now.Add(time.Duration(p.TTLS) * time.Second)
			if st.pendingUntil.IsZero() || expires.Before(st.pendingUntil) {
				st.pendingUntil = expires
			}
		}
	}

	if err := w.statistics(snap, st, now); err != nil {
		return err
	}
	period := periodIdle
	if now.Before(st.watchUntil) {
		period = periodWatched
	}
	lpPeriod := max(period, lpPeriodMin)
	full := st.full || now.Sub(st.lastFull) >= fullEvery
	hbDue := now.Sub(st.anyAt) >= h

	// While a phone watches, a poll's readings go as they land, one period
	// after the last live at the soonest, so the copy costs what it did.
	arrival := st.arrived && now.Before(st.watchUntil) && now.Sub(st.liveAt) >= period

	var ages map[string]int64
	if snap.allowed(cloudwire.MsgLive) && (full || hbDue || arrival || now.Sub(st.liveCheckedAt) >= liveCheckEvery) {
		st.liveCheckedAt, st.arrived = now, false
		live, tableChanged := l.projectLive(snap, l.opts.Site.Live())
		ages = live.Ages
		key := liveKey(live)
		changed := tableChanged || key != st.liveKey
		st.liveWaiting = false
		switch {
		case full || tableChanged || (changed && now.Sub(st.liveAt) >= period):
			if !full && !tableChanged {
				live.Sources, live.FieldSources = nil, nil
			}
			sent, err := w.sendBody(snap, cloudwire.MsgLive, &live, false)
			if err != nil {
				return err
			}
			if sent {
				st.liveKey, st.liveAt, st.anyAt = key, now, now
			}
		case changed:
			st.liveWaiting = true
		}
	}

	if snap.allowed(cloudwire.MsgLP) && (full || now.Sub(st.lpCheckedAt) >= lpPeriod) {
		st.lpCheckedAt = now
		st.lpWaiting = false
		if lp, ok := l.opts.Site.Loadpoints(); ok {
			lp = l.projectLP(snap, lp)
			changed := st.lpLast == nil || lpChanged(*st.lpLast, lp)
			switch {
			case full || (changed && now.Sub(st.lpAt) >= lpPeriod):
				sent, err := w.sendBody(snap, cloudwire.MsgLP, &lp, false)
				if err != nil {
					return err
				}
				if sent {
					st.lpLast, st.lpAt = &lp, now
				}
			case changed:
				st.lpWaiting = true
			}
		}
	}
	// car goes as lp does: at most once per lp's period, and in full with
	// it (docs/protocol-cloud.md, car).
	if cs, ok := l.opts.Site.(CarSite); ok && snap.allowed(cloudwire.MsgCar) && (full || now.Sub(st.carCheckedAt) >= lpPeriod) {
		st.carCheckedAt = now
		st.carWaiting = false
		if in, ok := cs.Cars(); ok {
			cars := ProjectCars(snap.g.GrantKey, in)
			changed := st.carLast == nil || carsChanged(*st.carLast, cars)
			switch {
			case full || (changed && now.Sub(st.carAt) >= lpPeriod):
				sent, err := w.sendBody(snap, cloudwire.MsgCar, &cars, false)
				if err != nil {
					return err
				}
				if sent {
					st.carLast, st.carAt = &cars, now
				}
			case changed:
				st.carWaiting = true
			}
		}
	}
	if full {
		st.full, st.lastFull = false, now
	}

	if snap.allowed(cloudwire.MsgPrices) && (st.pricesKey == "" || now.Sub(st.pricesAt) >= pricesEvery) {
		st.pricesAt = now
		if p, ok := l.opts.Site.Prices(); ok {
			if key := pricesKey(p); key != st.pricesKey {
				sent, err := w.sendBody(snap, cloudwire.MsgPrices, &p, false)
				if err != nil {
					return err
				}
				if sent {
					st.pricesKey = key
				}
			}
		}
	}

	// hb never vouches for a live or lp that waits for its period, and its
	// ages were read on this tick. It names readings, so it goes only with
	// the readable copy.
	if snap.allowed(cloudwire.MsgHB) && now.Sub(st.anyAt) >= h && !st.liveWaiting && !st.lpWaiting {
		hb := l.heartbeat(ages)
		sent, err := w.sendBody(snap, cloudwire.MsgHB, &hb, false)
		if err != nil {
			return err
		}
		if sent {
			st.anyAt = now
		}
	}
	// Tiles go last: a reading never waits for the ledger.
	return w.tiles(snap, st, now)
}

// takeRoster says whether a roster is due, and builds it from the link as it
// is now, after reading that it is due: never from a snapshot taken before,
// or a revoke that lands in between would wait six hours for the next
// renewal while the service kept the copy. A roster about to go may leave
// nothing that needs the link, which then starts to end.
func (l *Link) takeRoster(force bool) (cloudwire.Roster, bool) {
	l.mu.Lock()
	due := l.rosterDue
	l.rosterDue = false
	l.mu.Unlock()
	if !due && !force {
		return cloudwire.Roster{}, false
	}
	l.endIfIdle()
	_, roster := l.rosterNumber(l.current())
	return roster, true
}

// sendBody stamps the header, sends, and records the seq of a live or lp so
// hb can name it. body is a pointer to a cloudwire body with a Header.
func (w *writer) sendBody(snap grantSnapshot, t string, body any, force bool) (bool, error) {
	l := w.link
	l.mu.Lock()
	if !force && l.budget.total(l.uptimeMs()) >= budgetBytes {
		l.mu.Unlock()
		return false, nil
	}
	h := l.header()
	l.mu.Unlock()
	reflect.ValueOf(body).Elem().FieldByName("Header").Set(reflect.ValueOf(h))

	if err := w.send(snap, t, reflect.ValueOf(body).Elem().Interface()); err != nil {
		return false, err
	}
	l.mu.Lock()
	switch t {
	case cloudwire.MsgLive:
		l.liveSeq = h.Seq
	case cloudwire.MsgLP:
		l.lpSeq = h.Seq
	}
	l.mu.Unlock()
	return true, nil
}

func (w *writer) bye(snap grantSnapshot, why string, purge bool) error {
	_, err := w.sendBody(snap, cloudwire.MsgBye, &cloudwire.Bye{Why: why, Purge: purge}, true)
	return err
}

// heartbeat names the newest live and lp sent and how old each reading is.
func (l *Link) heartbeat(ages map[string]int64) cloudwire.HB {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloudwire.HB{Clock: l.clockName(), SrcRev: l.srcRev, LiveSeq: l.liveSeq, LpSeq: l.lpSeq, Ages: ages}
}

// project cuts opaque ids and ages from the box's names.
func (l *Link) project(gk []byte, in Live) cloudwire.Live {
	return ProjectLive(gk, l.uptimeMs(), in)
}

// ProjectLive cuts opaque ids under key, and ages at uptime up, from the
// box's names: the live body as FTW Cloud's copy carries it, or, under the
// box's agent key, as an agent reads it.
func ProjectLive(gk []byte, up int64, in Live) cloudwire.Live {
	out := cloudwire.Live{
		Fields: in.Fields, Sources: map[string]cloudwire.Source{},
		FieldSources: map[string][]string{}, Ages: map[string]int64{},
	}
	for name, s := range in.Sources {
		id := cloudwire.SourceID(gk, name)
		out.Sources[id] = cloudwire.Source{Kind: s.Kind, State: s.State,
			LastOkMs: max(0, s.LastOkUptimeMs), StaleAfterMs: s.StaleAfterMs}
		if (s.State == protocol.SourceLive || s.State == protocol.SourceLagging) && s.LastOkUptimeMs >= 0 {
			out.Ages[id] = max(0, up-s.LastOkUptimeMs)
		}
	}
	for f, names := range in.FieldSources {
		out.FieldSources[f] = ids(gk, names)
	}
	out.DispatchBlockedBy = ids(gk, in.DispatchBlockedBy)
	return out
}

// projectLive is project plus the source revision, which moves when the
// table changed. The comparison ignores lastOkMs, which moves every reading.
func (l *Link) projectLive(snap grantSnapshot, in Live) (cloudwire.Live, bool) {
	out := l.project(snap.g.GrantKey, in)
	table := tableKey(out)
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := table != l.tableKey
	if changed {
		l.srcRev++
		l.tableKey = table
		l.table = out.Sources
	}
	out.SrcRev = l.srcRev
	return out, changed
}

func ids(grantKey []byte, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, cloudwire.SourceID(grantKey, n))
	}
	sort.Strings(out)
	return out
}

// projectLP replaces loadpoint and driver names with opaque ids.
func (l *Link) projectLP(snap grantSnapshot, in cloudwire.LP) cloudwire.LP {
	return ProjectLP(snap.g.GrantKey, in)
}

// ProjectLP replaces loadpoint, driver and car ids with opaque ids under gk.
func ProjectLP(gk []byte, in cloudwire.LP) cloudwire.LP {
	out := cloudwire.LP{ControlRev: in.ControlRev}
	for _, lp := range in.Loadpoints {
		lp.ID = cloudwire.LoadpointID(gk, lp.ID)
		if lp.Src != "" {
			lp.Src = cloudwire.SourceID(gk, lp.Src)
		}
		lp.CarID = carID(gk, lp.CarID)
		lp.CarCorrectedFrom = carID(gk, lp.CarCorrectedFrom)
		if lp.Choose != nil {
			choose := make([]string, 0, len(lp.Choose))
			for _, c := range lp.Choose {
				choose = append(choose, carID(gk, c))
			}
			lp.Choose = choose
		}
		out.Loadpoints = append(out.Loadpoints, lp)
	}
	for _, f := range in.Feedback {
		if f.Src != "" {
			f.Src = cloudwire.SourceID(gk, f.Src)
		}
		out.Feedback = append(out.Feedback, f)
	}
	return out
}

// ProjectCars is the car class under gk (docs/cars.md): each car's opaque
// id, level and goal, and nothing that names a car. FTW Cloud's copy and an
// agent's cars.get read the same projection, each under its own key.
func ProjectCars(gk []byte, in []protocol.Car) cloudwire.Cars {
	out := cloudwire.Cars{Cars: make([]cloudwire.Car, 0, len(in))}
	for _, c := range in {
		car := cloudwire.Car{
			ID: carID(gk, c.ID), SocPermille: c.SocPermille, SocSource: c.SocSource, SocAgeS: c.SocAgeS,
			EnergySinceWh: c.EnergySinceWh, SocEstimate: c.SocEstimate, ChargeLimitPermille: c.ChargeLimitPermille,
			SurplusOnly: c.SurplusOnly,
		}
		if g := c.Goal; g != nil {
			car.Goal = &cloudwire.CarGoal{TargetPermille: g.TargetPermille, ReadyByMin: g.ReadyByMin, Days: g.Days,
				FinishAtLimit: g.FinishAtLimit}
		}
		out.Cars = append(out.Cars, car)
	}
	return out
}

// carID is a car's opaque id under gk; empty stays empty.
func carID(gk []byte, id string) string {
	if id == "" {
		return ""
	}
	return cloudwire.CarID(gk, id)
}

// The change tests: encodings of what a phone must hear about, without the
// fields that move every reading.

func digest(v any) string {
	raw, err := cloudwire.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func liveKey(l cloudwire.Live) string {
	return digest([]any{l.Fields, l.DispatchBlockedBy, l.SrcRev})
}

func tableKey(l cloudwire.Live) string {
	table := map[string]cloudwire.Source{}
	for id, s := range l.Sources {
		s.LastOkMs = 0
		table[id] = s
	}
	return digest([]any{table, l.FieldSources})
}

// lpKey is lp without its ages and its measured values, which lpChanged
// compares with a deadband instead.
func lpKey(lp cloudwire.LP) string {
	c := cloudwire.LP{ControlRev: lp.ControlRev}
	for _, p := range lp.Loadpoints {
		p.SocAgeS, p.SocPermille, p.PowerW, p.SessionWh = nil, nil, 0, 0
		if p.Manual != nil {
			m := *p.Manual
			m.SinceAgeMs = 0
			p.Manual = &m
		}
		c.Loadpoints = append(c.Loadpoints, p)
	}
	for _, f := range lp.Feedback {
		f.AgeMs, f.ActualW = nil, nil
		c.Feedback = append(c.Feedback, f)
	}
	return digest(c)
}

// Deadbands for lp's measured values. A charging car moves its power every
// reading; sending lp for each would cost about 12 MB a day. A change of
// state goes at once, a reading only when it moved by more than this, and
// the full lp every 15 minutes carries exact values. Live field 10 carries
// the site's charging power every period anyway.
const (
	lpPowerBandW      = 100
	lpPowerBandRatio  = 0.05
	lpEnergyBandWh    = 100
	lpSocBandPermille = 10
)

// lpChanged reports whether cur says something prev did not.
func lpChanged(prev, cur cloudwire.LP) bool {
	if lpKey(prev) != lpKey(cur) {
		return true
	}
	moved := func(a, b int64) bool {
		band := max(lpPowerBandW, int64(lpPowerBandRatio*float64(max(abs(a), abs(b)))))
		return abs(a-b) > band
	}
	for i, p := range cur.Loadpoints {
		q := prev.Loadpoints[i]
		if moved(q.PowerW, p.PowerW) || abs(q.SessionWh-p.SessionWh) >= lpEnergyBandWh ||
			(q.SocPermille == nil) != (p.SocPermille == nil) ||
			(p.SocPermille != nil && abs(*q.SocPermille-*p.SocPermille) >= lpSocBandPermille) {
			return true
		}
	}
	for i, f := range cur.Feedback {
		g := prev.Feedback[i]
		if (g.ActualW == nil) != (f.ActualW == nil) || (f.ActualW != nil && moved(*g.ActualW, *f.ActualW)) {
			return true
		}
	}
	return false
}

// carsKey is the car class without its ages and its measured values, which
// carsChanged compares with lp's deadbands instead.
func carsKey(c cloudwire.Cars) string {
	k := cloudwire.Cars{Cars: make([]cloudwire.Car, 0, len(c.Cars))}
	for _, car := range c.Cars {
		car.SocAgeS, car.SocPermille, car.EnergySinceWh = nil, nil, 0
		k.Cars = append(k.Cars, car)
	}
	return digest(k)
}

// carsChanged reports whether cur says something prev did not. The energy
// since a reading counts as lp's sessionWh does, and an estimated level as
// lp's level does, so a charging car does not send its class at every
// reading.
func carsChanged(prev, cur cloudwire.Cars) bool {
	if carsKey(prev) != carsKey(cur) {
		return true
	}
	for i, c := range cur.Cars {
		p := prev.Cars[i]
		if abs(p.EnergySinceWh-c.EnergySinceWh) >= lpEnergyBandWh ||
			(p.SocPermille == nil) != (c.SocPermille == nil) ||
			(c.SocPermille != nil && abs(*p.SocPermille-*c.SocPermille) >= lpSocBandPermille) {
			return true
		}
	}
	return false
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func pricesKey(p cloudwire.Prices) string {
	return digest([]any{p.T0Ms, p.SlotMs, p.SpotMinor, p.TotalMinor})
}

// rosterNumber builds the roster and its number. The number rises when the
// devices, the grant or the pending code differ from what it was last
// computed from.
//
// The grant has three shapes: the readable copy's while it is on; its id
// alone while a revoke waits for the receipt, which asks the service to
// delete the copy; and none while the copy is off.
func (l *Link) rosterNumber(snap grantSnapshot) (uint64, cloudwire.Roster) {
	r := cloudwire.Roster{TTLS: rosterTTLS, Grant: cloudwire.Grant{Controller: cloudwire.ControllerSourceful}}
	switch g := snap.g; {
	case g == nil:
	case g.Revoked:
		r.Grant.ID = g.ID
	default:
		r.Grant = cloudwire.Grant{ID: g.ID, Scopes: g.Scopes, Purposes: g.Purposes, Controller: g.Controller, TermsPolicy: g.Policy, StatsID: g.StatsID, StatsPolicy: g.StatsPolicy, StatsSinceMs: g.StatsAtMs}
	}
	if !snap.g.statisticsOnly() {
		roster := l.rosterSnapshot(snap.home)
		for _, d := range roster.Devices {
			var scopes []string
			for _, s := range protocol.CloudScopes {
				if slices.Contains(d.Scopes, s) {
					scopes = append(scopes, s)
				}
			}
			r.Devices = append(r.Devices, cloudwire.Device{ID: d.ID, Key: d.Key, Role: d.Role, Scopes: scopes,
				Epoch: d.Epoch, Tun: d.Tun, Agent: d.Agent})
		}
		sort.Slice(r.Devices, func(a, b int) bool { return r.Devices[a].ID < r.Devices[b].ID })
		r.Noise = roster.Noise
		if l.opts.WebPush != nil {
			var err error
			r.WebPush, err = l.opts.WebPush.WebPushRows()
			if err != nil {
				l.log.Warn("could not prepare browser offline notices")
			}
			sort.Slice(r.WebPush, func(a, b int) bool { return r.WebPush[a].ID < r.WebPush[b].ID })
		}
		for _, p := range roster.Pending {
			if left := time.Until(p.Expires); left > 0 && snap.home != "" {
				r.Pending = append(r.Pending, cloudwire.Pending{Hash: p.Hash, Kind: p.Kind, TTLS: int64((left + time.Second - 1) / time.Second)})
			}
		}
		sort.Slice(r.Pending, func(a, b int) bool { return string(r.Pending[a].Hash) < string(r.Pending[b].Hash) })
	}
	// The number follows what the roster says, not how long a code has
	// left, so it does not rise every second a code is shown.
	var pending []any
	for _, p := range r.Pending {
		pending = append(pending, []any{p.Kind, p.Hash})
	}
	sum := digest([]any{r.Devices, r.Grant, pending, r.Noise, r.WebPush})

	l.mu.Lock()
	defer l.mu.Unlock()
	state, err := loadJSON[rosterState](l.opts.KV, rosterKey)
	if err != nil || state == nil {
		state = &rosterState{}
	}
	if state.Digest != sum {
		state.Seq++
		state.Digest = sum
		if err := saveJSON(l.opts.KV, rosterKey, state); err != nil {
			l.log.Warn("could not store the FTW Cloud roster number", "err", err)
		}
	}
	r.RosterSeq = state.Seq
	return state.Seq, r
}

// bumpRoster moves the roster number past the service's.
func (l *Link) bumpRoster(past uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, err := loadJSON[rosterState](l.opts.KV, rosterKey)
	if err != nil || state == nil {
		state = &rosterState{}
	}
	state.Seq = past + 1
	if err := saveJSON(l.opts.KV, rosterKey, state); err != nil {
		l.log.Warn("could not store the FTW Cloud roster number", "err", err)
	}
	l.rosterDue = true
}

// --------------------------------------------------------------------------
// Preview
// --------------------------------------------------------------------------

// Preview is what GET /api/cloud/preview shows: the exact bytes that leave
// the house, and the same bytes decoded for reading.
type Preview struct {
	State string `json:"state"`
	Home  string `json:"home,omitempty"`
	// Sample is true when nothing has been sent yet: the frames are what the
	// box would send now, unsigned, with ids cut by the grant's key or, with
	// FTW Cloud off, by a key of zeros.
	Sample bool           `json:"sample"`
	Frames []PreviewFrame `json:"frames"`
}

// PreviewFrame is one class's last frame.
type PreviewFrame struct {
	T string `json:"t"`
	// Frame is the whole WebSocket message, base64 in JSON.
	Frame []byte `json:"frame"`
	// AgeMs is how long ago it was sent, by box uptime.
	AgeMs int64 `json:"ageMs"`
	// Body is the signed body decoded, for people to read. The frame is the
	// record; this is a rendering of it.
	Body any `json:"body"`
}

var previewDec, _ = cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}.DecMode()

// Preview returns the last frame of each class, or a sample.
func (l *Link) Preview() Preview {
	status := l.Status()
	l.mu.Lock()
	sent := make([]sentFrame, 0, len(l.sent))
	for _, f := range l.sent {
		sent = append(sent, f)
	}
	up := l.uptimeMs()
	l.mu.Unlock()
	sort.Slice(sent, func(a, b int) bool { return sent[a].T < sent[b].T })

	p := Preview{State: status.State, Home: status.Home, Frames: []PreviewFrame{}}
	if len(sent) == 0 {
		p.Sample = true
		sent = l.sample()
	}
	for _, f := range sent {
		pf := PreviewFrame{T: f.T, Frame: f.Frame, AgeMs: up - f.AtMs}
		if payload, err := cloudwire.DecodeFrame(f.Frame); err == nil {
			if env, err := cloudwire.DecodeEnvelope(payload); err == nil {
				var body any
				if previewDec.Unmarshal(env.B, &body) == nil {
					pf.Body = body
				}
			}
		}
		p.Frames = append(p.Frames, pf)
	}
	return p
}

// sample composes, unsigned and unsent, what the box would send now.
func (l *Link) sample() []sentFrame {
	snap := l.current()
	if snap.g == nil {
		snap.g = &grantRecord{GrantKey: make([]byte, cloudwire.GrantKeyBytes), Scopes: protocol.CloudScopes, Purposes: []string{protocol.CloudPurposeServe}}
	}
	up := l.uptimeMs()
	var out []sentFrame
	add := func(t string, body any) {
		env, err := cloudwire.Seal(nil, snap.home, t, body)
		if err != nil {
			return
		}
		raw, err := env.Encode()
		if err != nil {
			return
		}
		frame, err := cloudwire.EncodeFrame(raw)
		if err == nil {
			out = append(out, sentFrame{T: t, Frame: frame, AtMs: up})
		}
	}
	h := cloudwire.Header{Boot: l.boot, UptimeMs: up}
	gk := snap.g.GrantKey
	if snap.allowed(cloudwire.MsgLive) {
		live := l.project(gk, l.opts.Site.Live())
		live.Header = h
		add(cloudwire.MsgLive, live)
	}
	if lp, ok := l.opts.Site.Loadpoints(); ok && snap.allowed(cloudwire.MsgLP) {
		lp = l.projectLP(snap, lp)
		lp.Header = h
		add(cloudwire.MsgLP, lp)
	}
	if cs, ok := l.opts.Site.(CarSite); ok && snap.allowed(cloudwire.MsgCar) {
		if in, ok := cs.Cars(); ok {
			cars := ProjectCars(gk, in)
			cars.Header = h
			add(cloudwire.MsgCar, cars)
		}
	}
	if p, ok := l.opts.Site.Prices(); ok && snap.allowed(cloudwire.MsgPrices) {
		p.Header = h
		add(cloudwire.MsgPrices, p)
	}
	return out
}
