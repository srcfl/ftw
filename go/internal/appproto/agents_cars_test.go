package appproto

import (
	"bytes"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// carOf is a car's id as an agent reads it from cars.get.
func carOf(id string) string { return cloudwire.CarID(testAgentKey, id) }

// An agent reads the cars by the ids it can use, with their levels and
// goals, and nothing that names them.
func TestAnAgentReadsTheCarsByItsOwnIds(t *testing.T) {
	r := newAgentRig(t)
	r.cars = newFakeCars()
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLiveRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{Client: "Claude"})
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgCarsGet, ptrU32(1), struct{}{})
	got := body[cloudwire.Cars](t, rec.last(t, protocol.MsgCars))
	if len(got.Cars) != 1 || got.Cars[0].ID != carOf("c1") || *got.Cars[0].SocPermille != 140 {
		t.Fatalf("cars %+v", got)
	}

	// Without live.read, nothing.
	blind, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeLogsRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	bh, brec := r.agentSession(blind)
	deliver(t, bh, protocol.MsgCarsGet, ptrU32(2), struct{}{})
	if code := errCode(t, brec); code != ErrScopeDenied {
		t.Fatalf("cars.get without live.read: %s", code)
	}
}

// An agent asks for a car's goal or level by the id cars.get gave it. The
// box checks the car and the goal as the request arrives, an owner confirms,
// the right car changes, and the agent reads the outcome by its own ids.
func TestAnAgentAsksForACarsGoalByItsOwnId(t *testing.T) {
	r := newAgentRig(t)
	r.cars = newFakeCars()
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeGoalsRequest, protocol.ScopeLiveRead},
		[]string{OpCarGoalSet, OpCarSoCSet}, protocol.Caps{PerMin: 6}, time.Hour, protocol.Principal{Client: "Claude"})
	h, rec := r.agentSession(row)

	for name, args := range map[string]map[string]any{
		"a car the box lacks":     {"id": carOf("nope"), "target": 0.7, "ready_by_min": int64(420)},
		"the box's own id":        {"id": "c2", "target": 0.7, "ready_by_min": int64(420)},
		"a target without a time": {"id": carOf("c2"), "target": 0.7},
		"a goal and clear":        {"id": carOf("c2"), "target": 0.7, "ready_by_min": int64(420), "clear": true},
		"a key the op lacks":      {"id": carOf("c2"), "name": "Mine"},
	} {
		rec.reset()
		deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpCarGoalSet, Args: args})
		if code := errCode(t, rec); code != ErrUnknownOp {
			t.Errorf("%s: %s", name, code)
		}
	}

	rec.reset()
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(2), protocol.AgentRequest{Op: OpCarGoalSet,
		Args: map[string]any{"id": carOf("c2"), "target": 0.7, "ready_by_min": int64(420), "days": int64(31)}, Why: "Volvo to 70 % by 07:00"})
	reqID, _ := body[map[string]any](t, rec.last(t, protocol.MsgAgentOK))["reqId"].(string)
	if reqID == "" {
		t.Fatalf("no request: %s", rec.types())
	}
	owner, orec := r.ownerSession()
	deliver(t, owner, protocol.MsgAgentList, ptrU32(3), struct{}{})
	req := body[protocol.AgentList](t, orec.last(t, protocol.MsgAgentOK)).Requests[0]
	deliver(t, owner, protocol.MsgAgentConfirm, ptrU32(4),
		r.signer.Confirm(r.signedFor(), protocol.AgentConfirm{ReqID: reqID, Approve: true, AtMs: r.ms()}, req))
	if g := r.cars.cars["c2"].Goal; g == nil || g.TargetPermille != 700 || g.ReadyByMin != 420 || g.Days != 31 {
		t.Fatalf("the Volvo's goal %+v", g)
	}
	if r.cars.cars["c1"].Goal != nil {
		t.Fatal("another car changed")
	}
	d := body[protocol.AgentDecided](t, rec.last(t, protocol.MsgAgentDecided))
	if d.State != protocol.RequestApproved || d.Result == nil || d.Result.Car == nil || d.Result.Car.ID != carOf("c2") {
		t.Fatalf("the agent's outcome %+v", d)
	}
	raw, _ := Marshal(d)
	if bytes.Contains(raw, []byte("Volvo")) || bytes.Contains(raw, []byte(`"c2"`)) {
		t.Fatal("the outcome names the car")
	}
}

// An agent reads the plan's chargers and their cars by the ids its other
// reads give it, never by the box's names.
func TestAnAgentsPlanNamesNoChargerOrCar(t *testing.T) {
	r := newAgentRig(t)
	chargers := &fakeChargers{}
	chargers.set([]PlanCharger{{ID: "easee", CarID: "0123456789abcdef0123456789abcdef", PluggedIn: true, Windows: []PlanChargerWindow{}}})
	r.chargers = chargers
	h, rec := r.agentSession(r.controlAgent())
	deliver(t, h, MsgPlanGet, ptrU32(1), struct{}{})
	plan := body[Plan](t, rec.last(t, MsgPlan))
	if len(plan.Chargers) != 1 || plan.Chargers[0].ID != cloudwire.LoadpointID(testAgentKey, "easee") ||
		plan.Chargers[0].CarID != carOf("0123456789abcdef0123456789abcdef") {
		t.Fatalf("an agent's plan chargers %+v", plan.Chargers)
	}

	owner, orec := r.ownerSession()
	deliver(t, owner, MsgPlanGet, ptrU32(2), struct{}{})
	if got := body[Plan](t, orec.last(t, MsgPlan)).Chargers; got[0].ID != "easee" || got[0].CarID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("a phone's plan chargers %+v", got)
	}
}

// The box page names the car and the charger in a request by what the box
// calls them. One the box no longer knows keeps no name, and the args stay
// the box's own ids.
func TestTheBoxPageNamesARequestsCarAndCharger(t *testing.T) {
	r := newAgentRig(t)
	r.cars = newFakeCars()
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeGoalsRequest},
		[]string{OpCarGoalSet, OpCarSoCSet}, protocol.Caps{PerMin: 6}, time.Hour, protocol.Principal{Client: "Claude"})
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(1), protocol.AgentRequest{Op: OpCarGoalSet,
		Args: map[string]any{"id": carOf("c2"), "target": 0.7, "ready_by_min": int64(420)}})
	deliver(t, h, protocol.MsgAgentRequest, ptrU32(2), protocol.AgentRequest{Op: OpCarSoCSet,
		Args: map[string]any{"charger_id": lp1(), "soc": 0.4}})
	page, _ := r.ownerSession()
	if n := len(page.Requests()); n != 2 {
		t.Fatalf("requests taken: %d, %s", n, rec.types())
	}
	named := func() map[string]map[string]string {
		out := map[string]map[string]string{}
		for _, req := range page.Requests() {
			out[req.Op] = page.ArgNames(req)
		}
		return out
	}
	got := named()
	if len(got[OpCarGoalSet]) != 1 || got[OpCarGoalSet]["id"] != "Volvo" {
		t.Errorf("the car: %v", got[OpCarGoalSet])
	}
	if len(got[OpCarSoCSet]) != 1 || got[OpCarSoCSet]["charger_id"] != "lp1" {
		t.Errorf("the charger: %v", got[OpCarSoCSet])
	}

	// The car and the charger go while the requests wait.
	delete(r.cars.cars, "c2")
	r.lp.ids["lp1"] = false
	for op, names := range named() {
		if names != nil {
			t.Errorf("%s names what the box no longer knows: %v", op, names)
		}
	}
	for _, req := range page.Requests() {
		if id, _ := req.Args["id"].(string); req.Op == OpCarGoalSet && id != "c2" {
			t.Errorf("the car's id in the args: %v", req.Args)
		}
		if id, _ := req.Args["charger_id"].(string); req.Op == OpCarSoCSet && id != "lp1" {
			t.Errorf("the charger's id in the args: %v", req.Args)
		}
	}
}
