package gatewayidentity

import (
	"context"
	"errors"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"
)

type fakeRouteAuthority struct {
	ips        []net.IP
	lookupErr  error
	routes     map[string]routeResult
	routeErrs  map[string]error
	interfaces map[int]physicalInterface
}

func (f *fakeRouteAuthority) LookupIP(context.Context, string) ([]net.IP, error) {
	return append([]net.IP(nil), f.ips...), f.lookupErr
}

func (f *fakeRouteAuthority) ResolvedRoute(_ context.Context, ip net.IP) (routeResult, error) {
	if err := f.routeErrs[ip.String()]; err != nil {
		return routeResult{}, err
	}
	result, ok := f.routes[ip.String()]
	if !ok {
		return routeResult{}, ErrNoUsableRoute
	}
	return result, nil
}

func (f *fakeRouteAuthority) PhysicalInterface(index int) (physicalInterface, error) {
	iface, ok := f.interfaces[index]
	if !ok {
		return physicalInterface{}, errors.New("unknown interface")
	}
	return iface, nil
}

func physical(index int, name, mac string) physicalInterface {
	return physicalInterface{
		Index: index, Name: name, PermanentMAC: mustMAC(mac),
	}
}

func mustMAC(raw string) net.HardwareAddr {
	mac, err := net.ParseMAC(raw)
	if err != nil {
		panic(err)
	}
	return mac
}

func TestStableInterfaceUsesKernelResolvedOutputForEveryAddress(t *testing.T) {
	if SoftwareIdentityUplinkHost != "uplink.home.sourceful.energy" {
		t.Fatalf("software identity uplink = %q", SoftwareIdentityUplinkHost)
	}
	authority := &fakeRouteAuthority{
		ips: []net.IP{
			net.ParseIP("2001:db8::10"),
			net.ParseIP("192.0.2.10"),
			net.ParseIP("192.0.2.10"),
		},
		routes: map[string]routeResult{
			"192.0.2.10":   {InterfaceIndex: 7},
			"2001:db8::10": {InterfaceIndex: 7},
		},
		interfaces: map[int]physicalInterface{
			7: physical(7, "enp1s0", "00:11:22:33:44:55"),
		},
	}
	got, err := resolveStableSoftwareInterface(
		context.Background(), SoftwareIdentityUplinkHost, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Index != 7 || got.Name != "enp1s0" ||
		got.PermanentMAC.String() != "00:11:22:33:44:55" {
		t.Fatalf("stable interface = %+v", got)
	}

	authority.ips[0], authority.ips[1] = authority.ips[1], authority.ips[0]
	shuffled, err := resolveStableSoftwareInterface(
		context.Background(), SoftwareIdentityUplinkHost, authority,
	)
	if err != nil || !reflect.DeepEqual(shuffled, got) {
		t.Fatalf("shuffled DNS changed result: %+v, %v", shuffled, err)
	}
}

func TestStableInterfaceFailsOnDifferentIPv4IPv6Outputs(t *testing.T) {
	authority := &fakeRouteAuthority{
		ips: []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")},
		routes: map[string]routeResult{
			"192.0.2.10":   {InterfaceIndex: 7},
			"2001:db8::10": {InterfaceIndex: 8},
		},
		interfaces: map[int]physicalInterface{
			7: physical(7, "enp1s0", "00:11:22:33:44:55"),
			8: physical(8, "wlan0", "10:20:30:40:50:60"),
		},
	}
	_, err := resolveStableSoftwareInterface(
		context.Background(), SoftwareIdentityUplinkHost, authority,
	)
	if !errors.Is(err, ErrAmbiguousRoute) {
		t.Fatalf("different outputs error = %v", err)
	}
}

func TestStableInterfaceFailsOnKernelAmbiguity(t *testing.T) {
	for name, result := range map[string]routeResult{
		"ecmp":          {InterfaceIndex: 7, Multipath: true},
		"policy-or-vrf": {InterfaceIndex: 7, PolicyUnclear: true},
		"no-output":     {},
	} {
		t.Run(name, func(t *testing.T) {
			authority := &fakeRouteAuthority{
				ips:        []net.IP{net.ParseIP("192.0.2.10")},
				routes:     map[string]routeResult{"192.0.2.10": result},
				interfaces: map[int]physicalInterface{7: physical(7, "enp1s0", "00:11:22:33:44:55")},
			}
			_, err := resolveStableSoftwareInterface(
				context.Background(), SoftwareIdentityUplinkHost, authority,
			)
			if !errors.Is(err, ErrAmbiguousRoute) {
				t.Fatalf("ambiguity error = %v", err)
			}
		})
	}
}

func TestStableInterfaceSkipsOnlyUnreachableAddress(t *testing.T) {
	authority := &fakeRouteAuthority{
		ips: []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")},
		routes: map[string]routeResult{
			"192.0.2.10": {InterfaceIndex: 7},
		},
		routeErrs: map[string]error{
			"2001:db8::10": fmtError(ErrNoUsableRoute),
		},
		interfaces: map[int]physicalInterface{
			7: physical(7, "enp1s0", "00:11:22:33:44:55"),
		},
	}
	if _, err := resolveStableSoftwareInterface(
		context.Background(), SoftwareIdentityUplinkHost, authority,
	); err != nil {
		t.Fatal(err)
	}
	authority.routeErrs["192.0.2.10"] = ErrNoUsableRoute
	if _, err := resolveStableSoftwareInterface(
		context.Background(), SoftwareIdentityUplinkHost, authority,
	); !errors.Is(err, ErrNoUsableRoute) {
		t.Fatalf("all-unreachable error = %v", err)
	}
}

func TestStableInterfaceRejectsNonPhysicalMACs(t *testing.T) {
	tests := []physicalInterface{
		physical(7, "veth0", "02:11:22:33:44:55"),
		physical(7, "eth0", "01:11:22:33:44:55"),
		physical(7, "eth0", "00:00:00:00:00:00"),
		{Index: 7, Name: "eth0", PermanentMAC: []byte{0, 1}},
	}
	for _, iface := range tests {
		if err := validatePhysicalInterface(iface); err == nil {
			t.Fatalf("accepted interface %+v", iface)
		}
	}
}

func TestPermanentMACRequiresMatchingIndependentSources(t *testing.T) {
	good := mustMAC("00:11:22:33:44:55")
	if err := validatePermanentMACSources(true, true, good, good, good, good, good); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		current        net.HardwareAddr
		sysfsCurrent   net.HardwareAddr
		kernelCurrent  net.HardwareAddr
		permanent      net.HardwareAddr
		sysfsPermanent net.HardwareAddr
	}{
		{
			name: "current differs from permanent", current: good,
			sysfsCurrent: good, kernelCurrent: good,
			permanent: mustMAC("10:20:30:40:50:60"),
		},
		{
			name: "missing permanent", current: good,
			sysfsCurrent: good, kernelCurrent: good,
		},
		{
			name: "conflicting current source", current: good,
			sysfsCurrent:  mustMAC("10:20:30:40:50:60"),
			kernelCurrent: good, permanent: good,
		},
		{
			name: "conflicting permanent source", current: good,
			sysfsCurrent: good, kernelCurrent: good, permanent: good,
			sysfsPermanent: mustMAC("10:20:30:40:50:60"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePermanentMACSources(
				true, true,
				test.current, test.sysfsCurrent, test.kernelCurrent,
				test.permanent, test.sysfsPermanent,
			); err == nil {
				t.Fatal("unsafe MAC source set was accepted")
			}
		})
	}
	if err := validatePermanentMACSources(
		false, true, good, good, good, good, good,
	); err == nil {
		t.Fatal("down interface was accepted")
	}
	if err := validatePermanentMACSources(
		true, false, good, good, good, good, good,
	); err == nil {
		t.Fatal("netlink-down interface was accepted")
	}
}

func fmtError(err error) error { return errors.Join(errors.New("kernel"), err) }

func TestLinuxKernelResolvedRouteSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux route lookup only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := linuxResolvedRoute(ctx, net.ParseIP("1.1.1.1"))
	if errors.Is(err, ErrNoUsableRoute) {
		t.Skip("test host has no IPv4 uplink route")
	}
	if err != nil {
		t.Fatal(err)
	}
	if result.InterfaceIndex <= 0 || result.Multipath || result.PolicyUnclear {
		t.Fatalf("kernel resolved route = %+v", result)
	}
}
