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
	Port      int    `json:"port"`
	Protocol  string `json:"protocol"` // "modbus", "mqtt", "http", "https"
	LatencyMs int    `json:"latency_ms"`
}

// wellKnownPorts maps TCP ports to protocol names.
var wellKnownPorts = map[int]string{
	502:  "modbus",
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

	slog.Info("network scan complete", "found", len(found))
	return found, nil
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
	return subnets, nil
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
