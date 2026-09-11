package state

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/google/uuid"
)

const (
	historyQueueTicks         = 64
	historyQueueBytes         = 16 << 20
	historyBatchBytes         = 1 << 20
	historyMaintenanceTimeout = 2 * time.Minute
	historyCommitTimeout      = 2 * time.Minute
	historyCommitInterval     = 15 * time.Second
	historyCommitMaxTicks     = 32
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
	MaintenanceError  string `json:"maintenance_error,omitempty"`
	LastMaintenanceMS int64  `json:"last_maintenance_ms,omitempty"`
	MaintenanceRuns   uint64 `json:"maintenance_runs"`
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
	// Owned by run, outside the short status mutex. Tests set limits before
	// sending the first tick; production uses bounded rows, time and retries.
	maintenanceRows       int
	maintenanceRowsLimit  int
	maintenanceDue        time.Time
	maintenanceRetry      time.Time
	maintenanceRetryDelay time.Duration
	maintenanceRunning    atomic.Bool
	maintenanceMu         sync.Mutex
	maintenanceWG         sync.WaitGroup
	commitInterval        time.Duration
	commitTimeout         time.Duration
	commitMaxTicks        int
	commitFn              func(context.Context, []historyBatch, int64) (historyBatchCommit, error)
	flushCh               chan struct{}
	flushTarget           atomic.Uint64
	forceRotate           atomic.Bool
}

func newHistoryWriter(s *Store) *historyWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &historyWriter{store: s, queue: make(chan historyBatch, historyQueueTicks), changed: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel,
		maintenanceRowsLimit: 64 * historyImportRows, maintenanceDue: time.Now().Add(time.Hour), maintenanceRetryDelay: 30 * time.Second,
		commitInterval: historyCommitInterval, commitTimeout: historyCommitTimeout, commitMaxTicks: historyCommitMaxTicks,
		flushCh: make(chan struct{}, 1)}
	go w.run()
	return w
}

func (w *historyWriter) maxTicks() int {
	if w.commitMaxTicks > 0 {
		return w.commitMaxTicks
	}
	return historyCommitMaxTicks
}

func (w *historyWriter) timeout() time.Duration {
	if w.commitTimeout > 0 {
		return w.commitTimeout
	}
	return historyCommitTimeout
}

func (w *historyWriter) commitBatches(ctx context.Context, batches []historyBatch, ack int64) (historyBatchCommit, error) {
	if w.commitFn != nil {
		return w.commitFn(ctx, batches, ack)
	}
	return w.store.recordHistoryBatches(ctx, batches, ack)
}

// historyCommitInterrupted is a deadline or DuckDB interrupt. Retrying the
// same batch under the same budget cannot finish; a smaller prefix can.
func historyCommitInterrupted(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var dbErr *duckdb.Error
	return errors.As(err, &dbErr) && dbErr.Type == duckdb.ErrorTypeInterrupt
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

func (w *historyWriter) requestFlush(target uint64) {
	for {
		old := w.flushTarget.Load()
		if target <= old || w.flushTarget.CompareAndSwap(old, target) {
			break
		}
	}
	select {
	case w.flushCh <- struct{}{}:
	default:
	}
}

func (w *historyWriter) run() {
	defer close(w.done)
	var acknowledgedSequence int64
	var held []historyBatch
	var heldBytes int
	var firstHeld time.Time
	commitHeld := func() bool {
		maxAttempt := w.maxTicks()
		for len(held) > 0 {
			if w.ctx.Err() != nil {
				return false
			}
			n := min(len(held), maxAttempt)
			if n < 1 {
				n = 1
			}
			attempt := held[:n]
			attemptBytes := 0
			for _, b := range attempt {
				attemptBytes += b.bytes
			}
			ctx, cancel := context.WithTimeout(w.ctx, w.timeout())
			out, err := w.commitBatches(ctx, attempt, acknowledgedSequence)
			cancel()
			w.mu.Lock()
			if err == nil {
				w.status.Committed += uint64(out.committed)
				w.status.Pending -= out.committed
				w.status.PendingBytes -= attemptBytes
				if out.seq != 0 {
					w.status.Sequence = out.seq
				}
				w.status.LastCommitMS = time.Now().UnixMilli()
				for _, b := range attempt {
					if b.payload.Point != nil {
						w.status.LastMeasurementMS = max(w.status.LastMeasurementMS, b.payload.Point.TsMs)
					}
					for _, sm := range b.payload.Samples {
						w.status.LastMeasurementMS = max(w.status.LastMeasurementMS, sm.TsMs)
					}
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
				if out.seq != 0 {
					acknowledgedSequence = out.seq
				}
				w.scheduleMaintenance(out.rows)
				held = held[n:]
				heldBytes -= attemptBytes
				if len(held) == 0 {
					firstHeld = time.Time{}
				}
				maxAttempt = w.maxTicks()
				continue
			}
			var dbErr *duckdb.Error
			if errors.As(err, &dbErr) && dbErr.Type == duckdb.ErrorTypeOutOfMemory {
				w.maintenanceDue = time.Time{}
				w.forceRotate.Store(true)
				w.maintainHistory(0)
			}
			if historyCommitInterrupted(err) && n > 1 {
				next := max(1, n/2)
				slog.Warn("history commit interrupted; retrying a smaller batch", "ticks", n, "next", next, "err", err)
				maxAttempt = next
				continue
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-w.ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
		return true
	}
	drainQueue := func() {
		limit := w.maxTicks()
		for len(held) < limit {
			select {
			case b, ok := <-w.queue:
				if !ok {
					return
				}
				held = append(held, b)
				heldBytes += b.bytes
			default:
				return
			}
		}
	}
	shouldCommit := func() bool {
		if len(held) == 0 {
			return false
		}
		if len(held) >= w.maxTicks() {
			return true
		}
		w.mu.Lock()
		committed := w.status.Committed
		w.mu.Unlock()
		if target := w.flushTarget.Load(); target > 0 && committed+uint64(len(held)) >= target {
			return true
		}
		if w.commitInterval <= 0 {
			return true
		}
		return !firstHeld.IsZero() && time.Since(firstHeld) >= w.commitInterval
	}
	for {
		if len(held) == 0 {
			select {
			case b, ok := <-w.queue:
				if !ok {
					return
				}
				held = append(held, b)
				heldBytes += b.bytes
				firstHeld = time.Now()
			case <-w.ctx.Done():
				return
			}
			continue
		}
		if shouldCommit() {
			drainQueue()
			if !commitHeld() {
				return
			}
			continue
		}
		wait := w.commitInterval - time.Since(firstHeld)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case b, ok := <-w.queue:
			timer.Stop()
			if !ok {
				_ = commitHeld()
				return
			}
			held = append(held, b)
			heldBytes += b.bytes
		case <-w.flushCh:
			timer.Stop()
			drainQueue()
			if !commitHeld() {
				return
			}
		case <-timer.C:
			if !commitHeld() {
				return
			}
		case <-w.ctx.Done():
			timer.Stop()
			_ = commitHeld()
			return
		}
	}
}

// Maintenance follows a durable commit or an OOM rollback. It never holds a catalog/write/status
// lock, and admission can continue into the bounded queue. A long read only
// postpones maintenance; its transaction and the new committed data stay intact.
// Hourly rotation runs in the background so a multi-GB reopen cannot stall
// live commits past the site watchdog.
func (w *historyWriter) scheduleMaintenance(rows int) {
	w.maintenanceRows += rows
	now := time.Now()
	if now.Before(w.maintenanceRetry) || (w.maintenanceRows < w.maintenanceRowsLimit && now.Before(w.maintenanceDue)) {
		return
	}
	if !w.maintenanceRunning.CompareAndSwap(false, true) {
		return
	}
	w.maintenanceWG.Add(1)
	go func() {
		defer w.maintenanceWG.Done()
		defer w.maintenanceRunning.Store(false)
		w.runMaintenance()
	}()
}

func (w *historyWriter) maintainHistory(rows int) {
	w.maintenanceRows += rows
	now := time.Now()
	if now.Before(w.maintenanceRetry) || (w.maintenanceRows < w.maintenanceRowsLimit && now.Before(w.maintenanceDue)) {
		return
	}
	w.runMaintenance()
}

func (w *historyWriter) runMaintenance() {
	w.maintenanceMu.Lock()
	defer w.maintenanceMu.Unlock()
	ctx, cancel := context.WithTimeout(w.ctx, historyMaintenanceTimeout)
	err := w.store.CheckpointHistory(ctx)
	if err == nil && (w.forceRotate.Load() || shouldRotateNative()) {
		if rotErr := w.store.RotateHistory(ctx); rotErr != nil {
			err = rotErr
		} else {
			w.forceRotate.Store(false)
		}
	}
	cancel()
	// A successful rotation may still leave the same tick too large. Back off
	// every actual attempt; skipped calls above must not extend this deadline.
	w.maintenanceRetry = time.Now().Add(w.maintenanceRetryDelay)
	if err == nil {
		w.maintenanceRows = 0
		w.maintenanceDue = time.Now().Add(time.Hour)
	} else {
		slog.Warn("history maintenance postponed; committed data retained", "err", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.status.MaintenanceError = ""
		w.status.LastMaintenanceMS = time.Now().UnixMilli()
		w.status.MaintenanceRuns++
	} else {
		w.status.MaintenanceError = "History maintenance could not finish; committed data is retained and maintenance will retry."
	}
	w.signal()
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
	w.mu.Unlock()
	w.requestFlush(target)
	w.mu.Lock()
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
	target := w.status.Accepted
	if !w.status.Stopping {
		w.status.Stopping = true
		close(w.queue)
		w.signal()
	}
	w.mu.Unlock()
	w.requestFlush(target)
	timer := time.NewTimer(w.timeout())
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		w.cancel()
		<-w.done
	}
	w.maintenanceWG.Wait()
	w.cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.Pending != 0 {
		return fmt.Errorf("history shutdown left %d ticks uncommitted: %s", w.status.Pending, w.status.LastError)
	}
	return nil
}
