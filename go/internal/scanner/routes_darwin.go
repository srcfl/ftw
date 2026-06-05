//go:build darwin

package scanner

import (
	"net"
	"syscall"

	"golang.org/x/net/route"
)

// routedSubnets reads the kernel IPv4 routing table (PF_ROUTE) and returns the
// network routes it finds. localSubnets filters these down to RFC1918 /24../28
// so only real, scannable LANs survive.
func routedSubnets() []net.IPNet {
	rib, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return nil
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil
	}

	var out []net.IPNet
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok {
			continue
		}
		ip := net.IPv4(dst.IP[0], dst.IP[1], dst.IP[2], dst.IP[3])

		maskLen := 32 // default: treat as host route, filtered out later
		if nm, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok {
			mask := net.IPv4Mask(nm.IP[0], nm.IP[1], nm.IP[2], nm.IP[3])
			if ones, bits := mask.Size(); bits == 32 {
				maskLen = ones
			}
		}
		out = append(out, net.IPNet{IP: ip, Mask: net.CIDRMask(maskLen, 32)})
	}
	return out
}
