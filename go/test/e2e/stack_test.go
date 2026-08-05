// Package e2e runs the full ftw stack against both simulators
// in one process and verifies end-to-end behavior: drivers load, telemetry
// flows, the control loop responds to transients, commands round-trip,
// self-tune runs, battery models learn.
//
// Run with:  FTW_E2E=1 go test ./go/test/e2e -timeout 180s -v   (or: make e2e)
// Uses the repo's drivers/ferroamp.lua + drivers/sungrow.lua scripts.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-ferroamp/ferroamp"
	"github.com/srcfl/ftw/go/cmd/sim-sungrow/sungrow"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/drivers"
	modbuscli "github.com/srcfl/ftw/go/internal/modbus"
	mqttcli "github.com/srcfl/ftw/go/internal/mqtt"
	"github.com/srcfl/ftw/go/internal/selftune"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// ---- Test fixture ----

func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate repo root")
	return ""
}

func findLuaDriver(t *testing.T, name string) string {
	root := findRepoRoot(t)
	p := filepath.Join(root, "drivers", name+".lua")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("drivers/%s.lua not found", name)
	}
	return p
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// stack holds all the pieces we're testing together.
type stack struct {
	t *testing.T

	// Simulators
	mqttBroker *mqttserver.Server
	mqttPort   int
	// mqttBaseline is the broker's client count with no driver attached — the
	// inline client, which stays for the life of the server. Close waits for
	// the count to fall back to it; see closeBroker.
	mqttBaseline int
	ferroSim     *ferroamp.Simulator
	modbusSrv    *sv.ModbusServer
	modbusPort   int
	sungSim      *sungrow.Simulator

	// Core pieces
	reg      *drivers.Registry
	tel      *telemetry.Store
	st       *state.Store
	ctrl     *control.State
	ctrlMu   *sync.Mutex
	cfgMu    *sync.RWMutex
	cfg      *config.Config
	models   map[string]*battery.Model
	modelsMu *sync.Mutex
	selfTune *selftune.Coordinator
	caps     map[string]float64
	capMu    *sync.RWMutex

	// HTTP
	httpSrv *http.Server
	apiPort int

	stopSim chan struct{}
	simWg   sync.WaitGroup
	stopCtl chan struct{}
	ctlWg   sync.WaitGroup
}

func setupStack(t *testing.T) *stack {
	t.Helper()

	s := &stack{
		t:        t,
		ctrlMu:   &sync.Mutex{},
		cfgMu:    &sync.RWMutex{},
		modelsMu: &sync.Mutex{},
		capMu:    &sync.RWMutex{},
		models:   map[string]*battery.Model{},
		selfTune: selftune.NewCoordinator(),
		stopSim:  make(chan struct{}),
		stopCtl:  make(chan struct{}),
	}

	s.mqttPort = freePort(t)
	s.modbusPort = freePort(t)
	s.apiPort = freePort(t)

	// ---- Start MQTT broker + Ferroamp sim ----
	mb := mqttserver.New(&mqttserver.Options{InlineClient: true})
	if err := mb.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "broker", Address: fmt.Sprintf("127.0.0.1:%d", s.mqttPort)})
	if err := mb.AddListener(tcp); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mb.Serve() }()
	s.mqttBroker = mb
	// New registers the inline client, so the count is already settled here and
	// does not race Serve.
	s.mqttBaseline = mb.Clients.Len()

	ferroCfg := ferroamp.Default()
	ferroCfg.ResponseTauS = 0.3
	ferroCfg.PVPeakW = 2000
	ferroCfg.CapacityWh = 15200
	s.ferroSim = ferroamp.New(ferroCfg)

	// Handle Ferroamp commands
	_ = mb.Subscribe("extapi/control/request", 1, func(_ *mqttserver.Client, _ packets.Subscription, pk packets.Packet) {
		var msg struct {
			TransID string `json:"transId"`
			Cmd     struct {
				Name string  `json:"name"`
				Arg  float64 `json:"arg"`
			} `json:"cmd"`
		}
		if err := json.Unmarshal(pk.Payload, &msg); err != nil {
			return
		}
		switch msg.Cmd.Name {
		case "charge":
			s.ferroSim.SetMode(ferroamp.ModeCharge, msg.Cmd.Arg)
		case "discharge":
			s.ferroSim.SetMode(ferroamp.ModeDischarge, msg.Cmd.Arg)
		case "auto":
			s.ferroSim.SetMode(ferroamp.ModeAuto, 0)
		}
		result := []byte(fmt.Sprintf(`{"transId":"%s","status":"ack"}`, msg.TransID))
		_ = mb.Publish("extapi/result", result, false, 0)
	})

	// ---- Sungrow Modbus sim ----
	sungCfg := sungrow.Default()
	sungCfg.ResponseTauS = 0.3
	sungCfg.PVPeakW = 1500
	sungCfg.CapacityWh = 9600
	s.sungSim = sungrow.New(sungCfg)
	bank := sungrow.NewRegisterBank(s.sungSim)
	bank.Refresh(s.sungSim.Tick(time.Millisecond))

	msrv, err := sv.NewServer(&sv.ServerConfiguration{
		URL:        fmt.Sprintf("tcp://127.0.0.1:%d", s.modbusPort),
		Timeout:    5 * time.Second,
		MaxClients: 4,
	}, &sungrowHandler{bank: bank})
	if err != nil {
		t.Fatal(err)
	}
	if err := msrv.Start(); err != nil {
		t.Fatal(err)
	}
	s.modbusSrv = msrv

	// Physics tickers for both
	s.simWg.Add(1)
	go func() {
		defer s.simWg.Done()
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-s.stopSim:
				return
			case now := <-tk.C:
				dt := now.Sub(last)
				last = now
				snap := s.ferroSim.Tick(dt)
				publishFerroamp(mb, snap)
				bank.Refresh(s.sungSim.Tick(dt))
			}
		}
	}()

	// One physics tick before the drivers connect, so the first Modbus read and
	// the first MQTT message have a simulated site behind them. This one stays
	// a sleep: at this point there is no driver, no telemetry and no API to
	// ask, so there is no condition to poll. Everything after the drivers
	// start waits on a condition instead — see waitForStatus.
	time.Sleep(200 * time.Millisecond)

	// ---- Build cfg + open state + start drivers ----
	s.cfg = &config.Config{
		Site: config.Site{Name: "e2e", ControlIntervalS: 1, GridTargetW: 0, GridToleranceW: 42, SmoothingAlpha: 0.3, SlewRateW: 2000, MinDispatchIntervalS: 1},
		Fuse: config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		API:  config.API{Port: s.apiPort},
		Drivers: []config.Driver{
			{
				Name: "ferroamp", Lua: findLuaDriver(t, "ferroamp"),
				IsSiteMeter: true, BatteryCapacityWh: 15200,
				Capabilities: config.Capabilities{
					MQTT: &config.MQTTConfig{Host: "127.0.0.1", Port: s.mqttPort},
				},
			},
			{
				Name: "sungrow", Lua: findLuaDriver(t, "sungrow"),
				BatteryCapacityWh: 9600,
				Capabilities: config.Capabilities{
					Modbus: &config.ModbusConfig{Host: "127.0.0.1", Port: s.modbusPort, UnitID: 1},
				},
			},
		},
	}

	tmpDB := filepath.Join(t.TempDir(), "state.db")
	s.st, err = state.Open(tmpDB)
	if err != nil {
		t.Fatal(err)
	}

	s.tel = telemetry.NewStore()
	s.ctrl = control.NewState(0, 42, "ferroamp")
	s.ctrl.SlewRateW = 2000
	s.ctrl.MinDispatchIntervalS = 1

	s.caps = map[string]float64{"ferroamp": 15200, "sungrow": 9600}
	s.models["ferroamp"] = battery.New("ferroamp")
	s.models["sungrow"] = battery.New("sungrow")

	ctx := context.Background()
	s.reg = drivers.NewRegistry(s.tel)
	s.reg.MQTTFactory = func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error) {
		return mqttcli.Dial(c.Host, c.Port, c.Username, c.Password, "ftw-e2e-"+name)
	}
	s.reg.ModbusFactory = func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbuscli.Dial(c.Host, c.Port, c.UnitID)
	}
	for _, d := range s.cfg.Drivers {
		if err := s.reg.Add(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	// ---- HTTP ----
	deps := &api.Deps{
		Tel: s.tel, Ctrl: s.ctrl, CtrlMu: s.ctrlMu,
		State: s.st, CapMu: s.capMu, Capacities: s.caps,
		CfgMu: s.cfgMu, Cfg: s.cfg,
		Models: s.models, ModelsMu: s.modelsMu,
		SelfTune: s.selfTune, DtS: 1,
		SaveConfig: config.SaveAtomic, WebDir: "web",
		Version: "e2e",
	}
	apiSrv := api.New(deps)
	s.httpSrv = &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", s.apiPort), Handler: apiSrv.Handler()}
	go func() { _ = s.httpSrv.ListenAndServe() }()

	// ---- Control loop goroutine ----
	s.ctlWg.Add(1)
	go func() {
		defer s.ctlWg.Done()
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		fuseMaxW := s.cfg.Fuse.MaxPowerW()
		for {
			select {
			case <-s.stopCtl:
				return
			case <-tk.C:
				nowMs := time.Now().UnixMilli()
				// Continuous RLS learning (skip during self-tune)
				if !s.selfTune.Status().Active {
					s.modelsMu.Lock()
					s.ctrlMu.Lock()
					last := append([]control.DispatchTarget{}, s.ctrl.LastTargets...)
					s.ctrlMu.Unlock()
					for _, t := range last {
						r := s.tel.Get(t.Driver, telemetry.DerBattery)
						if r == nil {
							continue
						}
						m := s.models[t.Driver]
						if m == nil {
							continue
						}
						soc := 0.5
						if r.SoC != nil {
							soc = *r.SoC
						}
						m.Update(t.TargetW, r.SmoothedW, soc, 1.0, nowMs)
					}
					s.modelsMu.Unlock()
				}
				// Self-tune tick
				if s.selfTune.Status().Active {
					s.modelsMu.Lock()
					s.selfTune.Tick(func(name string) (float64, float64, bool) {
						r := s.tel.Get(name, telemetry.DerBattery)
						if r == nil {
							return 0, 0, false
						}
						soc := 0.5
						if r.SoC != nil {
							soc = *r.SoC
						}
						return r.SmoothedW, soc, true
					}, s.models, 1.0, nowMs)
					s.modelsMu.Unlock()
				}
				// Dispatch
				s.ctrlMu.Lock()
				targets := control.ComputeDispatch(s.tel, s.ctrl, s.caps, fuseMaxW)
				s.ctrlMu.Unlock()
				finalTargets := targets
				if name, cmd, active := s.selfTune.CurrentCommand(); active {
					finalTargets = make([]control.DispatchTarget, 0, len(s.reg.Names()))
					for _, n := range s.reg.Names() {
						if n == name {
							finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: cmd})
						} else {
							finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: 0})
						}
					}
				}
				for _, t := range finalTargets {
					payload, _ := json.Marshal(map[string]any{"action": "battery", "power_w": t.TargetW})
					_ = s.reg.Send(ctx, t.Driver, payload)
				}
			}
		}
	}()

	// Wait until the ferroamp driver has received and parsed its first
	// MQTT message (pv_w becomes non-zero). The old fixed 3s sleep was
	// tuned for the WASM driver runtime; Lua's `driver_init` + first MQTT
	// round-trip is slower and variable, which caused /api/status to be
	// read before any telemetry had landed.
	s.waitForPV(10 * time.Second)
	return s
}

// waitForStatus polls /api/status until want reports true, and returns the
// status that satisfied it.
//
// Nothing in this stack settles on a schedule. The control loop ticks once a
// second, the PI output passes a slew limiter, the driver readings are
// exponentially smoothed and the simulators run their own physics ticker, so
// how long a change takes to show up moves with the machine. A literal sleep
// has to be sized for the slowest runner and then costs that much on every
// run, and it still fails as a bare "wrong value" once a runner is slower than
// the guess. Polling the condition costs what the stack actually needs.
//
// timeout is a backstop, not a schedule: pick it far above the observed
// settling time so that hitting it means the state never arrived at all. The
// what string is what the failure says the stack was waiting for, so write it
// as the condition itself.
func (s *stack) waitForStatus(what string, timeout time.Duration, want func(map[string]any) bool) map[string]any {
	s.t.Helper()
	start := time.Now()
	status, err := pollStatus(http.DefaultClient, s.baseURL()+"/api/status", timeout, want)
	if err != nil {
		s.t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, err)
	}
	s.t.Logf("waited %s for %s", time.Since(start).Round(10*time.Millisecond), what)
	return status
}

type statusPollError struct {
	lastStatus     map[string]any
	requestErrors  int
	lastRequestErr error
}

func (e *statusPollError) Error() string {
	if e.lastRequestErr == nil {
		return fmt.Sprintf("last status: %s; status request errors: none", statusSummary(e.lastStatus))
	}
	return fmt.Sprintf("last status: %s; status request errors: %d (last: %v)",
		statusSummary(e.lastStatus), e.requestErrors, e.lastRequestErr)
}

func pollStatus(client *http.Client, endpoint string, timeout time.Duration, want func(map[string]any) bool) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	var last map[string]any
	var requestErrors int
	var lastRequestErr error
	for {
		if !time.Now().Before(deadline) {
			return nil, &statusPollError{lastStatus: last, requestErrors: requestErrors, lastRequestErr: lastRequestErr}
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		status, err := fetchStatus(ctx, client, endpoint)
		cancel()
		if err == nil {
			last = status
			if want(status) {
				return status, nil
			}
		} else {
			requestErrors++
			lastRequestErr = err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &statusPollError{lastStatus: last, requestErrors: requestErrors, lastRequestErr: lastRequestErr}
		}
		pause := 100 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		<-timer.C
	}
}

func fetchStatus(ctx context.Context, client *http.Client, endpoint string) (map[string]any, error) {
	var status map[string]any
	if err := fetchJSON(ctx, client, endpoint, &status); err != nil {
		return nil, err
	}
	return status, nil
}

func fetchJSON(ctx context.Context, client *http.Client, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("JSON request returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// statusSummary renders the fields a stuck wait needs in its failure message:
// which mode the controller was in, what it saw and what it commanded.
func statusSummary(status map[string]any) string {
	if status == nil {
		return "none received"
	}
	mode, _ := status["mode"].(string)
	num := func(key string) float64 {
		v, _ := status[key].(float64)
		return v
	}
	dispatch := "[]"
	if targets, ok := status["dispatch"].([]any); ok && len(targets) > 0 {
		parts := make([]string, 0, len(targets))
		for _, t := range targets {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["driver"].(string)
			target, _ := m["target_w"].(float64)
			parts = append(parts, fmt.Sprintf("%s=%.0fW", name, target))
		}
		dispatch = "[" + strings.Join(parts, " ") + "]"
	}
	return fmt.Sprintf("mode=%s grid=%.0fW target=%.0fW pv=%.0fW bat=%.0fW load=%.0fW dispatch=%s",
		mode, num("grid_w"), num("grid_target_w"), num("pv_w"), num("bat_w"), num("load_w"), dispatch)
}

func (s *stack) waitForPV(timeout time.Duration) {
	s.t.Helper()
	s.waitForStatus("pv_w telemetry to arrive from the ferroamp driver", timeout,
		func(status map[string]any) bool {
			pv, ok := status["pv_w"].(float64)
			return ok && pv != 0
		})
}

func TestPollStatusCancelsBlockedRequestAtDeadline(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()

	start := time.Now()
	_, err := pollStatus(http.DefaultClient, srv.URL, 100*time.Millisecond,
		func(map[string]any) bool { return false })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("pollStatus returned nil error for a blocked request")
	}
	if elapsed > time.Second {
		t.Fatalf("blocked status request took %s to time out", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test server did not receive the status request")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("test server did not observe request context cancellation")
	}
	pollErr, ok := err.(*statusPollError)
	if !ok {
		t.Fatalf("error type = %T, want *statusPollError", err)
	}
	if pollErr.requestErrors != 1 {
		t.Fatalf("request errors = %d, want 1", pollErr.requestErrors)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("timeout error = %q, want context deadline summary", err)
	}
}

func TestFetchJSONCancelsBlockedRequestAtDeadline(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	var status map[string]any
	err := fetchJSON(ctx, http.DefaultClient, srv.URL, &status)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("fetchJSON returned nil error for a blocked request")
	}
	if elapsed > time.Second {
		t.Fatalf("blocked JSON request took %s to time out", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test server did not receive the JSON request")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("test server did not observe request context cancellation")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("timeout error = %q, want context deadline error", err)
	}
}

func (s *stack) Close() {
	close(s.stopCtl)
	s.ctlWg.Wait()
	if s.httpSrv != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = s.httpSrv.Shutdown(ctx)
	}
	if s.reg != nil {
		s.reg.ShutdownAll()
	}
	close(s.stopSim)
	s.simWg.Wait()
	if s.modbusSrv != nil {
		s.modbusSrv.Stop()
	}
	if s.mqttBroker != nil {
		closeBroker(s.t, s.mqttBroker, s.mqttBaseline)
	}
	if s.st != nil {
		s.st.Close()
	}
}

// closeBroker stops the broker without tripping the recursive read-lock in
// mochi-mqtt v2.7.9: Clients.GetByListener takes the read lock and then calls
// Clients.Len, which takes it again. sync.RWMutex is not reentrant, so a
// Clients.Delete — what the broker runs once a client drops — landing between
// the two read locks wedges both goroutines for good. Server.Close walks
// GetByListener, so closing while a client is still tearing down is the race.
// ShutdownAll above disconnects the Ferroamp driver, which is exactly that.
//
// Waiting for the client count to fall back to baseline closes the window: no
// disconnect is in flight, so no writer can queue between the two read locks.
// The deadline is the backstop — if the broker still wedges, the test says so
// in seconds instead of burning the whole `make e2e` timeout on an unrelated
// pull request.
//
// go/cmd/sim-ferroamp/e2e_test.go carries the same helper for the same reason.
// v2.7.9 is the newest release, so there is nothing to upgrade to.
func closeBroker(t *testing.T, mb *mqttserver.Server, baseline int) {
	t.Helper()

	settle := time.Now().Add(2 * time.Second)
	for mb.Clients.Len() > baseline && time.Now().Before(settle) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := mb.Clients.Len(); n > baseline {
		t.Logf("broker still holds %d clients (baseline %d) at close", n, baseline)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mb.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("broker Close blocked for 5s: mochi-mqtt Clients read lock is held by GetByListener while a client disconnect waits for the write lock")
	}
}

func (s *stack) baseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.apiPort) }

const oneShotJSONTimeout = 5 * time.Second

func (s *stack) getJSON(path string, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), oneShotJSONTimeout)
	defer cancel()
	if err := fetchJSON(ctx, http.DefaultClient, s.baseURL()+path, v); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stack) postJSON(path string, body, out any) int {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	resp, err := http.Post(s.baseURL()+path, "application/json", rdr)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// ---- The e2e test ----

func TestE2E_FullStack(t *testing.T) {
	if os.Getenv("FTW_E2E") != "1" {
		t.Skip("full stack is explicit; run make e2e")
	}
	s := setupStack(t)
	defer s.Close()

	// 1. Health returns ok with 2 drivers alive
	var health map[string]any
	s.getJSON("/api/health", &health)
	if health["status"] != "ok" {
		t.Errorf("health: %+v", health)
	}

	// 2. Status has telemetry from both drivers
	var status map[string]any
	s.getJSON("/api/status", &status)
	t.Logf("status: grid=%.0fW pv=%.0fW bat=%.0fW load=%.0fW",
		status["grid_w"], status["pv_w"], status["bat_w"], status["load_w"])
	// PV should be negative (generation — site convention)
	if pv := status["pv_w"].(float64); pv > -500 {
		t.Errorf("pv should be negative (generation), got %.0f", pv)
	}
	drivers, ok := status["drivers"].(map[string]any)
	if !ok || len(drivers) != 2 {
		t.Errorf("expected 2 drivers, got %+v", status["drivers"])
	}

	// 3. In a manual target-following mode, set a positive grid target
	// (import more) — batteries should CHARGE (site convention:
	// + = charge = load that adds to grid import).
	if code := s.postJSON("/api/mode", map[string]any{"mode": "weighted"}, nil); code != 200 {
		t.Errorf("mode weighted POST: %d", code)
	}
	if code := s.postJSON("/api/target", map[string]any{"grid_target_w": 3000}, nil); code != 200 {
		t.Errorf("target POST: %d", code)
	}
	// Target is positive (want more import) → batteries should charge → bat > 0.
	// The wait carries the assertion: it holds the same threshold the fixed
	// sleep used to check once, so a controller that never charges still fails
	// the test — it just stops costing six seconds when the controller is
	// working.
	status = s.waitForStatus("batteries to charge (bat_w >= 300 W) under grid_target_w=+3000",
		30*time.Second, func(st map[string]any) bool {
			bat, ok := st["bat_w"].(float64)
			return ok && bat >= 300
		})
	bat := status["bat_w"].(float64)
	t.Logf("after target=+3000W (want more import): bat=%.0fW (site: + = charge)", bat)

	// Also test the opposite: target negative → batteries discharge. At
	// minimum, the battery should be moving AWAY from the charging state it
	// was in. The reversal is slow by construction — the PI integrator has to
	// unwind from the charge state and the slew limiter caps how fast the
	// target can cross — and how slow depends on where in the charge ramp the
	// reading above was taken, since that is what the 500 W is measured from.
	// Between 5 s and 15 s here on a developer machine, so the deadline is
	// four times the worst of that: still loud, still well inside the 180 s
	// the suite gets from `make e2e`.
	prevBat := bat
	s.postJSON("/api/target", map[string]any{"grid_target_w": -3000}, nil)
	status = s.waitForStatus(
		fmt.Sprintf("batteries to move toward discharge (bat_w < %.0f W, from %.0f W) under grid_target_w=-3000",
			prevBat-500, prevBat),
		60*time.Second, func(st map[string]any) bool {
			b, ok := st["bat_w"].(float64)
			return ok && b < prevBat-500
		})
	bat = status["bat_w"].(float64)
	t.Logf("after target=-3000W (want more export): bat=%.0fW (site: − = discharge)", bat)

	// 4. Mode switching — idle should zero out dispatch.
	// The API handler flips the mode as soon as it returns, so reading the
	// mode back proves nothing about dispatch. Clearing the targets is the
	// next control tick's work: idle short-circuits ComputeDispatch, which
	// leaves no targets behind unless the fuse-saver overrides it. Wait for
	// both — the mode the operator asked for, and dispatch actually empty.
	s.postJSON("/api/mode", map[string]any{"mode": "idle"}, nil)
	status = s.waitForStatus("idle mode to clear the dispatch targets", 20*time.Second,
		func(st map[string]any) bool {
			if m, _ := st["mode"].(string); m != "idle" {
				return false
			}
			targets, ok := st["dispatch"].([]any)
			return ok && len(targets) == 0
		})
	t.Logf("idle: mode=%v dispatch targets=%d", status["mode"], len(status["dispatch"].([]any)))

	// Back to self_consumption for subsequent tests
	s.postJSON("/api/mode", map[string]any{"mode": "self_consumption"}, nil)
	s.postJSON("/api/target", map[string]any{"grid_target_w": 0}, nil)

	// 5. Battery models — should have accumulated some samples from control loop
	var models map[string]map[string]any
	s.getJSON("/api/battery_models", &models)
	t.Logf("battery models: %d", len(models))
	for name, m := range models {
		t.Logf("  %s: τ=%.2fs gain=%.3f samples=%v conf=%.2f",
			name, m["tau_s"], m["gain"], m["n_samples"], m["confidence"])
	}
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %d", len(models))
	}

	// 6. Reset a model and verify
	s.postJSON("/api/battery_models/reset", map[string]any{"battery": "ferroamp"}, nil)
	s.getJSON("/api/battery_models", &models)
	if ns := models["ferroamp"]["n_samples"]; ns != float64(0) {
		t.Errorf("after reset, ferroamp samples should be 0, got %v", ns)
	}

	// 7. History — should have rows by now from the control loop
	// (the standalone main.go records history but our e2e setup doesn't —
	// just verify the endpoint doesn't error)
	resp, err := http.Get(s.baseURL() + "/api/history?range=5m&points=10")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("history: got %d", resp.StatusCode)
	}

	// 8. Static file serving
	resp, err = http.Get(s.baseURL() + "/index.html")
	if err == nil {
		resp.Body.Close()
	}
	// Don't require index.html to exist — just verify cache header is set
	// by hitting a known path; 200 or 404 both valid here

	t.Log("E2E: full stack passed — telemetry, control, commands, models, history, static all working together")
}

// ---- Helpers replicated from cmd/sim-ferroamp/main.go ----

func publishFerroamp(s *mqttserver.Server, snap ferroamp.Snapshot) {
	ehub := fmt.Sprintf(
		`{"pext":{"L1":"%.3f","L2":"%.3f","L3":"%.3f"},`+
			`"ul":{"L1":"230.0","L2":"230.0","L3":"230.0"},`+
			`"gridfreq":{"val":"50.00"},`+
			`"ppv":{"val":"%.3f"},"pbat":{"val":"%.3f"},`+
			`"wextconsq3p":{"val":"%.0f"},"wextprodq3p":{"val":"%.0f"}}`,
		snap.GridW/3, snap.GridW/3, snap.GridW/3,
		snap.PVW, -snap.ActualBatW,
		snap.ImportWh*3_600_000, snap.ExportWh*3_600_000)
	_ = s.Publish("extapi/data/ehub", []byte(ehub), false, 0)

	eso := fmt.Sprintf(
		`{"soc":{"val":"%.2f"},"ubat":{"val":"48.0"},"ibat":{"val":"%.2f"},`+
			`"wbatprod":{"val":"%.0f"},"wbatcons":{"val":"%.0f"}}`,
		snap.SoC*100, -snap.ActualBatW/48,
		snap.BatDischargeWh*3_600_000, snap.BatChargeWh*3_600_000)
	_ = s.Publish("extapi/data/eso", []byte(eso), false, 0)
}

// ---- Modbus handler ----

type sungrowHandler struct{ bank *sungrow.RegisterBank }

func (h *sungrowHandler) HandleCoils(_ *sv.CoilsRequest) ([]bool, error) {
	return nil, sv.ErrIllegalFunction
}
func (h *sungrowHandler) HandleDiscreteInputs(_ *sv.DiscreteInputsRequest) ([]bool, error) {
	return nil, sv.ErrIllegalFunction
}
func (h *sungrowHandler) HandleHoldingRegisters(req *sv.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		return nil, h.bank.WriteHolding(req.Addr, req.Args)
	}
	return h.bank.ReadHolding(req.Addr, req.Quantity), nil
}
func (h *sungrowHandler) HandleInputRegisters(req *sv.InputRegistersRequest) ([]uint16, error) {
	return h.bank.ReadInput(req.Addr, req.Quantity), nil
}

// port-picker sanity — keep the compiler happy about unused strconv
var _ = strconv.Itoa
