package roofmodel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// stubModule writes a script that stands in for the Python module, so the
// subprocess contract itself is exercised — argument passing, stdout parsing,
// stderr error reporting, timeouts — without needing the geospatial stack
// installed on the machine running the tests.
func stubModule(t *testing.T, script string) (command, dir string) {
	t.Helper()
	dir = t.TempDir()
	var path, interp string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "stub.bat")
		interp = path
		script = "@echo off\r\n" + script
	} else {
		path = filepath.Join(dir, "stub.sh")
		interp = "sh"
		script = "#!/bin/sh\n" + script
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return interp, dir
	}
	return interp, dir
}

func svc(t *testing.T, cfg *config.RoofModel) *Service {
	t.Helper()
	s := FromConfig(cfg)
	if s == nil {
		t.Fatal("FromConfig returned nil for an enabled config")
	}
	return s
}

const stockholmLat, stockholmLon = 59.33, 18.07

func TestDisabledWhenAbsentOrOff(t *testing.T) {
	if FromConfig(nil) != nil {
		t.Error("nil config must not produce a service")
	}
	if FromConfig(&config.RoofModel{Enabled: false}) != nil {
		t.Error("disabled config must not produce a service")
	}
	// A nil *Service must be safe to call, not a panic.
	var s *Service
	if s.Enabled() {
		t.Error("nil service must report disabled")
	}
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

// A site outside Sweden can never succeed, so it must fail before spawning an
// interpreter and hitting the network.
func TestDeriveRefusesOutsideSwedenWithoutSpawning(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled:           true,
		Command:           "definitely-not-a-real-command",
		GeotorgetUsername: "u", GeotorgetToken: "t",
	})
	for _, c := range []struct {
		name     string
		lat, lon float64
	}{
		{"Berlin", 52.52, 13.40},
		{"Sydney", -33.87, 151.21},
		{"New York", 40.71, -74.01},
		{"north of Sweden", 71.0, 20.0},
	} {
		_, err := s.Derive(context.Background(), c.lat, c.lon)
		if !errors.Is(err, ErrOutsideCoverage) {
			t.Errorf("%s: err = %v, want ErrOutsideCoverage", c.name, err)
		}
	}
}

// Sweden is a long diagonal, so no lat/lon rectangle can trace its border —
// any box containing Sweden also contains parts of Norway and Finland. Oslo is
// the clearest example: it is west of Sweden but inside the box.
//
// This is the same advisory-superset property the coverage package documents
// for STRÅNG, and it is resolved upstream rather than geometrically: the STAC
// search returns no tiles and the module reports "Sweden only". Pinned so the
// box is not "tightened" into something that starts excluding real Swedish
// addresses near the border.
func TestSwedishBoxAdmitsSomeNonSwedishPointsByDesign(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled:           true,
		Command:           "definitely-not-a-real-command",
		GeotorgetUsername: "u", GeotorgetToken: "t",
	})
	_, err := s.Derive(context.Background(), 59.91, 10.75) // Oslo
	if errors.Is(err, ErrOutsideCoverage) {
		t.Skip("box now excludes Oslo; verify it still admits Strömstad and Haparanda")
	}
	if err == nil {
		t.Fatal("want the spawn to fail, since the command does not exist")
	}
}

// The border towns are the reason the box stays generous: tightening it to
// exclude Oslo would start excluding real Swedish sites.
func TestSwedishBoxCoversBorderTowns(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled:           true,
		Command:           "definitely-not-a-real-command",
		GeotorgetUsername: "u", GeotorgetToken: "t",
	})
	for _, c := range []struct {
		name     string
		lat, lon float64
	}{
		{"Strömstad (west coast, near Norway)", 58.94, 11.17},
		{"Haparanda (east, near Finland)", 65.83, 24.14},
		{"Karesuando (far north)", 68.44, 22.49},
		{"Smygehuk (far south)", 55.34, 13.36},
	} {
		_, err := s.Derive(context.Background(), c.lat, c.lon)
		if errors.Is(err, ErrOutsideCoverage) {
			t.Errorf("%s: must not be excluded", c.name)
		}
	}
}

func TestDeriveRequiresCredentials(t *testing.T) {
	for _, c := range []struct {
		name, user, token string
	}{
		{"no username", "", "t"},
		{"no token", "u", ""},
		{"neither", "", ""},
	} {
		s := svc(t, &config.RoofModel{Enabled: true, Command: "no-such-command", GeotorgetUsername: c.user, GeotorgetToken: c.token})
		_, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
		if !errors.Is(err, ErrNoCredentials) {
			t.Errorf("%s: err = %v, want ErrNoCredentials", c.name, err)
		}
	}
}

func TestDeriveParsesAModel(t *testing.T) {
	doc := `{"schema_version":1,"planes_found":3,` +
		`"site":{"latitude":59.33,"longitude":18.07,"radius_m":40},` +
		`"source":{"provider":"lantmateriet","item_count":2,"dataset_datetime":"2018-03-01T00:00:00+00:00"},` +
		`"arrays":[{"name":"Roof south","kwp":7.2,"tilt_deg":35,"azimuth_deg":180,"area_m2":51.4,"segment_id":"seg-0"}],` +
		`"captured_at_ms":1519862400000,"derived_at_ms":1785456000000}`
	cmd, dir := stubModule(t, "echo "+quoteForShell(doc))
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, ModuleDir: dir, GeotorgetUsername: "u", GeotorgetToken: "t"})

	m, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 1 || m.PlanesFound != 3 {
		t.Errorf("unexpected model: %+v", m)
	}
	if len(m.Arrays) != 1 || m.Arrays[0].Name != "Roof south" {
		t.Fatalf("arrays = %+v", m.Arrays)
	}
	if m.CapturedAtMs == nil || *m.CapturedAtMs != 1519862400000 {
		t.Errorf("captured_at_ms = %v", m.CapturedAtMs)
	}
}

// The module signals failure as JSON on stderr precisely so an operator sees a
// cause rather than a traceback.
func TestDeriveSurfacesTheModuleErrorMessage(t *testing.T) {
	cmd, dir := stubModule(t, `echo {"error":"Geotorget rejected the credentials","kind":"MissingCredentials"} 1>&2
exit 1`)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, ModuleDir: dir, GeotorgetUsername: "u", GeotorgetToken: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Geotorget rejected the credentials") {
		t.Errorf("err = %v, want the module's own message", err)
	}
}

func TestDeriveRejectsUnknownSchemaVersion(t *testing.T) {
	cmd, dir := stubModule(t, "echo "+quoteForShell(`{"schema_version":99,"arrays":[]}`))
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, ModuleDir: dir, GeotorgetUsername: "u", GeotorgetToken: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("err = %v, want a schema-version rejection", err)
	}
}

func TestDeriveRejectsUnreadableOutput(t *testing.T) {
	cmd, dir := stubModule(t, "echo not-json-at-all")
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, ModuleDir: dir, GeotorgetUsername: "u", GeotorgetToken: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("err = %v, want an unreadable-output error", err)
	}
}

// LiDAR tiles are large and this runs on a Pi; an unbounded derive could hold
// memory indefinitely.
func TestDeriveIsTimeBoxed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no portable sleep in a .bat stub")
	}
	cmd, dir := stubModule(t, "sleep 30")
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, ModuleDir: dir,
		GeotorgetUsername: "u", GeotorgetToken: "t", TimeoutS: 1,
	})

	start := time.Now()
	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s, timeout was not enforced", elapsed)
	}
}

func TestToPVArraysMatchesConfigShape(t *testing.T) {
	m := &Model{Arrays: []Array{
		{Name: "Roof south", KWp: 7.2, TiltDeg: 35, AzimuthDeg: 180, AreaM2: 51.4},
		{Name: "Roof west", KWp: 4.1, TiltDeg: 35, AzimuthDeg: 270, AreaM2: 29.3},
	}}
	got := m.ToPVArrays()
	if len(got) != 2 {
		t.Fatalf("got %d arrays", len(got))
	}
	if got[0].TiltDeg == nil || got[0].AzimuthDeg == nil {
		t.Fatalf("derived array must carry both angles, got %+v", got[0])
	}
	if got[0].Name != "Roof south" || got[0].KWp != 7.2 ||
		*got[0].TiltDeg != 35 || *got[0].AzimuthDeg != 180 {
		t.Errorf("array 0 = %+v tilt=%v az=%v", got[0], *got[0].TiltDeg, *got[0].AzimuthDeg)
	}
	// Every entry must own its angles. Sharing one address across the slice
	// would make editing one array in the UI silently move the others.
	if got[0].AzimuthDeg == got[1].AzimuthDeg {
		t.Error("arrays share an azimuth pointer")
	}
	if *got[1].AzimuthDeg != 270 {
		t.Errorf("array 1 azimuth = %v, want 270", *got[1].AzimuthDeg)
	}
	var nilModel *Model
	if nilModel.ToPVArrays() != nil {
		t.Error("nil model must yield nil arrays")
	}
}

// quoteForShell wraps a JSON document so both sh and cmd.exe echo it intact.
func quoteForShell(s string) string {
	if runtime.GOOS == "windows" {
		// cmd.exe has no quoting that survives embedded quotes cleanly; escape
		// the shell metacharacters instead.
		r := strings.NewReplacer("^", "^^", "&", "^&", "<", "^<", ">", "^>", "|", "^|")
		return r.Replace(s)
	}
	return "'" + s + "'"
}
