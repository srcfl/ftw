package appuplink

import (
	"slices"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// CloudRoster is enrolment as FTW Cloud's roster reads it: every paired
// device with the scopes the box derives for it, a phone's role, or for a
// watch what its role and its phone's role share. With Tunnel set, a phone
// may open a session through FTW Cloud and the QR code on the screen is
// listed. Watch commands use the paired phone's session.
type CloudRoster struct {
	Enroll *appenroll.Identity
	Tunnel bool
}

// Devices lists the paired devices.
func (r CloudRoster) Devices() []cloudlink.RosterDevice {
	var out []cloudlink.RosterDevice
	for _, e := range r.Enroll.Roster() {
		d := cloudlink.RosterDevice{ID: e.ID, Key: e.Key, Role: e.Role, Epoch: e.Epoch,
			Scopes: appproto.GrantScopes(e.Role, e.ParentRole), Tun: r.Tunnel && e.Role != apiauth.RoleWatch}
		if a := e.Agent; a != nil {
			// An agent row carries its grant, at its approver's scopes now,
			// so FTW Cloud can hold to it as well (docs/agents.md). It has
			// no tun and no copy scopes: only the keys part opens its
			// session, and an agent reads the box, never the copy.
			scopes := appproto.AgentScopes(a.Scopes, e.Role, e.ParentRole)
			ops := make([]string, 0, len(a.Ops))
			for _, op := range a.Ops {
				if slices.Contains(scopes, protocol.RegistryAgentOps[op]) {
					ops = append(ops, op)
				}
			}
			d.Scopes = []string{}
			d.Tun = false
			d.Agent = &cloudwire.Agent{Scopes: scopes, Ops: ops, Caps: a.Caps, ExpiresAtMs: a.ExpiresAtMs,
				Principal: a.Principal}
		}
		out = append(out, d)
	}
	return out
}

// Noise is the box's Noise static key, which the roster carries for FTW
// Cloud's keys part to open an agent's session to.
func (r CloudRoster) Noise() []byte { return r.Enroll.StaticKey().Public() }

// Pending is the QR code on the screen, while the box takes the tunnel: a
// new phone can then pair through FTW Cloud.
func (r CloudRoster) Pending() ([]byte, time.Time, bool) {
	if !r.Tunnel {
		return nil, time.Time{}, false
	}
	return r.Enroll.PendingCode()
}
