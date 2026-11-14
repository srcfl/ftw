package api

import (
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The control.not_following rule reads the same verdict GET /api/status
// serves: a battery that charges while FTW asked it to discharge.
func TestControlStatesAreTheStatusDocumentsVerdicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		read := func(power float64) {
			tel.Update("hybrid", telemetry.DerBattery, power, nil, []byte(`{}`))
			tel.Update("hybrid", telemetry.DerMeter, 800+power, nil, []byte(`{"power_origin":"external_meter"}`))
			tel.RecordDriverSuccess("hybrid")
		}
		for range 5 {
			read(0)
			time.Sleep(3 * time.Second)
		}
		request, _ := json.Marshal(map[string]any{"action": "battery", "power_w": -5000})
		tel.CompleteCommand(tel.BeginCommand("hybrid", request, time.Now()), "accepted")
		for range 15 {
			read(4400)
			time.Sleep(3 * time.Second)
		}
		srv := New(&Deps{Tel: tel, Ctrl: &control.State{SiteMeterDriver: "hybrid"}, CtrlMu: &sync.Mutex{}})
		now := time.Now()
		rows := srv.controlFeedback(now)
		got := srv.ControlStates(now)
		if len(got) != len(rows) || len(got) == 0 {
			t.Fatalf("states %+v for rows %+v", got, rows)
		}
		want := notifications.ControlState{Driver: "hybrid", Kind: "battery", Status: "not_following"}
		if got[0] != want || rows[0].Reason != "power_wrong_direction" {
			t.Fatalf("got %+v (%s), want %+v", got[0], rows[0].Reason, want)
		}
	})
}
