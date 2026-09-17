package assistant

import (
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/typesafe"
)

func TestExtractPercentsAndClocks(t *testing.T) {
	if got := ExtractPercents("Ladda till 80% sen 90%"); len(got) != 2 || got[0] != 80 || got[1] != 90 {
		t.Fatalf("percents = %v", got)
	}
	if got := ExtractPercents("no percent here"); len(got) != 0 {
		t.Fatalf("percents = %v", got)
	}
	if got := ExtractClocks("klar till 7:00 och sen 18.30"); len(got) != 2 || got[0] != "07:00" || got[1] != "18:30" {
		t.Fatalf("clocks = %v", got)
	}
}

func TestHouseholdQuestionsIncludeExtractedCandidates(t *testing.T) {
	qs := HouseholdQuestions(SiteContext{
		Utterance:  "80% by 07:15 on the garage charger",
		Loadpoints: []LoadpointRef{{ID: "garage", Name: "Garage"}},
	})
	soc := qs[houseQSoC].Criteria.(map[string]string)
	if _, ok := soc["80"]; !ok {
		t.Fatal("extracted 80% must be a Choice option")
	}
	dead := qs[houseQDeadline].Criteria.(map[string]string)
	if _, ok := dead["07:15"]; !ok {
		t.Fatal("extracted 07:15 must be a Choice option")
	}
	lp := qs[houseQLoadpoint].Criteria.(map[string]string)
	if _, ok := lp["garage"]; !ok {
		t.Fatal("configured loadpoint must be a Choice option")
	}
}

func TestComposeHouseholdChargeNowUsesExistingHold(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage", Name: "Garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionChargeNow, 0.93),
		houseQUnsafe:    noul(0.04),
		houseQDurable:   noul(0.08),
		houseQSoCStated: noul(0.9),
		houseQSoC:       choice("80", 0.88),
		houseQDeadStat:  noul(0.05),
		houseQDeadline:  choice(houseUnstated, 0.9),
		houseQDays:      choice(houseDaysUnstated, 0.8),
		houseQLoadpoint: choice("garage", 0.7),
	}}
	got := ComposeHouseholdIntent(site, res)
	if got.Outcome != HouseholdPropose || got.CoreOp != appproto.OpLoadpointHold {
		t.Fatalf("got %+v", got)
	}
	if got.LoadpointID != "garage" || got.SoCPct == nil || *got.SoCPct != 80 || got.Recurring {
		t.Fatalf("args %+v", got)
	}
}

func TestComposeHouseholdWeekdaySchedule(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionSchedule, 0.91),
		houseQUnsafe:    noul(0.03),
		houseQDurable:   noul(0.94),
		houseQSoCStated: noul(0.96),
		houseQSoC:       choice("80", 0.9),
		houseQDeadStat:  noul(0.97),
		houseQDeadline:  choice("07:00", 0.92),
		houseQDays:      choice(houseDaysWeekdays, 0.9),
		houseQLoadpoint: choice("garage", 0.8),
	}}
	got := ComposeHouseholdIntent(site, res)
	if got.Outcome != HouseholdPropose || got.CoreOp != "loadpoint.schedule" {
		t.Fatalf("got %+v", got)
	}
	if got.DeadlineLocal != "07:00" || got.Days != DaysWeekdays || !got.Durable || !got.Recurring {
		t.Fatalf("schedule %+v", got)
	}
}

func TestComposeHouseholdEverydayDoesNotBecomeWeekdays(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionSchedule, 0.9),
		houseQUnsafe:    noul(0.02),
		houseQDurable:   noul(0.9),
		houseQSoCStated: noul(0.9),
		houseQSoC:       choice("80", 0.9),
		houseQDeadStat:  noul(0.9),
		houseQDeadline:  choice("07:00", 0.9),
		houseQDays:      choice(houseDaysEveryday, 0.85),
		houseQLoadpoint: choice("garage", 0.8),
	}}
	got := ComposeHouseholdIntent(site, res)
	if !got.DaysKnown || got.Days != 0 {
		t.Fatalf("everyday must stay a zero mask, got %+v", got)
	}
}

func TestComposeHouseholdUnstatedDaysDefaultWeekdays(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionSchedule, 0.9),
		houseQUnsafe:    noul(0.02),
		houseQDurable:   noul(0.9),
		houseQSoCStated: noul(0.9),
		houseQSoC:       choice("80", 0.9),
		houseQDeadStat:  noul(0.9),
		houseQDeadline:  choice("07:00", 0.9),
		houseQDays:      choice(houseDaysUnstated, 0.8),
		houseQLoadpoint: choice("garage", 0.8),
	}}
	got := ComposeHouseholdIntent(site, res)
	if got.DaysKnown || got.Days != DaysWeekdays {
		t.Fatalf("unstated standing goal should default to weekdays, got %+v", got)
	}
}

func TestComposeHouseholdRefusesSafetyBypass(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionChargeNow, 0.99),
		houseQUnsafe:    noul(0.88),
		houseQDurable:   noul(0.1),
		houseQSoCStated: noul(0.1),
		houseQSoC:       choice(houseUnstated, 0.9),
		houseQDeadStat:  noul(0.1),
		houseQDeadline:  choice(houseUnstated, 0.9),
		houseQDays:      choice(houseDaysUnstated, 0.9),
		houseQLoadpoint: choice("garage", 0.9),
	}}
	got := ComposeHouseholdIntent(site, res)
	if got.Outcome != HouseholdRefuse || got.CoreOp != "" || !got.Unsafe {
		t.Fatalf("unsafe must not propose a Core write: %+v", got)
	}
}

func TestComposeHouseholdClarifiesScheduleMissingDeadline(t *testing.T) {
	site := SiteContext{Loadpoints: []LoadpointRef{{ID: "garage"}}}
	res := &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction:    choice(houseActionSchedule, 0.9),
		houseQUnsafe:    noul(0.02),
		houseQDurable:   noul(0.9),
		houseQSoCStated: noul(0.9),
		houseQSoC:       choice("80", 0.9),
		houseQDeadStat:  noul(0.1),
		houseQDeadline:  choice(houseUnstated, 0.8),
		houseQDays:      choice(houseDaysWeekdays, 0.8),
		houseQLoadpoint: choice("garage", 0.8),
	}}
	got := ComposeHouseholdIntent(site, res)
	if got.Outcome != HouseholdClarify {
		t.Fatalf("got %+v", got)
	}
}

func TestComposeHouseholdLowConfidenceExplains(t *testing.T) {
	got := ComposeHouseholdIntent(SiteContext{}, &typesafe.Result{Answers: map[string]typesafe.Answer{
		houseQAction: choice(houseActionChargeNow, 0.4),
		houseQUnsafe: noul(0.1),
	}})
	if got.Outcome != HouseholdExplain || got.CoreOp != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestLiveJevHouseholdPhrases(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" || os.Getenv("TYPESAFE_LIVE") != "1" {
		t.Skip("set TYPESAFE_API_KEY and TYPESAFE_LIVE=1 to call Jev")
	}
	site := SiteContext{
		Utterance:  "Ladda bilen till 80% till klockan 7 varje vardag",
		Drivers:    []string{"sungrow", "sdm630"},
		Loadpoints: []LoadpointRef{{ID: "garage", Name: "Garage"}},
		Snapshot:   "mode=automatic sungrow=ok garage plugged in estimated SoC 41%",
	}
	cli := &typesafe.Client{APIKey: key}
	res, err := cli.Evaluate(t.Context(), HouseholdState(site), HouseholdQuestions(site))
	if err != nil {
		t.Fatal(err)
	}
	got := ComposeHouseholdIntent(site, res)
	t.Logf("live household: action=%s outcome=%s op=%s soc=%v deadline=%s days=%d conf=%.2f",
		got.Action, got.Outcome, got.CoreOp, got.SoCPct, got.DeadlineLocal, got.Days, got.ActionConfidence)
	if got.Unsafe {
		t.Fatal("weekday ready-by must not be classified as a safety bypass")
	}
}
