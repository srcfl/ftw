package mpc

import (
	"errors"
	"fmt"
)

func checkedSlotEndMs(startMs int64, lenMin int) (int64, error) {
	const minuteMs int64 = 60 * 1000
	if startMs <= 0 {
		return 0, errors.New("start must be positive")
	}
	if lenMin <= 0 {
		return 0, errors.New("length must be positive")
	}
	slotLenMs := int64(lenMin) * minuteMs
	if slotLenMs <= 0 || slotLenMs/minuteMs != int64(lenMin) {
		return 0, errors.New("length overflows milliseconds")
	}
	endMs := startMs + slotLenMs
	if endMs <= startMs {
		return 0, errors.New("end overflows milliseconds")
	}
	return endMs, nil
}

func validateSlotChronology(slots []Slot) error {
	if len(slots) == 0 {
		return errors.New("empty timeline")
	}
	var previousEndMs int64
	for i, slot := range slots {
		endMs, err := checkedSlotEndMs(slot.StartMs, slot.LenMin)
		if err != nil {
			return fmt.Errorf("slot %d: %w", i, err)
		}
		if err := validateExecutionStart(i, slot.StartMs, endMs, slot.ExecutionStartMs); err != nil {
			return err
		}
		if previousEndMs > 0 && slot.StartMs < previousEndMs {
			return fmt.Errorf("slot %d starts before the prior slot ends", i)
		}
		previousEndMs = endMs
	}
	return nil
}

func validateActionChronology(actions []Action) error {
	if len(actions) == 0 {
		return errors.New("empty timeline")
	}
	var previousEndMs int64
	for i, action := range actions {
		endMs, err := checkedSlotEndMs(action.SlotStartMs, action.SlotLenMin)
		if err != nil {
			return fmt.Errorf("action %d: %w", i, err)
		}
		if err := validateExecutionStart(i, action.SlotStartMs, endMs, action.ExecutionStartMs); err != nil {
			return err
		}
		if previousEndMs > 0 && action.SlotStartMs < previousEndMs {
			return fmt.Errorf("action %d starts before the prior action ends", i)
		}
		previousEndMs = endMs
	}
	return nil
}

func validatePlanSlotAlignment(slots []Slot, actions []Action) error {
	if err := validateSlotChronology(slots); err != nil {
		return err
	}
	if err := validateActionChronology(actions); err != nil {
		return err
	}
	if len(actions) != len(slots) {
		return fmt.Errorf("action count %d, want %d", len(actions), len(slots))
	}
	for i, slot := range slots {
		action := actions[i]
		if action.SlotStartMs != slot.StartMs || action.SlotLenMin != slot.LenMin || action.ExecutionStartMs != slot.ExecutionStartMs {
			return fmt.Errorf("action %d does not match its input slot", i)
		}
	}
	return nil
}

func validateExecutionStart(index int, start, end, execution int64) error {
	if execution != 0 && (index != 0 || execution < start || execution >= end) {
		return fmt.Errorf("slot %d execution start must be within the first interval", index)
	}
	return nil
}
