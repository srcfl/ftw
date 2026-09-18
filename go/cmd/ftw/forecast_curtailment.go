package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/srcfl/ftw/go/internal/state"
)

const forecastCurtailmentKey = "forecast/pv-curtailment-v1"

type forecastCurtailmentStore interface {
	LoadConfig(string) (string, bool)
	SaveConfig(string, string) error
	LookupDeviceByDriverName(string) *state.Device
}

type forecastCurtailmentState struct {
	Version   int      `json:"version"`
	DeviceIDs []string `json:"device_ids"`
	Unknown   bool     `json:"unknown"`
	Intent    bool     `json:"intent"`
}

type curtailmentEntry struct {
	active   bool
	revision uint64
}

// forecastCurtailment only labels observations. It never sends an extra
// command, changes a payload or changes the sender's result. Active is safe
// under ctrlMu: it reads one atomic flag and never calls back into control.
//
// One background worker resolves stable identities and coalesces persistence.
// SQLite cannot extend a control command's deadline. There is a crash window
// before a queued transition reaches disk; this is not durable-before-command
// tracking. Close flushes the final state after dispatch has stopped.
type forecastCurtailment struct {
	sender          driverCommandSender
	store           forecastCurtailmentStore
	active          atomic.Bool
	mu              sync.Mutex
	intent, unknown bool
	ids             map[string]bool
	entries         map[string]curtailmentEntry // runtime names only, never persisted
	revision        uint64
	wake            chan struct{}
	stop            chan struct{}
	done            chan struct{}
	closeOnce       sync.Once
	releaseEvidence func(string) bool
}

func newForecastCurtailment(sender driverCommandSender, store forecastCurtailmentStore) *forecastCurtailment {
	f := &forecastCurtailment{sender: sender, store: store, ids: map[string]bool{}, entries: map[string]curtailmentEntry{}, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	if store != nil {
		if raw, ok := store.LoadConfig(forecastCurtailmentKey); ok && raw != "" {
			var saved forecastCurtailmentState
			if json.Unmarshal([]byte(raw), &saved) != nil || saved.Version != 1 {
				f.unknown = true
			} else {
				f.unknown, f.intent = saved.Unknown, saved.Intent
				for _, id := range saved.DeviceIDs {
					if id != "" {
						f.ids[id] = true
					}
				}
			}
		}
	}
	f.publishLocked()
	go f.run()
	return f
}

func (f *forecastCurtailment) Active() bool { return f != nil && f.active.Load() }

// SetReleaseEvidence installs an immutable predicate for drivers whose release
// command attempts to remove the PV limit and reports failures. A nil predicate
// cannot confirm release: some drivers return success for unsupported no-ops.
func (f *forecastCurtailment) SetReleaseEvidence(evidence func(string) bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.releaseEvidence = evidence
	f.mu.Unlock()
}

// PendingDrivers maps pending hardware identities to current runtime names.
// Call outside control locks: identity lookup may read SQLite. The caller may
// restore these names into Core's normal curtailment state, which still decides
// whether fresh telemetry and current intent permit release. Unknown identities
// remain censored and never map to an arbitrary driver.
func (f *forecastCurtailment) PendingDrivers(currentNames []string) []string {
	if f == nil {
		return nil
	}
	identities := make(map[string]string, len(currentNames))
	for _, name := range currentNames {
		if f.store != nil {
			if device := f.store.LookupDeviceByDriverName(name); device != nil {
				identities[name] = device.DeviceID
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[string]bool, len(currentNames))
	var pending []string
	for _, name := range currentNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		entry, exists := f.entries[name]
		active := f.ids[identities[name]]
		if exists {
			// A recent successful release overrides the older persisted ID
			// while the background worker saves that transition.
			active = entry.active
		}
		if active {
			pending = append(pending, name)
		}
	}
	sort.Strings(pending)
	return pending
}

func (f *forecastCurtailment) publishLocked() {
	active := f.intent || f.unknown || len(f.ids) > 0
	for _, entry := range f.entries {
		active = active || entry.active
	}
	f.active.Store(active)
}

func (f *forecastCurtailment) notify() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// ObserveIntent covers active manual/planner holds, including zero targets
// for which Core may suppress a command. Clearing intent cannot clear an
// unacknowledged release or an unresolved hardware identity.
func (f *forecastCurtailment) ObserveIntent(active bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	changed := f.intent != active
	f.intent = active
	f.publishLocked()
	f.mu.Unlock()
	if changed {
		f.notify()
	}
}

func (f *forecastCurtailment) record(name string, active bool) {
	f.mu.Lock()
	f.revision++
	f.entries[name] = curtailmentEntry{active, f.revision}
	f.publishLocked()
	f.mu.Unlock()
	f.notify()
}

func (f *forecastCurtailment) Send(ctx context.Context, name string, payload []byte) error {
	var command struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(payload, &command)
	f.mu.Lock()
	evidence := f.releaseEvidence
	f.mu.Unlock()
	if command.Action == "curtail" {
		f.record(name, true)
	}
	err := f.sender.Send(ctx, name, payload)
	if command.Action == "curtail_disable" && err == nil && evidence != nil && evidence(name) {
		f.record(name, false)
	}
	return err
}

// RecordDefault may be called only when that driver's confirmed default
// restores unrestricted PV. Driver startup alone is not confirmation.
func (f *forecastCurtailment) RecordDefault(name string, success bool) {
	if f == nil || !success {
		return
	}
	f.record(name, false)
}

// ConfirmUncurtailed clears unknown identities only after the caller has
// authoritative confirmation that all PV devices are unrestricted.
func (f *forecastCurtailment) ConfirmUncurtailed() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.unknown = false
	f.ids = map[string]bool{}
	f.entries = map[string]curtailmentEntry{}
	f.publishLocked()
	f.mu.Unlock()
	f.notify()
}

func (f *forecastCurtailment) run() {
	defer close(f.done)
	for {
		select {
		case <-f.wake:
			f.persist()
		case <-f.stop:
			f.persist()
			return
		}
	}
}

func (f *forecastCurtailment) persist() {
	f.mu.Lock()
	entries := make(map[string]curtailmentEntry, len(f.entries))
	for name, e := range f.entries {
		entries[name] = e
	}
	f.mu.Unlock()
	for name, entry := range entries {
		var device *state.Device
		if f.store != nil {
			device = f.store.LookupDeviceByDriverName(name)
		}
		f.mu.Lock()
		if current, ok := f.entries[name]; ok && current.revision == entry.revision {
			if device != nil && device.DeviceID != "" {
				if entry.active {
					f.ids[device.DeviceID] = true
				} else {
					delete(f.ids, device.DeviceID)
				}
				delete(f.entries, name)
			} else if !entry.active {
				delete(f.entries, name)
			}
		}
		f.mu.Unlock()
	}
	f.mu.Lock()
	saved := forecastCurtailmentState{Version: 1, Unknown: f.unknown, Intent: f.intent}
	for id := range f.ids {
		saved.DeviceIDs = append(saved.DeviceIDs, id)
	}
	for _, entry := range f.entries {
		saved.Unknown = saved.Unknown || entry.active
	}
	f.publishLocked()
	f.mu.Unlock()
	sort.Strings(saved.DeviceIDs)
	if f.store == nil {
		return
	}
	encoded, err := json.Marshal(saved)
	if err == nil {
		err = f.store.SaveConfig(forecastCurtailmentKey, string(encoded))
	}
	if err != nil {
		slog.Warn("PV forecast curtailment state not persisted", "err", err)
	}
}

func (f *forecastCurtailment) Close() {
	if f == nil {
		return
	}
	f.closeOnce.Do(func() { close(f.stop) })
	<-f.done
}
