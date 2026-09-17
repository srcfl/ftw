package assistant

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/typesafe"
)

// Household mapping is proposed: Jev fills arguments for operations Core
// already admits. This file does not call those APIs and does not
// dispatch. Unavailable TypeSafe means the existing UI and typed routes
// keep working.

const (
	houseActionConfidenceMin = 0.75
	houseUnsafeNoulMin       = 0.6
	houseArgNoulMin          = 0.55

	houseActionExplain     = "explain"
	houseActionChargeNow   = "charge_now"
	houseActionSchedule    = "set_schedule"
	houseActionVehicleSoC  = "set_vehicle_soc"
	houseActionUnsupported = "unsupported"

	houseDaysWeekdays = "weekdays"
	houseDaysEveryday = "everyday"
	houseDaysWeekend  = "weekend"
	houseDaysUnstated = "not_stated"
	houseUnstated     = "not_stated"

	houseQAction    = "action"
	houseQUnsafe    = "asks_to_bypass_safety"
	houseQDurable   = "is_standing_goal"
	houseQSoC       = "soc_pct"
	houseQSoCStated = "soc_stated"
	houseQDeadline  = "deadline_local"
	houseQDeadStat  = "deadline_stated"
	houseQDays      = "days"
	houseQLoadpoint = "loadpoint"
)

// DaysWeekdays is ISO bits 0..4 (Mon–Fri), matching loadpoint.Schedule.
const DaysWeekdays uint8 = 0b0011111

// DaysWeekend is Saturday and Sunday (bits 5 and 6).
const DaysWeekend uint8 = 0b1100000

var (
	percentRe = regexp.MustCompile(`\b(\d{1,3})\s*%`)
	clockRe   = regexp.MustCompile(`\b([01]?\d|2[0-3])[:.]([0-5]\d)\b`)
)

// HouseholdOutcome is what code should do with a composed intent.
type HouseholdOutcome string

const (
	// HouseholdPropose is a typed intent Core can admit or reject.
	HouseholdPropose HouseholdOutcome = "propose"
	// HouseholdClarify is missing a closed-set argument the operation needs.
	HouseholdClarify HouseholdOutcome = "clarify"
	// HouseholdExplain should go to Ask why, not a write.
	HouseholdExplain HouseholdOutcome = "explain"
	// HouseholdRefuse is unsafe or unsupported. Do not call Core writes.
	HouseholdRefuse HouseholdOutcome = "refuse"
)

// HouseholdIntent is a proposed Core operation plus closed-set arguments.
// CoreOp is an existing protocol or HTTP name; empty means do not call Core.
type HouseholdIntent struct {
	Outcome           HouseholdOutcome
	Action            string
	ActionConfidence  float64
	CoreOp            string
	LoadpointID       string
	SoCPct            *float64
	DeadlineLocal     string
	Recurring         bool
	Days              uint8
	DaysKnown         bool
	Durable           bool
	Unsafe            bool
	UnsupportedReason string
}

// HouseholdState adds pre-parsed percent and clock candidates so Jev
// selects a value that appeared in the utterance (or not_stated).
func HouseholdState(site SiteContext) map[string]any {
	state := AskState(site)
	state["percent_candidates"] = ExtractPercents(site.Utterance)
	state["clock_candidates"] = ExtractClocks(site.Utterance)
	return state
}

// HouseholdQuestions fans out the function-calling pattern: pick one
// existing operation and fill only closed-set arguments. Numbers FTW
// cannot list (arbitrary watts) stay out of Jev and keep their Core defaults.
func HouseholdQuestions(site SiteContext) map[string]typesafe.Question {
	socOpts := map[string]string{
		houseUnstated: "The utterance does not name a battery percentage.",
		"50":          "About half full.",
		"60":          "Sixty percent.",
		"70":          "Seventy percent.",
		"80":          "Eighty percent. The usual weekday ready-by target.",
		"90":          "Ninety percent.",
		"100":         "Full.",
	}
	for _, p := range ExtractPercents(site.Utterance) {
		socOpts[strconv.Itoa(p)] = fmt.Sprintf("The utterance names %d percent.", p)
	}

	deadOpts := map[string]string{
		houseUnstated: "No ready-by clock time is named.",
		"06:00":       "Six in the morning.",
		"07:00":       "Seven in the morning. The usual weekday example.",
		"08:00":       "Eight in the morning.",
		"09:00":       "Nine in the morning.",
		"17:00":       "Five in the afternoon.",
		"18:00":       "Six in the evening.",
	}
	for _, c := range ExtractClocks(site.Utterance) {
		deadOpts[c] = "The utterance names " + c + " as a clock time."
	}

	lpOpts := map[string]string{
		houseUnstated: "No specific charger is named, or the site has none.",
	}
	for _, lp := range site.Loadpoints {
		id := strings.TrimSpace(lp.ID)
		if id == "" || id == houseUnstated {
			continue
		}
		label := strings.TrimSpace(lp.Name)
		if label == "" {
			label = id
		}
		lpOpts[id] = "The charger " + label + " (id " + id + ")."
	}

	return map[string]typesafe.Question{
		houseQAction: typesafe.Choice(
			"Which existing FTW operation does this household request map to?",
			map[string]string{
				houseActionExplain:     "They want to understand the site. Route to Ask why. Do not change goals.",
				houseActionChargeNow:   "Start charging the plugged-in car now. One-shot; Core already has Charge now (loadpoint.hold with a release SoC).",
				houseActionSchedule:    "A standing ready-by goal: be at a SoC by a local time on selected days. This is a durable schedule, not a control lease.",
				houseActionVehicleSoC:  "Correct the estimated car battery level. Not a command to charge.",
				houseActionUnsupported: "Something FTW must not do from a sentence: power off an inverter, disable the site-meter watchdog, apply a planner slot directly to hardware, or otherwise bypass Core.",
			},
		),
		houseQUnsafe: typesafe.Noul(
			"Does the request ask to bypass FTW safety or to command hardware that Core does not expose as a household goal?",
			"Disable the site-meter watchdog, send planner output to devices, turn off an inverter, ignore fuse limits, or similar.",
			"A normal household goal or an explanation request.",
		),
		houseQDurable: typesafe.Noul(
			"Is this a standing household goal that should persist after the caller disconnects?",
			"Every weekday, every morning, keep this until I change it.",
			"One-shot: charge now, this trip, or a one-time correction.",
		),
		houseQSoCStated: typesafe.Noul(
			"Does the utterance name a battery percentage?",
			"A percent is stated, including words like 80% or eighty percent.",
			"No percentage is named.",
		),
		houseQSoC: typesafe.Choice("Which battery percentage in `percent_candidates` or the usual targets did they mean?", socOpts),
		houseQDeadStat: typesafe.Noul(
			"Does the utterance name a ready-by clock time?",
			"A time of day such as 07:00, 7:00, klockan 7.",
			"No clock time is named.",
		),
		houseQDeadline: typesafe.Choice("Which local clock time is the ready-by deadline?", deadOpts),
		houseQDays: typesafe.Choice(
			"Which days should a standing charging goal apply to?",
			map[string]string{
				houseDaysWeekdays: "Monday to Friday for this household, not UTC.",
				houseDaysEveryday: "Every day, including the weekend.",
				houseDaysWeekend:  "Saturday and Sunday only.",
				houseDaysUnstated: "Days are not mentioned. Code may apply the weekday default only for a standing schedule.",
			},
		),
		houseQLoadpoint: typesafe.Choice("Which configured charger in `loadpoints` is this about?", lpOpts),
	}
}

// ComposeHouseholdIntent reads speculative answers and keeps only the
// branch that won. It never returns a Core write for an unsafe request.
func ComposeHouseholdIntent(site SiteContext, res *typesafe.Result) HouseholdIntent {
	out := HouseholdIntent{Outcome: HouseholdExplain, Action: houseActionExplain, CoreOp: ""}
	if res == nil {
		out.Outcome = HouseholdExplain
		return out
	}
	if v, ok := res.NoulOf(houseQUnsafe); ok && v >= houseUnsafeNoulMin {
		out.Unsafe = true
		out.Action = houseActionUnsupported
		out.Outcome = HouseholdRefuse
		out.UnsupportedReason = "the request asks to bypass safety or command hardware FTW does not expose as a household goal"
		return out
	}
	act, ok := res.ChoiceOf(houseQAction)
	if !ok || act.Confidence < houseActionConfidenceMin {
		out.Outcome = HouseholdExplain
		out.fillArgs(site, res)
		return out
	}
	out.Action = act.Choice
	out.ActionConfidence = act.Confidence
	if v, ok := res.NoulOf(houseQDurable); ok {
		out.Durable = v >= houseArgNoulMin
		out.Recurring = out.Durable
	}
	out.fillArgs(site, res)

	switch out.Action {
	case houseActionUnsupported:
		out.Outcome = HouseholdRefuse
		out.UnsupportedReason = "no existing household operation matches this request"
		return out
	case houseActionExplain:
		out.Outcome = HouseholdExplain
		return out
	case houseActionChargeNow:
		out.CoreOp = appproto.OpLoadpointHold
		if out.LoadpointID == "" {
			out.Outcome = HouseholdClarify
			return out
		}
		out.Outcome = HouseholdPropose
		out.Recurring = false
		out.Durable = false
		return out
	case houseActionVehicleSoC:
		out.CoreOp = appproto.OpLoadpointSoCSet
		if out.LoadpointID == "" || out.SoCPct == nil {
			out.Outcome = HouseholdClarify
			return out
		}
		out.Outcome = HouseholdPropose
		out.Recurring = false
		return out
	case houseActionSchedule:
		out.CoreOp = "loadpoint.schedule"
		if out.LoadpointID == "" || out.SoCPct == nil || out.DeadlineLocal == "" {
			out.Outcome = HouseholdClarify
			return out
		}
		out.Recurring = true
		out.Durable = true
		if !out.DaysKnown {
			// Unstated days on a standing goal follow the product example:
			// 80% by 07:00 every weekday. A stated "every day" keeps Days==0.
			out.Days = DaysWeekdays
		}
		out.Outcome = HouseholdPropose
		return out
	default:
		out.Outcome = HouseholdExplain
		return out
	}
}

func (out *HouseholdIntent) fillArgs(site SiteContext, res *typesafe.Result) {
	if len(site.Loadpoints) == 1 {
		out.LoadpointID = site.Loadpoints[0].ID
	} else if lp, ok := res.ChoiceOf(houseQLoadpoint); ok && lp.Choice != "" && lp.Choice != houseUnstated {
		out.LoadpointID = lp.Choice
	}
	if stated, ok := res.NoulOf(houseQSoCStated); ok && stated >= houseArgNoulMin {
		if soc, ok := res.ChoiceOf(houseQSoC); ok && soc.Choice != houseUnstated {
			if n, err := strconv.ParseFloat(soc.Choice, 64); err == nil && n >= 0 && n <= 100 {
				out.SoCPct = &n
			}
		}
	}
	if stated, ok := res.NoulOf(houseQDeadStat); ok && stated >= houseArgNoulMin {
		if d, ok := res.ChoiceOf(houseQDeadline); ok && d.Choice != houseUnstated {
			out.DeadlineLocal = d.Choice
		}
	}
	if days, ok := res.ChoiceOf(houseQDays); ok {
		switch days.Choice {
		case houseDaysWeekdays:
			out.Days = DaysWeekdays
			out.DaysKnown = true
		case houseDaysEveryday:
			out.Days = 0
			out.DaysKnown = true
		case houseDaysWeekend:
			out.Days = DaysWeekend
			out.DaysKnown = true
		}
	}
}

// ExtractPercents finds 0–100 percentages named in the utterance.
func ExtractPercents(utterance string) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range percentRe.FindAllStringSubmatch(utterance, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 100 {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ExtractClocks finds HH:MM / H.MM local times named in the utterance.
func ExtractClocks(utterance string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range clockRe.FindAllStringSubmatch(utterance, -1) {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			continue
		}
		s := fmt.Sprintf("%02d:%02d", h, min)
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
