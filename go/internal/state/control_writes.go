package state

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"
)

const controlWriteLimit = 128
const controlWriteBytes = 8 << 20
const controlCacheKeys = 4096

// ErrWritePending means accepted in memory, not committed to disk. Callers
// must not report durable storage until SaveConfig returns nil or Flush ends.
var ErrWritePending error = pendingControlWrite{}

type pendingControlWrite struct{}

func (pendingControlWrite) Error() string { return "state write pending" }
func (pendingControlWrite) Pending() bool { return true }

type controlWrite struct {
	first, seq uint64
	values     map[string]string
	active     map[string]string
	err        error
}

// ControlWrites keeps disk work outside control locks. Each group has one
// pending replacement; an in-flight write is immutable. Reads return only
// committed values and hide keys being replaced, including unplug tombstones.
// Flush is for API acknowledgements and shutdown, never for a control tick.
type ControlWrites struct {
	mu       sync.Mutex
	cache    map[string]string
	jobs     map[string]*controlWrite
	rejected map[string]error
	overflow error
	order    []string
	seq      uint64
	changed  chan struct{}
	wake     chan struct{}
	done     chan struct{}
	closed   bool
	write    func(map[string]string) error
}

func newControlWrites(initial map[string]string, write func(map[string]string) error) *ControlWrites {
	w := &ControlWrites{cache: maps.Clone(initial), jobs: map[string]*controlWrite{}, rejected: map[string]error{}, changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), write: write}
	if w.cache == nil {
		w.cache = map[string]string{}
	}
	go w.run()
	return w
}

// NewEVControlWrites loads restart state before the control loop starts.
// All later access to these keys must go through this instance.
func (s *Store) NewEVControlWrites() (*ControlWrites, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config WHERE key GLOB 'ev_*' OR key GLOB 'loadpoint_manual_hold:*' OR key GLOB 'loadpoint_battery_boost:*'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	initial := map[string]string{}
	bytes := 0
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		initial[key] = value
		bytes += len(key) + len(value)
		if len(initial) > controlCacheKeys || bytes > controlWriteBytes {
			return nil, errors.New("EV restart state exceeds control cache limit")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return newControlWrites(initial, s.SaveConfigValues), nil
}

func (s *Store) NewBatteryModelWrites() *ControlWrites {
	return newControlWrites(nil, func(values map[string]string) error {
		for name, value := range values {
			if err := s.SaveBatteryModel(name, value); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *ControlWrites) signalLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *ControlWrites) SaveConfig(key, value string) error {
	return w.SaveConfigGroup(key, map[string]string{key: value})
}

// SaveConfigGroup replaces one atomic intent, such as all of a manual hold's
// identity and fallback keys. Retry the same group, not its individual keys.
func (w *ControlWrites) SaveConfigGroup(group string, values map[string]string) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() {
		if err != nil && !errors.Is(err, ErrWritePending) {
			if _, exists := w.rejected[group]; exists || len(w.rejected) < controlWriteLimit {
				w.rejected[group] = err
			} else {
				w.overflow = errors.New("too many rejected control writes")
			}
		} else {
			delete(w.rejected, group)
		}
	}()
	if w.closed {
		return errors.New("control persistence closed")
	}
	if len(values) == 0 {
		return nil
	}
	job := w.jobs[group]
	if job != nil && maps.Equal(job.values, values) {
		if job.err != nil {
			return job.err
		}
		return ErrWritePending
	}
	for other, pending := range w.jobs {
		if other == group {
			continue
		}
		for key := range values {
			_, queued := pending.values[key]
			_, active := pending.active[key]
			if queued || active {
				return fmt.Errorf("control persistence groups overlap at %s", key)
			}
		}
	}
	if job == nil {
		same := true
		for key, value := range values {
			if old, ok := w.cache[key]; !ok || old != value {
				same = false
			}
		}
		if same {
			return nil
		}
	}
	if job == nil && len(w.jobs) >= controlWriteLimit {
		return errors.New("control persistence queue full")
	}
	bytes, keys := 0, len(w.cache)+len(values)
	for key, value := range w.cache {
		bytes += len(key) + len(value)
	}
	for key, value := range values {
		bytes += len(key) + len(value)
	}
	for other, pending := range w.jobs {
		keys += len(pending.active)
		for key, value := range pending.active {
			bytes += len(key) + len(value)
		}
		if other == group {
			continue
		}
		keys += len(pending.values)
		for key, value := range pending.values {
			bytes += len(key) + len(value)
		}
	}
	if bytes > controlWriteBytes || keys > controlCacheKeys {
		return errors.New("control persistence buffer full")
	}
	w.seq++
	if job == nil {
		job = &controlWrite{first: w.seq}
		w.jobs[group] = job
		w.order = append(w.order, group)
	}
	job.seq, job.values, job.err = w.seq, maps.Clone(values), nil
	w.signalLocked()
	return ErrWritePending
}

func (w *ControlWrites) LoadConfig(key string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, job := range w.jobs {
		if _, pending := job.values[key]; pending {
			return "", false
		}
		if _, pending := job.active[key]; pending {
			return "", false
		}
	}
	value, ok := w.cache[key]
	return value, ok
}

// GroupStatus reads only memory. A pending caller can use it to expose the
// actual commit result without waiting for another control tick.
func (w *ControlWrites) GroupStatus(group string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rejected[group]; err != nil {
		return err
	}
	if job := w.jobs[group]; job != nil {
		if job.err != nil {
			return job.err
		}
		return ErrWritePending
	}
	return nil
}

func (w *ControlWrites) run() {
	defer close(w.done)
	for {
		w.mu.Lock()
		if len(w.order) == 0 {
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			<-w.wake
			continue
		}
		group := w.order[0]
		w.order = w.order[1:]
		job := w.jobs[group]
		seq, values := job.seq, job.values
		job.active = values
		w.mu.Unlock()
		err := w.write(values)
		w.mu.Lock()
		job.active = nil
		if err == nil {
			delete(w.rejected, group)
			maps.Copy(w.cache, values)
			if job.seq == seq {
				delete(w.jobs, group)
			} else {
				job.first = seq + 1
				w.order = append(w.order, group)
			}
		} else {
			job.err = err
			w.order = append(w.order, group)
		}
		w.signalLocked()
		closed := w.closed
		w.mu.Unlock()
		if err != nil {
			if closed {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// Flush waits for requests accepted before this call. Later unrelated work
// cannot extend the wait; a superseding value satisfies the older request.
func (w *ControlWrites) Flush(ctx context.Context) error {
	w.mu.Lock()
	if w.overflow != nil {
		err := w.overflow
		w.mu.Unlock()
		return err
	}
	for _, err := range w.rejected {
		w.mu.Unlock()
		return err
	}
	want := make(map[string]uint64, len(w.jobs))
	for group, job := range w.jobs {
		want[group] = job.seq
	}
	for {
		pending := false
		for group, seq := range want {
			if job := w.jobs[group]; job != nil && job.first <= seq {
				if job.err != nil {
					w.mu.Unlock()
					return job.err
				}
				pending = true
			}
		}
		if !pending {
			w.mu.Unlock()
			return nil
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		w.mu.Lock()
	}
}

func (w *ControlWrites) Close(ctx context.Context) error {
	w.mu.Lock()
	w.closed = true
	w.signalLocked()
	w.mu.Unlock()
	if err := w.Flush(ctx); err != nil {
		return err
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
