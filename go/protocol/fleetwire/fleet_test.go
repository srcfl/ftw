package fleetwire

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const valid = `{"schema":"ftw.fleet/1","ftw_version":"v0.146.0-beta.1","channel":"beta","drivers":["pixii","pixii_pv"],"battery_kwh":"5-15","price_zone":"SE4","install_age":"1-6m"}`

func TestOnlySharedLabelsAndBucketsAreAccepted(t *testing.T) {
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"beta"`, `"stable"`, 1),
		strings.Replace(valid, `"pixii","pixii_pv"`, `"pixii","pixii"`, 1),
		strings.Replace(valid, `"pixii","pixii_pv"`, `"pixii_pv","pixii"`, 1),
		strings.Replace(valid, `["pixii","pixii_pv"]`, `null`, 1),
		strings.Replace(valid, `"SE4"`, `"myhouse"`, 1),
		strings.Replace(valid, `"5-15"`, `"12.456"`, 1),
		strings.Replace(valid, `"v0.146.0-beta.1"`, `"abcdef-dirty"`, 1),
		strings.TrimSuffix(valid, "}") + `,"home":"secret"}`,
		valid + valid, `[]`, `null`, strings.Repeat(" ", MaxBodyBytes+1) + valid,
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestCountersHaveFixedLimitsAndNoReportSurvives(t *testing.T) {
	d := NewDay("2026-10-17")
	for i := 0; i < MaxReports; i++ {
		if !d.Add(Report{Version: fmt.Sprint(i), Drivers: []string{fmt.Sprint(i)}, Channel: "beta", Battery: "5-15", Zone: "SE4", Age: "1-6m"}) {
			t.Fatal(i)
		}
	}
	if d.Add(Report{}) || len(d.Versions) > MaxLabels || len(d.Drivers) > MaxLabels || d.Reports != MaxReports {
		t.Fatal(d.Reports, len(d.Versions))
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), `"home"`) {
		t.Fatal("identifier persisted")
	}
}

const validV2 = `{"schema":"ftw.fleet/2","ftw_version":"v0.146.0-beta.1","channel":"beta","drivers":["pixii","pixii_pv"],"battery_kwh":"5-15","price_zone":"SE4","install_age":"1-6m","plan_solve":"1-5s","plan_fallbacks":"0","driver_errors":"0-1%","update":"none"}`

// A report that says how the box ran carries four health buckets, each from
// a short fixed list; one without them is the older shape, and nothing else
// passes.
func TestHealthBucketsAreAFewFixedWords(t *testing.T) {
	r, err := Decode([]byte(validV2))
	if err != nil || r.Solve != "1-5s" || r.Fallbacks != "0" || r.Errors != "0-1%" || r.Update != "none" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{
		strings.Replace(validV2, `"1-5s"`, `"2.3s"`, 1),
		strings.Replace(validV2, `"plan_fallbacks":"0"`, `"plan_fallbacks":"7"`, 1),
		strings.Replace(validV2, `"0-1%"`, `"0.4%"`, 1),
		strings.Replace(validV2, `"update":"none"`, `"update":"v0.145.0"`, 1),
		strings.Replace(validV2, `,"update":"none"`, ``, 1),
		strings.Replace(validV2, `"plan_solve":"1-5s"`, `"plan_solve":""`, 1),
		strings.Replace(validV2, `"ftw.fleet/2"`, `"ftw.fleet/1"`, 1),
		strings.Replace(valid, `"ftw.fleet/1"`, `"ftw.fleet/2"`, 1),
		strings.TrimSuffix(validV2, "}") + `,"home":"secret"}`,
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	for _, list := range [][]string{SolveBuckets, FallbackBuckets, ErrorBuckets, UpdateBuckets} {
		if len(list) > 4 {
			t.Fatalf("%v is wider than four buckets", list)
		}
	}
}

// A day counts health only from reports that carry it, and a day without
// any has no health fields at all.
func TestADayCountsHealthOnlyFromReportsThatCarryIt(t *testing.T) {
	v1, _ := Decode([]byte(valid))
	v2, _ := Decode([]byte(validV2))
	d := NewDay("2026-10-24")
	d.Add(v1)
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), "plan_solve") || strings.Contains(string(data), `"update"`) {
		t.Fatalf("a day of older reports has health fields: %s", data)
	}
	d.Add(v2)
	d.Add(v2)
	if d.Reports != 3 || d.Solve["1-5s"] != 2 || d.Fallbacks["0"] != 2 || d.Errors["0-1%"] != 2 || d.Update["none"] != 2 || d.Battery["5-15"] != 3 {
		t.Fatalf("%+v", d)
	}
	// A day stored before health existed takes it once a report brings it.
	var old Day
	_ = json.Unmarshal([]byte(`{"date":"2026-10-24","reports":1,"ftw_versions":{},"channels":{},"drivers":{},"battery_kwh":{},"price_zones":{},"install_age":{}}`), &old)
	if !old.Add(v2) || old.Update["none"] != 1 {
		t.Fatalf("%+v", old)
	}
}
