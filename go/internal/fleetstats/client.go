// Package fleetstats provides privacy-bounded, opt-in FTW installation and
// component-health statistics. It never carries raw energy telemetry or local
// device/network identities.
package fleetstats

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1
const installationIDKey = "fleet_statistics_installation_id"

var ErrDisabled = errors.New("fleet statistics are disabled")

type CoreStats struct {
	Version string `json:"version"`
	Channel string `json:"channel,omitempty"`
}

type OptimizerStats struct {
	Version         string `json:"version,omitempty"`
	Transport       string `json:"transport,omitempty"`
	Status          string `json:"status"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
}

type DriverStats struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Source     string   `json:"source"`
	Status     string   `json:"status"`
	Instances  int      `json:"instances"`
	HostAPIMin int      `json:"host_api_min"`
	HostAPIMax int      `json:"host_api_max"`
	Kinds      []string `json:"kinds,omitempty"`
}

type Payload struct {
	SchemaVersion           int             `json:"schema_version"`
	InstallationID          string          `json:"installation_id"`
	Core                    CoreStats       `json:"core"`
	Optimizer               *OptimizerStats `json:"optimizer,omitempty"`
	Drivers                 []DriverStats   `json:"drivers"`
	SiteMeterHealthy        bool            `json:"site_meter_healthy"`
	UnidentifiedDriverCount int             `json:"unidentified_driver_count,omitempty"`
}

var safeTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}$`)
var installIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (p Payload) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("fleet schema %d, want %d", p.SchemaVersion, SchemaVersion)
	}
	if !installIDRE.MatchString(p.InstallationID) {
		return errors.New("invalid anonymous installation id")
	}
	if !safeFleetToken(p.Core.Version) {
		return errors.New("invalid core version")
	}
	if p.Core.Channel != "" && !safeFleetToken(p.Core.Channel) {
		return errors.New("invalid update channel")
	}
	if len(p.Drivers) > 128 || p.UnidentifiedDriverCount < 0 || p.UnidentifiedDriverCount > 128 {
		return errors.New("driver counts exceed fleet payload bounds")
	}
	if p.Optimizer != nil {
		if p.Optimizer.Version != "" && !safeFleetToken(p.Optimizer.Version) {
			return errors.New("invalid optimizer version")
		}
		if p.Optimizer.Transport != "" && !safeFleetToken(p.Optimizer.Transport) {
			return errors.New("invalid optimizer transport")
		}
		if !validStatus(p.Optimizer.Status) || p.Optimizer.ProtocolVersion < 0 || p.Optimizer.ProtocolVersion > 1000 {
			return errors.New("invalid optimizer status")
		}
	}
	for _, driver := range p.Drivers {
		if !safeFleetToken(driver.ID) || !safeFleetToken(driver.Version) {
			return errors.New("invalid driver id or version")
		}
		if !validSource(driver.Source) || !validStatus(driver.Status) {
			return errors.New("invalid driver source or status")
		}
		if driver.Instances < 1 || driver.Instances > 64 || driver.HostAPIMin < 1 || driver.HostAPIMax < driver.HostAPIMin || driver.HostAPIMax > 1000 {
			return errors.New("invalid driver count or host API range")
		}
		if len(driver.Kinds) > 16 {
			return errors.New("too many driver kinds")
		}
		for _, kind := range driver.Kinds {
			if !safeFleetToken(kind) {
				return errors.New("invalid driver kind")
			}
		}
	}
	return nil
}

func safeFleetToken(value string) bool {
	if !safeTokenRE.MatchString(value) || net.ParseIP(value) != nil {
		return false
	}
	lower := strings.ToLower(value)
	for _, forbidden := range []string{"password", "secret", "token", "serial", "endpoint", "hostname", "email"} {
		if strings.Contains(lower, forbidden) {
			return false
		}
	}
	return true
}

func validStatus(value string) bool {
	switch value {
	case "healthy", "degraded", "offline", "starting", "disabled", "not_configured":
		return true
	default:
		return false
	}
}

func validSource(value string) bool {
	switch value {
	case "bundled", "managed":
		return true
	default:
		return false
	}
}

type IDStore interface {
	LoadConfig(string) (string, bool)
	SaveConfig(string, string) error
}

type SnapshotFunc func(context.Context) (Payload, error)

type Config struct {
	Enabled       bool
	Endpoint      string
	Interval      time.Duration
	HTTPClient    *http.Client
	AllowInsecure bool // tests and explicit local development only
}

type Reporter struct {
	cfg   Config
	store IDStore
	build SnapshotFunc

	mu             sync.Mutex
	installationID string
}

func NewReporter(cfg Config, store IDStore, build SnapshotFunc) (*Reporter, error) {
	if build == nil {
		return nil, errors.New("fleet snapshot function is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.Enabled {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(cfg.AllowInsecure && u.Scheme == "http")) {
			return nil, errors.New("fleet endpoint must use HTTPS")
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Reporter{cfg: cfg, store: store, build: build}, nil
}

func (r *Reporter) Enabled() bool { return r != nil && r.cfg.Enabled }

func (r *Reporter) Preview(ctx context.Context) (Payload, error) {
	if r == nil {
		return Payload{}, errors.New("fleet reporter unavailable")
	}
	payload, err := r.build(ctx)
	if err != nil {
		return Payload{}, err
	}
	payload.SchemaVersion = SchemaVersion
	payload.InstallationID, err = r.installID()
	if err != nil {
		return Payload{}, err
	}
	sort.Slice(payload.Drivers, func(i, j int) bool {
		if payload.Drivers[i].ID == payload.Drivers[j].ID {
			return payload.Drivers[i].Status < payload.Drivers[j].Status
		}
		return payload.Drivers[i].ID < payload.Drivers[j].ID
	})
	if err := payload.Validate(); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func (r *Reporter) Submit(ctx context.Context) (Payload, error) {
	if !r.Enabled() {
		return Payload{}, ErrDisabled
	}
	payload, err := r.Preview(ctx)
	if err != nil {
		return Payload{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Payload{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return Payload{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "FTW-fleet-statistics/1")
	resp, err := r.cfg.HTTPClient.Do(req)
	if err != nil {
		return Payload{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Payload{}, fmt.Errorf("fleet endpoint: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return payload, nil
}

func (r *Reporter) Run(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	submit := func() {
		submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, _ = r.Submit(submitCtx) // fail-soft; local control is never affected
	}
	submit()
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			submit()
		}
	}
}

func (r *Reporter) installID() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if installIDRE.MatchString(r.installationID) {
		return r.installationID, nil
	}
	if r.store != nil {
		if value, ok := r.store.LoadConfig(installationIDKey); ok && installIDRE.MatchString(value) {
			r.installationID = value
			return value, nil
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	r.installationID = hex.EncodeToString(raw)
	if r.store != nil {
		if err := r.store.SaveConfig(installationIDKey, r.installationID); err != nil {
			return "", err
		}
	}
	return r.installationID, nil
}
