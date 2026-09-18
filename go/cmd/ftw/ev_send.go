package main

import (
	"context"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/ocpp"
)

// ocppApprovedIDs is the OCPP allowlist. Lua loadpoint names are not charge-point
// identities: anyone holding the shared basic-auth secret could otherwise connect
// as "easee" and steal DerEV plus commands. Adopted OCPP loadpoints (driver names
// that are not Lua drivers) and ids listed under ocpp.chargers stay approved.
func ocppApprovedIDs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	lua := make(map[string]struct{}, len(cfg.Drivers))
	for _, d := range cfg.Drivers {
		if d.Name != "" {
			lua[d.Name] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if cfg.OCPP != nil {
		for _, c := range cfg.OCPP.Chargers {
			add(c.ID)
		}
	}
	for _, lp := range cfg.Loadpoints {
		if _, isLua := lua[lp.DriverName]; isLua {
			continue
		}
		add(lp.DriverName)
	}
	return out
}

// evCommandRouter sends EV commands to an online, adopted OCPP charger, or falls
// through to the Lua registry. Periodic dispatch uses SendWithOutcome / SendCycle;
// those must take the same route as the API's SenderFunc or planner ticks never
// reach a charger that has no Lua driver.
type evCommandRouter struct {
	ocpp        *ocpp.Server
	send        loadpoint.SenderFunc
	sendOutcome loadpoint.OutcomeSenderFunc
	sendCycle   loadpoint.CycleSenderFunc
}

func newEVCommandRouter(
	srv *ocpp.Server,
	send loadpoint.SenderFunc,
	sendOutcome loadpoint.OutcomeSenderFunc,
	sendCycle loadpoint.CycleSenderFunc,
) evCommandRouter {
	return evCommandRouter{ocpp: srv, send: send, sendOutcome: sendOutcome, sendCycle: sendCycle}
}

func (r evCommandRouter) ocppRoute(name string) bool {
	return r.ocpp != nil && r.ocpp.Handler().IsOnline(name) && r.ocpp.Handler().IsApproved(name)
}

func (r evCommandRouter) Send(ctx context.Context, name string, payload []byte) error {
	if r.ocppRoute(name) {
		return r.ocpp.Command(ctx, name, payload)
	}
	if r.send == nil {
		return nil
	}
	return r.send(ctx, name, payload)
}

func (r evCommandRouter) SendWithOutcome(ctx context.Context, name string, payload []byte, outcome func(error)) error {
	if r.ocppRoute(name) {
		err := r.ocpp.Command(ctx, name, payload)
		if outcome != nil {
			outcome(err)
		}
		return err
	}
	if r.sendOutcome != nil {
		return r.sendOutcome(ctx, name, payload, outcome)
	}
	err := r.Send(ctx, name, payload)
	if outcome != nil {
		outcome(err)
	}
	return err
}

func (r evCommandRouter) SendCycle(ctx context.Context, name string, payload []byte, cycleID uint64) error {
	if r.ocppRoute(name) {
		return r.ocpp.Command(ctx, name, payload)
	}
	if r.sendCycle != nil {
		return r.sendCycle(ctx, name, payload, cycleID)
	}
	return r.Send(ctx, name, payload)
}
