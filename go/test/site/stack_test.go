//go:build unix

package site

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ocppPass is the shared OCPP credential the rig gives FTW and the chargers.
const ocppPass = "sim-ocpp-rig-20261021"

// lane is the first of the 100 ports one rig uses on 127.0.0.1. The house
// (device-simulator's headless run) takes base+0 to base+59; FTW takes
// base+90 (API) and base+91 (OCPP).
type lane int

func (l lane) simHTTP() int { return int(l) }
func (l lane) ftwAPI() int  { return int(l) + 90 }
func (l lane) ftwOCPP() int { return int(l) + 91 }

// ---- processes ----

type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// procs runs each child in its own process group, so a stop reaches
// everything it started.
type procs struct {
	mu sync.Mutex
	m  map[string]*proc
}

func (ps *procs) start(name string, argv []string, dir string, env []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GOMAXPROCS=2"), env...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		f.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); f.Close(); close(p.done) }()
	ps.mu.Lock()
	if ps.m == nil {
		ps.m = map[string]*proc{}
	}
	ps.m[name] = p
	ps.mu.Unlock()
	return nil
}

func (ps *procs) stop(name string, sig syscall.Signal, wait time.Duration) {
	ps.mu.Lock()
	p := ps.m[name]
	delete(ps.m, name)
	ps.mu.Unlock()
	if p == nil {
		return
	}
	syscall.Kill(-p.cmd.Process.Pid, sig)
	select {
	case <-p.done:
	case <-time.After(wait):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

func (ps *procs) stopAll() {
	ps.mu.Lock()
	names := make([]string, 0, len(ps.m))
	for n := range ps.m {
		names = append(names, n)
	}
	ps.mu.Unlock()
	for _, n := range names {
		ps.stop(n, syscall.SIGTERM, 10*time.Second)
	}
}

// ---- HTTP ----

var client = &http.Client{Timeout: 5 * time.Second}

// call sends body as JSON and decodes the answer into out when out is not
// nil. It returns the raw answer.
func call(method, url string, body, out any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, bytes.TrimSpace(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("%s %s: %w", method, url, err)
		}
	}
	return raw, nil
}

func waitHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if _, last = call("GET", url, nil, nil); last == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not answer: %v", url, last)
}

// waitUntil polls pred every step until it holds or the timeout passes.
func waitUntil(timeout, step time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(step)
	}
	return false
}

func now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

// ---- the house: device-simulator's headless run ----

// houseSpec is device-simulator's run spec (docs/HEADLESS_RUN.md there).
type houseSpec struct {
	Name     string        `json:"name"`
	Site     houseSite     `json:"site"`
	Devices  []houseDevice `json:"devices"`
	Timeline []houseEvent  `json:"timeline,omitempty"`
	Random   *houseRandom  `json:"random,omitempty"`
}

// houseRandom asks the house to draw its own timeline from a seed.
type houseRandom struct {
	Seed      int64   `json:"seed"`
	DurationS float64 `json:"duration_s"`
	GapS      float64 `json:"gap_s,omitempty"`
}

type houseSite struct {
	FuseA               int        `json:"fuse_a"`
	FuseProfile         string     `json:"fuse_profile,omitempty"`
	HouseW              [3]float64 `json:"house_w"`
	SignedPhaseCurrents *bool      `json:"signed_phase_currents,omitempty"`
}

type houseDevice struct {
	ID               string            `json:"id"`
	Driver           string            `json:"driver"`
	CommandResponseS *float64          `json:"command_response_s,omitempty"`
	StartupDelayS    *float64          `json:"startup_delay_s,omitempty"`
	LoadBalancerA    *float64          `json:"load_balancer_a,omitempty"`
	BatteryKWh       float64           `json:"battery_kwh,omitempty"`
	SoCPct           *float64          `json:"soc_pct,omitempty"`
	MinSoCPct        *float64          `json:"min_soc_pct,omitempty"`
	PVW              float64           `json:"pv_w,omitempty"`
	OCPPURL          string            `json:"ocpp_url,omitempty"`
	MaxKW            float64           `json:"max_kw,omitempty"`
	Vendor           string            `json:"vendor,omitempty"`
	Model            string            `json:"model,omitempty"`
	PhaseSwitch      bool              `json:"phase_switch,omitempty"`
	OCPPConfig       map[string]string `json:"ocpp_config,omitempty"`
	CarMaxA          float64           `json:"car_max_a,omitempty"`
	CarPhases        int               `json:"car_phases,omitempty"`
	WiredPhases      int               `json:"wired_phases,omitempty"`
}

// houseEvent is one change to the house (device-simulator's runEvent).
type houseEvent struct {
	AtS    float64     `json:"at_s,omitempty"`
	Do     string      `json:"do"`
	Device string      `json:"device,omitempty"`
	Label  string      `json:"label,omitempty"`
	W      *[3]float64 `json:"w,omitempty"`
	A      *[3]float64 `json:"a,omitempty"`
	PVW    *float64    `json:"pv_w,omitempty"`
	Pct    *float64    `json:"pct,omitempty"`
	On     *bool       `json:"on,omitempty"`
	MaxA   *float64    `json:"max_a,omitempty"`
	Phases *int        `json:"phases,omitempty"`
	ForS   float64     `json:"for_s,omitempty"`
}

type housePorts struct {
	HTTP    int `json:"http"`
	Devices map[string]struct {
		Driver string `json:"driver"`
		Modbus int    `json:"modbus_port"`
		UnitID int    `json:"unit_id"`
		Serial string `json:"serial"`
	} `json:"devices"`
}

// ---- one run ----

// charger is one OCPP charger and the car on it. FTW's loadpoint has the
// charger's id.
type charger struct {
	kw          float64 // three-phase rating; default 11.04
	phaseSwitch bool
	delayS      float64 // applies a profile this late, like a cloud charger
	carPhases   int     // 0: as many as offered
	wiredPhases int     // 1: wired on L1 only, kw its rating there
	carMaxA     float64
}

type stackSpec struct {
	// house replays a spec the house wrote (timeline.json); random asks it
	// to draw a timeline. Without either the house has no timeline.
	house  *houseSpec
	random *houseRandom

	fuseA      float64
	loadsW     [3]float64
	pvW        float64
	socPct     float64
	chargers   []charger // nil: one 11 kW charger
	lpExtra    map[string]any
	heartbeatS int // FTW's OCPP heartbeat; 0 leaves Core's default
	// batteryDelayS is how long the battery takes to follow an EMS write;
	// 0 leaves the model's (2 s for sungrow).
	batteryDelayS float64
}

type stack struct {
	env    *rigEnv
	lane   lane
	rundir string
	spec   stackSpec
	ps     procs
	ports  housePorts
	ftw    string // FTW's API base URL
	sim    string // the house's API base URL
	rec    *recorder
	cfg    string
	// markErr is the first mark the house did not take; the run cannot be
	// judged without it.
	markErr error
}

func newStack(env *rigEnv, l lane, rundir string, spec stackSpec) *stack {
	if spec.chargers == nil {
		spec.chargers = []charger{{}}
	}
	return &stack{env: env, lane: l, rundir: rundir, spec: spec,
		ftw: fmt.Sprintf("http://127.0.0.1:%d", l.ftwAPI()),
		sim: fmt.Sprintf("http://127.0.0.1:%d", l.simHTTP())}
}

func f64(v float64) *float64 { return &v }

func (st *stack) houseSpec() houseSpec {
	if st.spec.house != nil {
		// A replay: the same house on this lane's ports.
		h := *st.spec.house
		h.Name = filepath.Base(st.rundir)
		h.Random = nil
		h.Devices = append([]houseDevice(nil), h.Devices...)
		for i := range h.Devices {
			if h.Devices[i].Driver == "ocpp" {
				h.Devices[i].OCPPURL = fmt.Sprintf("ws://ftw:%s@127.0.0.1:%d/", ocppPass, st.lane.ftwOCPP())
			}
		}
		return h
	}
	h := houseSpec{Name: filepath.Base(st.rundir), Random: st.spec.random,
		Site: houseSite{FuseA: int(st.spec.fuseA), FuseProfile: "iec_gg", HouseW: st.spec.loadsW},
		Devices: []houseDevice{{ID: "inverter", Driver: "sungrow", BatteryKWh: 10,
			SoCPct: f64(st.spec.socPct), MinSoCPct: f64(5), PVW: st.spec.pvW}}}
	if st.spec.batteryDelayS > 0 {
		h.Devices[0].CommandResponseS = f64(st.spec.batteryDelayS)
	}
	for i, c := range st.spec.chargers {
		var delay *float64
		if st.spec.random == nil {
			delay = f64(c.delayS) // a random house draws its own
		}
		kw := c.kw
		if kw == 0 {
			kw = 11.04
		}
		h.Devices = append(h.Devices, houseDevice{ID: lpIDs[i], Driver: "ocpp",
			OCPPURL: fmt.Sprintf("ws://ftw:%s@127.0.0.1:%d/", ocppPass, st.lane.ftwOCPP()),
			MaxKW:   kw, Vendor: "RigCo", Model: fmt.Sprintf("Rig%.0f", kw), PhaseSwitch: c.phaseSwitch, CommandResponseS: delay,
			CarPhases: c.carPhases, CarMaxA: c.carMaxA, WiredPhases: c.wiredPhases,
			OCPPConfig: map[string]string{"HeartbeatInterval": "60", "MeterValueSampleInterval": "10"}})
	}
	return h
}

func (st *stack) up() error {
	if err := os.RemoveAll(st.rundir); err != nil {
		return err
	}
	if err := os.MkdirAll(st.rundir, 0o755); err != nil {
		return err
	}
	spec, _ := json.MarshalIndent(st.houseSpec(), "", "  ")
	specPath := filepath.Join(st.rundir, "house.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		return err
	}
	houseDir := filepath.Join(st.rundir, "house")
	if err := st.ps.start("house", []string{st.env.devsimBin, "run", "-spec", specPath,
		"-port-base", fmt.Sprint(int(st.lane)), "-out", houseDir}, st.rundir, nil,
		filepath.Join(st.rundir, "house.log")); err != nil {
		return err
	}
	if !waitUntil(30*time.Second, 200*time.Millisecond, func() bool {
		b, err := os.ReadFile(filepath.Join(houseDir, "ports.json"))
		return err == nil && json.Unmarshal(b, &st.ports) == nil && len(st.ports.Devices) > 0
	}) {
		return fmt.Errorf("the house never wrote ports.json (see house.log)")
	}
	if err := waitHTTP(st.sim+"/api/run", 10*time.Second); err != nil {
		return err
	}
	var err error
	if st.cfg, err = st.ftwConfig(); err != nil {
		return err
	}
	if err := st.startFTW(); err != nil {
		return err
	}
	if _, err := call("POST", st.ftw+"/api/mode", map[string]any{"mode": "planner_self"}, nil); err != nil {
		return err
	}
	st.rec = &recorder{ftw: st.ftw, path: filepath.Join(st.rundir, "ftw.jsonl")}
	st.rec.start()
	return nil
}

func (st *stack) startFTW() error {
	if err := st.ps.start("ftw", []string{st.env.ftwBin, "-config", st.cfg,
		"-web", filepath.Join(st.env.root, "web"), "-drivers", filepath.Join(st.env.root, "drivers")},
		st.rundir, []string{"GOMAXPROCS=4"}, filepath.Join(st.rundir, "ftw.log")); err != nil {
		return err
	}
	return waitHTTP(st.ftw+"/api/status", 90*time.Second)
}

func (st *stack) stopFTW() { st.ps.stop("ftw", syscall.SIGTERM, 30*time.Second) }

func (st *stack) down() {
	if st.rec != nil {
		st.rec.close()
	}
	st.ps.stop("ftw", syscall.SIGTERM, 30*time.Second)
	st.ps.stop("house", syscall.SIGTERM, 15*time.Second)
	st.ps.stopAll()
}

// event changes the house now.
func (st *stack) event(ev houseEvent) error {
	_, err := call("POST", st.sim+"/api/run/event", ev, nil)
	return err
}

// mark names this moment in the house's record.
func (st *stack) mark(label string) {
	if _, err := call("POST", st.sim+"/api/run/mark", map[string]string{"label": label}, nil); err != nil && st.markErr == nil {
		st.markErr = fmt.Errorf("mark %s: %w", label, err)
	}
}

func (st *stack) get(path string, out any) error {
	_, err := call("GET", st.ftw+path, nil, out)
	return err
}

func (st *stack) post(path string, body, out any) error {
	_, err := call("POST", st.ftw+path, body, out)
	return err
}

func (st *stack) setLoadsA(a float64) error {
	return st.event(houseEvent{Do: "loads", A: &[3]float64{a, a, a}})
}

func (st *stack) serial(i int) string { return st.ports.Devices[lpIDs[i]].Serial }

// ---- FTW's API, once a second ----

type recorder struct {
	ftw  string
	path string
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

func (r *recorder) start() {
	r.stop, r.done = make(chan struct{}), make(chan struct{})
	go r.run()
}

func (r *recorder) close() {
	if r.stop == nil {
		return
	}
	close(r.stop)
	<-r.done
}

func (r *recorder) append(rec map[string]any) {
	b, _ := json.Marshal(rec)
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

func (r *recorder) run() {
	defer close(r.done)
	for {
		t0 := time.Now()
		rec := map[string]any{"t": now()}
		for key, path := range map[string]string{"status": "/api/status", "lps": "/api/loadpoints"} {
			if b, err := call("GET", r.ftw+path, nil, nil); err != nil {
				rec[key+"_err"] = err.Error()
			} else {
				rec[key] = json.RawMessage(b)
			}
		}
		r.append(rec)
		select {
		case <-r.stop:
			return
		case <-time.After(time.Second - time.Since(t0)):
		}
	}
}

// ---- FTW's configuration ----

var lpIDs = []string{"garage", "carport"}

func (st *stack) ftwConfig() (string, error) {
	var lps []map[string]any
	for i := range st.spec.chargers {
		lp := map[string]any{
			"id": lpIDs[i], "driver_name": st.serial(i),
			"min_charge_w": 4140, "max_charge_w": 11040, "vehicle_capacity_wh": 60000,
		}
		for k, v := range st.spec.lpExtra {
			lp[k] = v
		}
		lps = append(lps, lp)
	}
	ocpp := map[string]any{"enabled": true, "bind": "127.0.0.1", "port": st.lane.ftwOCPP(), "path": "/",
		"username": "ftw", "password": ocppPass}
	// 60 s is what every box configured from the old example has; 0 leaves
	// it unset so Core's default applies.
	if st.spec.heartbeatS > 0 {
		ocpp["heartbeat_interval_s"] = st.spec.heartbeatS
	}
	modbus := func(id string) map[string]any {
		d := st.ports.Devices[id]
		return map[string]any{"modbus": map[string]any{"host": "127.0.0.1", "port": d.Modbus, "unit_id": max(d.UnitID, 1)}}
	}
	cfg := map[string]any{
		"site": map[string]any{"name": "rig", "control_interval_s": 2, "grid_target_w": 0,
			"grid_tolerance_w": 42, "watchdog_timeout_s": 60},
		"fuse": map[string]any{"max_amps": st.spec.fuseA, "phases": 3, "voltage": 230},
		"drivers": []map[string]any{
			{"name": "meter", "lua": "drivers/sdm630.lua", "is_site_meter": true, "capabilities": modbus("meter")},
			{"name": "sungrow", "lua": "drivers/sungrow.lua", "battery_capacity_wh": 10000, "capabilities": modbus("inverter")},
		},
		"api":               map[string]any{"port": st.lane.ftwAPI()},
		"ocpp":              ocpp,
		"loadpoints":        lps,
		"state":             map[string]any{"path": filepath.Join(st.rundir, "state.db")},
		"app_link":          map[string]any{"enabled": false},
		"fleet_ping":        map[string]any{"enabled": false},
		"device_repository": map[string]any{"enabled": false},
		// A real box plans. A box without a planner runs Self, where the
		// battery chases the raw meter and covers the car (dispatch.go),
		// which hides the stress on the fuse.
		"planner": map[string]any{"enabled": true, "engine": "core", "mode": "self_consumption"},
		"price":   map[string]any{"provider": "elprisetjustnu", "zone": "SE3"},
	}
	path := filepath.Join(st.rundir, "config.yaml")
	b, err := json.MarshalIndent(cfg, "", "  ") // JSON is valid YAML
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}
