package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetiredCalendarDoesNotBlockConfigOrReturnCredentials(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML+`
caldav:
  enabled: true
  password: old-calendar-secret
  url: http://old-calendar.invalid
  poll_interval_s: -1
  ev_default_target_soc: 80
loadpoints:
  - id: garage
    driver: charger
`), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Loadpoints) != 1 || cfg.Loadpoints[0].ID != "garage" {
		t.Fatal("calendar removal lost the ordinary loadpoint")
	}
	if len(cfg.LoadWarnings) != 1 || !strings.Contains(cfg.LoadWarnings[0], "Calendar support has been removed") {
		t.Fatalf("missing upgrade warning: %v", cfg.LoadWarnings)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "caldav") || strings.Contains(string(raw), "old-calendar-secret") {
		t.Fatal("retired calendar settings or credentials returned to the UI")
	}
}
