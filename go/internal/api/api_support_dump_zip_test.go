package api

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The dump exists to be handed to somebody else, so the archive has to
// open with no extra tooling. This asserts it is a real zip, not just a
// renamed one.
func TestSupportDumpIsAReadableZip(t *testing.T) {
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	ring := telemetry.NewLogRing()
	ring.Append(telemetry.LogEntry{Level: "WARN", Msg: "something to log"})
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: ring, Version: "test",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/support/dump", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/dump = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Errorf("Content-Disposition = %q, want a .zip filename", cd)
	}

	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("archive does not open as a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			t.Errorf("entry %s will not open: %v", f.Name, err)
			continue
		}
		rc.Close()
	}
	joined := strings.Join(names, " ")
	// The report is the point of the archive for most recipients, so it
	// has to be in there and has to sort first.
	if len(zr.File) == 0 || !strings.HasSuffix(zr.File[0].Name, "00-help-report.md") {
		t.Errorf("first entry is %q, want the help report", names[0])
	}
	var reportBody string
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "00-help-report.md") {
			rc, _ := f.Open()
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(rc)
			rc.Close()
			reportBody = buf.String()
		}
	}
	if !strings.Contains(reportBody, "# FTW help report") {
		t.Error("the embedded report is not a help report")
	}
	if !strings.Contains(reportBody, "## Findings") {
		t.Error("the embedded report is missing Findings")
	}

	for _, want := range []string{"00-help-report.md", "manifest.json", "drivers.json", "logs/global.log"} {
		if !strings.Contains(joined, want) {
			t.Errorf("archive is missing %s; has %v", want, names)
		}
	}
}

func TestSupportDumpRedactsSecretsInLogsAndConfig(t *testing.T) {
	const (
		leakRefresh = "RT-leak-9f3c2e1a"
		leakAccess  = "AT-leak-9f3c2e1a"
		leakBearer  = "leak-b64-aabbccdd"
		leakMQTT    = "mqtt-pass-should-not-leave"
		leakPass    = "cfg-pass-should-not-leave"
		leakAuth    = "bare-auth-should-not-leave"
		leakAuthz   = "authz-should-not-leave"
		leakPasswd  = "passwd-should-not-leave"
		leakCred    = "cred-should-not-leave"
	)
	tel := telemetry.NewStore()
	tel.DriverHealthMut("myuplink").RecordSuccess()
	ring := telemetry.NewLogRing()
	ring.Append(telemetry.LogEntry{
		Level: "ERROR", Driver: "myuplink",
		Msg: `HTTP 400: {"refresh_token":"` + leakRefresh + `","access_token":"` + leakAccess + `"}`,
	})
	ring.Append(telemetry.LogEntry{
		Level: "WARN", Driver: "easee",
		Msg: "login failed Bearer " + leakBearer,
	})
	ring.Append(telemetry.LogEntry{
		Level: "INFO", Driver: "myuplink",
		Msg: "poll ok w=1234",
	})
	cfg := &config.Config{
		Drivers: []config.Driver{{
			Name: "myuplink",
			Lua:  "myuplink.lua",
			Config: map[string]any{
				"password":        leakPass,
				"auth":            leakAuth,
				"authorization":   leakAuthz,
				"passwd":          leakPasswd,
				"credential":      leakCred,
				"oauth_client_id": "public-client-id",
				"host":            "heatpump.example",
			},
			Capabilities: config.Capabilities{
				MQTT: &config.MQTTConfig{
					Host:     "mqtt.local",
					Password: leakMQTT,
				},
			},
		}},
	}
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: ring, Version: "test",
		Cfg: cfg, CfgMu: &sync.RWMutex{},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/support/dump", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/dump = %d, want 200", rec.Code)
	}

	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("archive does not open as a zip: %v", err)
	}
	var archive strings.Builder
	var logs, cfgJSON string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		archive.Write(raw)
		archive.WriteByte('\n')
		switch {
		case strings.HasSuffix(f.Name, "logs/global.log") || strings.Contains(f.Name, "/logs/"):
			logs += string(raw)
		case strings.HasSuffix(f.Name, "config.redacted.yaml"):
			cfgJSON = string(raw)
		}
	}
	joined := archive.String()
	for _, leak := range []string{
		leakRefresh, leakAccess, leakBearer, leakMQTT, leakPass,
		leakAuth, leakAuthz, leakPasswd, leakCred,
	} {
		if strings.Contains(joined, leak) {
			t.Errorf("support dump leaked %q", leak)
		}
	}
	if !strings.Contains(logs, "poll ok w=1234") {
		t.Errorf("benign log line missing from dump logs: %q", logs)
	}
	if !strings.Contains(logs, "HTTP 400") {
		t.Errorf("HTTP status missing from dump logs: %q", logs)
	}
	if cfgJSON == "" {
		t.Fatal("archive is missing config.redacted.yaml")
	}
	if !strings.Contains(cfgJSON, `"***"`) {
		t.Errorf("redacted config has no *** placeholders: %s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, "public-client-id") {
		t.Errorf("oauth_client_id should remain visible, got %s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, "heatpump.example") {
		t.Errorf("host should remain visible, got %s", cfgJSON)
	}
	if strings.Contains(joined, "state.db") {
		t.Error("dump must not include state.db")
	}
}
