package appproto

import (
	"math"
	"strconv"

	"github.com/srcfl/ftw/go/internal/units"
)

// Source id prefixes are the box's own; only the shape matters to the app.

// sourceState grades a device by how long ago it last answered, in box uptime.
//
// The ladder is multiples of the device's own StaleAfterMs rather than fixed
// seconds, because a P1 meter answering every second and a cloud-polled
// inverter answering every minute are not comparable. "Down" is reserved for
// the driver saying so outright, which is a stronger claim than an old
// reading: a quiet device may simply be slow.
func sourceState(s Source, uptimeMs int64) SourceState {
	if s.LastOkUptimeMs < 0 {
		return SourceNever
	}
	if s.Offline {
		return SourceDown
	}

	staleAfter := s.StaleAfterMs
	if staleAfter <= 0 {
		// A source with no declared cadence cannot be aged. Claiming it is
		// live would be an assertion nothing supports.
		return SourceNever
	}

	age := uptimeMs - s.LastOkUptimeMs
	if age < 0 {
		age = 0
	}

	switch {
	case age <= staleAfter:
		return SourceLive
	case age <= 2*staleAfter:
		return SourceLagging
	case age <= 6*staleAfter:
		return SourceStale
	default:
		return SourceDown
	}
}

// wireSources renders the source table the app materialises per-field
// freshness from.
func wireSources(snap Snapshot, uptimeMs int64) map[string]SourceWire {
	out := make(map[string]SourceWire, len(snap.Sources))
	for _, s := range snap.Sources {
		lastOk := s.LastOkUptimeMs
		if lastOk < 0 {
			lastOk = 0
		}
		out[s.ID] = SourceWire{
			Kind:         s.Kind,
			Name:         s.Name,
			LastOkMs:     lastOk,
			StaleAfterMs: s.StaleAfterMs,
			State:        sourceState(s, uptimeMs),
		}
	}
	return out
}

// modeIndex is field 1's value: the mode's position in the catalogue that
// hello_ok sent.
//
// An index rather than a string keeps lane 0 numeric, which is what lets a
// delta stay tens of bytes inside a fixed bucket. -1 means the running mode is
// not in the catalogue, which should be impossible and is reported rather than
// papered over.
func modeIndex(modes []ModeInfo, key string) int64 {
	for i, m := range modes {
		if m.Key == key {
			return int64(i)
		}
	}
	return -1
}

// fieldValues renders the frozen fields.
//
// Everything on lane 0 is an integer. Watts are rounded and state of charge is
// carried as permille, which is the registry's unit for field 5 — the box's
// internal fraction converts here, at the wire edge, and nowhere else.
func fieldValues(snap Snapshot, modes []ModeInfo) map[string]int64 {
	f := map[string]int64{
		fidKey(FidMode):     modeIndex(modes, string(snap.Mode)),
		fidKey(FidGridW):    roundW(snap.GridW),
		fidKey(FidPvW):      roundW(snap.PVW),
		fidKey(FidBatteryW): roundW(snap.BatteryW),
		fidKey(FidLoadW):    roundW(snap.LoadW),
	}
	if snap.BatterySoCKnown {
		f[fidKey(FidBatterySoc)] = units.PermilleFromFraction(snap.BatterySoC)
	}
	if snap.EVWKnown {
		f[fidKey(FidEvW)] = roundW(snap.EVW)
	}
	return f
}

// roundW is the one place watts become integers. A NaN from a driver that lost
// its device becomes 0 rather than an encoding failure that would drop the
// whole frame — the source state is what tells the app not to trust it.
func roundW(w float64) int64 {
	if math.IsNaN(w) || math.IsInf(w, 0) {
		return 0
	}
	return int64(math.Round(w))
}

// fidKey renders a field id as the text map key the wire uses.
func fidKey(f Fid) string { return strconv.FormatUint(uint64(f), 10) }

// fieldDict maps each frozen field to its name, unit and source.
//
// The client caches this across sessions, which is what lets a delta be bare
// id-to-value pairs. srcId is what turns a reading into a freshness claim:
// field 2's age is the grid meter's age, not a timestamp of its own.
func fieldDict(srcGrid, srcPV, srcBattery string) map[string]FieldDef {
	orNil := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	def := func(f Fid, srcID string) FieldDef {
		return FieldDef{
			Name:  FrozenFieldNames[f],
			Unit:  orNil(FrozenFieldUnits[f]),
			SrcID: orNil(srcID),
		}
	}
	return map[string]FieldDef{
		// The mode is the box's own state, not a device reading, so it has
		// no source and no freshness of its own.
		fidKey(FidMode):       def(FidMode, ""),
		fidKey(FidGridW):      def(FidGridW, srcGrid),
		fidKey(FidPvW):        def(FidPvW, srcPV),
		fidKey(FidBatteryW):   def(FidBatteryW, srcBattery),
		fidKey(FidBatterySoc): def(FidBatterySoc, srcBattery),
		// Load is derived at the meter boundary, so it is only as fresh as
		// the meter.
		fidKey(FidLoadW): def(FidLoadW, srcGrid),
		// The charger sum has no single source — a site can hold several —
		// so it carries none, like the mode. The value's absence, not a
		// per-driver age, is what says the site has no charger.
		fidKey(FidEvW): def(FidEvW, ""),
	}
}

// sourcesEqual reports whether two rendered source tables say the same thing.
//
// LastOkMs moves every tick for a live source, so comparing it would make
// every delta carry the whole table and defeat the point. What the app needs
// to be told about is a change of state — a device going quiet mid-session is
// exactly what the freshness model exists to show.
func sourcesEqual(a, b map[string]SourceWire) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		bv, ok := b[id]
		if !ok {
			return false
		}
		if av.State != bv.State || av.Kind != bv.Kind || av.Name != bv.Name ||
			av.StaleAfterMs != bv.StaleAfterMs {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
