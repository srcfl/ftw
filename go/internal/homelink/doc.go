// Package homelink defines the local read-only Home Link boundary.
//
// It does not open a listener or connect to a relay. Callers must pass every
// read through Core's API policy. User checks stay local, grants are short
// lived and one use, and machine reachability grants no write access.
//
// Treat the relay and every token holder as untrusted. A machine challenge is
// bound to a canonical public key from a trusted local lookup. The local
// credential authority owns each expected challenge, RP, origin, fixed expiry,
// one-use check and durable revoke state. In-process grants and machine
// challenges expire on a monotonic clock. A read grant is bound to one gateway
// and one scope, then consumed as Core resolves and dispatches the read.
//
// These are internal Core contracts, not relay wire types. A later versioned
// slice will define the relay envelope, stream rules and response schemas.
package homelink
