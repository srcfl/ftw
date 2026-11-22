package mpc

import (
	"encoding/json"
	"fmt"

	"github.com/srcfl/ftw/go/internal/battery"
)

// LearnedLimits are what a battery has shown it can do, from the limits its
// model trusts (battery.Plateaus). A battery that gives less than the plan
// asks ends its slots under-delivered and makes Core replan; planning within
// these limits keeps the plan to what the battery does. They only lower the
// configured limits, and zero means none is known.
type LearnedLimits struct {
	// ChargeW and DischargeW are the most the battery gives itself: a DC
	// current limit at the pack's voltage now, or a power limit.
	ChargeW    float64
	DischargeW float64
	// GridImportW is the grid import the inverter holds while the battery
	// charges. The battery then gets that import plus PV less the house.
	GridImportW float64
}

func (l LearnedLimits) known() bool {
	return l.ChargeW > 0 || l.DischargeW > 0 || l.GridImportW > 0
}

// learnedLimits reads the trusted plateaus of the temperature band the
// battery is in now. A current limit becomes watts at the pack's voltage now;
// without a voltage it is left out. A limit below the least power the learner
// samples (battery.PlateauMinShortfallW) cannot be one it learned, so it is
// left out too. Where a current and a power limit both hold, the lower wins.
func learnedLimits(known []battery.Plateau, voltageV *float64) LearnedLimits {
	var l LearnedLimits
	lower := func(have *float64, w float64) {
		if !finite(w) || w < battery.PlateauMinShortfallW {
			return
		}
		if *have == 0 || w < *have {
			*have = w
		}
	}
	for _, k := range known {
		var w float64
		switch k.Kind {
		case battery.LimitBatteryPower:
			w = k.Value
		case battery.LimitBatteryCurrent:
			if voltageV == nil {
				continue
			}
			w = k.Value * *voltageV
		case battery.LimitGridImport:
			if k.Direction == "charge" {
				lower(&l.GridImportW, k.Value)
			}
			continue
		default:
			// A grid export limit says what the site may export, not what
			// the battery can discharge.
			continue
		}
		switch k.Direction {
		case "charge":
			lower(&l.ChargeW, w)
		case "discharge":
			lower(&l.DischargeW, w)
		}
	}
	return l
}

// batteryPackNow is the pack temperature and DC voltage a battery's driver
// reports now (srcfl/device-drivers docs/WRITING-A-DRIVER.md, "What a battery
// can do now"), with the legacy names. A missing or implausible value is nil,
// by the same rules as the control verdict.
func batteryPackNow(data json.RawMessage) (tempC, voltageV *float64) {
	var b struct {
		TemperatureC *float64 `json:"temperature_C"`
		LegacyTempC  *float64 `json:"temp_c"`
		V            *float64 `json:"V"`
		LegacyV      *float64 `json:"v"`
	}
	if len(data) == 0 || json.Unmarshal(data, &b) != nil {
		return nil, nil
	}
	tempC, voltageV = b.TemperatureC, b.V
	if tempC == nil {
		tempC = b.LegacyTempC
	}
	if voltageV == nil {
		voltageV = b.LegacyV
	}
	if tempC != nil && (!finite(*tempC) || *tempC < -40 || *tempC > 100) {
		tempC = nil
	}
	if voltageV != nil && (!finite(*voltageV) || *voltageV <= 0) {
		voltageV = nil
	}
	return tempC, voltageV
}

// slotLimitsW is what the storage can charge and discharge in slot, in W:
// its configured limits, lowered by what it has shown it can do. Under a
// grid import limit the charge is that import plus the slot's PV less its
// house load, the forecasts the plan uses for the slot.
func (s StorageAssetSpec) slotLimitsW(slot Slot) (chargeW, dischargeW float64) {
	chargeW, dischargeW = s.MaxChargeW, s.MaxDischargeW
	if l := s.Learned.ChargeW; l > 0 {
		chargeW = min(chargeW, l)
	}
	if l := s.Learned.DischargeW; l > 0 {
		dischargeW = min(dischargeW, l)
	}
	if l := s.Learned.GridImportW; l > 0 {
		chargeW = min(chargeW, max(0, l-slot.LoadW-slot.PVW))
	}
	return chargeW, dischargeW
}

// storageSlotLimits is one storage's charge and discharge limit per slot for
// the worker. A side whose limits never fall below the configured one is nil,
// so a storage with nothing learned keeps today's request.
func storageSlotLimits(slots []Slot, s StorageAssetSpec) (charge, discharge []float64) {
	if !s.Learned.known() {
		return nil, nil
	}
	charge, discharge = make([]float64, len(slots)), make([]float64, len(slots))
	lowerCharge, lowerDischarge := false, false
	for i, slot := range slots {
		charge[i], discharge[i] = s.slotLimitsW(slot)
		lowerCharge = lowerCharge || charge[i] < s.MaxChargeW
		lowerDischarge = lowerDischarge || discharge[i] < s.MaxDischargeW
	}
	if !lowerCharge {
		charge = nil
	}
	if !lowerDischarge {
		discharge = nil
	}
	return charge, discharge
}

// fleetSlotLimitsW is the batteries' summed charge and discharge limit per
// slot, within the aggregate limits, for Core's own planner. Nil when no
// battery has a learned limit: the planner then keeps the aggregate limits.
func fleetSlotLimitsW(slots []Slot, p Params) (charge, discharge []float64) {
	known := false
	for _, s := range p.Storages {
		known = known || s.Learned.known()
	}
	if !known {
		return nil, nil
	}
	charge, discharge = make([]float64, len(slots)), make([]float64, len(slots))
	for i, slot := range slots {
		for _, s := range p.Storages {
			c, d := s.slotLimitsW(slot)
			charge[i] += c
			discharge[i] += d
		}
		charge[i] = min(charge[i], p.MaxChargeW)
		discharge[i] = min(discharge[i], p.MaxDischargeW)
	}
	return charge, discharge
}

// addStorageSlotLimits puts each storage's learned per-slot limits on the
// wire of a worker that announces storage_slot_limits.
func addStorageSlotLimits(request *externalRequest, slots []Slot, p Params) {
	byID := make(map[string]StorageAssetSpec, len(p.Storages))
	for _, s := range p.Storages {
		byID[s.ID] = s
	}
	for i := range request.Storages {
		s, ok := byID[request.Storages[i].ID]
		if !ok {
			continue
		}
		request.Storages[i].MaxChargeWBySlot, request.Storages[i].MaxDischargeWBySlot = storageSlotLimits(slots, s)
	}
}

// validateStorageSlotLimits holds a worker's plan to the per-slot limits Core
// sent it, with the same 2 W as the configured limits.
func validateStorageSlotLimits(request externalRequest, plan externalPlan) error {
	for _, s := range request.Storages {
		for i, a := range plan.Actions {
			w, ok := a.StoragePowerW[s.ID]
			if !ok {
				continue
			}
			if i < len(s.MaxChargeWBySlot) && w > s.MaxChargeWBySlot[i]+2 {
				return fmt.Errorf("slot %d storage %s power %.3f exceeds its slot charge limit %.3f", i, s.ID, w, s.MaxChargeWBySlot[i])
			}
			if i < len(s.MaxDischargeWBySlot) && w < -s.MaxDischargeWBySlot[i]-2 {
				return fmt.Errorf("slot %d storage %s power %.3f exceeds its slot discharge limit %.3f", i, s.ID, w, s.MaxDischargeWBySlot[i])
			}
		}
	}
	return nil
}
