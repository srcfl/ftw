// Package scanner discovers devices on the local network by TCP-dialing
// common energy-device ports (Modbus 502, MQTT 1883, HTTP 80, HTTPS 8443)
// across all /24 subnets attached to non-loopback, non-virtual interfaces.
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
	Hostname  string `json:"hostname,omitempty"`
	Port      int    `json:"port"`
	Protocol  string `json:"protocol"` // "modbus", "mqtt", "http", "https"
	LatencyMs int    `json:"latency_ms"`
}

// wellKnownPorts maps TCP ports to protocol names.
var wellKnownPorts = map[int]string{
	502:  "modbus",
	503:  "modbus", // common TCP-proxy port for inverters and batteries
	1502: "modbus", // solaredge-proxy default
	1883: "mqtt",
	80:   "http",
	8443: "https", // NIBE S-series Local REST API + other on-prem HTTPS device APIs
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

// resolveHostnames adds best-effort reverse DNS/mDNS labels without making a
// missing name fail the scan. Each IP is resolved once and all lookups run in
// parallel, so the extra latency is bounded by the slowest lookup.
func resolveHostnames(ctx context.Context, devices []FoundDevice) {
	var resolver net.Resolver
	names := make(map[string]string)
	seen := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, d := range devices {
		if seen[d.IP] {
			continue
		}
		seen[d.IP] = true
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			dnsCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
			hosts, err := resolver.LookupAddr(dnsCtx, ip)
			cancel()
			name := ""
			if err == nil && len(hosts) > 0 {
				name = strings.TrimSuffix(hosts[0], ".")
			}
			if name == "" && ctx.Err() == nil {
				mdnsCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
				name = reverseMDNS(mdnsCtx, ip)
				cancel()
			}
			if name != "" {
				mu.Lock()
				names[ip] = name
				mu.Unlock()
			}
		}(d.IP)
	}
	wg.Wait()
	for i := range devices {
		devices[i].Hostname = names[devices[i].IP]
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

	// Include small private routed networks, preserving their real masks. A
	// routed /28 must never be expanded into a /24 scan outside that route.
	for _, routed := range routedSubnets() {
		ip4 := routed.IP.To4()
		ones, bits := routed.Mask.Size()
		if ip4 == nil || bits != 32 || !isPrivateV4(ip4) || ones < 24 || ones > 28 {
			continue
		}
		routed.IP = ip4.Mask(routed.Mask)
		duplicate := false
		for _, existing := range subnets {
			existingOnes, _ := existing.Mask.Size()
			if existingOnes <= ones && existing.Contains(routed.IP) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			subnets = append(subnets, routed)
		}
	}
	return subnets, nil
}

func isPrivateV4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 10 ||
		(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
		(ip4[0] == 192 && ip4[1] == 168)
}

// subnetHosts returns usable IPv4 host addresses for a /24../30 subnet.
func subnetHosts(subnet net.IPNet) []string {
	base := subnet.IP.To4()
	if base == nil {
		return nil
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones < 24 || ones > 30 {
		return nil
	}
	network := base.Mask(subnet.Mask)
	count := 1 << (32 - ones)
	hosts := make([]string, 0, count-2)
	start := uint32(network[0])<<24 | uint32(network[1])<<16 | uint32(network[2])<<8 | uint32(network[3])
	for offset := 1; offset < count-1; offset++ {
		v := start + uint32(offset)
		hosts = append(hosts, fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v)))
	}
	return hosts
}
