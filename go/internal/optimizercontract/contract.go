// Package optimizercontract holds stable values shared by Core's
// configuration and optimizer wire protocol.
package optimizercontract

import "time"

const (
	// ProtocolVersion is the newest optimizer wire protocol this Core speaks;
	// MinProtocolVersion is the oldest it still accepts. Core and the optimizer
	// release independently, so an exact-match rule would reject every deployed
	// optimizer the moment either side moved.
	//
	// The strategy here is the one the driver host API already uses: bump this
	// rarely, and grow through additive `features` in the handshake instead.
	// A feature costs an old optimizer nothing — Core simply does not ask it
	// for what it cannot do — while a protocol bump makes every older optimizer
	// incompatible at once. Only bump when the framing or the request/response
	// shape genuinely changes, and widen the window rather than moving it, so
	// sites that have not updated the optimizer keep planning.
	MinProtocolVersion = 1
	ProtocolVersion    = 1

	DefaultTimeout = 30 * time.Second
)

// Compatible reports whether an optimizer advertising [min, max] shares at
// least one protocol version with this Core. A zero max means the optimizer
// reported a single version; a zero min means it reported none, which the
// caller resolves before calling.
func Compatible(min, max int) bool {
	if max == 0 {
		max = min
	}
	return max >= MinProtocolVersion && min <= ProtocolVersion
}
