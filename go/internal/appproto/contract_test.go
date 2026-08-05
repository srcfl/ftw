package appproto

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
	"github.com/srcfl/ftw/go/internal/control"
)

const registryPath = "../../../contract/registry.yaml"

// The generated constants must be what the registry currently says. A snapshot
// updated without rerunning the generator is exactly the drift the registry
// exists to prevent.
func TestContractGenIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	want, err := gencontract.Generate(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("contract_gen.go")
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("contract_gen.go is stale; run: go generate ./internal/appproto/...\n"+
			"(registry: %s)", filepath.Clean(registryPath))
	}
}

// The mode keys in the registry are this repository's, copied there so the app
// can validate them. control.ModeCatalog() stays the authority; this catches
// the two drifting apart.
func TestRegistryModesMatchCatalog(t *testing.T) {
	cat := control.ModeCatalog()
	if len(cat) != len(RegistryModeTiers) {
		t.Fatalf("catalogue has %d modes, registry has %d", len(cat), len(RegistryModeTiers))
	}
	for _, m := range cat {
		tier, ok := RegistryModeTiers[string(m.Key)]
		if !ok {
			t.Fatalf("mode %q is in the catalogue but not in contract/registry.yaml", m.Key)
		}
		if tier != string(m.Tier) {
			t.Fatalf("mode %q: catalogue tier %q, registry tier %q", m.Key, m.Tier, tier)
		}
	}
}

// Field ids 1-9 are frozen permanently as of v1. An app of any age draws the
// core view from these alone, so a change here breaks installs nobody can
// reach.
func TestFrozenFieldIdsAreFrozen(t *testing.T) {
	want := map[Fid]string{
		1: "mode",
		2: "grid_w",
		3: "pv_w",
		4: "battery_w",
		5: "battery_soc",
		6: "load_w",
		7: "src_grid",
		8: "src_pv",
		9: "src_battery",
	}
	for fid, name := range want {
		if got := FrozenFieldNames[fid]; got != name {
			t.Fatalf("field %d is %q, and was frozen as %q", fid, got, name)
		}
	}
	if len(FrozenFieldNames) != len(want) {
		t.Fatalf("the frozen set has %d fields, want %d", len(FrozenFieldNames), len(want))
	}
}

// Everything the box advertises must be something it can answer. A capability
// the app cannot use is hidden; one it can ask for and not get is a hang.
func TestDefaultCapsAreAllRegistryNames(t *testing.T) {
	known := map[string]bool{}
	for _, c := range AllCapabilities {
		known[c] = true
	}
	for _, c := range DefaultCaps() {
		if !known[c] {
			t.Fatalf("%q is not a capability in contract/registry.yaml", c)
		}
	}
}
