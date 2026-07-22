// Package homelink defines the local read-only Home Link boundary.
//
// It does not open a listener or connect to a relay. Callers must pass every
// read through Core's API policy. User checks stay local, grants are short
// lived and one use, and machine reachability grants no write access.
//
// Treat the relay and every token holder as untrusted. A machine challenge is
// bound to the public key already stored for that gateway. A read grant is
// bound to one gateway and one scope, then consumed before the read runs.
//
// These are internal Core contracts, not relay wire types. A later versioned
// slice will define the relay envelope, stream rules and response schemas.
package homelink
