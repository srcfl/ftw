package main

import (
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/heatrun"
	"github.com/srcfl/ftw/go/internal/state"
)

// heatCatalog is the driver catalog as the heat control reads it. The
// reload path replaces it; the heat tick reads it from another goroutine.
type heatCatalog struct {
	mu      sync.Mutex
	drivers []config.Driver
	catalog []drivers.CatalogEntry
}

func (h *heatCatalog) set(cfgDrivers []config.Driver, catalog []drivers.CatalogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drivers = append([]config.Driver(nil), cfgDrivers...)
	h.catalog = catalog
}

// control is the declared control behind a driver instance's name. A driver
// that is disabled, observe-only or unknown has none.
func (h *heatCatalog) control(driver, control string) (drivers.CatalogControl, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.drivers {
		if d.Name != driver || d.Disabled || d.ObserveOnly {
			continue
		}
		entry := drivers.EntryForDriver(h.catalog, d.Lua)
		if entry == nil {
			return drivers.CatalogControl{}, false
		}
		for _, c := range entry.Controls {
			if c.ID == control {
				return c, true
			}
		}
	}
	return drivers.CatalogControl{}, false
}

// heatPrices reads the stored total price, the one the owner's limits are
// written in.
func heatPrices(st *state.Store, zone func() string) func(from, to time.Time) []heatrun.Price {
	return func(from, to time.Time) []heatrun.Price {
		z := zone()
		if st == nil || z == "" {
			return nil
		}
		rows, err := st.LoadPrices(z, from.Add(-time.Hour).UnixMilli(), to.UnixMilli())
		if err != nil {
			return nil
		}
		out := make([]heatrun.Price, 0, len(rows))
		for _, r := range rows {
			if r.SlotLenMin <= 0 {
				continue
			}
			out = append(out, heatrun.Price{
				Start: time.UnixMilli(r.SlotTsMs),
				Len:   time.Duration(r.SlotLenMin) * time.Minute,
				Value: r.TotalOreKwh,
			})
		}
		return out
	}
}

// heatOutdoor is the outdoor forecast for n quarters from start, from rows
// fetched within the last 12 hours, or false when any quarter is missing.
func heatOutdoor(st *state.Store, since func() int64) func(start time.Time, n int) ([]float64, bool) {
	return func(start time.Time, n int) ([]float64, bool) {
		if st == nil || n <= 0 {
			return nil, false
		}
		end := start.Add(time.Duration(n) * 15 * time.Minute)
		rows, err := st.LoadForecasts(start.Add(-2*time.Hour).UnixMilli(), end.UnixMilli())
		if err != nil {
			return nil, false
		}
		now := time.Now()
		out := make([]float64, n)
		for k := 0; k < n; k++ {
			t := start.Add(time.Duration(k)*15*time.Minute + 7*time.Minute).UnixMilli()
			found := false
			for _, r := range rows {
				slotLen := r.SlotLenMin
				if slotLen <= 0 {
					slotLen = 60
				}
				if t >= r.SlotTsMs && t < r.SlotTsMs+int64(slotLen)*60*1000 && r.TempC != nil &&
					usableTrainingWeather(r, now) && r.FetchedAtMs >= since() {
					out[k], found = *r.TempC, true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
		return out, true
	}
}
