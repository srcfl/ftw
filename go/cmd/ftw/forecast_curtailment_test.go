package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type curtailStore struct {
	mu         sync.Mutex
	value      string
	identities map[string]string
	block      chan struct{}
}

func (s *curtailStore) LoadConfig(string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.value != ""
}
func (s *curtailStore) SaveConfig(_ string, value string) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
	return nil
}
func (s *curtailStore) LookupDeviceByDriverName(name string) *state.Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.identities[name]
	if id == "" {
		return nil
	}
	return &state.Device{DeviceID: id}
}

func waitCurtailment(t *testing.T, f *forecastCurtailment, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for f.Active() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.Active() != want {
		t.Fatalf("Active=%v want %v", f.Active(), want)
	}
}

func TestForecastCurtailmentFailedReleaseStaysCensored(t *testing.T) {
	store := &curtailStore{identities: map[string]string{"pv": "hardware-1"}}
	fail := false
	sender := &stubSender{handler: func(context.Context, string) error {
		if fail {
			return errors.New("release failed")
		}
		return nil
	}}
	f := newTestForecastCurtailment(sender, store)
	defer f.Close()
	if err := f.Send(context.Background(), "pv", []byte(`{"action":"curtail","power_w":2000}`)); err != nil {
		t.Fatal(err)
	}
	if !f.Active() {
		t.Fatal("commanded curtailment not censored before acknowledgement")
	}
	fail = true
	if err := f.Send(context.Background(), "pv", []byte(`{"action":"curtail_disable"}`)); err == nil {
		t.Fatal("underlying error changed")
	}
	f.ObserveIntent(false)
	if !f.Active() {
		t.Fatal("failed release lost censorship")
	}
	fail = false
	f.Send(context.Background(), "pv", []byte(`{"action":"curtail_disable"}`))
	waitCurtailment(t, f, false)
	if len(sender.recorded()) != 3 {
		t.Fatal("wrapper sent extra commands")
	}
}

func TestForecastCurtailmentRestartAndRenameUseHardwareIdentity(t *testing.T) {
	store := &curtailStore{identities: map[string]string{"old-pv-name": "hardware-1", "second-pv": "hardware-2"}}
	sender := &stubSender{}
	f := newTestForecastCurtailment(sender, store)
	f.Send(context.Background(), "old-pv-name", []byte(`{"action":"curtail","power_w":0}`))
	f.Send(context.Background(), "second-pv", []byte(`{"action":"curtail","power_w":2000}`))
	f.Close()
	stored, _ := store.LoadConfig(forecastCurtailmentKey)
	if strings.Contains(stored, "old-pv-name") || !strings.Contains(stored, "hardware-1") {
		t.Fatalf("not stable identity keyed: %s", stored)
	}
	store.mu.Lock()
	store.identities["new-pv-name"] = "hardware-1"
	delete(store.identities, "old-pv-name")
	store.mu.Unlock()
	restarted := newTestForecastCurtailment(sender, store)
	defer restarted.Close()
	if !restarted.Active() {
		t.Fatal("restart lost pending curtailment")
	}
	restarted.RecordDefault("new-pv-name", true)
	if !restarted.Active() {
		t.Fatal("one default cleared two devices")
	}
	restarted.RecordDefault("second-pv", false)
	if !restarted.Active() {
		t.Fatal("failed default cleared censorship")
	}
	restarted.RecordDefault("second-pv", true)
	waitCurtailment(t, restarted, false)
}

func TestForecastCurtailmentManualHoldAndUnknown(t *testing.T) {
	store := &curtailStore{identities: map[string]string{}}
	f := newTestForecastCurtailment(&stubSender{}, store)
	f.ObserveIntent(true)
	if !f.Active() {
		t.Fatal("manual zero hold must censor without a command")
	}
	f.ObserveIntent(false)
	if f.Active() {
		t.Fatal("cleared command-free intent retained false curtailment")
	}
	f.Send(context.Background(), "unidentified", []byte(`{"action":"curtail","power_w":0}`))
	f.Close()
	r := newTestForecastCurtailment(&stubSender{}, store)
	defer r.Close()
	r.RecordDefault("unrelated", true)
	if !r.Active() {
		t.Fatal("unrelated default cleared unknown prior identity")
	}
	r.ConfirmUncurtailed()
	waitCurtailment(t, r, false)
}

func TestForecastCurtailmentPersistenceCannotBlockDispatch(t *testing.T) {
	store := &curtailStore{identities: map[string]string{"pv": "hardware-1"}, block: make(chan struct{})}
	f := newTestForecastCurtailment(&stubSender{}, store)
	defer f.Close()
	defer close(store.block)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Send(ctx, "pv", []byte(`{"action":"curtail","power_w":2000}`)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("persistence blocked command return")
	}
	if !f.Active() {
		t.Fatal("slow persistence delayed live censoring")
	}
}

func TestForecastCurtailmentPendingDriversRecoverAndRetry(t *testing.T) {
	store := &curtailStore{identities: map[string]string{"renamed": "hardware-1", "unrelated": "hardware-2"}, value: `{"version":1,"device_ids":["hardware-1","absent-hardware"],"unknown":true}`}
	fail := true
	sender := &stubSender{handler: func(context.Context, string) error {
		if fail {
			return errors.New("release failed")
		}
		return nil
	}}
	f := newTestForecastCurtailment(sender, store)
	defer f.Close()
	names := []string{"unrelated", "renamed", "renamed", "missing"}
	if got := f.PendingDrivers(names); !reflect.DeepEqual(got, []string{"renamed"}) {
		t.Fatalf("restart identity mapping: %v", got)
	}
	f.Send(context.Background(), "renamed", []byte(`{"action":"curtail_disable"}`))
	if got := f.PendingDrivers(names); !reflect.DeepEqual(got, []string{"renamed"}) {
		t.Fatalf("failed release must remain eligible for Core retry: %v", got)
	}
	fail = false
	f.Send(context.Background(), "renamed", []byte(`{"action":"curtail_disable"}`))
	if got := f.PendingDrivers(names); len(got) != 0 {
		t.Fatalf("acknowledged release still pending: %v", got)
	}
	if !f.Active() {
		t.Fatal("release of one known device cleared unknown/absent identity")
	}
	if len(sender.recorded()) != 2 {
		t.Fatal("identity recovery sent commands")
	}
}

func newTestForecastCurtailment(sender driverCommandSender, store forecastCurtailmentStore) *forecastCurtailment {
	f := newForecastCurtailment(sender, store)
	f.SetReleaseEvidence(func(string) bool { return true })
	return f
}

func TestForecastCurtailmentReleaseRequiresEvidence(t *testing.T) {
	store := &curtailStore{identities: map[string]string{"pv": "hardware-1"}}
	f := newForecastCurtailment(&stubSender{}, store)
	defer f.Close()
	f.Send(context.Background(), "pv", []byte(`{"action":"curtail","power_w":2000}`))
	f.Send(context.Background(), "pv", []byte(`{"action":"curtail_disable"}`))
	if !f.Active() || len(f.PendingDrivers([]string{"pv"})) != 1 {
		t.Fatal("nil evidence trusted a successful no-op release")
	}
	f.SetReleaseEvidence(func(string) bool { return false })
	f.Send(context.Background(), "pv", []byte(`{"action":"curtail_disable"}`))
	if !f.Active() {
		t.Fatal("unverified driver cleared censorship")
	}
	f.SetReleaseEvidence(func(name string) bool { return name == "pv" })
	f.Send(context.Background(), "pv", []byte(`{"action":"curtail_disable"}`))
	waitCurtailment(t, f, false)
}
