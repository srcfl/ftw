package main

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestRetirePythonPreservesCoreAndCustomServices(t *testing.T) {
	for _, style := range []string{"mapping", "sequence"} {
		t.Run(style, func(t *testing.T) {
			env := "      FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock\n      KEEP: value"
			deps := "      ftw-optimizer: {condition: service_healthy}\n      mqtt: {condition: service_started}"
			if style == "sequence" {
				env = "      - FTW_OPTIMIZER_SOCKET=/run/ftw-optimizer/optimizer.sock\n      - KEEP=value"
				deps = "      - ftw-optimizer\n      - mqtt"
			}
			input := []byte(`services:
  forty-two-watts:
    image: ghcr.io/srcfl/ftw:v2.15.2-beta.1
    environment:
` + env + `
    depends_on:
` + deps + `
    volumes:
      - ./data:/app/data
      - optimizer-ipc:/run/ftw-optimizer
      - type: volume
        source: optimizer-ipc
        target: /run/ftw-optimizer
  ftw-optimizer:
    image: retired:latest
  mqtt:
    image: eclipse-mosquitto:2
volumes:
  optimizer-ipc:
`)
			out, changed, err := retiredPythonCompose(input)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			for _, want := range []string{"./data:/app/data", "ghcr.io/srcfl/ftw:v2.15.2-beta.1", "KEEP", "eclipse-mosquitto:2"} {
				if !strings.Contains(string(out), want) {
					t.Fatalf("lost %s: %s", want, out)
				}
			}
			if strings.Contains(string(out), "ftw-optimizer") || strings.Contains(string(out), "FTW_OPTIMIZER_") {
				t.Fatalf("retired wiring remains: %s", out)
			}
			var doc any
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			again, changed, err := retiredPythonCompose(out)
			if err != nil || changed || string(again) != string(out) {
				t.Fatalf("not idempotent: %v %v", changed, err)
			}
		})
	}
}

func TestUpdaterRejectsRetiredOptimizer(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.componentSpec("optimizer"); err == nil {
		t.Fatal("optimizer still updatable")
	}
}
