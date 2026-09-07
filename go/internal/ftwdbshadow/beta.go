package ftwdbshadow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	betaBatchTicks    = 128
	betaInterval      = 30 * time.Second
	betaMaxStoreBytes = 512 * 1024 * 1024
)

// Beta copies numeric site history from successful live SQLite ticks. Each
// process has a new source ID: this is a measured session, never a full replica.
// The memory queue may lose work on overload or restart; SQLite keeps the data.
type Beta struct {
	feed   *state.HistoryFeed
	mu     sync.Mutex
	status BetaStatus
	cancel context.CancelFunc
	done   chan struct{}
}

type BetaStatus struct {
	Enabled   bool      `json:"enabled"`
	State     string    `json:"state"`
	Scope     string    `json:"scope"`
	Session   string    `json:"session,omitempty"`
	StartedAt time.Time `json:"started_at"`
	state.HistoryFeedStats
	Pending          int        `json:"pending_ticks"`
	Acknowledged     uint64     `json:"acknowledged_ticks"`
	DurableThrough   uint64     `json:"durable_through_sequence"`
	LastAckAt        *time.Time `json:"last_ack_at,omitempty"`
	Errors           uint64     `json:"errors"`
	LastError        string     `json:"last_error,omitempty"`
	Sidecar          *HealthOps `json:"sidecar,omitempty"`
	SidecarCheckedAt *time.Time `json:"sidecar_checked_at,omitempty"`
	LastAckMS        float64    `json:"last_ack_ms"`
	MaxAckMS         float64    `json:"max_ack_ms"`
}

// Start never dials or writes to the sidecar in the caller. An empty socket
// disables the candidate. A missing sidecar cannot delay Core startup.
func Start(ctx context.Context, st *state.Store, socket, siteID, version string) *Beta {
	b := &Beta{status: BetaStatus{Enabled: socket != "", State: "disabled", Scope: "live_site_history_session", StartedAt: time.Now()}, done: make(chan struct{})}
	if socket == "" {
		close(b.done)
		return b
	}
	var source ID128
	if _, err := rand.Read(source[:]); err != nil || siteID == "" || st == nil {
		b.status.State = "unavailable"
		b.status.LastError = "shadow session identity unavailable"
		close(b.done)
		return b
	}
	b.status.Session = source.String()
	b.status.State = "waiting"
	b.feed = st.ObserveLiveHistory()
	ctx, b.cancel = context.WithCancel(ctx)
	go func() {
		defer close(b.done)
		b.run(ctx, ClientConfig{SocketPath: socket, SourceID: source, NodeID: "ftw-shadow-beta", ClientVersion: version, IOTimeout: 2 * time.Second}, siteID, betaInterval)
	}()
	return b
}

func (b *Beta) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	<-b.done
}

func (b *Beta) Status() BetaStatus {
	b.mu.Lock()
	s := b.status
	b.mu.Unlock()
	if b.feed != nil {
		s.HistoryFeedStats = b.feed.Stats()
	}
	if s.Dropped > 0 && s.State == "ok" {
		s.State = "gaps"
	}
	return s
}

func (b *Beta) failure(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	b.mu.Lock()
	changed := b.status.LastError != err.Error()
	b.status.Errors++
	b.status.State = "degraded"
	b.status.LastError = err.Error()
	b.mu.Unlock()
	if changed {
		slog.Warn("FTWDB shadow copy paused", "err", err)
	}
}

func (b *Beta) run(ctx context.Context, config ClientConfig, siteID string, interval time.Duration) {
	timer := time.NewTicker(interval)
	defer timer.Stop()
	var client *Client
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	var pending *PreparedCommit
	pendingTicks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if pending == nil {
			ticks := make([]state.CommittedHistory, 0, betaBatchTicks)
		drain:
			for len(ticks) < betaBatchTicks {
				select {
				case tick := <-b.feed.Events():
					ticks = append(ticks, tick)
				default:
					break drain
				}
			}
			if len(ticks) == 0 {
				continue
			}
			prepared, err := prepareHistory(config.SourceID, siteID, ticks)
			if err != nil {
				b.feed.MarkDropped(uint64(len(ticks)))
				b.failure(err)
				continue
			}
			pending = &prepared
			pendingTicks = len(ticks)
			b.mu.Lock()
			b.status.Pending = pendingTicks
			b.mu.Unlock()
		}
		if client == nil {
			var err error
			client, _, err = Connect(ctx, config)
			if err != nil {
				b.failure(err)
				continue
			}
		}
		health, err := client.Health(ctx, pending.Sequence())
		if err == nil {
			checkedAt := time.Now()
			b.mu.Lock()
			b.status.Sidecar = health.Ops
			b.status.SidecarCheckedAt = &checkedAt
			b.mu.Unlock()
			switch {
			case watermarkAtLeast(health.DurableThroughSequence, pending.Sequence()):
				// A lost acknowledgement can cross the store limit. Ask for the
				// existing receipt without adding data, even while writes are paused.
			case health.Ops == nil || health.Ops.SyncPolicy != 1:
				err = errors.New("shadow beta requires ops health and always-sync durability")
			case health.Ops.DatabaseBytes >= betaMaxStoreBytes:
				err = errors.New("shadow store reached the 512 MiB beta limit; stop and archive the candidate")
			case health.Status == HealthUnavailable:
				err = errors.New("shadow writer is unavailable")
			}
		}
		if err != nil {
			_ = client.Close()
			client = nil
			b.failure(err)
			continue
		}
		started := time.Now()
		ack, err := client.CommitDurable(ctx, *pending)
		if err != nil {
			_ = client.Close()
			client = nil
			b.failure(err)
			continue
		}
		now := time.Now()
		b.mu.Lock()
		b.status.State = "ok"
		b.status.LastError = ""
		b.status.LastAckAt = &now
		b.status.DurableThrough = ack.DurableThrough
		b.status.Acknowledged += uint64(pendingTicks)
		b.status.LastAckMS = float64(time.Since(started)) / float64(time.Millisecond)
		b.status.MaxAckMS = max(b.status.MaxAckMS, b.status.LastAckMS)
		b.status.Pending = 0
		b.mu.Unlock()
		pending = nil
		// The sidecar's idle deadline is shorter than the batch interval.
		// Start the next batch with a new connection and HELLO.
		_ = client.Close()
		client = nil
	}
}

func historyID(parts ...string) ID128 {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(part), part)
	}
	var id ID128
	copy(id[:], hash.Sum(nil))
	return id
}

func prepareHistory(source ID128, siteID string, ticks []state.CommittedHistory) (PreparedCommit, error) {
	if len(ticks) == 0 || len(ticks) > betaBatchTicks {
		return PreparedCommit{}, errors.New("invalid history batch size")
	}
	last := ticks[len(ticks)-1]
	owner := historyID("ftw-site-history-v1", siteID)
	commit := historyID("ftw-history-commit-v1", source.String(), fmt.Sprint(last.Sequence))
	batch := CommitBatchRequest{SourceID: source, Sequence: last.Sequence, CommitID: commit}
	batch.Entities = []Entity{{ID: owner, Kind: "site", Name: "FTW site", Properties: map[string]PropertyValue{"power_sign": TextProperty("positive_into_site"), "scope": TextProperty("live_site_history_session")}}}
	batch.Runs = []Run{{ID: commit, Kind: RunImport, Status: RunSucceeded, CreatedAt: last.CommittedAtMicros, KnowledgeTime: last.CommittedAtMicros, Workflow: "ftw.sqlite.live_history", ModelVersion: "1", Attributes: map[string]PropertyValue{"session": TextProperty(source.String()), "first_sequence": IntegerProperty(int64(ticks[0].Sequence)), "last_sequence": IntegerProperty(int64(last.Sequence)), "ticks": IntegerProperty(int64(len(ticks)))}}}
	names := []string{"grid_power", "pv_power", "battery_power", "house_load_power", "battery_soc"}
	series := make([]uint64, len(names))
	for i, name := range names {
		id := historyID("ftw-site-series-v1", siteID, name)
		series[i] = binary.BigEndian.Uint64(id[:8])
		unit, quantity := "W", "power"
		if name == "battery_soc" {
			unit, quantity = "1", "state_of_charge"
		}
		gap := int64(15_000_000)
		batch.Series = append(batch.Series, SeriesDefinition{ID: series[i], OwnerEntity: &owner, Name: name, PhysicalQuantity: quantity, CanonicalUnit: unit, Semantics: SeriesGauge, MaximumGapMicros: &gap})
	}
	var previous uint64
	for _, tick := range ticks {
		p := tick.Point
		if tick.Sequence <= previous || p.TsMs < 0 || p.TsMs > math.MaxInt64/1000 {
			return PreparedCommit{}, errors.New("invalid history sequence or timestamp")
		}
		previous = tick.Sequence
		for i, value := range []float64{p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC} {
			batch.Points = append(batch.Points, Point{SeriesID: series[i], ValidTime: p.TsMs * 1000, ValidTimeEnd: p.TsMs * 1000, KnowledgeTime: tick.CommittedAtMicros, ChangeTime: tick.CommittedAtMicros, RunID: commit, Value: value})
		}
	}
	return PrepareCommit(batch)
}
