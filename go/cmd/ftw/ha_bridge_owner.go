package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/ha"
)

// haBridgeHandle is the part of *ha.Bridge the owner drives. Tests use a fake.
type haBridgeHandle interface {
	Reload(cfg *config.HomeAssistant, driverNames []string) error
	Stop()
}

const (
	haRetryMinDelay = 5 * time.Second
	haRetryMaxDelay = 60 * time.Second
)

// haBridgeOwner owns the Home Assistant bridge for the life of the process.
//
// ha.Start fails when the broker is not up yet (FTW often boots first after a
// power cut) or refuses the login for now. The owner then keeps trying with
// a capped backoff until a start succeeds, the config changes, or Stop runs.
// A bridge from a retry gets the same Reload and Stop handling as one started
// at once.
type haBridgeOwner struct {
	start    func(cfg *config.HomeAssistant, driverNames []string) (haBridgeHandle, error)
	names    func() []string
	minDelay time.Duration
	maxDelay time.Duration

	// lifecycleMu serializes Apply and Stop. They wait for a retry to exit
	// without holding mu, which the retry takes to hand over its bridge.
	lifecycleMu sync.Mutex
	retryCancel chan struct{}
	retryDone   chan struct{}
	stopped     bool

	mu     sync.Mutex
	bridge haBridgeHandle
}

func newHABridgeOwner(start func(*config.HomeAssistant, []string) (haBridgeHandle, error), names func() []string) *haBridgeOwner {
	return &haBridgeOwner{start: start, names: names, minDelay: haRetryMinDelay, maxDelay: haRetryMaxDelay}
}

// Bridge returns the running bridge, or nil while none is connected.
func (o *haBridgeOwner) Bridge() *ha.Bridge {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	b, _ := o.bridge.(*ha.Bridge)
	return b
}

// Apply brings the bridge in line with cfg. A pending retry always ends
// first, since it was trying the old settings.
func (o *haBridgeOwner) Apply(cfg *config.HomeAssistant) {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	if o.stopped {
		return
	}
	o.cancelRetry()

	o.mu.Lock()
	bridge := o.bridge
	o.mu.Unlock()

	enabled := cfg != nil && cfg.Enabled
	switch {
	case bridge != nil && !enabled:
		o.setBridge(nil)
		bridge.Stop()
		slog.Info("HA bridge stopped (disabled in config)")
	case bridge != nil:
		if err := bridge.Reload(cfg, o.names()); err != nil {
			// Reload leaves the bridge without a client. Drop it and retry
			// as a fresh start, so the bridge comes back when the broker does.
			o.setBridge(nil)
			bridge.Stop()
			o.startRetry(cfg, err)
			return
		}
		slog.Info("HA bridge reloaded", "broker", cfg.Broker)
	case enabled:
		b, err := o.start(cfg, o.names())
		if err != nil {
			o.startRetry(cfg, err)
			return
		}
		o.setBridge(b)
		slog.Info("HA bridge started", "broker", cfg.Broker)
	}
}

// Stop ends any retry and stops the bridge. Apply does nothing afterwards.
func (o *haBridgeOwner) Stop() {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	if o.stopped {
		return
	}
	o.stopped = true
	o.cancelRetry()
	o.mu.Lock()
	bridge := o.bridge
	o.bridge = nil
	o.mu.Unlock()
	if bridge != nil {
		bridge.Stop()
	}
}

func (o *haBridgeOwner) setBridge(b haBridgeHandle) {
	o.mu.Lock()
	o.bridge = b
	o.mu.Unlock()
}

// cancelRetry ends a pending retry and waits for it to exit. A start in
// flight finishes first (ha.Start gives up after its connect timeout), and a
// bridge it produced is stopped rather than handed over. Caller holds
// lifecycleMu.
func (o *haBridgeOwner) cancelRetry() {
	if o.retryCancel == nil {
		return
	}
	close(o.retryCancel)
	<-o.retryDone
	o.retryCancel, o.retryDone = nil, nil
}

// startRetry runs retry in the background. Caller holds lifecycleMu.
func (o *haBridgeOwner) startRetry(cfg *config.HomeAssistant, firstErr error) {
	slog.Warn("HA bridge start failed; retrying until the broker accepts",
		"broker", cfg.Broker, "err", firstErr)
	o.retryCancel = make(chan struct{})
	o.retryDone = make(chan struct{})
	go o.retry(cfg, firstErr, o.retryCancel, o.retryDone)
}

func (o *haBridgeOwner) retry(cfg *config.HomeAssistant, lastErr error, cancel, done chan struct{}) {
	defer close(done)
	delay := o.minDelay
	for attempt := 2; ; attempt++ {
		timer := time.NewTimer(delay)
		select {
		case <-cancel:
			timer.Stop()
			slog.Info("HA bridge retry cancelled", "broker", cfg.Broker)
			return
		case <-timer.C:
		}

		b, err := o.start(cfg, o.names())
		if err != nil {
			// Log only when the reason changes, e.g. from "connection
			// refused" to "not authorised", not on every attempt.
			if err.Error() != lastErr.Error() {
				slog.Warn("HA bridge still not connected", "broker", cfg.Broker, "attempt", attempt, "err", err)
			}
			lastErr = err
			delay = min(delay*2, o.maxDelay)
			continue
		}

		o.mu.Lock()
		select {
		case <-cancel:
			o.mu.Unlock()
			b.Stop()
			slog.Info("HA bridge retry cancelled", "broker", cfg.Broker)
			return
		default:
		}
		o.bridge = b
		o.mu.Unlock()
		slog.Info("HA bridge started after retry", "broker", cfg.Broker, "attempt", attempt)
		return
	}
}
