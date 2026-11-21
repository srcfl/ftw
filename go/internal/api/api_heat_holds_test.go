package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivercontrol"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/heatrun"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// sharedHoldsServer is controlServer with the holds handed in, the way main
// hands the same holds to the API and to the heat control.
func sharedHoldsServer(t *testing.T) (*Server, *drivercontrol.Holds, *telemetry.Store) {
	t.Helper()
	dir := t.TempDir()
	lua := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(lua, []byte(controlProbeLua), 0o600); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	cfg := config.Driver{Name: "heat", Lua: lua}
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("add driver: %v", err)
	}
	t.Cleanup(reg.ShutdownAll)
	holds := drivercontrol.New(func() drivercontrol.Registry { return reg })
	srv := New(&Deps{
		Tel:          tel,
		Registry:     reg,
		Cfg:          &config.Config{Drivers: []config.Driver{cfg}},
		CfgMu:        &sync.RWMutex{},
		DriverDir:    dir,
		ConfigPath:   filepath.Join(dir, "config.yaml"),
		ControlHolds: holds,
	})
	return srv, holds, tel
}

// With one set of holds, an operator's hold is the heat control's to see and
// leave alone, and a planned value is the operator's to see and release. Two
// sets would each think the driver free and expire into a default that
// undoes the other.
func TestTheAPIAndTheHeatControlShareOneHold(t *testing.T) {
	srv, holds, tel := sharedHoldsServer(t)
	if srv.controlHolds != holds {
		t.Fatal("the API built its own holds instead of using the ones it was given")
	}
	heat := heatrun.HoldsWriter{Holds: holds}

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("operator POST = %d, body %s", rec.Code, rec.Body.String())
	}
	if control, _, ok := heat.Active("heat"); !ok || control != "set_offset" {
		t.Fatalf("heat control sees hold %q %v, want the operator's set_offset", control, ok)
	}
	if rec := del(t, srv, "/api/drivers/heat/control"); rec.Code != http.StatusOK {
		t.Fatalf("operator DELETE = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
	if _, _, ok := heat.Active("heat"); ok {
		t.Fatal("the operator released, the heat control still sees a hold")
	}

	if _, err := heat.Hold(context.Background(), "heat", "set_offset", 1, 10*time.Minute); err != nil {
		t.Fatalf("planned hold: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/heat", nil))
	var detail driverDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Hold == nil || detail.Hold.Control != "set_offset" {
		t.Fatalf("the API shows hold %+v, want the planned set_offset", detail.Hold)
	}
	if rec := del(t, srv, "/api/drivers/heat/control"); rec.Code != http.StatusOK {
		t.Fatalf("operator DELETE of a planned hold = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "defaulted", 2)
	if _, _, ok := heat.Active("heat"); ok {
		t.Fatal("the operator released a planned hold, it is still armed")
	}
}

func del(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	return rec
}
