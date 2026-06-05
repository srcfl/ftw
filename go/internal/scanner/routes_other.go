//go:build !darwin && !linux

package scanner

import "net"

// routedSubnets is a no-op on platforms without a supported routing-table
// reader; scanning falls back to interface-attached subnets only.
func routedSubnets() []net.IPNet { return nil }
