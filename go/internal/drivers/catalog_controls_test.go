package drivers

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDriver puts a .lua file in a temp dir and parses it the way the
// catalog does, so these tests exercise the real entry point rather than
// pickControls alone.
func writeDriver(t *testing.T, body string) CatalogEntry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := ParseCatalogFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return entry
}

const offsetControl = `DRIVER = {
  id      = "probe",
  name    = "Probe",
  version = "1.0.0",
  controls = {
    {
      id       = "set_heat_curve_offset",
      label    = "Heat curve offset",
      evidence = "readback",
      input    = { type = "number", min = -3, max = 3, step = 1, unit = "°C" },
    },
  },
}
`

func TestControlsParseFromDriverBlock(t *testing.T) {
	entry := writeDriver(t, offsetControl)

	if entry.ID != "probe" || entry.Version != "1.0.0" {
		t.Fatalf("nested controls broke the surrounding block: %+v", entry)
	}
	if len(entry.Controls) != 1 {
		t.Fatalf("controls = %d, want 1", len(entry.Controls))
	}
	got := entry.Controls[0]
	if got.ID != "set_heat_curve_offset" || got.Label != "Heat curve offset" ||
		got.Evidence != "readback" {
		t.Errorf("control = %+v", got)
	}
	if got.Input.Type != "number" || got.Input.Unit != "°C" {
		t.Errorf("input = %+v", got.Input)
	}
	for name, pair := range map[string]struct {
		got  *float64
		want float64
	}{
		"min":  {got.Input.Min, -3},
		"max":  {got.Input.Max, 3},
		"step": {got.Input.Step, 1},
	} {
		if pair.got == nil || *pair.got != pair.want {
			t.Errorf("%s = %v, want %v", name, pair.got, pair.want)
		}
	}
}

// A driver that declares nothing must stay exactly as it parsed before, or
// every existing catalog entry changes shape for a field it never had.
func TestControlsAbsentLeavesEntryUnchanged(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  capabilities = { "battery" },
}
`)
	if entry.Controls != nil {
		t.Errorf("controls = %+v, want nil", entry.Controls)
	}
	if len(entry.Capabilities) != 1 || entry.Capabilities[0] != "battery" {
		t.Errorf("capabilities = %v", entry.Capabilities)
	}
}

// Each of these is a declaration the UI could not render. Surfacing one puts
// a broken control in front of an operator; dropping it leaves the driver
// exactly as visible as it was before it tried.
func TestControlsDropUnrenderableDeclarations(t *testing.T) {
	cases := map[string]string{
		"no id":          `{ input = { type = "number", min = 0, max = 1 } }`,
		"bad id":         `{ id = "Set Offset", input = { type = "number", min = 0, max = 1 } }`,
		"no input":       `{ id = "set_offset" }`,
		"unknown type":   `{ id = "set_offset", input = { type = "table" } }`,
		"number no min":  `{ id = "set_offset", input = { type = "number", max = 3 } }`,
		"number no max":  `{ id = "set_offset", input = { type = "number", min = -3 } }`,
		"inverted range": `{ id = "set_offset", input = { type = "number", min = 3, max = -3 } }`,
		"bad evidence":   `{ id = "set_offset", evidence = "trust_me", input = { type = "number", min = 0, max = 1 } }`,
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			parsed := writeDriver(t, "DRIVER = {\n  id = \"probe\",\n  controls = {\n    "+entry+",\n  },\n}\n")
			if len(parsed.Controls) != 0 {
				t.Errorf("controls = %+v, want none", parsed.Controls)
			}
		})
	}
}

// Zero is a real bound. If min were an ordinary float, an undeclared minimum
// and a minimum of zero would be the same value and a 0..100 control would
// silently become unbounded below.
func TestControlsKeepZeroBound(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "set_limit", input = { type = "number", min = 0, max = 100, unit = "%" } },
  },
}
`)
	if len(entry.Controls) != 1 {
		t.Fatalf("controls = %d, want 1", len(entry.Controls))
	}
	min := entry.Controls[0].Input.Min
	if min == nil || *min != 0 {
		t.Errorf("min = %v, want 0", min)
	}
}

func TestControlsAcceptSeveralAndNonNumericInputs(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
    { id = "set_boost", label = "Hot water boost", input = { type = "boolean" } },
  },
}
`)
	if len(entry.Controls) != 2 {
		t.Fatalf("controls = %d, want 2", len(entry.Controls))
	}
	if entry.Controls[1].ID != "set_boost" || entry.Controls[1].Input.Type != "boolean" {
		t.Errorf("second control = %+v", entry.Controls[1])
	}
}

// The brace matcher has to survive a brace inside a quoted string, or a label
// mentioning one truncates the declaration.
func TestControlsSurviveBraceInsideString(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "set_offset", label = "Offset {zone 1}", input = { type = "number", min = -3, max = 3 } },
  },
}
`)
	if len(entry.Controls) != 1 {
		t.Fatalf("controls = %d, want 1", len(entry.Controls))
	}
	if entry.Controls[0].Label != "Offset {zone 1}" {
		t.Errorf("label = %q", entry.Controls[0].Label)
	}
}

func TestControlsMatchConfiguredPathBeforeFilenameFallback(t *testing.T) {
	local := []CatalogControl{{ID: "local"}}
	bundled := []CatalogControl{{ID: "bundled"}}
	catalog := []CatalogEntry{
		{Path: "drivers/foo.lua", Filename: "foo.lua", Controls: local},
		{Path: "drivers/selected/foo.lua", Filename: "foo.lua", Controls: bundled},
	}

	got := ControlsForDriver(catalog, "drivers/selected/foo.lua")
	if len(got) != 1 || got[0].ID != "bundled" {
		t.Fatalf("controls = %+v, want the path-matched entry", got)
	}
}

func TestControlsIgnoreCommentedDeclarations(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  -- controls = { { id = "line_disabled", input = { type = "number", min = 0, max = 1 } } },
  --[[ controls = { { id = "block_disabled", input = { type = "number", min = 0, max = 1 } } } ]]
}
`)
	if len(entry.Controls) != 0 {
		t.Fatalf("commented controls = %+v, want none", entry.Controls)
	}
}

func TestControlsParseExponentNumbers(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "set_limit", input = { type = "number", min = -1e-3, max = 1e6, step = 1e-1 } },
  },
}
`)
	if len(entry.Controls) != 1 {
		t.Fatalf("controls = %+v, want one exponent-valued control", entry.Controls)
	}
	input := entry.Controls[0].Input
	if input.Min == nil || *input.Min != -1e-3 || input.Max == nil || *input.Max != 1e6 ||
		input.Step == nil || *input.Step != 1e-1 {
		t.Fatalf("input = %+v, want min=-1e-3 max=1e6 step=1e-1", input)
	}
}

func TestControlsRejectMalformedExponentInsteadOfParsingPrefix(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "set_limit", input = { type = "number", min = 0, max = 1e6oops } },
  },
}
`)
	if len(entry.Controls) != 0 {
		t.Fatalf("malformed exponent controls = %+v, want none", entry.Controls)
	}
}

// Every bundled driver must still parse. This is the check that would catch
// a controls reader that quietly ate an unrelated field.
func TestBundledCatalogStillParses(t *testing.T) {
	entries, err := LoadCatalog(filepath.Join("..", "..", "..", "drivers"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no bundled drivers parsed")
	}
	for _, e := range entries {
		if e.ID == "" {
			continue // files without a DRIVER block are returned deliberately
		}
		if e.Name == "" {
			t.Errorf("%s: name went missing", e.ID)
		}
		for _, c := range e.Controls {
			if c.ID == "" || c.Input.Type == "" {
				t.Errorf("%s: surfaced an unrenderable control %+v", e.ID, c)
			}
		}
	}
}
