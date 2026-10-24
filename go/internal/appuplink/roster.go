package appuplink

import (
	"slices"
	"time"

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
// listed, and a watch may reach the box's exchange through it: the box
// answers a watch's key on the exchange alone, never on a stream.
type CloudRoster struct {
	Enroll *appenroll.Identity
	Tunnel bool
}

// Devices lists the paired devices.
func (r CloudRoster) Devices() []cloudlink.RosterDevice {
	return r.devices(r.Enroll.Roster())
}

func (r CloudRoster) devices(entries []appenroll.RosterEntry) []cloudlink.RosterDevice {
	var out []cloudlink.RosterDevice
	for _, e := range entries {
		d := cloudlink.RosterDevice{ID: e.ID, Key: e.Key, Role: e.Role, Epoch: e.Epoch,
			Scopes: appproto.GrantScopes(e.Role, e.ParentRole), Tun: r.Tunnel}
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

// Snapshot keeps invitation consumption and the newly listed key together.
func (r CloudRoster) Snapshot(home string) cloudlink.RosterSnapshot {
	s := r.Enroll.CloudSnapshot()
	out := cloudlink.RosterSnapshot{Devices: r.devices(s.Devices), Noise: s.Noise}
	if !r.Tunnel {
		return out
	}
	if len(s.PairingCode) > 0 {
		out.Pending = append(out.Pending, cloudlink.RosterPending{Hash: cloudwire.PairingHash(home, s.PairingCode), Expires: s.PairingExpires})
	}
	for _, v := range s.Invites {
		out.Pending = append(out.Pending, cloudlink.RosterPending{Hash: v.Hash, Kind: protocol.RoleAgent, Expires: v.Expires})
	}
	return out
}
