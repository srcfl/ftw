package roofmodel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// The module is a subprocess, so exercising the contract -- argument passing,
// stdout parsing, stderr error reporting, timeouts -- needs something to spawn.
// This test binary stands in for it: with stubEnvVar set, TestMain impersonates
// the Python module instead of running tests.
//
// The first version of this harness wrote little sh and .bat scripts instead.
// It passed on Windows and failed on Linux in two separate ways: the service
// invokes the command as `<cmd> -m ftw_roofmodel ...`, which dash reads as
// "run the script named ftw_roofmodel" (exit 2, the stub never ran at all), and
// an unquoted JSON document loses its double quotes to shell word-splitting.
// Re-executing a compiled binary has no shell in the path, so there is no
// quoting to get wrong and nothing that can behave differently per platform.
const (
	stubModeVar    = "FTW_ROOFMODEL_TEST_STUB"
	stubPayloadVar = "FTW_ROOFMODEL_TEST_PAYLOAD"
)

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(stubModeVar); ok {
		os.Exit(runStub(mode, os.Getenv(stubPayloadVar)))
	}
	os.Exit(m.Run())
}

// stubInvocation is what the stub records about how it was called, so a test
// can assert what core actually handed the module.
type stubInvocation struct {
	Args       []string `json:"args"`
	PythonPath string   `json:"pythonpath"`
}

// runStub plays the part of `python3 -m ftw_roofmodel`.
func runStub(mode, payload string) int {
	switch mode {
	case "stdout":
		os.Stdout.WriteString(payload)
		return 0
	case "stderr":
		// How the real module reports failure: JSON on stderr, non-zero exit.
		os.Stderr.WriteString(payload)
		return 1
	case "record":
		enc, err := json.Marshal(stubInvocation{
			Args:       os.Args[1:],
			PythonPath: os.Getenv("PYTHONPATH"),
		})
		if err != nil {
			return 3
		}
		if err := os.WriteFile(payload, enc, 0o600); err != nil {
			return 3
		}
		os.Stdout.WriteString(minimalModel)
		return 0
	case "hang":
		time.Sleep(30 * time.Second)
		return 0
	}
	os.Stderr.WriteString("unknown stub mode " + mode)
	return 2
}

// stubModule points the service at this test binary running in stub mode.
func stubModule(t *testing.T, mode, payload string) string {
	t.Helper()
	t.Setenv(stubModeVar, mode)
	t.Setenv(stubPayloadVar, payload)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	return exe
}

const minimalModel = `{"schema_version":1,"arrays":[]}`

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
	// The service is always constructed — a later Reconfigure may enable it —
	// but stays inert until config says otherwise.
	if FromConfig(nil).Enabled() {
		t.Error("nil config must report disabled")
	}
	off := FromConfig(&config.RoofModel{Enabled: false})
	if off.Enabled() {
		t.Error("disabled config must report disabled")
	}
	if _, err := off.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
	// A nil *Service must be safe to call, not a panic.
	var s *Service
	if s.Enabled() {
		t.Error("nil service must report disabled")
	}
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
	s.Reconfigure(&config.RoofModel{Enabled: true}) // no-op, not a panic
}

// Geotorget credentials arrive through Settings while the process runs; the
// service used to keep its boot-time config, so the very save that stored them
// changed nothing until a restart nothing asked for. Reconfigure is what the
// hot-reload applier calls — it must both deliver credentials and flip
// enablement, in both directions.
func TestReconfigureAppliesCredentialsWithoutRestart(t *testing.T) {
	s := svc(t, &config.RoofModel{Enabled: true})
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("before reconfigure: err = %v, want ErrNoCredentials", err)
	}

	s.Reconfigure(&config.RoofModel{
		Enabled: true,
		Command: stubModule(t, "stdout", minimalModel),
		StacUsername: "operator", StacPassword: "secret",
	})
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); err != nil {
		t.Fatalf("after reconfigure with credentials: %v", err)
	}

	s.Reconfigure(&config.RoofModel{Enabled: false})
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("after disabling: err = %v, want ErrDisabled", err)
	}
}

// A site outside Sweden can never succeed, so it must fail before spawning an
// interpreter and hitting the network.
func TestDeriveRefusesOutsideSwedenWithoutSpawning(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled:           true,
		Command:           "definitely-not-a-real-command",
		StacUsername: "u", StacPassword: "t",
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
		_, err := s.Derive(context.Background(), c.lat, c.lon, "", nil)
		if !errors.Is(err, ErrOutsideCoverage) {
			t.Errorf("%s: err = %v, want ErrOutsideCoverage", c.name, err)
		}
	}
}

// Sweden is a long diagonal, so no lat/lon rectangle can trace its border --
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
		StacUsername: "u", StacPassword: "t",
	})
	_, err := s.Derive(context.Background(), 59.91, 10.75, "", nil) // Oslo
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
		StacUsername: "u", StacPassword: "t",
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
		_, err := s.Derive(context.Background(), c.lat, c.lon, "", nil)
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
		s := svc(t, &config.RoofModel{Enabled: true, Command: "no-such-command", StacUsername: c.user, StacPassword: c.token})
		_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
		if !errors.Is(err, ErrNoCredentials) {
			t.Errorf("%s: err = %v, want ErrNoCredentials", c.name, err)
		}
	}
}

func TestDeriveParsesAModel(t *testing.T) {
	doc := `{"schema_version":1,"planes_found":3,` +
		`"site":{"latitude":59.33,"longitude":18.07,"radius_m":40},` +
		`"source":{"provider":"lantmateriet","item_count":2,"dataset_datetime":"2018-03-01T00:00:00+00:00"},` +
		`"arrays":[{"name":"Roof south","rated_w":7200,"tilt_deg":35,"azimuth_deg":180,"area_m2":51.4,"segment_id":"seg-0"}],` +
		`"captured_at_ms":1519862400000,"derived_at_ms":1785456000000}`
	cmd := stubModule(t, "stdout", doc)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	m, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
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

// Whether the module streamed a window out of the COPC tile or pulled the whole
// thing changes what ReturnsInRadius counts, so the answer has to reach core
// rather than being inferred from the numbers.
func TestDeriveCarriesHowTheLidarWasFetched(t *testing.T) {
	doc := `{"schema_version":1,"planes_found":2,` +
		`"source":{"provider":"lantmateriet","collection":"laserdata-nedladdning-skog",` +
		`"item_count":1,"fetch":"copc-window"},` +
		`"building":{"building_id":"b-1","area_m2":144,"returns_used":220,"returns_in_radius":260},` +
		`"arrays":[]}`
	cmd := stubModule(t, "stdout", doc)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	m, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "b-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source.Fetch != "copc-window" {
		t.Errorf("fetch = %q, want copc-window", m.Source.Fetch)
	}
	if m.Building == nil || m.Building.ReturnsUsed != 220 {
		t.Fatalf("building = %+v", m.Building)
	}
}

// The site and the operator's credentials have to survive the process boundary,
// or the module derives a roof somewhere else entirely.
func TestDerivePassesTheSiteAndCredentials(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, ModuleDir: dir,
		StacUsername: "operator", StacPassword: "secret-token",
		RadiusM: 25,
	})

	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub recorded nothing: %v", err)
	}
	var got stubInvocation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got.Args, " ")
	for _, want := range []string{
		"-m ftw_roofmodel",
		"--lat 59.330000",
		"--lon 18.070000",
		"--username operator",
		"--password secret-token",
		"--radius-m 25.0",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
	// Without PYTHONPATH the module is only importable if it happens to be
	// installed system-wide, which on a Pi it is not.
	if got.PythonPath != dir {
		t.Errorf("PYTHONPATH = %q, want %q", got.PythonPath, dir)
	}
	// vostok is opt-in: absent config must not silently enable a GPL tool.
	if strings.Contains(line, "--vostok") {
		t.Errorf("args %q passed --vostok without configuration", line)
	}
}

// Picking a building is the whole point of the picker: the id has to reach the
// module, or the derive silently segments the neighbourhood instead.
func TestDerivePassesThePickedBuilding(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd,
		StacUsername: "u", StacPassword: "t",
	})

	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "bldg-42", nil); err != nil {
		t.Fatal(err)
	}

	line := strings.Join(readInvocation(t, record).Args, " ")
	if !strings.Contains(line, "--building-id bldg-42") {
		t.Errorf("args %q did not carry the picked building", line)
	}
	if !strings.Contains(line, "--mode derive") {
		t.Errorf("args %q did not select derive mode", line)
	}
}

// A hand-drawn footprint is the picker for catalogs with no building dataset;
// it must reach the module as the exact [lon, lat] ring that was traced.
func TestDerivePassesTheDrawnFootprint(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd,
		StacUsername: "u", StacPassword: "t",
	})

	ring := [][]float64{{18.06, 59.32}, {18.07, 59.32}, {18.07, 59.33}}
	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", ring); err != nil {
		t.Fatal(err)
	}

	line := strings.Join(readInvocation(t, record).Args, " ")
	if !strings.Contains(line, `--footprint-json [[18.06,59.32],[18.07,59.32],[18.07,59.33]]`) {
		t.Errorf("args %q did not carry the drawn footprint", line)
	}
	if strings.Contains(line, "--building-id") {
		t.Errorf("args %q sent a building id nobody picked", line)
	}
}

// Not picking one must not send an empty flag the module would treat as a
// building named "".
func TestDeriveOmitsTheBuildingFlagWhenNoneIsPicked(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t",
	})

	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); err != nil {
		t.Fatal(err)
	}

	if line := strings.Join(readInvocation(t, record).Args, " "); strings.Contains(line, "--building-id") {
		t.Errorf("args %q passed an empty building id", line)
	}
}

func TestBuildingsListsFootprints(t *testing.T) {
	doc := `{"schema_version":1,"site":{"latitude":59.33,"longitude":18.07},"buildings":[` +
		`{"type":"Feature","id":"b1","geometry":{"type":"Polygon","coordinates":[[[18.0,59.3],[18.001,59.3],[18.001,59.301],[18.0,59.3]]]},"properties":{"area_m2":120.5}},` +
		`{"type":"Feature","id":"b2","geometry":{"type":"Polygon","coordinates":[[[18.01,59.3],[18.011,59.3],[18.011,59.301],[18.01,59.3]]]},"properties":{"area_m2":64.0}}]}`
	cmd := stubModule(t, "stdout", doc)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	list, err := s.Buildings(context.Background(), stockholmLat, stockholmLon)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Buildings) != 2 {
		t.Fatalf("got %d buildings", len(list.Buildings))
	}
	// Geometry is ferried to the map untouched, so it must survive intact.
	if !strings.Contains(string(list.Buildings[0]), `"id":"b1"`) {
		t.Errorf("first feature = %s", list.Buildings[0])
	}
}

func TestBuildingsUsesBuildingsMode(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	// The stub answers with a roof model, not a building list; only the
	// invocation matters here.
	_, _ = s.Buildings(context.Background(), stockholmLat, stockholmLon)

	if line := strings.Join(readInvocation(t, record).Args, " "); !strings.Contains(line, "--mode buildings") {
		t.Errorf("args %q did not select buildings mode", line)
	}
}

// Every guard that protects a derive has to protect a building search too --
// it is the same credentials and the same country.
func TestBuildingsRefusesOutsideCoverageAndWithoutCredentials(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: "definitely-not-a-real-command",
		StacUsername: "u", StacPassword: "t",
	})
	if _, err := s.Buildings(context.Background(), 52.52, 13.40); !errors.Is(err, ErrOutsideCoverage) {
		t.Errorf("Berlin: err = %v, want ErrOutsideCoverage", err)
	}

	noCreds := svc(t, &config.RoofModel{Enabled: true, Command: "no-such-command"})
	if _, err := noCreds.Buildings(context.Background(), stockholmLat, stockholmLon); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}

	var nilService *Service
	if _, err := nilService.Buildings(context.Background(), stockholmLat, stockholmLon); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

func readInvocation(t *testing.T, path string) stubInvocation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub recorded nothing: %v", err)
	}
	var got stubInvocation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// The module signals failure as JSON on stderr precisely so an operator sees a
// cause rather than a traceback.
func TestDeriveSurfacesTheModuleErrorMessage(t *testing.T) {
	cmd := stubModule(t, "stderr",
		`{"error":"Geotorget rejected the credentials","kind":"MissingCredentials"}`)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Geotorget rejected the credentials") {
		t.Errorf("err = %v, want the module's own message", err)
	}
}

// Third-party libraries write warnings to stderr above the module's JSON —
// requests' RequestsDependencyWarning did exactly this in live testing and
// reduced a real "STAC search returned HTTP 404" to "exit status 1". The
// module's contract is one JSON document as the final line; the parse must
// hold whatever gets printed above it.
func TestModuleErrorSurvivesLibraryWarnings(t *testing.T) {
	cmd := stubModule(t, "stderr",
		"site-packages/requests/__init__.py:113: RequestsDependencyWarning: urllib3 mismatch\n"+
			"  warnings.warn(\n"+
			`{"error":"STAC search returned HTTP 404","kind":"GeotorgetError"}`+"\n")
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "STAC search returned HTTP 404") {
		t.Errorf("err = %v, want the module's own message despite the warnings above it", err)
	}
}

// A crash that is not the module's own JSON must still surface as an error
// rather than being mistaken for a successful empty model.
func TestDeriveReportsNonJSONFailure(t *testing.T) {
	cmd := stubModule(t, "stderr", "Traceback (most recent call last):\n  MemoryError\n")
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil || !strings.Contains(err.Error(), "roof model failed") {
		t.Errorf("err = %v, want a plain failure", err)
	}
}

func TestDeriveRejectsUnknownSchemaVersion(t *testing.T) {
	cmd := stubModule(t, "stdout", `{"schema_version":99,"arrays":[]}`)
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("err = %v, want a schema-version rejection", err)
	}
}

func TestDeriveRejectsUnreadableOutput(t *testing.T) {
	cmd := stubModule(t, "stdout", "not-json-at-all")
	s := svc(t, &config.RoofModel{Enabled: true, Command: cmd, StacUsername: "u", StacPassword: "t"})

	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("err = %v, want an unreadable-output error", err)
	}
}

// LiDAR tiles are large and this runs on a Pi; an unbounded derive could hold
// memory indefinitely.
func TestDeriveIsTimeBoxed(t *testing.T) {
	cmd := stubModule(t, "hang", "")
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd,
		StacUsername: "u", StacPassword: "t", TimeoutS: 1,
	})

	start := time.Now()
	_, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s, timeout was not enforced", elapsed)
	}
}

func TestToPVArraysMatchesConfigShape(t *testing.T) {
	m := &Model{Arrays: []Array{
		{Name: "Roof south", RatedW: 7200, TiltDeg: 35, AzimuthDeg: 180, AreaM2: 51.4},
		{Name: "Roof west", RatedW: 4100, TiltDeg: 35, AzimuthDeg: 270, AreaM2: 29.3},
	}}
	got := m.ToPVArrays()
	if len(got) != 2 {
		t.Fatalf("got %d arrays", len(got))
	}
	if got[0].TiltDeg == nil || got[0].AzimuthDeg == nil {
		t.Fatalf("derived array must carry both angles, got %+v", got[0])
	}
	if got[0].Name != "Roof south" || got[0].RatedW != 7200 ||
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

// A config written before the basic-auth redesign still derives: the legacy
// geotorget_* keys resolve through the accessors and the secret reaches the
// module as --password, whatever key it was stored under.
func TestDeriveAcceptsLegacyGeotorgetKeys(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, ModuleDir: dir,
		GeotorgetUsername: "legacy-op", GeotorgetToken: "legacy-secret",
	})

	if _, err := s.Derive(context.Background(), stockholmLat, stockholmLon, "", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub recorded nothing: %v", err)
	}
	var got stubInvocation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got.Args, " ")
	for _, want := range []string{"--username legacy-op", "--password legacy-secret"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "--token") {
		t.Errorf("args %q still use the retired --token flag", line)
	}
}

// A custom STAC catalog lifts the Sweden-only gate — FTW cannot know what a
// third-party catalog covers — and every stac_* setting must cross the
// process boundary.
func TestDeriveCustomCatalogSkipsSwedenGateAndPassesStacArgs(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, ModuleDir: dir,
		StacUsername: "u", StacPassword: "p",
		StacBaseURL:             "https://stac.example.org",
		StacBuildingsCollection: "buildings-vector",
		StacLidarCollection:     "lidar-pointcloud",
		StacBboxEPSG:            4326,
	})

	// Berlin: outside Lantmäteriet coverage, fine for a custom catalog.
	if _, err := s.Derive(context.Background(), 52.52, 13.40, "", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub recorded nothing: %v", err)
	}
	var got stubInvocation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got.Args, " ")
	for _, want := range []string{
		"--stac-base-url https://stac.example.org",
		"--buildings-collection buildings-vector",
		"--lidar-collection lidar-pointcloud",
		"--bbox-epsg 4326",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
}

// An open catalog needs no credentials: with a custom base URL and neither
// half of a credential stored, the derive runs anonymously and no empty
// --username/--password ever reaches the command line.
func TestDeriveCustomCatalogWorksAnonymously(t *testing.T) {
	dir := t.TempDir()
	record := dir + string(os.PathSeparator) + "invocation.json"
	cmd := stubModule(t, "record", record)
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: cmd, ModuleDir: dir,
		StacBaseURL: "https://stac.example.org",
	})

	if _, err := s.Derive(context.Background(), 52.52, 13.40, "", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub recorded nothing: %v", err)
	}
	var got stubInvocation
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got.Args, " ")
	if !strings.Contains(line, "--stac-base-url https://stac.example.org") {
		t.Errorf("args %q missing the base URL", line)
	}
	for _, banned := range []string{"--username", "--password"} {
		if strings.Contains(line, banned) {
			t.Errorf("args %q carry %s despite no stored credential", line, banned)
		}
	}
}

// Without a custom catalog the Sweden gate still holds — the redesign must
// not have quietly opened the default catalog to the whole planet.
func TestDeriveDefaultCatalogStillRefusesOutsideSweden(t *testing.T) {
	s := svc(t, &config.RoofModel{
		Enabled: true, Command: "no-such-command",
		StacUsername: "u", StacPassword: "p",
	})
	_, err := s.Derive(context.Background(), 52.52, 13.40, "", nil)
	if !errors.Is(err, ErrOutsideCoverage) {
		t.Errorf("err = %v, want ErrOutsideCoverage", err)
	}
}
