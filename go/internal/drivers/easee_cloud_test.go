package drivers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func easeeCloudDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repo, "drivers", "easee_cloud.lua")
}

// loadEaseePhaseFunctions evaluates the real driver source while promoting
// its two local, pure phase helpers to test globals. Keeping the functions in
// the original Lua chunk means the test exercises the shipped constants and
// implementation without making driver internals part of the host API.
func loadEaseePhaseFunctions(t *testing.T) *lua.LState {
	t.Helper()
	src, err := os.ReadFile(easeeCloudDriverPath(t))
	if err != nil {
		t.Fatalf("read Easee driver: %v", err)
	}
	promoted := strings.Replace(string(src),
		"local function pick_phases(", "function __test_pick_phases(", 1)
	promoted = strings.Replace(promoted,
		"local function per_phase_amps(", "function __test_per_phase_amps(", 1)

	L := lua.NewState()
	if err := L.DoString(promoted); err != nil {
		L.Close()
		t.Fatalf("load Easee driver source: %v", err)
	}
	t.Cleanup(L.Close)
	return L
}

func callEaseePickPhases(t *testing.T, L *lua.LState, mode string, powerW, voltage, maxA, splitW float64) int {
	t.Helper()
	fn := L.GetGlobal("__test_pick_phases")
	err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true},
		lua.LString(mode), lua.LNumber(powerW), lua.LNumber(voltage),
		lua.LNumber(maxA), lua.LNumber(splitW), lua.LNumber(90), lua.LNumber(1_000_000))
	if err != nil {
		t.Fatalf("pick_phases: %v", err)
	}
	defer L.Pop(1)
	return int(lua.LVAsNumber(L.Get(-1)))
}

func callEaseePerPhaseAmps(t *testing.T, L *lua.LState, powerW, voltage float64, phases int, maxA float64) int {
	t.Helper()
	fn := L.GetGlobal("__test_per_phase_amps")
	err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true},
		lua.LNumber(powerW), lua.LNumber(voltage), lua.LNumber(phases), lua.LNumber(maxA))
	if err != nil {
		t.Fatalf("per_phase_amps: %v", err)
	}
	defer L.Pop(1)
	return int(lua.LVAsNumber(L.Get(-1)))
}

func TestEaseeAutoPhaseAvoidsMinimumCurrentDeadZone(t *testing.T) {
	tests := []struct {
		name              string
		powerW, voltage   float64
		maxA, splitW      float64
		wantPhases, wantA int
	}{
		{name: "one phase ceiling", powerW: 3680, voltage: 230, maxA: 16, wantPhases: 1, wantA: 16},
		{name: "old dead zone", powerW: 4000, voltage: 230, maxA: 16, wantPhases: 1, wantA: 16},
		{name: "last watt below 3p minimum", powerW: 4139, voltage: 230, maxA: 16, wantPhases: 1, wantA: 16},
		{name: "first deliverable 3p step", powerW: 4140, voltage: 230, maxA: 16, wantPhases: 3, wantA: 6},
		{name: "low custom split cannot create gap", powerW: 3600, voltage: 230, maxA: 16, splitW: 3000, wantPhases: 1, wantA: 16},
		{name: "minimum follows live voltage", powerW: 4320, voltage: 240, maxA: 16, wantPhases: 3, wantA: 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			L := loadEaseePhaseFunctions(t)
			phases := callEaseePickPhases(t, L, "auto", tc.powerW, tc.voltage, tc.maxA, tc.splitW)
			if phases != tc.wantPhases {
				t.Fatalf("phases = %d, want %d", phases, tc.wantPhases)
			}
			amps := callEaseePerPhaseAmps(t, L, tc.powerW, tc.voltage, phases, tc.maxA)
			if amps != tc.wantA {
				t.Errorf("amps = %d, want %d", amps, tc.wantA)
			}
		})
	}
}

func TestEaseeLockedThreePhaseStillHonorsOperatorChoice(t *testing.T) {
	L := loadEaseePhaseFunctions(t)
	phases := callEaseePickPhases(t, L, "3p", 3600, 230, 16, 0)
	if phases != 3 {
		t.Fatalf("phases = %d, want locked 3p", phases)
	}
	if amps := callEaseePerPhaseAmps(t, L, 3600, 230, phases, 16); amps != 0 {
		t.Errorf("amps = %d, want 0 below Easee minimum in explicitly locked 3p mode", amps)
	}
}
