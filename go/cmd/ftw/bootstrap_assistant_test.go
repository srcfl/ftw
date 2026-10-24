package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestBootstrapAssistantCreatesARealEmptyInstallation(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	restarted := make(chan struct{}, 1)
	handler := secureBootstrapMutations(bootstrapAssistant(func(cfg *config.Config) error { return saveInitialConfig(path, cfg) }, func() error { restarted <- struct{}{}; return nil }))
	for _, tc := range []struct {
		body, origin string
		want         int
	}{
		{`{"confirmed":true}`, "https://attacker.example", 403},
		{`{"confirmed":false}`, "http://box.local", 400},
		{`{"confirmed":true,"cloud":true}`, "http://box.local", 400},
		{`{"confirmed":true,"password":"first-owner-password"}`, "http://box.local", 200},
	} {
		req := httptest.NewRequest("POST", "http://box.local/api/setup/assistant", strings.NewReader(tc.body))
		req.RemoteAddr = "192.168.1.12:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	<-restarted
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MeasurementOnly() || len(cfg.Drivers) != 0 || cfg.Fuse.MaxAmps != 0 || cfg.FleetPing.Enabled || !cfg.AppLink.Enabled || cfg.Setup.ConnectionConfirmed || !cfg.API.LANAuth || cfg.LANPasswordHash == "" {
		t.Fatal("first boot invented equipment, limits or consent", cfg)
	}

	runtimeStore, err := state.Open(cfg.ConfigDatabase)
	if err != nil {
		t.Fatal("first real boot could not open its history", err)
	}
	if err := runtimeStore.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if strings.Contains(string(before), "first-owner-password") || strings.Contains(string(before), cfg.LANPasswordHash) {
		t.Fatal("locator contains the house password or its private hash")
	}
	if err := saveInitialConfig(path, config.NewSetup()); err == nil {
		t.Fatal("a late first-run request replaced settings")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("refused first-run request changed settings")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	recovered, err := config.Load(path)
	if err != nil || recovered.Revision != cfg.Revision || recovered.LANPasswordHash != cfg.LANPasswordHash {
		t.Fatal("interrupted locator publication lost first-claim settings", err)
	}
}

func TestBootstrapDoesNotReplaceAnOrphanedDatabase(t *testing.T) {
	dir := t.TempDir()
	data := []byte("existing database must stay untouched")
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, data, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := saveInitialConfig(path, config.NewSetup()); err == nil {
		t.Fatal("offered a new home over existing data")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created a new settings locator")
	}
	got, _ := os.ReadFile(db)
	if string(got) != string(data) {
		t.Fatal("changed existing database")
	}
}

func TestSetupCannotEnableChangedMeterConfiguration(t *testing.T) {
	active := &config.Config{Drivers: []config.Driver{{Name: "meter", Lua: "meter.lua", IsSiteMeter: true, Config: map[string]any{"host": "192.0.2.1", "scale": 1.0}}}}
	candidate := &config.Config{Drivers: []config.Driver{{Name: "meter", Lua: "meter.lua", IsSiteMeter: true, Config: map[string]any{"host": "192.0.2.1", "scale": 1.0}}}}
	if !sameSetupMeterConfiguration(active, candidate) {
		t.Fatal("same meter rejected")
	}
	candidate.Drivers[0].Config["scale"] = 2.0
	if sameSetupMeterConfiguration(active, candidate) {
		t.Fatal("old readings accepted for new scaling")
	}
	candidate.Drivers[0].Config["scale"] = 1.0
	candidate.Drivers[0].Config["host"] = "192.0.2.2"
	if sameSetupMeterConfiguration(active, candidate) {
		t.Fatal("old readings accepted for another endpoint")
	}
}

func TestHomeAssistantBootstrapNeedsNoSecondPassword(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	restarted := make(chan struct{}, 1)
	handler := api.WithHomeAssistantIngress(secureBootstrapMutations(bootstrapAssistant(func(cfg *config.Config) error { return saveInitialConfig(path, cfg) }, func() error { restarted <- struct{}{}; return nil })))
	req := httptest.NewRequest("POST", "http://ha.local/api/setup/assistant", strings.NewReader(`{"confirmed":true}`))
	req.RemoteAddr = "172.30.32.2:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ingress-Path", "/api/hassio_ingress/test")
	req.Header.Set("X-Remote-User-Id", "owner")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	<-restarted
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MeasurementOnly() || cfg.API.LANAuth || cfg.LANPasswordHash != "" || len(cfg.Drivers) != 0 {
		t.Fatal("HA setup invented a password or equipment")
	}
}
