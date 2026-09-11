package main

import (
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestAppSolarPreferenceRejectsFailedSaveAndAcceptsRetry(t *testing.T) {
	m := loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", SurplusOnly: true}})
	failure := errors.New("disk full")
	saved := true
	m.SetSurplusOnlySaver(func(_ string, v bool) error {
		if failure != nil {
			return failure
		}
		saved = v
		return nil
	})
	if previous, found, err := m.SetSurplusOnlyChecked("garage", false); !previous || !found || !errors.Is(err, failure) {
		t.Fatalf("checked setter = %v, %v, %v", previous, found, err)
	}
	if previous, ok := m.SetSurplusOnly("garage", false); !previous || ok {
		t.Fatalf("compatibility wrapper = %v, %v", previous, ok)
	}
	port := &appLoadpoints{mgr: m}
	if _, ok := port.SetSurplusOnly("garage", false); ok {
		t.Fatal("app port accepted a failed save")
	}
	if actual, ok := port.ObservedSurplusOnly("garage"); !ok || !actual || !saved {
		t.Fatalf("failed save changed the active choice: actual=%v known=%v saved=%v", actual, ok, saved)
	}
	failure = nil
	if previous, ok := port.SetSurplusOnly("garage", false); !ok || !previous {
		t.Fatalf("retry = %v, %v", previous, ok)
	}
	if actual, ok := port.ObservedSurplusOnly("garage"); !ok || actual || saved {
		t.Fatalf("retry did not save the choice: actual=%v known=%v saved=%v", actual, ok, saved)
	}
}
