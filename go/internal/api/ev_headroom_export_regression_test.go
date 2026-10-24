package api

import (
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVHeadroomRequiresPhaseImportDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		grid float64
		data string
	}{
		{"missing phase power", 13800, `{"l1_a":20,"l2_a":20,"l3_a":20}`},
		{"missing one phase power", 13800, `{"l1_a":20,"l2_a":20,"l3_a":20,"l1_w":4600,"l2_w":4600}`},
		{"null phase power", 13800, `{"l1_a":20,"l2_a":20,"l3_a":20,"l1_w":null,"l2_w":4600,"l3_w":4600}`},
		{"zero phase power leaves direction unknown", 9200, `{"l1_a":20,"l2_a":20,"l3_a":20,"l1_w":0,"l2_w":4600,"l3_w":4600}`},
		{"all phases export with magnitude currents", -13800, `{"l1_a":20,"l2_a":20,"l3_a":20,"l1_w":-4600,"l2_w":-4600,"l3_w":-4600}`},
		{"mixed flow with net import and magnitude currents", 920, `{"l1_a":20,"l2_a":12,"l3_a":12,"l1_w":-4600,"l2_w":2760,"l3_w":2760}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			tel.Update("meter", telemetry.DerMeter, tc.grid, nil, []byte(tc.data))
			tel.DriverHealthMut("meter").RecordSuccess()
			cfg := &config.Config{Fuse: config.Fuse{MaxAmps: 25, Phases: 3, Voltage: 230}, Loadpoints: []config.Loadpoint{{DriverName: "ev", PhaseMode: "3p"}}}
			srv := New(&Deps{Tel: tel, Cfg: cfg, CfgMu: &sync.RWMutex{}})
			now := time.Now()
			f := ControlFeedback{Driver: "ev", Kind: "ev", Reason: "power_below_target", ReadingsFresh: true, SentW: watts(10350), ActualW: watts(4155), ObservedAtMs: now.UnixMilli()}
			got := srv.evSiteHeadroom(f, loadpoint.State{Phases: 3, VoltageV: 230}, "meter", now)
			if got != nil {
				t.Fatalf("unqualified phase direction produced an import-headroom explanation: %+v", got)
			}
		})
	}
}
