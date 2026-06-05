// Package scanner discovers devices on the local network by TCP-dialing
// common energy-device ports (Modbus 502, MQTT 1883, HTTP 80) across all
// /24 subnets attached to non-loopback, non-virtual interfaces.
//
// Used by the bootstrap wizard to help users find their inverters/meters
// and by the settings UI for re-scanning.
package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// FoundDevice is one open port discovered on the local network.
type FoundDevice struct {
	IP        string `json:"ip"`
	Hostname  string `json:"hostname,omitempty"` // reverse-DNS name, best-effort
	Port      int    `json:"port"`
	Protocol  string `json:"protocol"` // "modbus", "mqtt", "http"
	LatencyMs int    `json:"latency_ms"`
}

// wellKnownPorts maps TCP ports to protocol names.
var wellKnownPorts = map[int]string{
	502:  "modbus",
	503:  "modbus", // proxied Modbus units (e.g. a Pixii behind a TCP proxy) often expose here
	1502: "modbus", // solaredge-proxy default — multiplexes a SolarEdge's single Modbus/TCP socket
	1883: "mqtt",
	80:   "http",
}

// Scan probes all /24 subnets on local interfaces for open ports.
// Results are sorted by IP then port. The context controls cancellation.
func Scan(ctx context.Context) ([]FoundDevice, error) {
	subnets, err := localSubnets()
	if err != nil {
		return nil, fmt.Errorf("enumerate subnets: %w", err)
	}
	if len(subnets) == 0 {
		return nil, nil
	}

	slog.Info("network scan starting", "subnets", len(subnets))

	var ports []int
	for p := range wellKnownPorts {
		ports = append(ports, p)
	}

	var (
		mu    sync.Mutex
		found []FoundDevice
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 64) // concurrency limiter
	)

	for _, subnet := range subnets {
		hosts := subnetHosts(subnet)
		for _, ip := range hosts {
			for _, port := range ports {
				// Check cancellation before launching work.
				select {
				case <-ctx.Done():
					goto done
				default:
				}

				wg.Add(1)
				go func(ip string, port int) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					if dev, ok := probe(ctx, ip, port); ok {
						mu.Lock()
						found = append(found, dev)
						mu.Unlock()
					}
				}(ip, port)
			}
		}
	}

done:
	wg.Wait()

	sort.Slice(found, func(i, j int) bool {
		if found[i].IP != found[j].IP {
			return found[i].IP < found[j].IP
		}
		return found[i].Port < found[j].Port
	})

	resolveHostnames(ctx, found)

	slog.Info("network scan complete", "found", len(found))
	return found, nil
}

// resolveHostnames fills in the Hostname of each device via reverse DNS,
// best-effort. Each unique IP is looked up once, concurrently, with a short
// per-lookup budget — a name is a convenience label for the scan UI, never a
// reason to slow down or fail the scan. IPs that don't resolve keep an empty
// Hostname.
func resolveHostnames(ctx context.Context, devices []FoundDevice) {
	var resolver net.Resolver
	names := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := make(map[string]bool)

	for _, d := range devices {
		if seen[d.IP] {
			continue
		}
		seen[d.IP] = true
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
			defer cancel()
			name := ""
			if hosts, err := resolver.LookupAddr(lctx, ip); err == nil && len(hosts) > 0 {
				name = strings.TrimSuffix(hosts[0], ".")
			}
			// Most home-energy gear has no unicast reverse-DNS record but does
			// answer mDNS, so fall back to a reverse mDNS PTR query.
			if name == "" {
				name = reverseMDNS(lctx, ip)
			}
			if name == "" {
				return
			}
			mu.Lock()
			names[ip] = name
			mu.Unlock()
		}(d.IP)
	}
	wg.Wait()

	for i := range devices {
		if n, ok := names[devices[i].IP]; ok {
			devices[i].Hostname = n
		}
	}
}

// probe tries to TCP-connect to ip:port with a 500 ms timeout.
func probe(ctx context.Context, ip string, port int) (FoundDevice, bool) {
	addr := fmt.Sprintf("%s:%d", ip, port)
	start := time.Now()

	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return FoundDevice{}, false
	}
	latency := time.Since(start).Milliseconds()
	conn.Close()

	return FoundDevice{
		IP:        ip,
		Port:      port,
		Protocol:  wellKnownPorts[port],
		LatencyMs: int(latency),
	}, true
}

// localSubnets returns the /24 prefix for each IPv4 address on
// non-loopback, non-virtual interfaces.
func localSubnets() ([]net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var subnets []net.IPNet

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		// Skip common virtual/container interfaces.
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "virbr") {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue
			}
			// Force /24 regardless of actual mask.
			prefix := fmt.Sprintf("%d.%d.%d", ip4[0], ip4[1], ip4[2])
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			subnets = append(subnets, net.IPNet{
				IP:   net.IPv4(ip4[0], ip4[1], ip4[2], 0),
				Mask: net.CIDRMask(24, 32),
			})
		}
	}

	// Also pull in any other local networks the host knows how to reach via
	// the routing table — e.g. a second LAN reachable through a static route
	// that isn't bound to one of our own interfaces. Bounded to RFC1918 /24../28
	// routes so we never try to sweep a routed /8 or a public range.
	for _, rn := range routedSubnets() {
		ip4 := rn.IP.To4()
		if ip4 == nil || !isPrivateV4(ip4) {
			continue
		}
		if ones, _ := rn.Mask.Size(); ones < 24 || ones > 28 {
			continue
		}
		prefix := fmt.Sprintf("%d.%d.%d", ip4[0], ip4[1], ip4[2])
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		subnets = append(subnets, net.IPNet{
			IP:   net.IPv4(ip4[0], ip4[1], ip4[2], 0),
			Mask: net.CIDRMask(24, 32),
		})
	}

	return subnets, nil
}

// isPrivateV4 reports whether ip is in an RFC1918 range (10/8, 172.16/12,
// 192.168/16). Scans are restricted to these so routing-table discovery can't
// point the scanner at a public or VPN-routed range.
func isPrivateV4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	}
	return false
}

// subnetHosts returns all 254 usable host IPs in a /24 subnet.
func subnetHosts(subnet net.IPNet) []string {
	base := subnet.IP.To4()
	if base == nil {
		return nil
	}
	hosts := make([]string, 0, 254)
	for i := 1; i <= 254; i++ {
		hosts = append(hosts, fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], i))
	}
	return hosts
}
