package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"

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

// defaultSettings turns the bridge on for a site that has never set it up,
// when Home Assistant offers a broker. The caller keeps the result in memory;
// it is saved only when the owner saves settings.
func (b *haBundleBroker) defaultSettings(cfg *config.HomeAssistant) *config.HomeAssistant {
	if b == nil || cfg != nil {
		return cfg
	}
	return &config.HomeAssistant{Enabled: true, Port: 1883, PublishIntervalS: 5}
}

// fill returns the settings the bridge connects with. An enabled bridge with
// no saved broker uses Home Assistant's broker and its login.
func (b *haBundleBroker) fill(cfg *config.HomeAssistant) *config.HomeAssistant {
	if b == nil || cfg == nil || !cfg.Enabled || strings.TrimSpace(cfg.Broker) != "" {
		return cfg
	}
	out := *cfg
	out.Broker, out.Port = b.Host, b.Port
	out.Username, out.Password = b.Username, b.Password
	return &out
}
