//go:build darwin

package scanner

import (
	"net"
	"syscall"

	"golang.org/x/net/route"
)

func routedSubnets() []net.IPNet {
	rib, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return nil
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil
	}
	var out []net.IPNet
	for _, message := range messages {
		rm, ok := message.(*route.RouteMessage)
		if !ok || len(rm.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok {
			continue
		}
		maskAddr, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr)
		if !ok {
			continue
		}
		mask := net.IPv4Mask(maskAddr.IP[0], maskAddr.IP[1], maskAddr.IP[2], maskAddr.IP[3])
		ones, bits := mask.Size()
		if bits != 32 {
			continue
		}
		ip := net.IPv4(dst.IP[0], dst.IP[1], dst.IP[2], dst.IP[3]).Mask(mask)
		out = append(out, net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)})
	}
	return out
}
