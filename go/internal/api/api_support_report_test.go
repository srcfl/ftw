package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func reportTestServer(t *testing.T) (*Server, *control.State, *telemetry.Store) {
	t.Helper()
	st := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	tel.Update("meter", telemetry.DerMeter, 500, nil, nil)
	srv := New(&Deps{
		Ctrl:    st,
		CtrlMu:  &sync.Mutex{},
		Tel:     tel,
		Version: "test-version",
	})
	return srv, st, tel
}

func TestSupportReportServesMarkdownAttachment(t *testing.T) {
	srv, _, _ := reportTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/support/report", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/report = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "ftw-help-") {
		t.Errorf("Content-Disposition = %q, want an ftw-help attachment", cd)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# FTW help report",
		"## Findings",
		"## Right now",
		"## Plan",
		"## Forecast quality",
		"## Devices",
		"## Versions",
		"test-version",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

// The report must survive a host where every optional service is nil —
// that is exactly the state a confused user is most likely to be in.
func TestSupportReportWithNoDependencies(t *testing.T) {
	srv := New(&Deps{Ctrl: control.NewState(0, 50, ""), CtrlMu: &sync.Mutex{}, Tel: telemetry.NewStore()})
	req := httptest.NewRequest(http.MethodGet, "/api/support/report", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/report = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No plan exists") {
		t.Error("expected the report to say no plan exists")
	}
	if !strings.Contains(body, "No site meter reading") {
		t.Error("expected a finding about the missing site meter")
	}
}

// The load-forecast check is the reason this report exists: a plan built
// against 383 W while the house draws 7.9 kW must be called out in
// Findings, not left for someone to spot in a table.
func TestSupportReportFlagsLoadForecastMiss(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	now := time.Now()
	snap := liveSnapshot{
		HaveGrid:    true,
		LoadW:       7900,
		PredictedLd: 383,
	}
	findings := srv.collectFindings(*ctrl, snap, nil, nil, nil, nil, control.SlotEnergySnapshot{}, now)

	var got *finding
	for i := range findings {
		if strings.Contains(findings[i].Title, "load forecast") {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no load-forecast finding; got %+v", findings)
	}
	if got.Severity != sevProblem {
		t.Errorf("severity = %q, want %q", got.Severity, sevProblem)
	}
	if !strings.Contains(got.Detail, "383 W") || !strings.Contains(got.Detail, "7.90 kW") {
		t.Errorf("detail should quote both numbers, got %q", got.Detail)
	}
}

func TestForecastMiss(t *testing.T) {
	cases := []struct {
		name              string
		predicted, actual float64
		want              bool
	}{
		{"the 383 W case", 383, 7900, true},
		// Both of these scored just under the old 0.6 relative-error
		// threshold and stayed silent in a real report. They are the
		// reason the measure is a ratio now.
		{"active slot planned 1.47 kW, house drawing 3.65 kW", 1470, 3650, true},
		{"model says 1.74 kW, house drawing 3.65 kW", 1740, 3650, true},
		{"solar planned 11.7 kW, actual 7.4 kW", 11720, 7400, true},
		{"close enough", 2000, 2200, false},
		{"exact", 1000, 1000, false},
		// A quiet house: 200 W of absolute error must not fire just
		// because the ratio looks bad against a small denominator.
		{"small absolute error on a quiet house", 100, 300, false},
		{"forecast far too high", 8000, 400, true},
		{"forecast of nothing against real load", 0, 3000, true},
		{"forecast of nothing against nothing much", 0, 200, false},
		{"just under the ratio", 1000, 1400, false},
		{"just over the ratio", 1000, 1500, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forecastMiss(tc.predicted, tc.actual); got != tc.want {
				t.Errorf("forecastMiss(%v, %v) = %v, want %v",
					tc.predicted, tc.actual, got, tc.want)
			}
			// The check must not care which way round it is asked.
			if got := forecastMiss(tc.actual, tc.predicted); got != tc.want {
				t.Errorf("forecastMiss(%v, %v) = %v, want %v (asymmetric)",
					tc.actual, tc.predicted, got, tc.want)
			}
		})
	}
}

// "Now" must be answered before "next". The dashboard's plan card leads
// with the next action, and that ambiguity is what made a support thread
// run for six hours.
func TestSupportReportMarksTheActiveSlot(t *testing.T) {
	_, ctrl, _ := reportTestServer(t)
	now := time.Now()
	slotStart := now.Add(-5 * time.Minute).UnixMilli()
	plan := &mpc.Plan{
		GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
		Mode:          mpc.ModeArbitrage,
		Actions: []mpc.Action{
			{SlotStartMs: slotStart, SlotLenMin: 15, BatteryW: -8400, Reason: "discharge — export at peak"},
			{SlotStartMs: slotStart + 15*60_000, SlotLenMin: 15, BatteryW: 2900, Reason: "absorb PV surplus"},
		},
		Solver: &mpc.SolverInfo{Engine: "highspy", Backend: "highs", Status: "optimal"},
	}

	var b strings.Builder
	writePlanSection(&b, plan, now.Add(-time.Minute), "reactive-load", now)
	out := b.String()

	lines := strings.Split(out, "\n")
	var marked string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "| → |") {
			marked = ln
		}
	}
	if marked == "" {
		t.Fatalf("no slot marked as active:\n%s", out)
	}
	if !strings.Contains(marked, "export at peak") {
		t.Errorf("the wrong slot is marked active: %q", marked)
	}
	if !strings.Contains(out, "highspy / highs") {
		t.Error("solver identity should be in the plan section")
	}
	if !strings.Contains(out, "reactive-load") {
		t.Error("the replan reason should be in the plan section")
	}

	// And the live section should state the active slot's intent in prose.
	var live strings.Builder
	writeRightNow(&live, *ctrl, liveSnapshot{HaveGrid: true, BatW: -7500}, &plan.Actions[0], nil, control.SlotEnergySnapshot{}, now)
	if !strings.Contains(live.String(), "-8.40 kW") {
		t.Errorf("active-slot intent missing from Right now:\n%s", live.String())
	}
}

func TestSupportReportFlagsFallbackSolver(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	plan := &mpc.Plan{
		Solver: &mpc.SolverInfo{
			Engine:         "go-dp",
			Backend:        "bellman",
			Fallback:       true,
			FallbackReason: "optimizer handshake failed",
		},
	}
	findings := srv.collectFindings(*ctrl,
		liveSnapshot{HaveGrid: true, LoadW: 1000, PredictedLd: 1000},
		plan, nil, nil, nil, control.SlotEnergySnapshot{}, time.Now())

	found := false
	for _, f := range findings {
		if strings.Contains(f.Title, "fallback") {
			found = true
			if !strings.Contains(f.Detail, "optimizer handshake failed") {
				t.Errorf("detail should carry the reason, got %q", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no fallback finding; got %+v", findings)
	}
}

func TestSupportReportFlagsOfflineAndFaultedDevices(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	health := map[string]telemetry.DriverHealth{
		"healthy":  {Name: "healthy", LastSuccess: ptrTime(time.Now())},
		"gone":     {Name: "gone", Status: telemetry.StatusOffline},
		"faulting": {Name: "faulting", DeviceFault: true, DeviceFaultReason: "Fault mode"},
	}
	findings := srv.collectFindings(*ctrl,
		liveSnapshot{HaveGrid: true, LoadW: 1000, PredictedLd: 1000},
		nil, nil, nil, health, control.SlotEnergySnapshot{}, time.Now())

	var sawOffline, sawFault bool
	for _, f := range findings {
		if strings.Contains(f.Detail, "gone") {
			sawOffline = true
		}
		if strings.Contains(f.Detail, "faulting") {
			sawFault = true
		}
	}
	if !sawOffline {
		t.Error("offline driver not reported")
	}
	if !sawFault {
		t.Error("faulted driver not reported")
	}
}

// Findings are sorted worst-first so a reader hits the real problem before
// the notes.
func TestFindingsAreSortedBySeverity(t *testing.T) {
	var b strings.Builder
	writeFindings(&b, []finding{
		{sevNote, "a note", "detail"},
		{sevProblem, "a problem", "detail"},
		{sevWarning, "a warning", "detail"},
	})
	out := b.String()
	pi := strings.Index(out, "a problem")
	wi := strings.Index(out, "a warning")
	ni := strings.Index(out, "a note")
	if !(pi < wi && wi < ni) {
		t.Errorf("findings out of order:\n%s", out)
	}
}

// A control loop warning every tick produced 49 identical lines in the
// first real report. Grouping is what keeps a single unique error from
// being pushed out of the window by that noise.
func TestLogGroupsCollapseRepeats(t *testing.T) {
	base := time.Now().Add(-2 * time.Minute)
	var entries []telemetry.LogEntry
	for i := 0; i < 49; i++ {
		entries = append(entries, telemetry.LogEntry{
			TS:    base.Add(time.Duration(i) * 2 * time.Second),
			Level: "WARN",
			Msg:   "dispatch: meter clamp reduced battery target",
			// The attribute tail differs every tick; grouping must ignore it.
			Attrs: fmt.Sprintf("requested_total_w=%d", 9000+i),
		})
	}
	entries = append(entries, telemetry.LogEntry{
		TS: base.Add(100 * time.Second), Level: "ERROR",
		Msg: "the one that matters", Driver: "ferroamp",
	})

	groups := groupLogs(entries)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(groups), groups)
	}
	var clamp, unique *logGroup
	for i := range groups {
		if groups[i].Level == "ERROR" {
			unique = &groups[i]
		} else {
			clamp = &groups[i]
		}
	}
	if clamp == nil || clamp.Count != 49 {
		t.Fatalf("clamp group = %+v, want count 49", clamp)
	}
	if clamp.Attrs != "requested_total_w=9048" {
		t.Errorf("attrs should come from the newest occurrence, got %q", clamp.Attrs)
	}
	if unique == nil {
		t.Fatal("the unique error was dropped")
	}
}

func TestLogSectionSurvivesNoisyNeighbour(t *testing.T) {
	// Far more distinct messages than the cap, plus one loud repeater.
	var entries []telemetry.LogEntry
	base := time.Now().Add(-time.Hour)
	for i := 0; i < reportLogGroups+20; i++ {
		entries = append(entries, telemetry.LogEntry{
			TS: base.Add(time.Duration(i) * time.Minute), Level: "WARN",
			Msg: fmt.Sprintf("distinct message %d", i),
		})
	}
	groups := groupLogs(entries)
	if len(groups) != reportLogGroups+20 {
		t.Fatalf("got %d groups, want %d", len(groups), reportLogGroups+20)
	}
	// Newest last, so trimming to the cap keeps the most recent.
	if !groups[len(groups)-1].Last.After(groups[0].Last) {
		t.Error("groups should be ordered oldest-first")
	}
}

// 49 clamp warnings with a Findings section reading only "no planning
// strategy is running" is the failure this guards.
func TestRepeatedWarningBecomesAFinding(t *testing.T) {
	st := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	ring := telemetry.NewLogRing()
	for i := 0; i < 20; i++ {
		ring.Append(telemetry.LogEntry{
			TS: time.Now(), Level: "WARN",
			Msg: "dispatch: meter clamp reduced battery target",
		})
	}
	srv := New(&Deps{Ctrl: st, CtrlMu: &sync.Mutex{}, Tel: tel, LogRing: ring})

	findings := srv.collectFindings(*st,
		liveSnapshot{HaveGrid: true, LoadW: 1000, PredictedLd: 1000},
		nil, nil, nil, nil, control.SlotEnergySnapshot{}, time.Now())

	var got *finding
	for i := range findings {
		if strings.Contains(findings[i].Title, "repeating") {
			got = &findings[i]
		}
	}
	if got == nil {
		t.Fatalf("a warning repeated 20 times produced no finding: %+v", findings)
	}
	if !strings.Contains(got.Detail, "20 times") {
		t.Errorf("detail should carry the count, got %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "meter clamp") {
		t.Errorf("detail should name the message, got %q", got.Detail)
	}
}

// "Held below request: no" on every row must not read as "nothing limited
// anything" — the site-meter clamp caps the fleet total upstream of these
// per-device numbers and cannot appear in the column.
func TestDispatchTableDisclaimsSiteLevelClamps(t *testing.T) {
	st := control.NewState(0, 50, "meter")
	var b strings.Builder
	writeRightNow(&b, *st, liveSnapshot{HaveGrid: true},
		nil,
		[]control.DispatchTarget{{Driver: "ferroamp", TargetW: 6400, Clamped: false}}, control.SlotEnergySnapshot{}, time.Now())
	out := b.String()
	if !strings.Contains(out, "per-device limits") {
		t.Errorf("dispatch table lacks its scope caveat:\n%s", out)
	}
	if !strings.Contains(out, "site total") {
		t.Errorf("dispatch table should point at site-level clamps:\n%s", out)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// A fresh install predicting badly is the model working, not a fault.
// Shouting PROBLEM at it while the next line says "still learning"
// contradicts itself and teaches the reader to skim the section.
func TestForecastMissIsANoteWhileTheModelIsStillLearning(t *testing.T) {
	st := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	// A load model with no trained buckets — the day-one state.
	lm := loadmodel.NewService(nil, tel, "meter", 4000, 17250)
	srv := New(&Deps{Ctrl: st, CtrlMu: &sync.Mutex{}, Tel: tel, LoadModel: lm})

	findings := srv.collectFindings(*st,
		liveSnapshot{HaveGrid: true, LoadW: 3650, PredictedLd: 1470},
		nil, nil, nil, nil, control.SlotEnergySnapshot{}, time.Now())

	var got *finding
	for i := range findings {
		if strings.Contains(findings[i].Title, "load forecast") {
			got = &findings[i]
		}
	}
	if got == nil {
		t.Fatalf("the miss should still be reported; got %+v", findings)
	}
	if got.Severity != sevNote {
		t.Errorf("severity = %q, want %q while the model is untrained",
			got.Severity, sevNote)
	}
	if !strings.Contains(got.Detail, "not finished learning") {
		t.Errorf("detail should explain why it is only a note, got %q", got.Detail)
	}
	if strings.Contains(got.Detail, "reset") && !strings.Contains(got.Detail, "start that clock again") {
		t.Error("a reset must not be recommended to someone whose model is still filling in")
	}
}

// Björn's second report: plan card reading "Charge battery at 4.5 kW ·
// Now, until 15:00", live target 0 W, 4 kW going out to the grid. The
// report showed the plan and the target and gave no way to tell whether
// the plan reached dispatch at all.
func TestSlotEnergyShortfallIsAFinding(t *testing.T) {
	srv, ctrl, _ := reportTestServer(t)
	now := time.Now()
	slot := control.SlotEnergySnapshot{
		HasSlot:   true,
		PlannedWh: 1125, // 4.5 kW across a 15-minute slot
		ActualWh:  20,   // the batteries are doing nothing
		SlotStart: now.Add(-8 * time.Minute),
		SlotEnd:   now.Add(7 * time.Minute),
	}
	findings := srv.collectFindings(*ctrl,
		liveSnapshot{HaveGrid: true, LoadW: 700, PredictedLd: 700},
		nil, nil, nil, nil, slot, now)

	var got *finding
	for i := range findings {
		if strings.Contains(findings[i].Title, "energy is not being delivered") {
			got = &findings[i]
		}
	}
	if got == nil {
		t.Fatalf("a slot delivering nothing produced no finding: %+v", findings)
	}
	if got.Severity != sevProblem {
		t.Errorf("severity = %q, want %q", got.Severity, sevProblem)
	}
	if !strings.Contains(got.Detail, "1.12 kWh") {
		t.Errorf("detail should quote the planned energy, got %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "energy-allocation path has delivered nothing") {
		t.Errorf("detail should call out the idle energy path, got %q", got.Detail)
	}
}

func TestSlotPaceShortfall(t *testing.T) {
	now := time.Now()
	slot := func(planned, actual float64, elapsed, total time.Duration) control.SlotEnergySnapshot {
		return control.SlotEnergySnapshot{
			HasSlot: true, PlannedWh: planned, ActualWh: actual,
			SlotStart: now.Add(-elapsed), SlotEnd: now.Add(total - elapsed),
		}
	}
	cases := []struct {
		name string
		in   control.SlotEnergySnapshot
		want bool
	}{
		{"delivering nothing against a real ask",
			slot(1125, 20, 8*time.Minute, 15*time.Minute), true},
		{"moving energy the wrong way",
			slot(1125, -300, 8*time.Minute, 15*time.Minute), true},
		{"tracking the plan",
			slot(1125, 560, 8*time.Minute, 15*time.Minute), false},
		{"a little behind but working",
			slot(1125, 400, 8*time.Minute, 15*time.Minute), false},
		// A slot is allowed to start slowly and catch up.
		{"barely started",
			slot(1125, 0, 30*time.Second, 15*time.Minute), false},
		// An idle slot asks for nothing and cannot fall behind.
		{"idle slot",
			slot(10, 0, 8*time.Minute, 15*time.Minute), false},
		{"discharge slot delivering",
			slot(-1125, -560, 8*time.Minute, 15*time.Minute), false},
		{"discharge slot doing nothing",
			slot(-1125, -20, 8*time.Minute, 15*time.Minute), true},
		{"no slot in flight", control.SlotEnergySnapshot{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := slotPaceShortfall(tc.in, now)
			if got != tc.want {
				t.Errorf("slotPaceShortfall = %v, want %v", got, tc.want)
			}
		})
	}
}

// The books have to be in the report even when nothing is wrong — that is
// what lets somebody else check the reasoning rather than trust a verdict.
func TestSlotEnergyBooksAreInTheReport(t *testing.T) {
	_, ctrl, _ := reportTestServer(t)
	now := time.Now()
	var b strings.Builder
	writeRightNow(&b, *ctrl, liveSnapshot{HaveGrid: true}, nil, nil,
		control.SlotEnergySnapshot{
			HasSlot: true, PlannedWh: 1125, ActualWh: 560, EnergyPathWh: 545,
			SlotStart: now.Add(-8 * time.Minute), SlotEnd: now.Add(7 * time.Minute),
		}, now)
	out := b.String()
	for _, want := range []string{"1.12 kWh", "560 Wh", "545 Wh", "Energy booked for this slot"} {
		if !strings.Contains(out, want) {
			t.Errorf("Right now is missing %q:\n%s", want, out)
		}
	}
}
