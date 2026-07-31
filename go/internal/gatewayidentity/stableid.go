package gatewayidentity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
)

const (
	SoftwareIdentityUplinkHost = "uplink.home.sourceful.energy"
	SoftwareIdentityUplinkPort = 443
)

var (
	ErrNoUsableRoute      = errors.New("no usable uplink route")
	ErrAmbiguousRoute     = errors.New("uplink route is ambiguous")
	ErrUnsupportedBinding = errors.New("home link identity binding is unsupported")
)

type StableInterface struct {
	Name         string
	Index        int
	PermanentMAC net.HardwareAddr
}

type routeResult struct {
	InterfaceIndex int
	Multipath      bool
	PolicyUnclear  bool
}

type physicalInterface struct {
	Name         string
	Index        int
	PermanentMAC net.HardwareAddr
}

type routeAuthority interface {
	LookupIP(context.Context, string) ([]net.IP, error)
	ResolvedRoute(context.Context, net.IP) (routeResult, error)
	PhysicalInterface(int) (physicalInterface, error)
}

func ResolveStableSoftwareInterface(ctx context.Context) (StableInterface, error) {
	return resolveStableSoftwareInterface(ctx, SoftwareIdentityUplinkHost, newRouteAuthority())
}

func resolveStableSoftwareInterface(
	ctx context.Context,
	host string,
	authority routeAuthority,
) (StableInterface, error) {
	if authority == nil {
		return StableInterface{}, errors.New("route authority is missing")
	}
	ips, err := authority.LookupIP(ctx, host)
	if err != nil {
		return StableInterface{}, fmt.Errorf("resolve identity uplink: %w", err)
	}
	ips = uniqueUsableIPs(ips)
	if len(ips) == 0 {
		return StableInterface{}, ErrNoUsableRoute
	}

	var selected physicalInterface
	haveSelected := false
	var unreachable []string
	for _, ip := range ips {
		result, err := authority.ResolvedRoute(ctx, ip)
		if err != nil {
			if errors.Is(err, ErrNoUsableRoute) {
				unreachable = append(unreachable, ip.String())
				continue
			}
			return StableInterface{}, fmt.Errorf("resolve kernel route for %s: %w", ip, err)
		}
		if result.Multipath || result.PolicyUnclear || result.InterfaceIndex <= 0 {
			return StableInterface{}, fmt.Errorf("%w for %s", ErrAmbiguousRoute, ip)
		}
		iface, err := authority.PhysicalInterface(result.InterfaceIndex)
		if err != nil {
			return StableInterface{}, fmt.Errorf("verify route interface for %s: %w", ip, err)
		}
		if err := validatePhysicalInterface(iface); err != nil {
			return StableInterface{}, fmt.Errorf("verify route interface for %s: %w", ip, err)
		}
		if !haveSelected {
			selected = iface
			haveSelected = true
			continue
		}
		if selected.Index != iface.Index ||
			selected.Name != iface.Name ||
			!bytes.Equal(selected.PermanentMAC, iface.PermanentMAC) {
			outputs := []string{
				fmt.Sprintf("%s[%d]/%s", selected.Name, selected.Index, selected.PermanentMAC),
				fmt.Sprintf("%s[%d]/%s", iface.Name, iface.Index, iface.PermanentMAC),
			}
			sort.Strings(outputs)
			return StableInterface{}, fmt.Errorf(
				"%w: uplink addresses resolve through %s and %s",
				ErrAmbiguousRoute, outputs[0], outputs[1],
			)
		}
	}
	if !haveSelected {
		sort.Strings(unreachable)
		return StableInterface{}, fmt.Errorf("%w: %v", ErrNoUsableRoute, unreachable)
	}
	return StableInterface{
		Name:         selected.Name,
		Index:        selected.Index,
		PermanentMAC: append(net.HardwareAddr(nil), selected.PermanentMAC...),
	}, nil
}

func uniqueUsableIPs(ips []net.IP) []net.IP {
	seen := make(map[string]struct{}, len(ips))
	out := make([]net.IP, 0, len(ips))
	for _, raw := range ips {
		ip := normalizedIP(raw)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
			ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		key := string(ip)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out
}

func normalizedIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return append(net.IP(nil), v4...)
	}
	if v6 := ip.To16(); v6 != nil {
		return append(net.IP(nil), v6...)
	}
	return nil
}

func validatePhysicalInterface(iface physicalInterface) error {
	if iface.Index <= 0 || iface.Name == "" {
		return errors.New("physical interface identity is incomplete")
	}
	mac := iface.PermanentMAC
	if len(mac) != 6 {
		return errors.New("permanent physical MAC must contain 6 bytes")
	}
	if mac[0]&1 != 0 {
		return errors.New("permanent physical MAC is multicast")
	}
	if mac[0]&2 != 0 {
		return errors.New("permanent physical MAC is locally administered")
	}
	allZero := true
	allFF := true
	for _, b := range mac {
		allZero = allZero && b == 0
		allFF = allFF && b == 0xff
	}
	if allZero || allFF {
		return errors.New("permanent physical MAC is not usable")
	}
	return nil
}

func validatePermanentMACSources(
	interfaceUp bool,
	kernelUp bool,
	current net.HardwareAddr,
	sysfsCurrent net.HardwareAddr,
	kernelCurrent net.HardwareAddr,
	permanent net.HardwareAddr,
	sysfsPermanent net.HardwareAddr,
) error {
	if !interfaceUp || !kernelUp {
		return errors.New("route interface is down")
	}
	sources := []struct {
		name string
		mac  net.HardwareAddr
	}{
		{"kernel current", current},
		{"sysfs current", sysfsCurrent},
		{"netlink current", kernelCurrent},
		{"netlink permanent", permanent},
	}
	for _, source := range sources {
		if len(source.mac) != 6 {
			return fmt.Errorf("%s MAC must contain 6 bytes", source.name)
		}
	}
	if len(sysfsPermanent) != 0 && len(sysfsPermanent) != 6 {
		return errors.New("sysfs permanent MAC must contain 6 bytes")
	}
	if !bytes.Equal(current, sysfsCurrent) ||
		!bytes.Equal(current, kernelCurrent) {
		return errors.New("current MAC sources disagree")
	}
	if !bytes.Equal(current, permanent) {
		return errors.New("current and permanent MAC differ")
	}
	if len(sysfsPermanent) != 0 && !bytes.Equal(permanent, sysfsPermanent) {
		return errors.New("permanent MAC sources disagree")
	}
	return validatePhysicalInterface(physicalInterface{
		Name: "verified", Index: 1, PermanentMAC: permanent,
	})
}
