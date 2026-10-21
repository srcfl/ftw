package cars

import (
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

func TestCarAcceptsTwoPhases(t *testing.T) {
	for _, phases := range []int{0, 1, 2, 3} {
		var car Car
		if err := applyUpdate(&car, Update{Phases: &phases}); err != nil || car.Phases != phases {
			t.Fatalf("phases %d: car=%+v, err=%v", phases, car, err)
		}
		if got := bindingFor(&car); got.Phases != phases {
			t.Fatalf("phases %d lost at the charger: %+v", phases, got)
		}
	}
	for _, phases := range []int{-1, 4} {
		if err := applyUpdate(&Car{}, Update{Phases: &phases}); err == nil {
			t.Fatalf("accepted %d phases", phases)
		}
	}
}

func TestTwoPhaseCarSurvivesRestart(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.add(protocol.Car{Name: "V60", Phases: 2, MaxW: 6400})
	again := newHarness(t, homeConfig(), nil, h.kv)
	again.start()
	for _, stored := range again.rt.Cars() {
		if stored.ID == car.ID {
			if stored.Phases != 2 || stored.MaxW != 6400 {
				t.Fatalf("car lost its AC limits after restart: %+v", stored)
			}
			return
		}
	}
	t.Fatal("car missing after restart")
}
