package forecasting

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func testBand() Band {
	return Band{LowW: 0, HighW: 5000, Method: BandMethodColdStart}
}

func testPoint(start, end int64, pv, load float64) Point {
	return Point{
		StartMS: start, EndMS: end, PVW: pv, LoadW: load, PVKnown: true, LoadKnown: true,
		PVQuality: "measured", LoadQuality: "measured", PVBand: testBand(), LoadBand: testBand(), NetBand: testBand(),
	}
}

func testIssue(id, config, series string, origin, issued int64, points []Point) Issue {
	return Issue{
		Schema: Schema, ID: id, OriginMS: origin, IssuedAtMS: issued, LatestInputMS: origin,
		ConfigVersion: config, Series: []Series{{Name: series, ModelVersion: "v1", Points: points}},
	}
}

func TestIssueBoundsFullPlanningHorizon(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	origin := start + 7*time.Minute.Milliseconds()
	for _, field := range []string{"series", "weather"} {
		for _, count := range []int{192, 193, 194} {
			t.Run(fmt.Sprintf("%s_%d", field, count), func(t *testing.T) {
				issue := testIssue("horizon", "cfg", "champion", origin, origin, []Point{testPoint(start, start+900000, 100, 1000)})
				for i := 0; i < count; i++ {
					at := start + int64(i)*900000
					if field == "series" {
						if i == 0 {
							issue.Series[0].Points = nil
						}
						issue.Series[0].Points = append(issue.Series[0].Points, testPoint(at, at+900000, 100, 1000))
					} else {
						issue.Weather = append(issue.Weather, Weather{StartMS: at, EndMS: at + 900000, AvailableAtMS: origin, Source: "open_meteo"})
					}
				}
				issue.Series[0].Points[0].PredictionStartMS = origin
				if err := issue.Validate(); (err == nil) != (count <= 193) {
					t.Fatalf("%d %s intervals: %v", count, field, err)
				}
			})
		}
	}
}

func TestIssueRejectsUnknownOrFutureWeatherAvailability(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	issue := testIssue("i", "cfg", "champion", start-hourMS, start-hourMS, []Point{testPoint(start, start+hourMS, 100, 1000)})
	ghi, cloud, temp := 500.0, 40.0, 15.0
	issue.Weather = []Weather{{StartMS: start, EndMS: start + hourMS, AvailableAtMS: 0, Source: "open_meteo", GHIWm2: &ghi, CloudPct: &cloud, TempC: &temp}}
	if err := issue.Validate(); err == nil {
		t.Fatal("unknown weather receipt time must not be eligible")
	}
	issue.Weather[0].AvailableAtMS = issue.OriginMS + 1
	if err := issue.Validate(); err == nil {
		t.Fatal("future weather receipt time must not be eligible")
	}
	issue.Weather[0].AvailableAtMS = issue.OriginMS
	if err := issue.Validate(); err != nil {
		t.Fatalf("valid weather rejected: %v", err)
	}
	badCloud := 101.0
	issue.Weather[0].CloudPct = &badCloud
	if err := issue.Validate(); err == nil {
		t.Fatal("cloud cover above 100 must be rejected")
	}
	issue.Weather[0].CloudPct = &cloud
	badGHI := -1.0
	issue.Weather[0].GHIWm2 = &badGHI
	if err := issue.Validate(); err == nil {
		t.Fatal("negative irradiance must be rejected")
	}
	issue.Weather[0].GHIWm2 = &ghi
	badPV := math.Inf(1)
	issue.Weather[0].DirectPVW = &badPV
	if err := issue.Validate(); err == nil {
		t.Fatal("nonfinite direct provider PV must be rejected")
	}
}

func TestIssueModelStateQualityControlsTime(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	issue := testIssue("i", "cfg", "champion", start-hourMS, start-hourMS, []Point{testPoint(start, start+hourMS, 100, 1000)})
	issue.Models = []ModelState{{Name: "pv", Version: "v1", Quality: ModelQualityColdStart, State: json.RawMessage(`{}`)}}
	if err := issue.Validate(); err != nil {
		t.Fatalf("cold-start state rejected: %v", err)
	}
	issue.Models[0].State = nil
	if err := issue.Validate(); err != nil {
		t.Fatalf("empty cold-start model rejected: %v", err)
	}
	issue.Models[0].State = json.RawMessage(`{}`)
	issue.Models[0].UpdatedAtMS = issue.OriginMS
	if err := issue.Validate(); err == nil {
		t.Fatal("cold-start state must have zero update time")
	}
	issue.Models[0].Quality = ModelQualityWarm
	if err := issue.Validate(); err != nil {
		t.Fatalf("warm state at origin rejected: %v", err)
	}
	issue.Models[0].UpdatedAtMS = 0
	if err := issue.Validate(); err == nil {
		t.Fatal("warm state must have a positive update time")
	}
	issue.Models[0].UpdatedAtMS = issue.OriginMS
	issue.Models[0].State = nil
	issue.Models[0].StateID = strings.Repeat("a", 64)
	if err := issue.Validate(); err != nil {
		t.Fatalf("valid state reference rejected: %v", err)
	}
	issue.Models[0].State = json.RawMessage(`{}`)
	if err := issue.Validate(); err == nil {
		t.Fatal("payload and reference together must be rejected")
	}
}

func TestIssueRejectsUntrustedEmpiricalBand(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	point := testPoint(start, start+hourMS, 100, 1000)
	point.PVBand = Band{LowW: 0, HighW: 1000, Method: BandMethodEmpirical, Samples: 47, Days: 7}
	issue := testIssue("i", "cfg", "champion", start-hourMS, start-hourMS, []Point{point})
	if err := issue.Validate(); err == nil {
		t.Fatal("empirical band below the sample floor must be rejected")
	}
	point.PVBand = Band{LowW: 0, HighW: 1000, Method: BandMethodEmpirical, Samples: 48, Days: 6}
	issue.Series[0].Points[0] = point
	if err := issue.Validate(); err == nil {
		t.Fatal("empirical band below the independent-day floor must be rejected")
	}
	point.PVBand = Band{LowW: 0, HighW: math.Inf(1), Method: BandMethodColdStart}
	issue.Series[0].Points[0] = point
	if err := issue.Validate(); err == nil {
		t.Fatal("nonfinite band must be rejected")
	}
}

func TestIssueKeepsProvisionalModelEvidenceDistinct(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	point := testPoint(start, start+hourMS, 100, 1000)
	point.ModelPV = &ModelEstimateEvidence{LowerW: 50, UpperW: 180, Uncertainty: "provisional", Coverage: .75}
	point.ModelLoad = &ModelEstimateEvidence{LowerW: 800, UpperW: 1200, Uncertainty: "provisional", Coverage: .5}
	issue := testIssue("i", "cfg", "champion", start-hourMS, start-hourMS, []Point{point})
	if err := issue.Validate(); err != nil {
		t.Fatalf("valid model evidence rejected: %v", err)
	}
	data, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"model_pv"`) || !strings.Contains(text, `"model_load"`) ||
		!strings.Contains(text, `"pv_band"`) || !strings.Contains(text, `"load_band"`) {
		t.Fatalf("model evidence and empirical bands are not distinct: %s", text)
	}
	point.ModelPV.Coverage = 1.01
	issue.Series[0].Points[0] = point
	if err := issue.Validate(); err == nil {
		t.Fatal("coverage above one must be rejected")
	}
	point.ModelPV.Coverage = .75
	point.ModelPV.LowerW = -1
	issue.Series[0].Points[0] = point
	if err := issue.Validate(); err == nil {
		t.Fatal("negative model range must be rejected")
	}
	point.ModelPV.LowerW = 50
	point.ModelPV.Uncertainty = "empirical"
	issue.Series[0].Points[0] = point
	if err := issue.Validate(); err == nil {
		t.Fatal("model-native range must remain provisional")
	}
}
