package homelink

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestReadTargetsAreFixedCoreGETs(t *testing.T) {
	want := map[Scope]string{
		ScopeStatusRead:        "/api/status",
		ScopeHealthRead:        "/api/health",
		ScopePlanRead:          "/api/mpc/plan",
		ScopeEnergyAssetsRead:  "/api/energy/assets",
		ScopeEnergyHistoryRead: "/api/energy/history",
	}
	for scope, path := range want {
		request := ReadRequest{Version: ReadContractVersion, GatewayID: testGatewayID, Scope: scope}
		if scope == ScopeEnergyHistoryRead {
			request.History = validHistoryQuery()
		}
		target, err := request.AuthorizedTarget(testAuthorization(scope))
		if err != nil {
			t.Fatalf("target %q: %v", scope, err)
		}
		if target.Method != http.MethodGet || target.Path != path {
			t.Errorf("target %q = %+v, want GET %s", scope, target, path)
		}
	}

	request := ReadRequest{Version: ReadContractVersion, GatewayID: testGatewayID, Scope: Scope("ftw.control.write")}
	if _, err := request.AuthorizedTarget(testAuthorization(ScopeStatusRead)); err == nil {
		t.Fatal("write scope resolved to a Core target")
	}
}

func TestReadTargetRequiresMatchingConsumedGrant(t *testing.T) {
	request := ReadRequest{Version: ReadContractVersion, GatewayID: testGatewayID, Scope: ScopeStatusRead}
	if _, err := request.AuthorizedTarget(Authorization{}); err == nil {
		t.Fatal("zero authorization resolved a Core target")
	}
	if _, err := request.AuthorizedTarget(testAuthorization(ScopeHealthRead)); err != ErrWrongScope {
		t.Fatalf("wrong-scope authorization = %v", err)
	}
	wrongSite := testAuthorization(ScopeStatusRead)
	wrongSite.gatewayID = "0123aabbcc01ddeeff"
	if _, err := request.AuthorizedTarget(wrongSite); err != ErrWrongSite {
		t.Fatalf("wrong-site authorization = %v", err)
	}
}

func TestHistoryQueryIsBoundedByLedgerV1(t *testing.T) {
	valid := *validHistoryQuery()
	if err := ValidateHistoryQuery(valid); err != nil {
		t.Fatal(err)
	}

	cases := []state.EnergyHistoryQuery{
		{SinceMS: 2, UntilMS: 1, BucketMS: state.EnergyLedgerBucketMS, Limit: 1},
		{SinceMS: 1, UntilMS: 1 + state.EnergyLedgerRetention.Milliseconds() + 1, BucketMS: state.EnergyLedgerRollupBucketMS, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS - 1, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: MaxHistoryWindowMS + 1, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS, Limit: 0},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS, Limit: MaxHistoryLimit + 1},
	}
	for _, query := range cases {
		if err := ValidateHistoryQuery(query); err == nil {
			t.Fatalf("accepted unbounded history query: %+v", query)
		}
	}
}

func TestEnergyTypesAreLedgerV1Types(t *testing.T) {
	if EnergyLedgerSchemaVersion != 1 {
		t.Fatalf("ledger schema = %d", EnergyLedgerSchemaVersion)
	}
	point := EnergyLedgerPoint{
		SchemaVersion: 1, AssetID: "site/grid_meter", Flow: state.FlowGridImport,
		BucketStartMS: 1, BucketLenMS: state.EnergyLedgerBucketMS, EnergyWh: 2,
		Source: "hardware_counter", Quality: "measured", Provenance: "counter", SampleCount: 1,
	}
	raw, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema_version", "asset_id", "flow", "bucket_start_ms", "bucket_len_ms", "energy_wh", "source", "quality", "provenance", "sample_count"}
	for _, field := range want {
		if _, ok := fields[field]; !ok {
			t.Fatalf("ledger point lacks %q: %s", field, raw)
		}
	}
	if reflect.TypeOf(point) != reflect.TypeOf(state.EnergyLedgerPoint{}) {
		t.Fatal("remote history created a second point type")
	}
}

func validHistoryQuery() *state.EnergyHistoryQuery {
	until := time.Unix(1_800_000_000, 0).UnixMilli()
	return &state.EnergyHistoryQuery{
		SinceMS: until - int64(24*time.Hour/time.Millisecond), UntilMS: until,
		BucketMS: int64(15 * time.Minute / time.Millisecond), Limit: 100,
	}
}

func testAuthorization(scope Scope) Authorization {
	return Authorization{
		gatewayID: testGatewayID,
		purpose:   GrantPurposeAccess,
		scope:     scope,
		principal: testPrincipal(),
	}
}
