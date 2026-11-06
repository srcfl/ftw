package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHouseholdSubtotalStatusAndDailyKeepIncompleteTotalSeparate(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if now.Sub(midnight) < 16*time.Minute {
		t.Skip("need fifteen completed minutes today")
	}
	for _, day := range []time.Time{midnight.AddDate(0, 0, -1), midnight} {
		for i := int64(0); i <= 90; i++ {
			at := day.Add(time.Duration(i) * 10 * time.Second).UnixMilli()
			v := &state.HistorySources{AtMS: at, Topology: "site", GridW: 1000}
			if i >= 42 && i <= 54 {
				// Invalid household configuration cannot be bridged as a source hold.
				v.Missing = state.MissingLoad
			}
			p := &state.HistoryPoint{TsMs: at, GridW: 1000, LoadW: 1000, JSON: `{}`, Sources: v}
			if err := st.EnqueueTelemetryTick(p, nil, nil); err != nil {
				t.Fatal(err)
			}
			if i%10 == 0 {
				if err := st.FlushHistory(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: st, Tel: telemetry.NewStore(), Ctrl: &control.State{}, CtrlMu: &sync.Mutex{},
		CapMu: &sync.RWMutex{}, CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
	check := func(fields map[string]any) {
		t.Helper()
		if fields["load_wh"] != nil || fields["history_quality"] != "incomplete" {
			t.Fatal("partial data claimed as a complete day", fields)
		}
		sub, ok := fields["load_subtotal"].(map[string]any)
		if !ok || sub["quality"] != "estimated" || sub["coverage_ms"] != float64(600000) {
			t.Fatal("accepted subtotal or its coverage missing", fields)
		}
		if wh, ok := sub["wh"].(float64); !ok || wh < 166.66 || wh > 166.67 {
			t.Fatal(sub)
		}
	}
	rr := httptest.NewRecorder()
	srv.handleStatus(rr, httptest.NewRequest("GET", "/api/status", nil))
	var status struct {
		Energy struct {
			Today map[string]any `json:"today"`
		} `json:"energy"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	check(status.Energy.Today)
	if status.Energy.Today["measured"].(map[string]any)["load_wh"] != nil {
		t.Fatal("estimated subtotal leaked into measured energy")
	}
	// Repeat the read: yesterday takes the persistent/in-memory cache path,
	// while its independently queried subtotal must retain the same coverage.
	for i := 0; i < 2; i++ {
		rr = httptest.NewRecorder()
		srv.handleEnergyDaily(rr, httptest.NewRequest("GET", "/api/energy/daily?days=2", nil))
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
		var daily struct {
			Days []map[string]any `json:"days"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &daily); err != nil {
			t.Fatal(err)
		}
		if len(daily.Days) != 2 {
			t.Fatal(daily)
		}
		for _, fields := range daily.Days {
			check(fields)
		}
	}
}
