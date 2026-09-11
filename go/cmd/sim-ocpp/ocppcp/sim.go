package ocppcp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
	"github.com/lorenzodonini/ocpp-go/ws"
)

// DialOpts is how a simulated charge point reaches FTW.
type DialOpts struct {
	URL16    string
	URL201   string
	Username string
	Password string
	// TauS is physics lag. Zero is instant, which is what tests want.
	TauS float64
}

// Sim is one charge point speaking either OCPP 1.6J or 2.0.1.
type Sim struct {
	Model    Model
	mu       sync.Mutex
	physics  Physics
	attempts []ProfileAttempt
	txID     int
	txRef    string
	seq      int
	idTag    string

	cp ocpp16.ChargePoint
	cs ocpp201.ChargingStation

	stopOnce sync.Once
	stopped  atomic.Bool
}

// New builds a disconnected simulator for m.
func New(m Model) *Sim {
	return &Sim{
		Model:   m,
		physics: newPhysics(m, 0),
		idTag:   idTagFor(m),
	}
}

func idTagFor(m Model) string {
	if !m.RFID {
		return "AUTO"
	}
	// OCPP 1.6 idTag is CiString20. The catalog slug is often longer.
	tag := "RFID-" + m.ID
	if len(tag) <= 20 {
		return tag
	}
	return m.Serial
}

// DialID is the identity this sim presents on the wire.
func (s *Sim) DialID() string { return s.Model.DialID() }

// Dial connects to FTW. Boot is separate so tests can observe pending vs booted.
func (s *Sim) Dial(opts DialOpts) error {
	if !s.Model.SpeaksOCPP() {
		return fmt.Errorf("%s does not speak OCPP", s.Model.ID)
	}
	s.physics = newPhysics(s.Model, opts.TauS)

	client := ws.NewClient()
	if opts.Username != "" || opts.Password != "" {
		client.SetBasicAuth(opts.Username, opts.Password)
	}

	id := s.DialID()
	switch s.Model.Protocol {
	case ProtocolOCPP201:
		if opts.URL201 == "" {
			return fmt.Errorf("%s needs a 2.0.1 URL", s.Model.ID)
		}
		cs := ocpp201.NewChargingStation(id, nil, client)
		h := &handlers201{sim: s}
		cs.SetProvisioningHandler(h)
		cs.SetSmartChargingHandler(h)
		cs.SetRemoteControlHandler(h)
		if err := cs.Start(opts.URL201); err != nil {
			return fmt.Errorf("connect %s: %w", id, err)
		}
		s.cs = cs
	default:
		if opts.URL16 == "" {
			return fmt.Errorf("%s needs a 1.6 URL", s.Model.ID)
		}
		cp := ocpp16.NewChargePoint(id, nil, client)
		cp.SetCoreHandler(s)
		cp.SetSmartChargingHandler(s)
		cp.SetRemoteTriggerHandler(s)
		if err := cp.Start(opts.URL16); err != nil {
			return fmt.Errorf("connect %s: %w", id, err)
		}
		s.cp = cp
	}
	return nil
}

// Close drops the WebSocket. Idempotent: ocpp-go panics on a second Stop.
func (s *Sim) Close() {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		if s.cp != nil {
			s.cp.Stop()
		}
		if s.cs != nil {
			s.cs.Stop()
		}
	})
}

// Boot sends BootNotification with the catalog vendor/model/serial.
func (s *Sim) Boot() error {
	m := s.Model
	if s.cp != nil {
		_, err := s.cp.BootNotification(m.Name, m.Vendor, func(req *core.BootNotificationRequest) {
			req.ChargePointSerialNumber = m.Serial
			req.FirmwareVersion = m.Firmware
		})
		return err
	}
	if s.cs != nil {
		_, err := s.cs.BootNotification(provisioning.BootReasonPowerUp, m.Name, m.Vendor, func(req *provisioning.BootNotificationRequest) {
			req.ChargingStation.SerialNumber = m.Serial
			req.ChargingStation.FirmwareVersion = m.Firmware
		})
		return err
	}
	return fmt.Errorf("%s is not connected", m.ID)
}

// Plug puts a car on connector 1, starts a transaction, and reports Charging.
func (s *Sim) Plug() error {
	s.mu.Lock()
	s.physics.Plugged = true
	s.mu.Unlock()
	if err := s.statusPlugged(true); err != nil {
		return err
	}
	return s.startTx()
}

// Unplug ends the transaction and reports Available. Charge Amps IgnoreRemoteStop
// does not apply here: this is the cable coming out, not a remote stop.
func (s *Sim) Unplug() error {
	if err := s.stopTx(); err != nil {
		return err
	}
	s.mu.Lock()
	s.physics.Plugged = false
	s.physics.DrawA = 0
	s.mu.Unlock()
	return s.statusPlugged(false)
}

// Tick advances physics. Tests call this with a positive dt and TauS=0 to settle.
func (s *Sim) Tick(dt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.physics.Tick(dt)
}

// Report pushes the current power and energy as MeterValues / TransactionEvent.
func (s *Sim) Report() error {
	s.mu.Lock()
	w := s.physics.PowerW()
	wh := s.physics.EnergyWh
	txID := s.txID
	txRef := s.txRef
	seq := s.seq
	s.seq++
	s.mu.Unlock()

	if s.cp != nil {
		mv := []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: fmt.Sprintf("%.1f", w), Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
				{Value: fmt.Sprintf("%.1f", wh), Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}}
		_, err := s.cp.MeterValues(1, mv, func(req *core.MeterValuesRequest) {
			if txID > 0 {
				req.TransactionId = &txID
			}
		})
		return err
	}
	if s.cs != nil {
		now := types201.NewDateTime(time.Now())
		mv := []types201.MeterValue{{
			Timestamp: *now,
			SampledValue: []types201.SampledValue{
				{Value: w, Measurand: types201.MeasurandPowerActiveImport},
				{Value: wh, Measurand: types201.MeasurandEnergyActiveImportRegister},
			},
		}}
		if txRef != "" {
			_, err := s.cs.TransactionEvent(
				transactions.TransactionEventUpdated,
				now,
				transactions.TriggerReasonMeterValuePeriodic,
				seq,
				transactions.Transaction{TransactionID: txRef},
				func(req *transactions.TransactionEventRequest) {
					req.MeterValue = mv
					req.Evse = &types201.EVSE{ID: 1, ConnectorID: intp(1)}
				},
			)
			return err
		}
		_, err := s.cs.MeterValues(1, mv)
		return err
	}
	return fmt.Errorf("%s is not connected", s.Model.ID)
}

// PowerW is the instantaneous EV load.
func (s *Sim) PowerW() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.physics.PowerW()
}

// Plugged reports whether a cable is in.
func (s *Sim) Plugged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.physics.Plugged
}

// LimitA is the last applied charging-profile limit.
func (s *Sim) LimitA() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.physics.LimitA
}

// Attempts is the SetChargingProfile log, oldest first.
func (s *Sim) Attempts() []ProfileAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProfileAttempt, len(s.attempts))
	copy(out, s.attempts)
	return out
}

func (s *Sim) recordAttempt(a ProfileAttempt) {
	s.mu.Lock()
	s.attempts = append(s.attempts, a)
	if a.Applied {
		s.physics.LimitA = a.LimitA
		s.physics.Tick(0)
	}
	s.mu.Unlock()
}

func (s *Sim) statusPlugged(plugged bool) error {
	if s.cp != nil {
		st := core.ChargePointStatusAvailable
		if plugged {
			st = core.ChargePointStatusCharging
		}
		_, err := s.cp.StatusNotification(1, core.NoError, st)
		return err
	}
	if s.cs != nil {
		st := availability.ConnectorStatusAvailable
		if plugged {
			st = availability.ConnectorStatusOccupied
		}
		_, err := s.cs.StatusNotification(types201.NewDateTime(time.Now()), st, 1, 1)
		return err
	}
	return fmt.Errorf("%s is not connected", s.Model.ID)
}

func (s *Sim) startTx() error {
	s.mu.Lock()
	wh := int(s.physics.EnergyWh)
	tag := s.idTag
	s.mu.Unlock()
	if s.cp != nil {
		conf, err := s.cp.StartTransaction(1, tag, wh, types.NewDateTime(time.Now()))
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.txID = conf.TransactionId
		s.mu.Unlock()
		return nil
	}
	if s.cs != nil {
		s.mu.Lock()
		s.seq++
		seq := s.seq
		ref := s.Model.Serial + "-tx"
		s.txRef = ref
		energy := s.physics.EnergyWh
		s.mu.Unlock()
		now := types201.NewDateTime(time.Now())
		_, err := s.cs.TransactionEvent(
			transactions.TransactionEventStarted,
			now,
			transactions.TriggerReasonCablePluggedIn,
			seq,
			transactions.Transaction{TransactionID: ref},
			func(req *transactions.TransactionEventRequest) {
				req.IDToken = &types201.IdToken{IdToken: tag, Type: types201.IdTokenTypeISO14443}
				req.Evse = &types201.EVSE{ID: 1, ConnectorID: intp(1)}
				req.MeterValue = []types201.MeterValue{{
					Timestamp:    *now,
					SampledValue: []types201.SampledValue{{Value: energy, Measurand: types201.MeasurandEnergyActiveImportRegister}},
				}}
			},
		)
		return err
	}
	return fmt.Errorf("%s is not connected", s.Model.ID)
}

func (s *Sim) stopTx() error {
	s.mu.Lock()
	txID := s.txID
	txRef := s.txRef
	wh := int(s.physics.EnergyWh)
	energy := s.physics.EnergyWh
	s.txID = 0
	s.txRef = ""
	s.mu.Unlock()
	if s.cp != nil {
		if txID == 0 {
			return nil
		}
		_, err := s.cp.StopTransaction(wh, types.NewDateTime(time.Now()), txID)
		return err
	}
	if s.cs != nil {
		if txRef == "" {
			return nil
		}
		s.mu.Lock()
		s.seq++
		seq := s.seq
		s.mu.Unlock()
		now := types201.NewDateTime(time.Now())
		_, err := s.cs.TransactionEvent(
			transactions.TransactionEventEnded,
			now,
			transactions.TriggerReasonEVCommunicationLost,
			seq,
			transactions.Transaction{TransactionID: txRef},
			func(req *transactions.TransactionEventRequest) {
				req.Evse = &types201.EVSE{ID: 1, ConnectorID: intp(1)}
				req.MeterValue = []types201.MeterValue{{
					Timestamp:    *now,
					SampledValue: []types201.SampledValue{{Value: energy, Measurand: types201.MeasurandEnergyActiveImportRegister}},
				}}
			},
		)
		return err
	}
	return nil
}

func intp(v int) *int { return &v }
