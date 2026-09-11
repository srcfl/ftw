package ocpp

// Hardware identity for a driverless device.
//
// Everything else in FTW is a device because config named a driver and the
// driver reported a serial. A charge point is the other way round: it dials
// us, picks its own name, and tells us what it is only in BootNotification.
//
// The name it picked is not identity. It is the last segment of a URL the
// installer typed, it is what a charger entry adopts, and it can be changed on
// the charger's own web page — that makes it a YAML name by another route, and
// persistent state keyed on it would not survive a re-commissioning. The
// vendor and serial from BootNotification are the hardware-stable pair, and
// they are what the device row is keyed on. The URL identity is the fallback
// for a charger that reports no serial at all, recorded as an endpoint so it
// reads as what it is: stable only until someone changes it.

import "log/slog"

// ChargerIdentity is what a charge point told us about itself, in the shape
// the device registry wants.
type ChargerIdentity struct {
	// ID is the identity the charger dialled with — its driver name in
	// telemetry, and what a charger entry adopts.
	ID string
	// Vendor, Model, Serial and Firmware come from BootNotification. Any of
	// them may be empty; chargers vary in what they bother to report.
	Vendor   string
	Model    string
	Serial   string
	Firmware string
}

// Identities returns the adopted chargers that have told us what they are.
//
// Pending chargers are left out on purpose: a device row is a statement that
// this hardware is part of the site, and quarantine says an unadopted charge
// point is not. It is visible in the Chargers panel for an operator to adopt,
// and gets its row on the next boot or config apply after that.
func (h *Handler) Identities() []ChargerIdentity {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ChargerIdentity, 0, len(h.chargers))
	for id, s := range h.chargers {
		if !h.approved[id] {
			continue
		}
		if s.vendor == "" && s.serial == "" {
			// Nothing to key on yet — the charger has connected but not
			// booted. Registering now would create a row keyed on the URL
			// identity that the real serial could never replace.
			continue
		}
		out = append(out, ChargerIdentity{
			ID:       id,
			Vendor:   s.vendor,
			Model:    s.model,
			Serial:   s.serial,
			Firmware: s.firmware,
		})
	}
	return out
}

// SetIdentityReported registers the callback fired when an adopted charger
// reports what it is. main.go writes the device row from it.
func (h *Handler) SetIdentityReported(fn func(ChargerIdentity)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.identityReported = fn
	h.mu.Unlock()
}

// noteIdentity fires the callback after a BootNotification. Quarantine
// applies: a pending charger's identity is recorded and shown, and never
// becomes a device.
func (h *Handler) noteIdentity(id string) {
	h.mu.Lock()
	s := h.chargersLocked(id)
	ident := ChargerIdentity{
		ID:       id,
		Vendor:   s.vendor,
		Model:    s.model,
		Serial:   s.serial,
		Firmware: s.firmware,
	}
	fn := h.identityReported
	approved := h.approved[id]
	h.mu.Unlock()
	if !approved || fn == nil {
		return
	}
	if ident.Vendor == "" && ident.Serial == "" {
		slog.Info("ocpp: charger reported neither vendor nor serial — device identity falls back to the name it dialled with",
			"charger", id)
	}
	fn(ident)
}

// CurrentIdentity returns only an adopted charger's identity reported on its
// current connection. Historical UI labels cannot bind a new control request.
func (h *Handler) CurrentIdentity(id string) (ChargerIdentity, bool) {
	if h == nil {
		return ChargerIdentity{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.chargers[id]
	if s == nil || !h.approved[id] || !s.online || !s.identityCurrent {
		return ChargerIdentity{}, false
	}
	return ChargerIdentity{ID: id, Vendor: s.vendor, Model: s.model, Serial: s.serial, Firmware: s.firmware}, true
}
