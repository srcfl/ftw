package drivers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func zaptecCloudDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "zaptec_cloud.lua")
}

func zaptecStateJSON(opMode int, powerW, sessionKWh float64) []byte {
	type obs struct {
		StateId       int    `json:"StateId"`
		ValueAsString string `json:"ValueAsString"`
	}
	rows := []obs{
		{StateId: 710, ValueAsString: strconv.Itoa(opMode)},
		{StateId: 513, ValueAsString: strconv.FormatFloat(powerW, 'f', -1, 64)},
		{StateId: 553, ValueAsString: strconv.FormatFloat(sessionKWh, 'f', -1, 64)},
		{StateId: 510, ValueAsString: "16"},
		{StateId: 512, ValueAsString: "3"},
		{StateId: 507, ValueAsString: "10"},
		{StateId: 501, ValueAsString: "230"},
	}
	b, _ := json.Marshal(rows)
	return b
}

type zaptecFake struct {
	loginHits   int
	listHits    int
	stateHits   int
	updateHits  int
	commandHits []int
	lastForm    string
	lastUpdate  string
	lastAuth    string
	loginCT     string
}

func (f *zaptecFake) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		f.loginHits++
		f.loginCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		f.lastForm = string(body)
		if !strings.Contains(f.lastForm, "grant_type=password") ||
			!strings.Contains(f.lastForm, "username=user%40example.com") ||
			!strings.Contains(f.lastForm, "password=secret") {
			http.Error(w, "bad creds", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-xyz",
			"expires_in":   3600,
		})
	case r.URL.Path == "/api/chargers" && r.Method == http.MethodGet:
		f.listHits++
		f.lastAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"Data":[{"Id":"chg-uuid-1","Name":"Garage","SerialNo":"ZAP123"}]}`))
	case strings.HasSuffix(r.URL.Path, "/state") && r.Method == http.MethodGet:
		f.stateHits++
		_, _ = w.Write(zaptecStateJSON(3, 7360, 4.2))
	case strings.HasSuffix(r.URL.Path, "/update") && r.Method == http.MethodPost:
		f.updateHits++
		body, _ := io.ReadAll(r.Body)
		f.lastUpdate = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case strings.Contains(r.URL.Path, "/sendCommand/"):
		idx := strings.LastIndex(r.URL.Path, "/")
		code, _ := strconv.Atoi(r.URL.Path[idx+1:])
		f.commandHits = append(f.commandHits, code)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Code":0}`))
	default:
		http.Error(w, "unknown route "+r.URL.Path, http.StatusNotFound)
	}
}

func TestZaptecCloudInitPollAndCommands(t *testing.T) {
	fake := &zaptecFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("zaptec", tel).WithHTTP()
	d, err := NewLuaDriver(zaptecCloudDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"email":    "user@example.com",
		"password": "secret",
		"base_url": srv.URL,
		"phases":   3,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if fake.loginHits != 1 {
		t.Fatalf("login hits=%d, want 1", fake.loginHits)
	}
	if !strings.HasPrefix(fake.loginCT, "application/x-www-form-urlencoded") {
		t.Errorf("login Content-Type=%q, want form-urlencoded", fake.loginCT)
	}
	if fake.listHits != 1 {
		t.Fatalf("list hits=%d, want 1 (auto-detect charger)", fake.listHits)
	}
	if fake.lastAuth != "Bearer tok-xyz" {
		t.Errorf("list auth=%q, want Bearer tok-xyz", fake.lastAuth)
	}

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	reading := tel.Get("zaptec", telemetry.DerEV)
	if reading == nil {
		t.Fatal("no EV reading after poll")
	}
	if reading.RawW != 7360 {
		t.Errorf("EV W=%v, want 7360 (charging, positive)", reading.RawW)
	}
	var extra map[string]any
	if err := json.Unmarshal(reading.Data, &extra); err != nil {
		t.Fatalf("decode EV data: %v", err)
	}
	if extra["connected"] != true {
		t.Errorf("connected=%v, want true", extra["connected"])
	}
	if extra["charging"] != true {
		t.Errorf("charging=%v, want true", extra["charging"])
	}
	if extra["session_wh"] != 4200.0 {
		t.Errorf("session_wh=%v, want 4200 (4.2 kWh)", extra["session_wh"])
	}

	// 11040 W / (230 V × 3) = 16 A.
	if err := d.Command(context.Background(), []byte(`{"action":"ev_set_current","power_w":11040}`)); err != nil {
		t.Fatalf("ev_set_current: %v", err)
	}
	if fake.updateHits != 1 {
		t.Fatalf("update hits=%d, want 1", fake.updateHits)
	}
	if !strings.Contains(fake.lastUpdate, "maxChargeCurrent") {
		t.Errorf("update body=%q, want maxChargeCurrent", fake.lastUpdate)
	}
	if !strings.Contains(fake.lastUpdate, "16") {
		t.Errorf("update body=%q, want 16 A", fake.lastUpdate)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"ev_pause"}`)); err != nil {
		t.Fatalf("ev_pause: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"ev_resume"}`)); err != nil {
		t.Fatalf("ev_resume: %v", err)
	}
	if len(fake.commandHits) < 2 {
		t.Fatalf("sendCommand hits=%v, want pause 506 then resume 507", fake.commandHits)
	}
	if fake.commandHits[0] != 506 {
		t.Errorf("first command=%d, want 506 (stop-final)", fake.commandHits[0])
	}
	if fake.commandHits[1] != 507 {
		t.Errorf("second command=%d, want 507 (resume)", fake.commandHits[1])
	}

	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}
}

func TestZaptecCloudLoginDoesNotLeakPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		http.Error(w, "invalid login: "+string(body), http.StatusUnauthorized)
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("zaptec", tel).WithHTTP()
	d, err := NewLuaDriver(zaptecCloudDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	err = d.Init(context.Background(), map[string]any{
		"email":    "user@example.com",
		"password": "supersecret",
		"base_url": srv.URL,
	})
	if err != nil && strings.Contains(err.Error(), "supersecret") {
		t.Errorf("password leaked into init error: %v", err)
	}
}
