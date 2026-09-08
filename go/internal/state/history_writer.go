package state

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	historyQueueTicks = 64
	historyQueueBytes = 16 << 20
	historyBatchBytes = 1 << 20
)

type historyPayload struct {
	Point        *HistoryPoint
	Samples      []Sample
	Observations []EnergyObservation
}

type historyBatch struct {
	id, hash string
	payload  historyPayload
	bytes    int
}

// HistoryWriterStatus distinguishes volatile admission from a durable commit.
// A full queue rejects a tick explicitly; it never acknowledges it as saved.
type HistoryWriterStatus struct {
	Accepted          uint64 `json:"accepted_ticks"`
	Committed         uint64 `json:"committed_ticks"`
	Rejected          uint64 `json:"rejected_ticks"`
	Pending           int    `json:"pending_ticks"`
	PendingBytes      int    `json:"pending_bytes"`
	Sequence          int64  `json:"commit_sequence"`
	LastCommitMS      int64  `json:"last_commit_ms"`
	LastMeasurementMS int64  `json:"last_measurement_ms"`
	LastError         string `json:"last_error,omitempty"`
	LastRejectMS      int64  `json:"last_reject_ms,omitempty"`
	LastRejectError   string `json:"last_reject_error,omitempty"`
	Stopping          bool   `json:"stopping"`
}

type historyWriter struct {
	store   *Store
	mu      sync.Mutex
	status  HistoryWriterStatus
	queue   chan historyBatch
	changed chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

func newHistoryWriter(s *Store) *historyWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &historyWriter{store: s, queue: make(chan historyBatch, historyQueueTicks), changed: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	go w.run()
	return w
}

// EnqueueTelemetryTick copies a whole tick without waiting on disk. The caller
// must report an error as a collection gap. Successful admission is volatile
// until HistoryWriterStatus reports the commit; FlushHistory waits for that.
func (s *Store) EnqueueTelemetryTick(p *HistoryPoint, samples []Sample, observations []EnergyObservation) error {
	w := s.historyWriter
	if w == nil {
		return errors.New("history writer is unavailable")
	}
	if err := validateHistorySamples(samples); err != nil {
		return w.reject(err.Error())
	}
	for _, o := range observations {
		if err := validateEnergyObservation(o); err != nil {
			return w.reject(err.Error())
		}
	}
	// Bound serialization before allocating the copy, including variable strings.
	size := 128
	if p != nil {
		size += len(p.JSON) + 128
	}
	for _, sm := range samples {
		size += 128 + len(sm.Driver) + len(sm.Metric) + len(sm.Unit)
	}
	for _, o := range observations {
		size += 256 + len(o.AssetID) + len(o.DeviceID) + len(o.Label) + len(o.AssetKind) + len(o.Flow)
	}
	if size > historyBatchBytes {
		return w.reject("history tick exceeds the buffer limit")
	}
	encoded, err := json.Marshal(historyPayload{p, samples, observations})
	if err != nil {
		return w.reject("invalid history tick: " + err.Error())
	}
	if len(encoded) > historyBatchBytes {
		return w.reject("history tick exceeds the buffer limit")
	}
	var payload historyPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return w.reject(err.Error())
	}
	b := historyBatch{id: uuid.NewString(), hash: fmt.Sprintf("%x", sha256.Sum256(encoded)), payload: payload, bytes: max(size, len(encoded))}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.Stopping || w.status.Pending >= historyQueueTicks || w.status.PendingBytes+b.bytes > historyQueueBytes {
		w.status.Rejected++
		w.status.LastRejectMS = time.Now().UnixMilli()
		w.status.LastRejectError = "history queue is full or stopping; tick was not accepted"
		w.signal()
		return errors.New("history queue is full or stopping; tick was not accepted")
	}
	w.status.Accepted++
	w.status.Pending++
	w.status.PendingBytes += b.bytes
	w.queue <- b // capacity was reserved above; never waits on the writer
	w.signal()
	return nil
}

func (w *historyWriter) reject(message string) error {
	w.mu.Lock()
	w.status.Rejected++
	w.status.LastRejectMS = time.Now().UnixMilli()
	w.status.LastRejectError = message
	w.signal()
	w.mu.Unlock()
	return errors.New(message)
}

func (w *historyWriter) signal() { close(w.changed); w.changed = make(chan struct{}) }

func (w *historyWriter) run() {
	defer close(w.done)
	var acknowledgedSequence int64
	for b := range w.queue {
		for {
			if w.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
			seq, err := w.store.recordHistoryBatch(ctx, b.id, b.hash, b.payload.Point, b.payload.Samples, b.payload.Observations, acknowledgedSequence)
			cancel()
			w.mu.Lock()
			if err == nil {
				w.status.Committed++
				w.status.Pending--
				w.status.PendingBytes -= b.bytes
				w.status.Sequence = seq
				w.status.LastCommitMS = time.Now().UnixMilli()
				if b.payload.Point != nil {
					w.status.LastMeasurementMS = max(w.status.LastMeasurementMS, b.payload.Point.TsMs)
				}
				for _, sm := range b.payload.Samples {
					w.status.LastMeasurementMS = max(w.status.LastMeasurementMS, sm.TsMs)
				}
				w.status.LastError = ""
			} else {
				if w.status.LastError != err.Error() {
					slog.Error("history commit failed; retaining tick for retry", "err", err)
				}
				w.status.LastError = err.Error()
			}
			w.signal()
			w.mu.Unlock()
			if err == nil {
				acknowledgedSequence = seq
				break
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-w.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (s *Store) HistoryWriterStatus() HistoryWriterStatus {
	if s.historyWriter == nil {
		return HistoryWriterStatus{}
	}
	w := s.historyWriter
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// FlushHistory waits only for ticks accepted before this call. New ticks may
// continue arriving, so a backup cannot wait forever on an active household.
func (s *Store) FlushHistory(ctx context.Context) error {
	w := s.historyWriter
	if w == nil {
		return nil
	}
	w.mu.Lock()
	target := w.status.Accepted
	for w.status.Committed < target {
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		w.mu.Lock()
	}
	w.mu.Unlock()
	return nil
}

func (w *historyWriter) close() error {
	w.mu.Lock()
	if !w.status.Stopping {
		w.status.Stopping = true
		close(w.queue)
		w.signal()
	}
	w.mu.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		w.cancel()
		<-w.done
	}
	w.cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.Pending != 0 {
		return fmt.Errorf("history shutdown left %d ticks uncommitted: %s", w.status.Pending, w.status.LastError)
	}
	return nil
}
