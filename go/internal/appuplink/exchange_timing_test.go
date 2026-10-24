package appuplink

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
)

// TestExchangeTimingAtTheBox times an applied and a refused command at the
// box, the gap decision F's floor hides from whoever carries the exchange.
// Skipped unless FTW_EXCHANGE_TIMING is set; it reports and asserts nothing.
func TestExchangeTimingAtTheBox(t *testing.T) {
	if os.Getenv("FTW_EXCHANGE_TIMING") == "" {
		t.Skip("set FTW_EXCHANGE_TIMING=1 to time answers at the box")
	}
	save, _ := strconv.Atoi(os.Getenv("FTW_EV_SAVE_MS"))
	r := newXRig(t)
	r.x.cmdFloor = ExchangeCmdFloor
	r.charger.save = time.Duration(save) * time.Millisecond
	var applied, refused []time.Duration
	var w *watchClient
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			// A second on the rig's clock per handshake keeps the run under
			// the sixty a minute the box allows.
			r.advance(time.Second)
			w, _ = r.addWatch(t)
		}
		started := time.Now()
		r.ask(t, w, appproto.MsgCmd, chargeNow(fmt.Sprintf("box-timing-%d", i)))
		applied = append(applied, time.Since(started))
		started = time.Now()
		r.ask(t, w, appproto.MsgCmd, appproto.Cmd{CmdID: fmt.Sprintf("box-refused-%d", i), Op: appproto.OpSetMode,
			Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}})
		refused = append(refused, time.Since(started))
	}
	t.Logf("at the box, EV save %d ms: applied %s; refused %s", save, spread(applied), spread(refused))
}

// spread is p50 and p95 of a sample.
func spread(d []time.Duration) string {
	s := slices.Clone(d)
	slices.Sort(s)
	at := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return fmt.Sprintf("p50 %s p95 %s (n=%d)", at(0.5).Round(10*time.Microsecond), at(0.95).Round(10*time.Microsecond), len(s))
}
