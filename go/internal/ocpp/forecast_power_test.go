package ocpp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type forecastOCPPFixture struct {
	h      *Handler
	tel    *telemetry.Store
	power  func(float64, time.Time)
	energy func()
	status func()
	stop   func()
}

func newForecastOCPPFixture(t *testing.T, version string) forecastOCPPFixture {
	t.Helper()
	tel := telemetry.NewStore()
	h := NewHandler(tel, 60)
	h.SetApprovedIDs([]string{"charger"})
	h.OnConnect("charger")
	f := forecastOCPPFixture{h: h, tel: tel}
	if version == "1.6" {
		f.power = func(w float64, at time.Time) {
			h.OnMeterValues("charger", &core.MeterValuesRequest{ConnectorId: 1, MeterValue: []types16.MeterValue{{Timestamp: types16.NewDateTime(at), SampledValue: []types16.SampledValue{{Value: fmt.Sprint(w), Measurand: types16.MeasurandPowerActiveImport}}}}})
		}
		f.energy = func() {
			h.OnMeterValues("charger", &core.MeterValuesRequest{ConnectorId: 1, MeterValue: []types16.MeterValue{{Timestamp: types16.NewDateTime(time.Now()), SampledValue: []types16.SampledValue{{Value: "1234", Measurand: types16.MeasurandEnergyActiveImportRegister}}}}})
		}
		f.status = func() {
			h.OnStatusNotification("charger", &core.StatusNotificationRequest{ConnectorId: 1, Status: core.ChargePointStatusCharging})
		}
		f.stop = func() { h.OnStopTransaction("charger", &core.StopTransactionRequest{}) }
	} else {
		v := &handlerV201{h}
		f.power = func(w float64, at time.Time) {
			v.OnMeterValues("charger", &meter.MeterValuesRequest{EvseID: 1, MeterValue: []types201.MeterValue{{Timestamp: *types201.NewDateTime(at), SampledValue: []types201.SampledValue{{Value: w, Measurand: types201.MeasurandPowerActiveImport}}}}})
		}
		f.energy = func() {
			v.OnMeterValues("charger", &meter.MeterValuesRequest{EvseID: 1, MeterValue: []types201.MeterValue{{Timestamp: *types201.NewDateTime(time.Now()), SampledValue: []types201.SampledValue{{Value: 1234, Measurand: types201.MeasurandEnergyActiveImportRegister}}}}})
		}
		f.status = func() {
			v.OnStatusNotification("charger", &availability.StatusNotificationRequest{EvseID: 1, ConnectorID: 1, ConnectorStatus: availability.ConnectorStatusOccupied})
		}
		f.stop = func() {
			v.OnTransactionEvent("charger", &transactions.TransactionEventRequest{EventType: transactions.TransactionEventEnded})
		}
	}
	return f
}
func (f forecastOCPPFixture) reading() telemetry.ForecastReading {
	f.tel.Update("site", telemetry.DerMeter, 5000, nil, nil)
	f.tel.RecordDriverSuccess("site")
	f.tel.Update("pv", telemetry.DerPV, -1000, nil, nil)
	f.tel.RecordDriverSuccess("pv")
	return f.tel.ForecastMeasurement(time.Now(), "site", telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "charger", DerType: telemetry.DerEV}}, MaxSkew: time.Minute})
}

func TestForecastOCPPRequiresRealPowerInBothVersions(t *testing.T) {
	for _, version := range []string{"1.6", "2.0.1"} {
		t.Run(version, func(t *testing.T) {
			f := newForecastOCPPFixture(t, version)
			for _, stage := range []func(){func() {}, f.status, f.energy} {
				stage()
				r := f.reading()
				if r.Valid || !r.PVValid || !strings.HasPrefix(r.Reason, "unknown_power:charger:") {
					t.Fatalf("status/energy-only became measured zero: %+v", r)
				}
			}
			at := time.Now()
			f.power(0, at)
			r := f.reading()
			if !r.Valid || r.EVW != 0 || r.HouseholdW != 6000 {
				t.Fatalf("measured zero rejected: %+v", r)
			}
			f.stop()
			r = f.reading()
			if r.Valid || !r.PVValid {
				t.Fatalf("synthetic stop zero became a measured sample: %+v", r)
			}
			// Replaying a pre-stop sample must not revive a synthetic zero.
			f.power(0, at)
			if r = f.reading(); r.Valid {
				t.Fatal("duplicate timestamp revived stopped power")
			}
			f.h.OnDisconnect("charger")
			f.h.OnConnect("charger")
			f.status()
			if r = f.reading(); r.Valid {
				t.Fatal("reconnect reused prior-socket power")
			}
			time.Sleep(2 * time.Millisecond) // next distinct source timestamp
			f.power(0, time.Now())
			if r = f.reading(); !r.Valid {
				t.Fatalf("new socket's real zero rejected: %+v", r)
			}
		})
	}
}

func TestForecastOCPPStatusCannotRefreshOldPower(t *testing.T) {
	for _, version := range []string{"1.6", "2.0.1"} {
		t.Run(version, func(t *testing.T) {
			f := newForecastOCPPFixture(t, version)
			// The connection predates the delayed source sample; receipt is fresh.
			f.h.mu.Lock()
			f.h.chargers["charger"].powerConnectedAt = time.Now().Add(-time.Hour)
			f.h.mu.Unlock()
			f.power(700, time.Now().Add(-2*time.Minute))
			f.status()
			f.energy()
			r := f.reading()
			if r.Valid || !r.PVValid || !strings.HasPrefix(r.Reason, "stale:charger:") {
				t.Fatalf("later status/energy refreshed old power: %+v", r)
			}
		})
	}
}

func TestForecastOCPPRejectsFutureAndOutOfOrderSamples(t *testing.T) {
	for _, version := range []string{"1.6", "2.0.1"} {
		t.Run(version, func(t *testing.T) {
			f := newForecastOCPPFixture(t, version)
			f.power(900, time.Now().Add(time.Hour))
			if f.reading().Valid {
				t.Fatal("future source timestamp accepted")
			}
			at := time.Now()
			f.power(700, at)
			f.power(9000, at.Add(-time.Second))
			r := f.reading()
			if !r.Valid || r.EVW != 700 {
				t.Fatalf("out-of-order power replaced accepted measurement: %+v", r)
			}
			if raw := f.tel.Get("charger", telemetry.DerEV); raw.RawW != 9000 {
				t.Fatal("forecast qualification changed existing dispatch publication")
			}
		})
	}
}

func TestForecastOCPP201TransactionPowerAndMissingPower(t *testing.T) {
	f := newForecastOCPPFixture(t, "2.0.1")
	v := &handlerV201{f.h}
	v.OnTransactionEvent("charger", &transactions.TransactionEventRequest{EventType: transactions.TransactionEventUpdated})
	if f.reading().Valid {
		t.Fatal("power-free transaction event became zero measurement")
	}
	v.OnTransactionEvent("charger", &transactions.TransactionEventRequest{EventType: transactions.TransactionEventUpdated, MeterValue: []types201.MeterValue{{Timestamp: *types201.NewDateTime(time.Now()), SampledValue: []types201.SampledValue{{Value: 0, Measurand: types201.MeasurandPowerActiveImport}}}}})
	if r := f.reading(); !r.Valid || r.EVW != 0 {
		t.Fatalf("real transaction zero rejected: %+v", r)
	}
}
