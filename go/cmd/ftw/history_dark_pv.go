package main

import (
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/sunpos"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Sun positions as zenith angles. After sunset (upper limb below the horizon,
// refraction included) no panel gets direct sunlight, but diffuse light
// remains until civil dusk, when the sun is 6° down.
const (
	sunsetZenithDeg    = 90.833
	civilDuskZenithDeg = 96
)

// historySun places the site for history. The zero value has no position,
// and then nothing is inferred.
type historySun struct {
	Lat, Lon float64
	Placed   bool
}

func historySunOf(lat, lon float64, ok bool) historySun {
	return historySun{Lat: lat, Lon: lon, Placed: ok}
}

func (s historySun) zenith(atMS int64) (float64, bool) {
	if !s.Placed {
		return 0, false
	}
	return sunpos.At(time.UnixMilli(atMS), s.Lat, s.Lon).ZenithDeg, true
}

// darkness tells live Home use how dark it is at the site, by the same
// sunset and civil-dusk limits as history.
func (s historySun) darkness(at time.Time) telemetry.Darkness {
	z, placed := s.zenith(at.UnixMilli())
	switch {
	case !placed || z <= sunsetZenithDeg:
		return telemetry.DarknessUnknown
	case z <= civilDuskZenithDeg:
		return telemetry.DarknessTwilight
	}
	return telemetry.DarknessNight
}

// darkPV records an unknown PV source as 0 W while the sun is down at the
// site: physics, not interpolation. Past civil dusk the 0 W is as good as a
// reading and carries no marker, so a night with a sleeping inverter also
// trains the load models; no separate bit records it. In twilight the
// snapshot carries EstimatedLoad: Home use counts it, training does not.
// Every other flow, and Load, keeps its own evidence.
func darkPV(p *state.HistorySources, sun historySun) bool {
	if p == nil || p.Missing&state.MissingPV == 0 {
		return false
	}
	z, placed := sun.zenith(p.AtMS)
	if !placed || z <= sunsetZenithDeg {
		return false
	}
	p.PVW = 0
	p.Missing &^= state.MissingPV
	if z <= civilDuskZenithDeg {
		p.Missing |= state.EstimatedLoad
	}
	return true
}

// optionalPVUnread marks a snapshot as an estimate while an optional PV
// source has no live reading. History leaves that flow out of Home use, but
// unseen PV would make a training label under-count the house. Past civil
// dusk the PV is 0 W by physics, so the night stays measured.
func optionalPVUnread(p *state.HistorySources, tel *telemetry.Store, flows []telemetry.ForecastFlow, sun historySun) bool {
	if p == nil || tel == nil {
		return false
	}
	unread := false
	for _, f := range flows {
		if f.DerType != telemetry.DerPV {
			continue
		}
		if h := tel.DriverHealth(f.Driver); tel.Get(f.Driver, telemetry.DerPV) == nil || h == nil || !h.TelemetryLive() {
			unread = true
		}
	}
	if !unread {
		return false
	}
	if z, placed := sun.zenith(p.AtMS); placed && z > civilDuskZenithDeg {
		return false
	}
	p.Missing |= state.EstimatedLoad
	return true
}

// historyDevices names the hardware behind a history snapshot, as the
// topology's devices line. The watchdog drops an offline driver from the
// energy record. After sunset a silent inverter is asleep, not replaced, so it
// keeps its place: the night stays one topology, and its minutes stay
// measured.
func historyDevices(st *state.Store, tel *telemetry.Store, identity energyIdentityLookup, observations []state.EnergyObservation, dark bool) string {
	var devices []string
	for _, o := range observations {
		if o.DeviceID != "" {
			devices = append(devices, string(o.AssetKind)+":"+o.DeviceID)
		}
	}
	if dark && st != nil && tel != nil && identity != nil {
		known := st.CachedDevices()
		for _, driver := range tel.OfflineDrivers(telemetry.DerPV) {
			if id := confirmedEnergyDeviceID(identity(driver), known); id != "" {
				devices = append(devices, string(state.AssetPV)+":"+id)
			}
		}
	}
	sort.Strings(devices)
	return "\ndevices:" + strings.Join(devices, "\n")
}
