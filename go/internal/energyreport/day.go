package energyreport

import (
	"context"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/protocol/energywire"
)

// Ledger is the part of the store a day is read from.
type Ledger interface {
	EnergyAssets() ([]state.EnergyAsset, error)
	LoadEnergyHistoryContext(context.Context, state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error)
}

// usable are the ledger qualities that count as a recorded bucket. A reset
// is one: a charger's session counter starts again at every plug-in, and the
// ledger covers that interval from the power reading. A gap, an invalid read
// or anything this list does not name makes the day incomplete.
var usable = map[string]bool{"measured": true, "recovered": true, "integrated": true, "estimated": true, "reset": true}

// sums maps a ledger flow to its place in energywire.Fields.
var sums = map[state.EnergyFlow]int{
	state.FlowPVGeneration:     0,
	state.FlowGridImport:       1,
	state.FlowGridExport:       2,
	state.FlowBatteryCharge:    3,
	state.FlowBatteryDischarge: 4,
	state.FlowVehicleCharge:    5,
}

const dayMS = int64(24 * time.Hour / time.Millisecond)

// Day reads the closed UTC day that starts at start. ok is false unless the
// whole day is there.
//
// Whole means: a site meter, and every grid meter, solar, battery and charger
// asset that was in use that day recorded every bucket of it, with nothing the
// ledger marks as a gap or invalid read. An asset that arrived or went
// silent during the day makes it incomplete; one that left before the day
// began is not part of it. A box that misses part of a day sends nothing for
// it rather than a sum that looks like a quiet day.
func Day(ctx context.Context, l Ledger, start time.Time) (energywire.Report, bool, error) {
	start = start.UTC().Truncate(24 * time.Hour)
	from := start.UnixMilli()
	to := from + dayMS
	assets, err := l.EnergyAssets()
	if err != nil {
		return energywire.Report{}, false, err
	}
	var wh [6]float64
	meter := false
	for _, a := range assets {
		switch a.Kind {
		case state.AssetGridMeter, state.AssetPV, state.AssetBattery, state.AssetVehicleCharger:
		default:
			continue
		}
		// Gone before the day began, or first seen after it ended.
		if a.LastSeenMS < from || a.FirstSeenMS >= to {
			continue
		}
		if a.FirstSeenMS > from || a.LastSeenMS < to {
			return energywire.Report{}, false, nil
		}
		points, truncated, err := l.LoadEnergyHistoryContext(ctx, state.EnergyHistoryQuery{
			AssetID: a.AssetID, SinceMS: from, UntilMS: to,
			BucketMS: state.EnergyLedgerBucketMS, Limit: int(dayMS / state.EnergyLedgerBucketMS),
		})
		if err != nil {
			return energywire.Report{}, false, err
		}
		if truncated || !covers(points, from, to) {
			return energywire.Report{}, false, nil
		}
		for _, p := range points {
			if i, ok := sums[p.Flow]; ok {
				wh[i] += p.EnergyWh
			}
		}
		meter = meter || a.Kind == state.AssetGridMeter
	}
	if !meter {
		return energywire.Report{}, false, nil
	}
	r := energywire.Report{Schema: energywire.Schema, Date: start.Format(time.DateOnly)}
	fields := []*int64{&r.PVGeneration, &r.GridImport, &r.GridExport, &r.BatteryCharge, &r.BatteryDischarge, &r.VehicleCharge}
	for i, v := range wh {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return energywire.Report{}, false, nil
		}
		*fields[i] = int64(math.Round(v / 1000))
	}
	return r, r.Valid(), nil
}

// covers reports whether the points fill [from, to) with usable buckets of
// one width and hold nothing else.
func covers(points []state.EnergyLedgerPoint, from, to int64) bool {
	if len(points) == 0 {
		return false
	}
	width := points[0].BucketLenMS
	if width <= 0 || (to-from)%width != 0 {
		return false
	}
	seen := map[int64]bool{}
	for _, p := range points {
		if p.BucketLenMS != width || p.BucketStartMS < from || p.BucketStartMS >= to || (p.BucketStartMS-from)%width != 0 || !usable[p.Quality] {
			return false
		}
		seen[p.BucketStartMS] = true
	}
	return int64(len(seen)) == (to-from)/width
}
