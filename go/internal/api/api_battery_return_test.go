package api

import (
	"encoding/csv"
	"encoding/json"
	"math"
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
// full. Each driver named reads a battery that does the same; the first is
// "battery".
func seedBatteryReturn(t *testing.T, now time.Time, drivers ...string) *state.Store {
	t.Helper()
	if len(drivers) == 0 {
		drivers = []string{"battery"}
	}
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
		imported += charge * dt * float64(len(drivers))
		exported += discharge * dt * float64(len(drivers))
		prev = at
		obs = append(obs,
			state.EnergyObservation{AssetID: "meter:1/grid_meter", DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridImport, AtMs: at.UnixMilli(), CounterWh: apiEnergyPtr(imported)},
			state.EnergyObservation{AssetID: "meter:1/grid_meter", DeviceID: "meter:1", AssetKind: state.AssetGridMeter, Flow: state.FlowGridExport, AtMs: at.UnixMilli(), CounterWh: apiEnergyPtr(exported)},
		)
		for i, driver := range drivers {
			device := "inverter:" + strconv.Itoa(i+1)
			obs = append(obs,
				state.EnergyObservation{AssetID: device + "/battery", DeviceID: device, AssetKind: state.AssetBattery, Label: driver, Flow: state.FlowBatteryCharge, AtMs: at.UnixMilli(), PowerW: apiEnergyPtr(charge)},
				state.EnergyObservation{AssetID: device + "/battery", DeviceID: device, AssetKind: state.AssetBattery, Label: driver, Flow: state.FlowBatteryDischarge, AtMs: at.UnixMilli(), PowerW: apiEnergyPtr(discharge)},
			)
		}
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
	if len(rows) != got.Hours+1 || rows[0][0] != "hour_start" || rows[0][11] != "returned_wh" || rows[1][1] != "inverter:1/battery" || rows[1][16] != "4000.0" {
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

func TestBatteryReturnAPICountsEachBattery(t *testing.T) {
	loc, err := time.LoadLocation(batteryreturn.Zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	year := strconv.Itoa(now.In(loc).Year())
	st := seedBatteryReturn(t, now, "battery", "garage")
	// The car's size does not make its charger a battery FTW must read.
	cfg := &config.Config{
		Drivers:    []config.Driver{{Name: "battery", BatteryCapacityWh: 4000}, {Name: "garage", BatteryCapacityWh: 4000}, {Name: "car", BatteryCapacityWh: 60_000}},
		Loadpoints: []config.Loadpoint{{ID: "drive", DriverName: "car"}},
	}
	srv := New(&Deps{State: st, Cfg: cfg, CfgMu: &sync.RWMutex{}})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/energy/battery-return?year="+year, nil))
	var got struct {
		ReturnedWh float64  `json:"returned_wh"`
		Hours      int      `json:"hours"`
		Problem    string   `json:"problem"`
		Unread     []string `json:"unread"`
		Batteries  []struct {
			Label      string  `json:"label"`
			Sized      bool    `json:"sized"`
			ReturnedWh float64 `json:"returned_wh"`
		} `json:"batteries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Problem != "" || len(got.Unread) != 0 || len(got.Batteries) != 2 || !got.Batteries[0].Sized || !got.Batteries[1].Sized ||
		got.Batteries[0].ReturnedWh != got.Batteries[1].ReturnedWh || math.Abs(got.Batteries[0].ReturnedWh+got.Batteries[1].ReturnedWh-got.ReturnedWh) > 0.01 {
		t.Fatalf("response %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/energy/battery-return.csv?year="+year, nil))
	rows, err := csv.NewReader(rr.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// One row per battery and hour; the figure is the sum of returned_wh.
	if len(rows) != 2*got.Hours+1 || rows[1][1] != "inverter:1/battery" || rows[2][1] != "inverter:2/battery" || rows[1][0] != rows[2][0] {
		t.Fatalf("%d rows for %d hours: %v %v", len(rows), got.Hours, rows[1], rows[2])
	}
	var sum float64
	for _, row := range rows[1:] {
		v, err := strconv.ParseFloat(row[11], 64)
		if err != nil {
			t.Fatal(err)
		}
		sum += v
	}
	if math.Abs(sum-got.ReturnedWh) > 0.1*float64(len(rows)) {
		t.Fatalf("csv sums to %.1f Wh, figure %.1f", sum, got.ReturnedWh)
	}
}

func TestOnlyASwedishPositionPlacesTheSite(t *testing.T) {
	for _, c := range []struct {
		lat, lon float64
		want     bool
	}{
		{59.6, 16.5, true}, {67.86, 20.23, true}, {55.6, 13.0, true},
		{16.5, 59.6, false},       // swapped
		{59.6, -16.5, false},      // lost sign
		{59.3293, 18.0686, false}, // setup's starting pin
		{0, 0, false}, {60.17, 24.94 + 1, false},
	} {
		if got := placed(c.lat, c.lon); got != c.want {
			t.Fatalf("placed(%v, %v) = %v", c.lat, c.lon, got)
		}
	}
}
