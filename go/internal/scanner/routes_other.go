//go:build !darwin && !linux

package scanner

import "net"

func routedSubnets() []net.IPNet { return nil }
