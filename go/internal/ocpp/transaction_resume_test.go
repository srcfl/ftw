package ocpp

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// restartedHandler is Core just after a restart: a fresh handler and an
// adopted charger that reconnected while its car kept charging.
func restartedHandler(t *testing.T, tel *telemetry.Store, ids ...string) *Handler {
	t.Helper()
	h := NewHandler(tel, 60)
	h.SetApprovedIDs(ids)
	for _, id := range ids {
		h.OnConnect(id)
		h.OnStatusNotification(id, &core.StatusNotificationRequest{ConnectorId: 1, Status: core.ChargePointStatusCharging})
	}
	return h
}

func meterRegister(h *Handler, id string, tx int, wh string) {
	h.OnMeterValues(id, &core.MeterValuesRequest{ConnectorId: 1, TransactionId: &tx, MeterValue: []types.MeterValue{{
		Timestamp:    types.NewDateTime(time.Now()),
		SampledValue: []types.SampledValue{{Value: wh, Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh}},
	}}})
}

func readingSessionWh(t *testing.T, tel *telemetry.Store, id string) float64 {
	t.Helper()
	r := tel.Get(id, telemetry.DerEV)
	if r == nil {
		t.Fatalf("no EV reading for %s", id)
	}
	var d struct {
		SessionWh float64 `json:"session_wh"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.SessionWh
}

// The charger keeps its transaction while Core restarts and goes on sending
// meter values for it. Core takes the transaction up from the first of them,
// so session energy counts again instead of staying at 0 until unplug.
func TestOCPP16PicksUpRunningTransactionAfterRestart(t *testing.T) {
	tel := telemetry.NewStore()
	h := restartedHandler(t, tel, "charger")

	meterRegister(h, "charger", 41, "15200")
	meterRegister(h, "charger", 41, "15700")

	if v := h.Snapshot()["charger"]; v.TxID != 41 || v.SessionWh != 500 {
		t.Fatalf("after restart: tx %d, session %.0f Wh; want tx 41, 500 Wh", v.TxID, v.SessionWh)
	}
	if got := readingSessionWh(t, tel, "charger"); got != 500 {
		t.Fatalf("reading session_wh = %.0f, want 500", got)
	}

	h.OnStopTransaction("charger", &core.StopTransactionRequest{TransactionId: 41, MeterStop: 15900})
	if v := h.Snapshot()["charger"]; v.TxID != -1 || v.SessionWh != 700 {
		t.Fatalf("after stop: tx %d, session %.0f Wh; want tx -1, 700 Wh", v.TxID, v.SessionWh)
	}
}

// A stop for a transaction Core never saw start carries only the meter's
// lifetime total. That is not the session's energy.
func TestOCPP16StopOfUnseenTransactionKeepsLifetimeMeterOut(t *testing.T) {
	tel := telemetry.NewStore()
	h := restartedHandler(t, tel, "charger")

	h.OnStopTransaction("charger", &core.StopTransactionRequest{TransactionId: 41, MeterStop: 1234567})

	if v := h.Snapshot()["charger"]; v.SessionWh != 0 {
		t.Fatalf("session_wh = %.0f, want 0: MeterStop is the lifetime register", v.SessionWh)
	}
}

// A late meter value for a transaction that has stopped must not start it
// again: session energy would fall to 0 while the car is still plugged in.
func TestOCPP16StoppedTransactionIsNotTakenUpAgain(t *testing.T) {
	tel := telemetry.NewStore()
	h := restartedHandler(t, tel, "charger")
	conf, err := h.OnStartTransaction("charger", &core.StartTransactionRequest{ConnectorId: 1, IdTag: "tag", MeterStart: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.OnStopTransaction("charger", &core.StopTransactionRequest{TransactionId: conf.TransactionId, MeterStop: 8500})

	meterRegister(h, "charger", conf.TransactionId, "8500")

	if v := h.Snapshot()["charger"]; v.TxID != -1 || v.SessionWh != 7500 {
		t.Fatalf("tx %d, session %.0f Wh; want tx -1, 7500 Wh", v.TxID, v.SessionWh)
	}
}

// Every transaction id goes to one transaction, across restarts too.
func TestOCPP16TransactionIDsDoNotRepeatAfterRestart(t *testing.T) {
	tel := telemetry.NewStore()
	before := restartedHandler(t, tel, "a", "b")
	old, err := before.OnStartTransaction("a", &core.StartTransactionRequest{ConnectorId: 1, IdTag: "tag", MeterStart: 100})
	if err != nil {
		t.Fatal(err)
	}
	if old.TransactionId == 1 {
		t.Fatalf("first id after a start is 1, which every earlier run also handed out")
	}

	// Core restarts within the same second. Charger a reports its running
	// transaction; then b starts one.
	after := restartedHandler(t, tel, "a", "b")
	meterRegister(after, "a", old.TransactionId, "400")
	next, err := after.OnStartTransaction("b", &core.StartTransactionRequest{ConnectorId: 1, IdTag: "tag", MeterStart: 0})
	if err != nil {
		t.Fatal(err)
	}
	if next.TransactionId == old.TransactionId {
		t.Fatalf("b got transaction id %d, which a still holds", next.TransactionId)
	}
}

// A run starts its ids at the seconds since txIDEpoch, so a later run starts
// above the ids an earlier one used unless it started more transactions than
// it ran seconds.
func TestOCPP16FirstTransactionID(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 13, 1, 49, 0, time.UTC)
	if firstTxID(t0.Add(20*time.Second))-firstTxID(t0) != 20 {
		t.Fatalf("ids do not move on one per second")
	}
	if got := firstTxID(time.Unix(0, 0)); got != 1 {
		t.Fatalf("clock before the epoch: first id %d, want 1", got)
	}
	if got := firstTxID(time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)); got >= math.MaxInt32 {
		t.Fatalf("first id %d in 2090 does not fit the 32 bits chargers store it in", got)
	}
}
