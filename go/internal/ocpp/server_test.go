package ocpp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// freePort returns a port the kernel just allocated and immediately gave back.
// Cheaper than racing a hardcoded port across parallel test runs.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startServer brings up an OCPP CS on a free port and returns the port + a
// matching ws://… URL plus the running server. Caller must defer Stop.
func startServer(t *testing.T, tel *telemetry.Store) (int, *Server) {
	t.Helper()
	port := freePort(t)
	cfg := &Config{Enabled: true, Bind: "127.0.0.1", Port: port, HeartbeatIntervalS: 60}
	srv, err := Start(context.Background(), cfg, tel)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Wait for the listener to be reachable. cs.Start launches goroutines —
	// without a tiny pause the client will racily connect to nothing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return port, srv
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not bind on port %d within deadline", port)
	return 0, nil
}

func TestStopIsConcurrentAndIdempotent(t *testing.T) {
	_, srv := startServer(t, telemetry.NewStore())

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.Stop()
		}()
	}
	wg.Wait()
	srv.Stop()
}

func TestBootAndMeterValuesPushDerEV(t *testing.T) {
	tel := telemetry.NewStore()
	port, srv := startServer(t, tel)
	defer srv.Stop()

	cp := ocpp16.NewChargePoint("EH123456", nil, nil)
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	if err := cp.Start(url); err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cp.Stop()

	if _, err := cp.BootNotification("EaseeHome", "Easee"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	// Charging status — handler should mark connected+charging.
	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusCharging); err != nil {
		t.Fatalf("status: %v", err)
	}

	// MeterValues with a 7200 W power sample.
	mv := []types.MeterValue{{
		Timestamp: types.NewDateTime(time.Now()),
		SampledValue: []types.SampledValue{
			{
				Value:     "7200",
				Measurand: types.MeasurandPowerActiveImport,
				Unit:      types.UnitOfMeasureW,
			},
		},
	}}
	if _, err := cp.MeterValues(1, mv); err != nil {
		t.Fatalf("meter values: %v", err)
	}

	// Allow the handler goroutine to flush.
	deadline := time.Now().Add(2 * time.Second)
	var r *telemetry.DerReading
	for time.Now().Before(deadline) {
		r = tel.Get("EH123456", telemetry.DerEV)
		if r != nil && r.RawW > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r == nil {
		t.Fatal("expected DerEV reading for EH123456, got nil")
	}
	if r.RawW != 7200 {
		t.Errorf("expected 7200 W, got %f", r.RawW)
	}

	view := srv.Handler().Snapshot()["EH123456"]
	if !view.Connected || !view.Charging {
		t.Errorf("expected connected+charging, got %+v", view)
	}
}

func TestStartStopTransactionTracksSession(t *testing.T) {
	tel := telemetry.NewStore()
	port, srv := startServer(t, tel)
	defer srv.Stop()

	cp := ocpp16.NewChargePoint("EH-SESSION", nil, nil)
	if err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cp.Stop()

	if _, err := cp.BootNotification("Home", "Easee"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	startConf, err := cp.StartTransaction(1, "RFID-ABCD", 1000, types.NewDateTime(time.Now()))
	if err != nil {
		t.Fatalf("start tx: %v", err)
	}
	if startConf.IdTagInfo.Status != types.AuthorizationStatusAccepted {
		t.Errorf("expected accepted, got %s", startConf.IdTagInfo.Status)
	}

	if _, err := cp.StopTransaction(8500, types.NewDateTime(time.Now()), startConf.TransactionId); err != nil {
		t.Fatalf("stop tx: %v", err)
	}

	// Session energy = 8500 - 1000 = 7500 Wh
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v := srv.Handler().Snapshot()["EH-SESSION"]
		if v.SessionWh == 7500 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	view := srv.Handler().Snapshot()["EH-SESSION"]
	t.Errorf("expected session_wh=7500, got %+v", view)
}

func TestBasicAuthRejectsWrongCredentials(t *testing.T) {
	tel := telemetry.NewStore()
	port := freePort(t)
	cfg := &Config{Enabled: true, Bind: "127.0.0.1", Port: port, Username: "easee", Password: "secret"}
	srv, err := Start(context.Background(), cfg, tel)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	wsClient := ws.NewClient()
	wsClient.SetBasicAuth("easee", "wrong-password")
	cp := ocpp16.NewChargePoint("EH-AUTH", nil, wsClient)
	err = cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port))
	if err == nil {
		cp.Stop()
		t.Error("expected auth failure with wrong password, got nil")
	}
}
