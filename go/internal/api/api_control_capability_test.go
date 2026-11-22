package api

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The owner's home box, 8-22 Nov 2026: a Sungrow SH-RT hybrid with a 9.6 kWh
// SBR096 battery on 3 x 20 A, relaying its own grid meter, polled once a
// second. Its control journal held 23 control.not_following notices in two
// weeks. Night grid charging ordered 5 kW; the inverter held grid import at
// about 4.3 kW, so the battery took 4.3 kW less the house load. Discharge
// ordered 5 kW; the battery gave a flat 21.6-23.1 A DC, 4.0-4.4 kW as the
// pack voltage moved. These traces replay those patterns.
type sungrowTrace struct {
	tel    *telemetry.Store
	srv    *Server
	models map[string]*battery.Model
	cmdW   float64
	// battery reports what the battery does this second: its power, DC
	// current and the extra fields of its emit.
	battery func() (w, a, v float64, extra string)
	houseW  float64
}

func newSungrowTrace() *sungrowTrace {
	tel := telemetry.NewStore()
	tel.DriverHealthMut("sungrow")
	models := map[string]*battery.Model{"sungrow": battery.New("sungrow")}
	ctrl := &control.State{Mode: control.ModePlannerArbitrage, SiteMeterDriver: "sungrow"}
	srv := New(&Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{}, Cfg: &config.Config{}, CfgMu: &sync.RWMutex{},
		Models: models, ModelsMu: &sync.Mutex{}})
	return &sungrowTrace{tel: tel, srv: srv, models: models}
}

func (s *sungrowTrace) order(w float64) {
	s.cmdW = w
	c := s.tel.BeginCommand("sungrow", []byte(fmt.Sprintf(`{"action":"battery","power_w":%g}`, w)), time.Now())
	s.tel.CompleteCommand(c, "accepted")
}

func (s *sungrowTrace) poll() {
	w, a, v, extra := s.battery()
	s.tel.Update("sungrow", telemetry.DerMeter, s.houseW+w, nil, []byte(`{"power_origin":"external_meter"}`))
	soc := 0.5
	s.tel.Update("sungrow", telemetry.DerBattery, w, &soc, []byte(fmt.Sprintf(`{"setpoint_w":%g,"a":%.1f,"v":%.1f%s}`, s.cmdW, a, v, extra)))
	s.tel.RecordDriverSuccess("sungrow")
}

// run holds the order for d, polling every second. The control journal
// samples every 5 s, which feeds the learner; each sample's verdict is
// passed to seen.
func (s *sungrowTrace) run(d time.Duration, orderW float64, seen func(ControlFeedback)) {
	start := time.Now()
	for sec := 0; time.Since(start) < d; sec++ {
		if sec%30 == 0 {
			s.order(orderW)
		}
		time.Sleep(time.Second)
		s.poll()
		if sec%5 == 4 {
			now := time.Now()
			s.srv.JournalVerdicts(now)
			if seen != nil {
				seen(s.batteryRow(now))
			}
		}
	}
}

func (s *sungrowTrace) batteryRow(now time.Time) ControlFeedback {
	for _, f := range s.srv.controlFeedback(now) {
		if f.Kind == "battery" {
			return f
		}
	}
	return ControlFeedback{}
}

// idle rests the battery until the same time next day.
func (s *sungrowTrace) idle(until time.Time) {
	s.battery = func() (float64, float64, float64, string) { return 0, 0, 190, "" }
	s.order(0)
	for time.Now().Before(until) {
		time.Sleep(5 * time.Minute)
		s.poll()
		s.srv.JournalVerdicts(time.Now())
	}
}

// tally counts verdicts after the order settles.
type tally struct {
	n    int
	hits map[string]int
	last ControlFeedback
}

func (t *tally) add(f ControlFeedback) {
	if t.hits == nil {
		t.hits = map[string]int{}
	}
	t.n++
	t.hits[f.Status+"/"+f.Reason+"/"+f.Severity]++
	t.last = f
}

func (t *tally) share(key string) float64 {
	if t.n == 0 {
		return 0
	}
	return float64(t.hits[key]) / float64(t.n)
}

// gridCharge is a night of grid charging under the inverter's import cap:
// grid import sits at 4.3 kW while the house takes 0.3-3 kW, so the battery
// gets what is left.
func (s *sungrowTrace) gridCharge(rng *rand.Rand, battery func(house float64) float64) {
	s.battery = func() (float64, float64, float64, string) {
		s.houseW = math.Max(300, math.Min(3000, s.houseW+rng.NormFloat64()*150))
		w := battery(s.houseW)
		v := 190.0
		return w, w / v, v, ""
	}
}

func importCap(rng *rand.Rand) func(house float64) float64 {
	return func(house float64) float64 { return 4316 + rng.NormFloat64()*15 - house }
}

func TestGridImportCapIsLearnedOverNights(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(3))
		s := newSungrowTrace()
		s.houseW = 1000
		start := time.Now()
		var nights [3]tally
		notices := 0
		for n := range 3 {
			s.gridCharge(rng, importCap(rng))
			s.run(5*time.Hour, 5000, func(f ControlFeedback) {
				if f.Status == "waiting" {
					return
				}
				nights[n].add(f)
				// control.not_following reads the same status, so a
				// known limit never fires it.
				for _, st := range s.srv.ControlStates(time.Now()) {
					if f.Reason == "learned_limit" && st.Kind == "battery" && st.Status != "limited" {
						notices++
					}
				}
			})
			s.idle(start.Add(time.Duration(n+1) * 24 * time.Hour))
		}
		if got := nights[0].share("not_following/power_below_target/warning"); got < 0.95 {
			t.Fatalf("first night not following in %.0f%% of samples: %v", got*100, nights[0].hits)
		}
		if got := nights[2].share("limited/learned_limit/info"); got < 0.95 {
			t.Fatalf("third night limited in %.0f%% of samples: %v", got*100, nights[2].hits)
		}
		f := nights[2].last
		if f.LimitSource != "learned" || f.LimitKind != "grid_import" || f.LimitW == nil || math.Abs(*f.LimitW-4316) > 30 ||
			f.LimitSeen < 4 || f.LimitRoomW == nil || math.Abs(*f.LimitRoomW-math.Abs(*f.ActualW)) > 100 || f.LimitEarlierW != nil {
			t.Fatalf("limit fields %s", jsonOf(f))
		}
		if since := time.UnixMilli(f.LimitSinceMs); since.Before(start) || since.After(start.Add(time.Hour)) {
			t.Fatalf("limit since %v, want the first night", since)
		}
		if notices > 0 {
			t.Fatalf("a known limit read as %d other statuses for notifications", notices)
		}

		// Discharging against a charge order is never explained or learned.
		runs := runsOf(s.models["sungrow"])
		s.battery = func() (float64, float64, float64, string) { return -2000, 2000 / 190.0, 190, "" }
		var wrong tally
		s.run(10*time.Minute, 5000, func(f ControlFeedback) {
			if f.Status != "waiting" {
				wrong.add(f)
			}
		})
		if got := wrong.share("not_following/power_wrong_direction/warning"); got < 0.9 {
			t.Fatalf("wrong direction: %v", wrong.hits)
		}
		if got := runsOf(s.models["sungrow"]); got != runs {
			t.Fatalf("wrong direction was learned: %d runs, had %d", got, runs)
		}

		// Later the battery takes 1 kW while the house takes 300 W: far
		// below the plateau, so it warns.
		s.houseW = 300
		s.battery = func() (float64, float64, float64, string) { return 1000, 1000 / 190.0, 190, "" }
		var low tally
		s.run(10*time.Minute, 5000, func(f ControlFeedback) {
			if f.Status != "waiting" {
				low.add(f)
			}
		})
		if got := low.share("not_following/power_below_target/warning"); got < 0.9 {
			t.Fatalf("a drop to 1 kW was explained: %v", low.hits)
		}
	})
}

func TestDischargeCurrentLimitIsLearned(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"driver reports no limits", ""},
		// sungrow 1.7.0 on the home box, 22 Nov 2026: the BMS reports its
		// 30 A rating both ways (5,973 W at 199 V) and 20 °C, well above the
		// 22.5 A the battery gives. That report explains nothing here; the
		// learned current limit still does.
		{"BMS reports its 30 A rating", `,"available_charge_W":5973,"available_discharge_W":5973,"temperature_C":20`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rng := rand.New(rand.NewSource(5))
				s := newSungrowTrace()
				s.houseW = 800
				start := time.Now()
				var days [3]tally
				for d := range 3 {
					began := time.Now()
					s.battery = func() (float64, float64, float64, string) {
						s.houseW = math.Max(300, math.Min(3000, s.houseW+rng.NormFloat64()*150))
						v := 192 - 5*time.Since(began).Hours()/3
						a := 22.4 + rng.NormFloat64()*0.25
						return -a * v, a, v, tc.extra
					}
					s.run(3*time.Hour, -5000, func(f ControlFeedback) {
						if f.Status != "waiting" {
							days[d].add(f)
						}
					})
					s.idle(start.Add(time.Duration(d+1) * 24 * time.Hour))
				}
				if got := days[0].share("not_following/power_below_target/warning"); got < 0.95 {
					t.Fatalf("first discharge: %v", days[0].hits)
				}
				if got := days[2].share("limited/learned_limit/info"); got < 0.95 {
					t.Fatalf("third discharge: %v", days[2].hits)
				}
				for _, day := range days {
					if day.hits["limited/battery_limit/info"] > 0 {
						t.Fatalf("a reported limit far above the delivery explained it: %v", day.hits)
					}
				}
				f := days[2].last
				if f.LimitKind != "battery_current" || f.LimitSource != "learned" || f.LimitA == nil || math.Abs(*f.LimitA-22.4) > 0.3 ||
					f.LimitW == nil || math.Abs(*f.LimitW-*f.LimitA*187) > 60 || f.LimitRoomW != nil {
					t.Fatalf("limit fields %s", jsonOf(f))
				}
			})
		})
	}
}

// A battery whose driver reports what it can give now is limited at once.
func TestReportedBatteryLimitExplainsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name, extra    string
		orderW, w      float64
		status, reason string
	}{
		{"reported discharge limit", `,"available_discharge_W":3200`, -5000, -3150, "limited", "battery_limit"},
		{"canonical names", `,"available_charge_W":2400,"temperature_C":2`, 5000, 2380, "limited", "battery_limit"},
		{"charging not allowed now", `,"available_charge_W":0`, 5000, 0, "limited", "battery_limit"},
		{"gives far less than it reports", `,"available_discharge_W":3200`, -5000, -1000, "not_following", "power_below_target"},
		{"limit above the order", `,"available_discharge_W":6000`, -5000, -3150, "not_following", "power_below_target"},
		{"rating far above the delivery", `,"available_discharge_W":5973,"temperature_C":20`, -5000, -4300, "not_following", "power_below_target"},
		{"rating far above the delivery, larger order", `,"available_discharge_W":5973`, -6500, -4300, "not_following", "power_below_target"},
		{"no limit reported", ``, -5000, -3150, "not_following", "power_below_target"},
		{"limit out of range", `,"available_discharge_W":-3200`, -5000, -3150, "not_following", "power_below_target"},
		{"standby", `,"device_state":"standby"`, -5000, 0, "not_following", "device_standby"},
		{"starting", `,"device_state":"starting","available_discharge_W":0`, -5000, -200, "not_following", "device_standby"},
		{"running", `,"device_state":"running"`, -5000, 0, "not_following", "no_power_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSungrowTrace()
				s.houseW = 800
				s.battery = func() (float64, float64, float64, string) { return tc.w, math.Abs(tc.w) / 190, 190, tc.extra }
				var last ControlFeedback
				s.run(90*time.Second, tc.orderW, func(f ControlFeedback) { last = f })
				if last.Status != tc.status || last.Reason != tc.reason {
					t.Fatalf("got %s/%s/%s, want %s/%s", last.Status, last.Reason, last.Severity, tc.status, tc.reason)
				}
				switch tc.reason {
				case "battery_limit":
					if last.Severity != "info" || last.LimitSource != "device" || last.LimitKind != "battery_reported" || last.LimitW == nil || last.LimitSeen != 0 {
						t.Fatalf("limit fields %s", jsonOf(last))
					}
				case "device_standby":
					if last.Severity != "warning" || last.LimitW != nil {
						t.Fatalf("standby %s", jsonOf(last))
					}
				default:
					if last.LimitW != nil || last.LimitSource != "" {
						t.Fatalf("unexplained shortfall carries a limit: %s", jsonOf(last))
					}
				}
			})
		})
	}
}

// A limit learned in the cold does not explain a warm day, and the reverse.
func TestColdPlateauDoesNotExplainWarmShortfall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(9))
		s := newSungrowTrace()
		s.houseW = 800
		start := time.Now()
		discharge := func(tempC float64) {
			s.battery = func() (float64, float64, float64, string) {
				a := 12 + rng.NormFloat64()*0.2
				return -a * 190, a, 190, fmt.Sprintf(`,"temp_c":%.1f`, tempC)
			}
		}
		for d := range 3 {
			discharge(-5 + 4*rng.Float64())
			s.run(3*time.Hour, -5000, nil)
			s.idle(start.Add(time.Duration(d+1) * 24 * time.Hour))
		}
		var cold, warm tally
		discharge(-3)
		s.run(10*time.Minute, -5000, func(f ControlFeedback) {
			if f.Status != "waiting" {
				cold.add(f)
			}
		})
		if got := cold.share("limited/learned_limit/info"); got < 0.9 {
			t.Fatalf("cold shortfall not explained: %v", cold.hits)
		}
		discharge(15)
		s.run(10*time.Minute, -5000, func(f ControlFeedback) {
			if f.Status != "waiting" {
				warm.add(f)
			}
		})
		if got := warm.share("not_following/power_below_target/warning"); got < 0.9 {
			t.Fatalf("a cold limit explained a warm shortfall: %v", warm.hits)
		}
	})
}

// GET /api/battery_models shows what was learned, and the learner never
// runs on HTTP reads: only the journal's cadence feeds it.
func TestBatteryModelsShowPlateaus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(3))
		s := newSungrowTrace()
		s.houseW = 1000
		start := time.Now()
		s.gridCharge(rng, importCap(rng))
		for sec := 0; time.Since(start) < 10*time.Minute; sec++ {
			if sec%30 == 0 {
				s.order(5000)
			}
			time.Sleep(time.Second)
			s.poll()
			s.srv.controlFeedback(time.Now())
		}
		if s.models["sungrow"].Plateaus != nil {
			t.Fatal("status reads fed the learner")
		}
		for n := range 3 {
			s.gridCharge(rng, importCap(rng))
			s.run(3*time.Hour, 5000, nil)
			s.idle(start.Add(time.Duration(n+1) * 24 * time.Hour))
		}
		rec := httptest.NewRecorder()
		s.srv.handleGetModels(rec, httptest.NewRequest("GET", "/api/battery_models", nil))
		var body map[string]struct {
			Plateaus battery.PlateauView `json:"plateaus"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		view := body["sungrow"].Plateaus
		if len(view.Known) != 1 || view.Known[0].Kind != "grid_import" || view.Known[0].Unit != "W" || len(view.Series) == 0 {
			t.Fatalf("battery models %s", rec.Body.String())
		}
	})
}

func runsOf(m *battery.Model) int {
	n := 0
	for _, s := range m.Plateaus.View().Series {
		n += len(s.Runs)
	}
	return n
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Grid power reaches the learner only from a site meter Core trusts for site
// evidence: a separate sensor, live, without a fault, fresh, while dispatch
// is free.
func TestShortfallGridComesFromATrustedMeter(t *testing.T) {
	for _, tc := range []struct {
		name, data, blocked string
		age                 time.Duration
		fault, charge       bool
		want                *float64
	}{
		{"meter it relays", `{"power_origin":"external_meter"}`, "", 0, false, true, watts(4300)},
		{"export while discharging", `{"power_origin":"external_meter"}`, "", 0, false, false, nil},
		{"meter computed from its own flows", `{"power_origin":"derived"}`, "", 0, false, true, nil},
		{"origin unknown", `{}`, "", 0, false, true, nil},
		{"old reading", `{"power_origin":"external_meter"}`, "", 20 * time.Second, false, true, nil},
		{"meter fault", `{"power_origin":"external_meter"}`, "", 0, true, true, nil},
		{"dispatch blocked", `{"power_origin":"external_meter"}`, "site_meter_stale", 0, false, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSungrowTrace()
				s.tel.Update("sungrow", telemetry.DerMeter, 4300, nil, []byte(tc.data))
				s.tel.RecordDriverSuccess("sungrow")
				if tc.fault {
					s.tel.SetDriverDeviceFault("sungrow", true, "test")
				}
				time.Sleep(tc.age)
				got := s.srv.siteGridW("sungrow", "sungrow", tc.blocked, tc.charge, time.Now())
				if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
					t.Fatalf("grid %v, want %v", got, tc.want)
				}
			})
		})
	}
}
