package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol"
)

// The runtime is what main.go hands the routes.
var _ CarEdits = (*cars.Runtime)(nil)

type carRows struct {
	mu   sync.Mutex
	rows map[string]string
}

func (k *carRows) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.rows[key]
	return v, ok
}

func (k *carRows) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows[key] = value
	return nil
}

// carsServer is a box with one charger and the cars runtime, as main.go
// wires them; on first start the box makes Car 1 from the charger.
func carsServer(t *testing.T) (*Server, *loadpoint.Manager, *cars.Runtime) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", VehicleCapacityWh: 60000, MaxChargeW: 11000}})
	rt := cars.New(cars.Deps{Store: &carRows{rows: map[string]string{}}, Loadpoints: mgr})
	if err := rt.Start(nil); err != nil {
		t.Fatal(err)
	}
	return New(&Deps{Cars: rt, Loadpoints: mgr}), mgr, rt
}

func carRequest(srv *Server, method, path, body string) (int, map[string]any) {
	rr := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const (
	carA = "0123456789abcdef0123456789abcdef"
	carB = "fedcba9876543210fedcba9876543210"
)

// The box page keeps the owner's list as the owner's app does: add, rename,
// resize and remove, with the ops' refusals.
func TestTheBoxPageKeepsTheCars(t *testing.T) {
	srv, _, rt := carsServer(t)

	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carA+`","name":"Volvo","capacity_wh":69000,"vin":"YV1AB0CD9EF123456"}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carB+`","name":"Kia","capacity_wh":64000,"vin":"YV1AB0CD9EF123456"}`); code != http.StatusConflict ||
		body["code"] != protocol.ErrDuplicate || body["reason"] != "duplicate" {
		t.Fatalf("a VIN another car has: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `{"id":"short","name":"Kia"}`); code != http.StatusBadRequest ||
		body["reason"] != "invalid" {
		t.Fatalf("a bad id: %d %v", code, body)
	}

	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"name":"Family Volvo","capacity_wh":70000}`); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, body)
	}
	got, err := rt.Car(carA)
	if err != nil || got.Name != "Family Volvo" || got.CapacityWh != 70000 || got.VIN != "YV1AB0CD9EF123456" {
		t.Fatalf("after the edit: %+v %v", got, err)
	}
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"capacity_wh":10}`); code != http.StatusBadRequest {
		t.Fatalf("a capacity the box refuses: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carB, `{"name":"Kia"}`); code != http.StatusNotFound ||
		body["reason"] != "car" {
		t.Fatalf("an unknown car: %d %v", code, body)
	}

	if code, body := carRequest(srv, http.MethodDelete, "/api/cars/"+carA, ""); code != http.StatusOK {
		t.Fatalf("remove: %d %v", code, body)
	}
	if code, _ := carRequest(srv, http.MethodDelete, "/api/cars/"+carA, ""); code != http.StatusNotFound {
		t.Fatalf("removed twice: %d", code)
	}
	_, list := carRequest(srv, http.MethodGet, "/api/cars", "")
	removed, _ := list["removed"].([]any)
	if cs, _ := list["cars"].([]any); len(cs) != 1 || len(removed) != 1 {
		t.Fatalf("after the remove: %v", list)
	}
}

// A viewer may see the cars but not keep them.
func TestOnlyAnOwnerKeepsTheCars(t *testing.T) {
	srv, _, _ := carsServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cars", strings.NewReader(`{"id":"`+carA+`","name":"Volvo"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(apiauth.WithCaller(req.Context(), apiauth.Caller{Role: apiauth.RoleViewer,
		Scopes: apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleViewer]...)}))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a viewer added a car: %d %s", rr.Code, rr.Body.String())
	}
}

// The one tap from the box page names the plug-in it answers, and the box
// refuses a tap for one that has ended.
func TestTheBoxPageTapsWhichCar(t *testing.T) {
	srv, mgr, rt := carsServer(t)
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `{"id":"`+carA+`","name":"Volvo","capacity_wh":69000}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	mgr.ObserveSession("garage", true, 0, 0, true, "easee:EH1", "s1")
	rt.Tick()
	session := rt.ChargerCar("garage").Session
	if session == "" {
		t.Fatal("no session after the plug-in")
	}

	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`"}`); code != http.StatusBadRequest {
		t.Fatalf("a tap that names no car: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car",
		`{"session":"old","car_id":"`+carA+`"}`); code != http.StatusConflict || body["reason"] != "session" {
		t.Fatalf("a tap for an ended plug-in: %d %v", code, body)
	}
	code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`","car_id":"`+carA+`"}`)
	charger, _ := body["charger"].(map[string]any)
	if code != http.StatusOK || charger["car_id"] != carA || charger["car_identified_by"] != protocol.CarIdentifiedByTap {
		t.Fatalf("the tap: %d %v", code, body)
	}
	if st, _ := mgr.State("garage"); st.CarID != carA || st.VehicleCapacityWh != 69000 {
		t.Fatalf("the charger does not plan for the Volvo: %+v", st)
	}
	// None of these: a visitor's.
	code, body = carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`","car_id":""}`)
	charger, _ = body["charger"].(map[string]any)
	if code != http.StatusOK || charger["car_id"] != nil || charger["car_identified_by"] != protocol.CarIdentifiedByNone {
		t.Fatalf("none of these: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/nowhere/car", `{"session":"s","car_id":""}`); code != http.StatusNotFound {
		t.Fatalf("an unknown charger: %d %v", code, body)
	}
}

// The routes that keep the list are the box page's own: a phone sends the
// ops as cmd frames, with their id and expiry, so the passthrough refuses
// these. The tap is actuation a phone sends as session.car.set.
func TestTheCarRoutesArePricedLikeTheOps(t *testing.T) {
	srv, _, _ := carsServer(t)
	for _, tc := range []struct {
		method, path string
		tier         apiauth.Tier
		op           string
	}{
		{http.MethodPost, "/api/cars", apiauth.TierLocal, ""},
		{http.MethodPatch, "/api/cars/" + carA, apiauth.TierLocal, ""},
		{http.MethodDelete, "/api/cars/" + carA, apiauth.TierLocal, ""},
		{http.MethodPost, "/api/loadpoints/garage/car", apiauth.TierActuate, protocol.OpSessionCarSet},
		{http.MethodGet, "/api/cars", apiauth.TierRead, ""},
	} {
		facts := srv.Route(httptest.NewRequest(tc.method, tc.path, nil))
		if facts.Tier != tc.tier || facts.CmdOp != tc.op {
			t.Errorf("%s %s: %+v", tc.method, tc.path, facts)
		}
	}
}

// The routes read their bodies with the ops' own checks, so the page and
// a phone are refused alike.
func TestTheCarRoutesCheckArgumentsAsTheOpsDo(t *testing.T) {
	srv, _, rt := carsServer(t)
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `{"id":"`+carA+`","name":"Volvo","capacity_wh":-69000}`); code != http.StatusBadRequest ||
		body["arg"] != "capacity_wh" || body["code"] != protocol.ErrUnknownOp {
		t.Fatalf("a negative size: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `["not","a","car"]`); code != http.StatusBadRequest {
		t.Fatalf("a body that is not an object: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carA+`","name":"Volvo","capacity_wh":69000,"identifiers":["RFID-1"]}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"identifiers":["RFID-1",2]}`); code != http.StatusBadRequest ||
		body["arg"] != "identifiers" {
		t.Fatalf("an identifier that is not text: %d %v", code, body)
	}
	// null clears the list, as car.update does.
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"identifiers":null}`); code != http.StatusOK {
		t.Fatalf("clear identifiers: %d %v", code, body)
	}
	if got, _ := rt.Car(carA); len(got.Identifiers) != 0 {
		t.Fatalf("identifiers after null: %v", got.Identifiers)
	}
	// The path names the car; an id in the body does not move the edit.
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"id":"`+carB+`","name":"Family Volvo"}`); code != http.StatusOK {
		t.Fatalf("rename: %d %v", code, body)
	}
	if got, _ := rt.Car(carA); got.Name != "Family Volvo" {
		t.Fatalf("the path's car was not renamed: %+v", got)
	}
}

// A request that carries a caller, as the app session's passthrough or a
// test puts one on the context.
func carsAs(srv *Server, method, path, body string, caller apiauth.Caller) int {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req.WithContext(apiauth.WithCaller(req.Context(), caller)))
	return rr.Code
}

func roleCaller(kind, role string) apiauth.Caller {
	return apiauth.Caller{Subject: kind + ":x", Kind: kind, Role: role, Scopes: apiauth.NewScopeSet(apiauth.RoleScopes[role]...)}
}

// Only the owner keeps the list: a viewer phone, a watch and an agent are
// refused even where a route reaches the handler.
func TestOnlyAnOwnersCallerKeepsTheCars(t *testing.T) {
	srv, _, _ := carsServer(t)
	for _, c := range []apiauth.Caller{
		roleCaller(apiauth.KindApp, apiauth.RoleViewer),
		roleCaller(apiauth.KindApp, apiauth.RoleWatch),
		roleCaller(apiauth.KindAgent, apiauth.RoleAgent),
	} {
		if code := carsAs(srv, http.MethodPost, "/api/cars", `{"id":"`+carA+`","name":"Volvo","capacity_wh":69000}`, c); code != http.StatusForbidden {
			t.Errorf("%s %s added a car: %d", c.Kind, c.Role, code)
		}
		if code := carsAs(srv, http.MethodDelete, "/api/cars/"+carA, "", c); code != http.StatusForbidden {
			t.Errorf("%s %s removed a car: %d", c.Kind, c.Role, code)
		}
	}
	if code := carsAs(srv, http.MethodPost, "/api/cars", `{"id":"`+carA+`","name":"Volvo","capacity_wh":69000}`,
		roleCaller(apiauth.KindApp, apiauth.RoleOwner)); code != http.StatusOK {
		t.Fatalf("the owner: %d", code)
	}
}

// With the house password on, a device on the LAN keeps the cars only with
// it, and a page on another site never does.
func TestTheHousePasswordGuardsTheCarRoutes(t *testing.T) {
	resetLANGuesses(t)
	srv, _, _ := carsServer(t)
	srv.deps.MutationPolicy = lanAuthPolicy(testHousePassword)
	post := func(auth, origin string) int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080/api/cars",
			strings.NewReader(`{"id":"`+carA+`","name":"Volvo","capacity_wh":69000}`))
		req.RemoteAddr = "192.168.1.10:43210"
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		srv.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	if code := post("", ""); code != http.StatusUnauthorized {
		t.Fatalf("without the house password: %d", code)
	}
	if code := post("Bearer "+testHousePassword, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("from another site: %d", code)
	}
	if code := post("Bearer "+testHousePassword, ""); code != http.StatusOK {
		t.Fatalf("with the house password: %d", code)
	}
}

// GET /api/cars names a car by its VIN and charge tags only to a caller
// that may see them and has proved itself as a protected request must.
func TestTheCarsListHidesWhoTheCarsAreFromAnUnprovedCaller(t *testing.T) {
	resetLANGuesses(t)
	srv, _, _ := carsServer(t)
	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carA+`","name":"Volvo","capacity_wh":69000,"vin":"YV1AB0CD9EF123456","identifiers":["RFID-1"]}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	read := func(policy MutationPolicy, url, remote, auth string, caller *apiauth.Caller) map[string]any {
		t.Helper()
		srv.deps.MutationPolicy = policy
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if caller != nil {
			req = req.WithContext(apiauth.WithCaller(req.Context(), *caller))
		}
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s from %s: %d %s", url, remote, rr.Code, rr.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return out
	}
	volvo := func(list map[string]any) map[string]any {
		for _, c := range list["cars"].([]any) {
			if m := c.(map[string]any); m["id"] == carA {
				return m
			}
		}
		t.Fatalf("no Volvo in %v", list)
		return nil
	}
	shows := func(name string, list map[string]any, full bool) {
		t.Helper()
		c := volvo(list)
		if got := c["vin"] != nil && c["identifiers"] != nil && list["redacted"] == nil; got != full {
			t.Errorf("%s: full = %v, want %v: %v", name, got, full, list)
		}
		if c["name"] != "Volvo" {
			t.Errorf("%s: the name is part of every view: %v", name, c)
		}
	}
	public := MutationPolicy{RequireTokenForRemote: true, Token: testMutationToken}
	shows("a public name without the token",
		read(public, "http://box.example.com/api/cars", "203.0.113.9:443", "", nil), false)
	shows("a public name with the token",
		read(public, "http://box.example.com/api/cars", "203.0.113.9:443", "Bearer "+testMutationToken, nil), true)
	shows("the LAN with that policy",
		read(public, "http://192.168.1.2:8080/api/cars", "192.168.1.10:43210", "", nil), true)
	shows("the LAN with the house password off",
		read(MutationPolicy{}, "http://ftw.local:8080/api/cars", "192.168.1.10:43210", "", nil), true)
	house := lanAuthPolicy(testHousePassword)
	shows("the LAN without the house password",
		read(house, "http://ftw.local:8080/api/cars", "192.168.1.10:43210", "", nil), false)
	shows("the LAN with the house password",
		read(house, "http://ftw.local:8080/api/cars", "192.168.1.10:43210", "Bearer "+testHousePassword, nil), true)
	shows("the box itself",
		read(house, "http://127.0.0.1:8080/api/cars", "127.0.0.1:43210", "", nil), true)
	for _, c := range []apiauth.Caller{roleCaller(apiauth.KindApp, apiauth.RoleWatch), roleCaller(apiauth.KindAgent, apiauth.RoleAgent),
		roleCaller(apiauth.KindApp, apiauth.RoleViewer)} {
		shows(c.Kind+" "+c.Role, read(public, "http://box.example.com/api/cars", "203.0.113.9:443", "", &c), false)
	}
	owner := roleCaller(apiauth.KindApp, apiauth.RoleOwner)
	shows("the owner's phone", read(public, "http://box.example.com/api/cars", "203.0.113.9:443", "", &owner), true)
}
