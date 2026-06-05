//go:build linux

package scanner

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strings"
)

// routedSubnets reads /proc/net/route and returns the network routes it finds.
// localSubnets filters these to RFC1918 /24../28 so only real LANs survive.
func routedSubnets() []net.IPNet {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	return parseProcNetRoute(string(data))
}

// parseProcNetRoute parses the kernel's /proc/net/route table. Destination and
// Mask are little-endian hex (network byte order as stored on the wire), e.g.
// Destination "0032A8C0" = 192.168.50.0, Mask "00FFFFFF" = /24.
func parseProcNetRoute(content string) []net.IPNet {
	var out []net.IPNet
	for i, line := range strings.Split(content, "\n") {
		if i == 0 { // header row
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		dst, ok := leHexIP(f[1])
		if !ok {
			continue
		}
		mask, ok := leHexIP(f[7])
		if !ok {
			continue
		}
		ones, bits := net.IPMask(mask).Size()
		if bits != 32 {
			continue
		}
		out = append(out, net.IPNet{IP: dst, Mask: net.CIDRMask(ones, 32)})
	}
	return out
}

// leHexIP decodes an 8-char little-endian hex word (as /proc/net/route stores
// addresses) into a 4-byte net.IP.
func leHexIP(s string) (net.IP, bool) {
	if len(s) != 8 {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return nil, false
	}
	v := binary.LittleEndian.Uint32(b)
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip, true
}
