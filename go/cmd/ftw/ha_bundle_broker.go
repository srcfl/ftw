package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
)

// haBundleBroker is the MQTT broker that the Home Assistant app found through
// the Supervisor (`services: mqtt:want` in srcfl/home-assistant-addons). The
// app passes it in FTW_HA_MQTT_*, so entities appear in Home Assistant
// without anyone typing a broker address or password.
//
// The broker never enters the saved settings. A saved broker always wins; an
// empty one means "Home Assistant's own broker".
type haBundleBroker struct {
	Host     string
	Port     int
	Username string
	Password string
}

// errNoHABroker means Home Assistant has no MQTT broker yet: the Mosquitto
// broker app is missing or not started.
var errNoHABroker = errors.New("Home Assistant has no MQTT broker yet")

const (
	supervisorURL     = "http://supervisor"
	supervisorTimeout = 5 * time.Second
)

// haBundleBrokerFromEnv returns the broker the Home Assistant app passed, or
// nil outside that app or when Home Assistant has no broker.
func haBundleBrokerFromEnv() *haBundleBroker {
	if os.Getenv("FTW_BUNDLE") != components.KindHomeAssistantAddon {
		return nil
	}
	host := strings.TrimSpace(os.Getenv("FTW_HA_MQTT_HOST"))
	if host == "" {
		return nil
	}
	port := 1883
	if raw := strings.TrimSpace(os.Getenv("FTW_HA_MQTT_PORT")); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			slog.Warn("ignoring Home Assistant's MQTT broker: bad port", "port", raw)
			return nil
		}
		port = p
	}
	return &haBundleBroker{
		Host:     host,
		Port:     port,
		Username: os.Getenv("FTW_HA_MQTT_USERNAME"),
		Password: os.Getenv("FTW_HA_MQTT_PASSWORD"),
	}
}

// haAppBroker is Home Assistant's MQTT broker as the Home Assistant app sees
// it. The app asks Supervisor once, before Core starts. When Mosquitto comes
// later, lookup asks Supervisor again, so the bridge can connect without a
// restart.
type haAppBroker struct {
	// lookup asks Supervisor for the broker; nil without a Supervisor token.
	lookup func(ctx context.Context) (*haBundleBroker, error)

	mu    sync.Mutex
	known *haBundleBroker // the last broker the app or Supervisor named
}

// haAppBrokerFromEnv returns nil outside the Home Assistant app, and also
// when the app passed no broker and Core cannot ask Supervisor for one.
func haAppBrokerFromEnv() *haAppBroker {
	if os.Getenv("FTW_BUNDLE") != components.KindHomeAssistantAddon {
		return nil
	}
	a := &haAppBroker{known: haBundleBrokerFromEnv()}
	// Supervisor gives every app this token; run.sh keeps it for Core.
	if token := strings.TrimSpace(os.Getenv("SUPERVISOR_TOKEN")); token != "" {
		a.lookup = supervisorMQTT(supervisorURL, token, &http.Client{Timeout: supervisorTimeout})
	}
	if a.known == nil && a.lookup == nil {
		return nil
	}
	return a
}

func (a *haAppBroker) current() *haBundleBroker {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.known
}

// defaultSettings turns the bridge on for a site that has never set it up.
// The caller keeps the result in memory; it is saved only when the owner
// saves settings.
func (a *haAppBroker) defaultSettings(cfg *config.HomeAssistant) *config.HomeAssistant {
	if a == nil || cfg != nil {
		return cfg
	}
	return &config.HomeAssistant{Enabled: true, Port: 1883, PublishIntervalS: 5}
}

// wants reports an enabled bridge with no saved broker: it uses Home
// Assistant's broker.
func (a *haAppBroker) wants(cfg *config.HomeAssistant) bool {
	return a != nil && cfg != nil && cfg.Enabled && strings.TrimSpace(cfg.Broker) == ""
}

// fill returns the settings the bridge connects with. An enabled bridge with
// no saved broker uses Home Assistant's broker and its login. ready is false
// while no such broker is known yet; the caller then waits for one.
func (a *haAppBroker) fill(cfg *config.HomeAssistant) (filled *config.HomeAssistant, ready bool) {
	if !a.wants(cfg) {
		return cfg, true
	}
	b := a.current()
	if b == nil {
		return cfg, false
	}
	out := *cfg
	out.Broker, out.Port = b.Host, b.Port
	out.Username, out.Password = b.Username, b.Password
	return &out, true
}

// refresh asks Supervisor for the broker again. Supervisor's answer replaces
// the one the app passed at start, since Mosquitto may have been added,
// removed or given a new login since. When Supervisor cannot be reached or
// names a broker FTW cannot use, the last known broker stays and refresh
// returns the reason.
func (a *haAppBroker) refresh(ctx context.Context) error {
	if a == nil || a.lookup == nil {
		return nil
	}
	b, err := a.lookup(ctx)
	if err != nil && !errors.Is(err, errNoHABroker) {
		return err
	}
	a.mu.Lock()
	before := a.known
	a.known = b
	a.mu.Unlock()
	if b != nil && (before == nil || before.Host != b.Host || before.Port != b.Port) {
		slog.Info("found Home Assistant's MQTT broker", "broker", fmt.Sprintf("%s:%d", b.Host, b.Port))
	}
	return err
}

// supervisorMQTT asks Supervisor for Home Assistant's MQTT broker, with the
// same rules as the app's mqtt-service.sh: a host is required, the port
// defaults to 1883, and a broker that needs TLS is not used.
func supervisorMQTT(base, token string, client *http.Client) func(ctx context.Context) (*haBundleBroker, error) {
	return func(ctx context.Context) (*haBundleBroker, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/services/mqtt", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return nil, err
		}
		// Supervisor answers 400 while no app provides the service.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
			return nil, errNoHABroker
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("supervisor: HTTP %d", resp.StatusCode)
		}
		var reply struct {
			Data struct {
				Host     any  `json:"host"`
				Port     any  `json:"port"`
				SSL      bool `json:"ssl"`
				Username any  `json:"username"`
				Password any  `json:"password"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
			return nil, fmt.Errorf("supervisor: %w", err)
		}
		d := reply.Data
		host, _ := d.Host.(string)
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, errNoHABroker
		}
		port := 1883
		if d.Port != nil {
			p, ok := d.Port.(float64)
			if !ok || p != math.Floor(p) || p < 1 || p > 65535 {
				return nil, errors.New("Home Assistant's MQTT broker has a bad port")
			}
			port = int(p)
		}
		if d.SSL {
			return nil, errors.New("Home Assistant's MQTT broker needs TLS, which FTW does not speak yet; set a broker in FTW's settings")
		}
		user, _ := d.Username.(string)
		pass, _ := d.Password.(string)
		return &haBundleBroker{Host: host, Port: port, Username: user, Password: pass}, nil
	}
}
