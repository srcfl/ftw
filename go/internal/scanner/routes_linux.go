//go:build linux

package scanner

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

func routedSubnets() []net.IPNet {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	return parseProcNetRoute(string(data))
}

func parseProcNetRoute(content string) []net.IPNet {
	var out []net.IPNet
	for i, line := range strings.Split(content, "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&1 == 0 { // RTF_UP
			continue
		}
		dst, ok := littleEndianHexIP(fields[1])
		if !ok {
			continue
		}
		maskIP, ok := littleEndianHexIP(fields[7])
		if !ok {
			continue
		}
		mask := net.IPMask(maskIP.To4())
		ones, bits := mask.Size()
		if bits == 32 {
			out = append(out, net.IPNet{IP: dst.Mask(mask), Mask: net.CIDRMask(ones, 32)})
		}
	}
	return out
}

func littleEndianHexIP(s string) (net.IP, bool) {
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
