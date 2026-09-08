package mpc

import "time"

// ExecutionStart is the first instant modeled from the supplied battery/EV
// state. Price and forecast intervals keep their original boundaries.
func (s Slot) ExecutionStart() int64 {
	if s.ExecutionStartMs != 0 {
		return s.ExecutionStartMs
	}
	return s.StartMs
}

func (s Slot) DurationHours() float64 {
	return float64(s.StartMs+int64(s.LenMin)*60000-s.ExecutionStart()) / 3600000
}

func (a Action) ExecutionStart() int64 {
	if a.ExecutionStartMs != 0 {
		return a.ExecutionStartMs
	}
	return a.SlotStartMs
}

func (a Action) DurationHours() float64 {
	return float64(a.SlotStartMs+int64(a.SlotLenMin)*60000-a.ExecutionStart()) / 3600000
}

func (s *Service) planningNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func trimFirstExecutionSlot(slots []Slot, now time.Time) bool {
	if len(slots) == 0 {
		return false
	}
	end := slots[0].StartMs + int64(slots[0].LenMin)*60000
	if now.UnixMilli() >= end {
		return false
	}
	if now.UnixMilli() > slots[0].StartMs {
		slots[0].ExecutionStartMs = now.UnixMilli()
	}
	return true
}

func firstSlotExpired(actions []Action, now time.Time) bool {
	return len(actions) == 0 || now.UnixMilli() >= actions[0].SlotStartMs+int64(actions[0].SlotLenMin)*60000
}

func (s *Service) expiredReplan(request replanRequest) *Plan {
	// A slot that ended during solve never gets published with fictional energy.
	// One immediate retry rebuilds from live state; repeated expiry stays failed.
	if request.reason != "slot_elapsed" && !request.wasCanceledByService() {
		s.RequestReplan("slot_elapsed")
	}
	return s.Latest()
}

func validatePartialSlots(slots []Slot) error {
	for i, s := range slots {
		if s.ExecutionStartMs == 0 {
			continue
		}
		end, err := checkedSlotEndMs(s.StartMs, s.LenMin)
		if err != nil {
			return err
		}
		if err = validateExecutionStart(i, s.StartMs, end, s.ExecutionStartMs); err != nil {
			return err
		}
	}
	return nil
}
