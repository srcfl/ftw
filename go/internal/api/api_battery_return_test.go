package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/batteryreturn"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// seedBatteryReturn records three cycles of two hours charging from the
// grid and one hour sending the battery to the grid, ending in the hour
// before now. One cycle proves no efficiency: the battery may have started
// full.
func seedBatteryReturn(t *testing.T, now time.Time) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := now.Truncate(time.Hour).Add(-10 * time.Hour)
	var obs []state.EnergyObservation
	var imported, exported float64
	prev := base.Add(-10 * time.Minute)
	for at := prev; at.Before(now); at = at.Add(2 * time.Minute) {
		charge, discharge := 0.0, 0.0
		if h := int(at.Sub(base).Hours()); h >= 0 && h < 9 {
			if h%3 < 2 {
				charge = 2000
			} else {
				discharge = 3600
			}
		}
		dt := at.Sub(prev).Hours()
		imported += charge * dt
		exported += discharge * dt
		prev = at
		obs = append(obs,
			state.EnergyObservation{AssetID: "meter:1/grid_meter", DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridImport, AtMs: at.UnixMilli(), CounterWh: apiEnergyPtr(imported)},
			state.EnergyObservation{AssetID: "meter:1/grid_meter", DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridExport, AtMs: at.UnixMilli(), CounterWh: apiEnergyPtr(exported)},
			state.EnergyObservation{AssetID: "inverter:1/battery", DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: apiEnergyPtr(charge)},
			state.EnergyObservation{AssetID: "inverter:1/battery", DeviceID: "inverter:1", AssetKind: state.AssetBattery, Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: apiEnergyPtr(discharge)},
		)
	}
	if err := st.RecordTickWithOptionalHistory(nil, nil, obs); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBatteryReturnAPIGivesTheYearAndEveryHour(t *testing.T) {
	loc, err := time.LoadLocation(batteryreturn.Zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	year := now.In(loc).Year()
	st := seedBatteryReturn(t, now)
	cfg := &config.Config{Drivers: []config.Driver{{Name: "battery", BatteryCapacityWh: 4000}}}
	srv := New(&Deps{State: st, Cfg: cfg, CfgMu: &sync.RWMutex{}})
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	rr := get("/api/energy/battery-return?year=" + strconv.Itoa(year))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Year          int     `json:"year"`
		Zone          string  `json:"zone"`
		ReturnedWh    float64 `json:"returned_wh"`
		Hours         int     `json:"hours"`
		CompleteHours int     `json:"complete_hours"`
		Problem       string  `json:"problem"`
		Months        []struct {
			Month int `json:"month"`
		} `json:"months"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Year != year || got.Zone != batteryreturn.Zone || len(got.Months) != 12 || got.CompleteHours < 3 || got.Problem != "" {
		t.Fatalf("response %+v", got)
	}
	// Early on 1 January the charging hours belong to last year.
	if now.Add(-11*time.Hour).In(loc).Year() == year && got.ReturnedWh <= 0 {
		t.Fatalf("returned %.1f Wh after charging from the grid and exporting", got.ReturnedWh)
	}

	rr = get("/api/energy/battery-return.csv?year=" + strconv.Itoa(year))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Header().Get("Content-Disposition"), "ftw-battery-return-"+strconv.Itoa(year)+".csv") {
		t.Fatalf("csv status=%d headers=%v", rr.Code, rr.Header())
	}
	rows, err := csv.NewReader(rr.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != got.Hours+1 || rows[0][0] != "hour_start" || rows[0][9] != "returned_wh" || rows[1][14] != "4000.0" {
		t.Fatalf("%d rows for %d hours, header %v", len(rows), got.Hours, rows[0])
	}

	// The default is the year an owner applies for.
	rr = get("/api/energy/battery-return")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"year":`+strconv.Itoa(year-1)) {
		t.Fatalf("default status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Without a configured battery size there is no figure.
	rr = httptest.NewRecorder()
	New(&Deps{State: st}).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/energy/battery-return?year="+strconv.Itoa(year), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"problem":"battery_size"`) || !strings.Contains(rr.Body.String(), `"returned_wh":0`) {
		t.Fatalf("no size: status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, bad := range []string{"2017", strconv.Itoa(year + 1), "last"} {
		if rr := get("/api/energy/battery-return?year=" + bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("year=%s status=%d", bad, rr.Code)
		}
	}
}
