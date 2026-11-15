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

// A support file from a real site carried the owner's e-mail twice: the
// cloud charger's username and a driver's `email` setting. Neither is a
// password, so key-based secret redaction let both through. The address
// identifies the household wherever it sits, so it is removed by key and
// by shape, in the config and in the logs.
func TestSupportDumpRemovesTheOwnersEmail(t *testing.T) {
	const (
		email    = "bjorn.example@example.se"
		username = "bjorn-cloud-login"
		inLog    = "owner.name@example.org"
	)
	tel := telemetry.NewStore()
	ring := telemetry.NewLogRing()
	ring.Append(telemetry.LogEntry{Level: "WARN", Driver: "easee", Msg: "login failed for " + inLog})
	cfg := &config.Config{
		Drivers: []config.Driver{{
			Name: "easee", Lua: "easee_cloud.lua",
			Config: map[string]any{"email": email, "site": "home", "note": "contact " + email},
		}},
		EVCharger: &config.EVCharger{Provider: "easee", Username: username, Serial: "EH123456"},
	}
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: ring, Version: "test",
		Cfg: cfg, CfgMu: &sync.RWMutex{},
	})
	files := readSupportZip(t, srv)
	for name, body := range files {
		for _, leak := range []string{email, username, inLog} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaked %q", name, leak)
			}
		}
	}
	cfgJSON := files["config.redacted.yaml"]
	if !strings.Contains(cfgJSON, `"site": "home"`) || !strings.Contains(cfgJSON, "EH123456") {
		t.Errorf("redaction removed settings that are not personal: %s", cfgJSON)
	}
}

// The site's coordinates say where the house is, and an ntfy topic on the
// public server is the key that reads its alerts. Neither may leave in the
// support file, from the config or from a log line that quotes a URL or an
// HTTP body. Whole degrees stay, so a wrong sign or a missing location shows.
func TestSupportDumpHidesWhereTheHouseIsAndTheNtfyTopic(t *testing.T) {
	const topic = "ftw-home-7f3k2q9x"
	tel := telemetry.NewStore()
	ring := telemetry.NewLogRing()
	ring.Append(telemetry.LogEntry{Level: "WARN", Msg: "forecast: fetch failed",
		Attrs: `err=Get "https://api.met.no/weatherapi/locationforecast/2.0/compact?lat=56.1235&lon=13.6543": timeout`})
	ring.Append(telemetry.LogEntry{Level: "WARN", Msg: "notifications: publish failed",
		Attrs: `err=ntfy: Post "https://ntfy.sh/` + topic + `": dial tcp: lookup ntfy.sh: no such host`})
	ring.Append(telemetry.LogEntry{Level: "INFO", Driver: "vw", Msg: `car: {"position":{"latitude":59.334591,"longitude":18.063240},"latest_ms":1763200000000}`})
	cfg := &config.Config{
		Weather: &config.Weather{Provider: "met_no", Latitude: 56.123456, Longitude: 13.654321},
		Notifications: &config.Notifications{Enabled: true, Provider: "ntfy",
			Ntfy: &config.NtfyConfig{Server: "https://ntfy.sh", Topic: topic}},
		Drivers: []config.Driver{{
			Name: "vw", Lua: "vw.lua",
			Config: map[string]any{"home_lat": "56.123456", "home_lon": 13.654321, "topic_prefix": "ftw/site"},
		}},
	}
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: ring, Version: "test",
		Cfg: cfg, CfgMu: &sync.RWMutex{},
	})
	files := readSupportZip(t, srv)
	for name, body := range files {
		for _, leak := range []string{topic, "56.12", "13.65", "59.33", "18.06"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaked %q", name, leak)
			}
		}
	}
	cfgJSON := files["config.redacted.yaml"]
	for _, want := range []string{`"latitude": 56`, `"longitude": 14`, `"home_lat": "56"`, `"home_lon": 14`, `"topic_prefix": "ftw/site"`, `"server": "https://ntfy.sh"`} {
		if !strings.Contains(cfgJSON, want) {
			t.Errorf("config.redacted.yaml lacks %s:\n%s", want, cfgJSON)
		}
	}
	if log := files["logs/global.log"]; !strings.Contains(log, "lat=56&lon=14") || !strings.Contains(log, `"latitude":59`) || !strings.Contains(log, `"latest_ms":1763200000000`) {
		t.Errorf("global.log lost what is not personal:\n%s", log)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/support/report", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/support/report = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), topic) {
		t.Errorf("help report leaked the ntfy topic:\n%s", rec.Body.String())
	}
}

// readSupportZip fetches /api/support/dump and returns each file by its path
// inside the archive's top folder.
func readSupportZip(t *testing.T, srv *Server) map[string]string {
	t.Helper()
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
	out := map[string]string{}
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
		name := f.Name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		out[name] = string(raw)
	}
	return out
}
