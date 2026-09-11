package ocpp

// OCPP version handling.
//
// A charge point picks its dialect during the WebSocket handshake, via the
// subprotocol header — "ocpp1.6", "ocpp2.0.1". The library's ws.Server keeps a
// single message handler, so one listener serves exactly one version and each
// enabled version gets its own port. Which port a charger dialled is therefore
// what tells us how to talk back to it.
//
// Everything above this file is version-neutral: chargers land in the same
// state map, produce the same DerEV telemetry, and take the same commands. Only
// the message encoding differs, which is what the per-version handlers own.

// Version is an OCPP protocol version FTW can serve.
type Version string

const (
	// Version16 is OCPP 1.6J. Every charger on the market speaks it, and every
	// charger currently on the bench speaks only it.
	Version16 Version = "1.6"

	// Version201 is OCPP 2.0.1. Newer hardware and the version the industry is
	// migrating to.
	Version201 Version = "2.0.1"
)

// String makes Version printable in logs and errors.
func (v Version) String() string { return string(v) }

// Valid reports whether this is a version FTW can serve. Used by config
// validation so a typo fails at startup rather than silently serving nothing.
func (v Version) Valid() bool {
	switch v {
	case Version16, Version201:
		return true
	default:
		return false
	}
}

// Version returns the OCPP dialect a charge point connected with, and whether
// it has been seen at all.
func (h *Handler) Version(id string) (Version, bool) {
	if h == nil {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	if !ok || s.version == "" {
		return "", false
	}
	return s.version, true
}

// setVersion records the dialect a charge point connected with. Called from
// each version's connect callback, where the listener identity is known.
func (h *Handler) setVersion(id string, v Version) {
	if h == nil {
		return
	}
	s := h.state(id)
	h.mu.Lock()
	s.version = v
	h.mu.Unlock()
}
