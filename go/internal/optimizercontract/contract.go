// Package optimizercontract holds stable values shared by Core's
// configuration and optimizer wire protocol.
package optimizercontract

import "time"

const (
	ProtocolVersion = 1
	DefaultTimeout  = 30 * time.Second
)
