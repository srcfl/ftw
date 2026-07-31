// Package mdnsresolve resolves RFC 6762 ".local" host names for outbound
// connections, and provides a dialer that uses it.
//
// Go will not do this itself. net/conf.go routes a ".local" lookup to libc
// only when cgo is available, and every FTW build sets CGO_ENABLED=0, so the
// pure Go resolver is always selected: it reads /etc/resolv.conf and sends a
// *unicast* query to the site router, which has no idea what "inverter.local"
// is. That holds on every base image and every libc, glibc included — the
// image shipping libnss-mdns changes what `getent` and `curl` can resolve
// inside the container, not what this process can.
//
// # Where the answer comes from
//
// First choice is to ask the machine's own mDNS responder, avahi-daemon, over
// its simple-protocol socket. One line out, one line back, no DNS wire format
// decoded here — and it is the same daemon over the same socket that
// libnss_mdns4_minimal.so.2 uses, so a name resolves to the same address
// whether FTW dials it or an operator runs `getent hosts zap.local` in the
// container. avahi also holds a record cache, so a repeat lookup usually costs
// no LAN traffic at all.
//
// Second choice, in multicast.go, is to query the LAN directly. It exists
// because the socket cannot always be reached: it has to be bind-mounted, and
// under the Home Assistant Supervisor an add-on cannot mount arbitrary host
// paths at all — only a fixed set of named ones. FTW ships as an add-on, so a
// resolver that required that socket would simply not work there. The fallback
// also covers any Docker host that does not run avahi.
//
// Only ".local" names take either path. Literal IPs and ordinary DNS names are
// handed straight to the standard dialer.
package mdnsresolve

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// now is swappable so cache-expiry tests do not have to sleep.
var now = time.Now

const (
	// queryTimeout bounds each backend separately, so falling back costs at
	// most two of these rather than an unbounded wait. It is long enough for a
	// sleepy device and short enough that a driver dial does not stall a
	// control tick.
	queryTimeout = 900 * time.Millisecond

	// A responder's TTL is advisory here. The floor stops a device that
	// advertises a very short TTL from turning every Modbus reconnect into a
	// multicast storm; the ceiling keeps a DHCP move from taking effect
	// arbitrarily late, which is the whole point of binding by name.
	minTTL = 30 * time.Second
	maxTTL = 120 * time.Second

	// negativeTTL is deliberately short: a device that was off when we first
	// looked should become reachable soon after it boots.
	negativeTTL = 5 * time.Second
)

type cacheEntry struct {
	addrs   []netip.Addr // empty means a cached negative answer
	expires time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// IsLocal reports whether host is a ".local" name that mDNS should resolve.
// A literal IP is never one, so a configured "192.168.1.5" keeps the plain
// dial path and never touches the network for resolution.
func IsLocal(host string) bool {
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".local")
}

func canonical(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func cacheLookup(key string) ([]netip.Addr, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cache[key]
	if !ok || now().After(entry.expires) {
		return nil, false
	}
	return entry.addrs, true
}

func cacheStore(key string, addrs []netip.Addr, ttl time.Duration) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache[key] = cacheEntry{addrs: addrs, expires: now().Add(ttl)}
}

// Flush drops every cached answer. Tests use it; nothing in production does.
func Flush() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]cacheEntry{}
}

// Lookup resolves a ".local" name to its advertised addresses, asking
// avahi-daemon first and the LAN directly if that is not answerable.
func Lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	key := canonical(name)
	if addrs, ok := cacheLookup(key); ok {
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no mDNS responder for %s (cached)", name)
		}
		return addrs, nil
	}

	addrs, ttl, via, err := resolve(ctx, key)
	if err != nil || len(addrs) == 0 {
		cacheStore(key, nil, negativeTTL)
		if err == nil {
			err = fmt.Errorf("no mDNS responder for %s", name)
		}
		return nil, err
	}

	cacheStore(key, addrs, ttl)
	// Logged on a cache miss only, so this is at most one line per TTL per
	// device rather than one per reconnect. "via" is here because the two
	// backends fail for completely different reasons, and an operator reading
	// a support report needs to know which one was in play.
	slog.Info("resolved host over mDNS", "host", key, "addr", addrs[0].String(), "ttl", ttl, "via", via)
	return addrs, nil
}

// resolve tries avahi, then a direct query. An avahi socket that is present
// but unhelpful still falls through: a daemon that is starting up, wedged or
// misconfigured should degrade to a slower answer, not to no answer.
func resolve(ctx context.Context, key string) ([]netip.Addr, time.Duration, string, error) {
	var avahiErr error
	if avahiAvailable() {
		actx, cancel := context.WithTimeout(ctx, queryTimeout)
		addrs, err := avahiLookup(actx, key)
		cancel()
		if err == nil && len(addrs) > 0 {
			return addrs, avahiTTL, "avahi", nil
		}
		avahiErr = err
	}

	addrs, ttl, err := queryAddrs(ctx, key)
	if err != nil && avahiErr != nil {
		// Both were tried; report both, because "avahi said no and the LAN
		// said nothing" and "there is no avahi and the LAN said nothing" call
		// for different fixes.
		return nil, 0, "", fmt.Errorf("%w (avahi: %v)", err, avahiErr)
	}
	if err != nil {
		return nil, 0, "", err
	}
	return addrs, ttl, "multicast", nil
}

// Dialer dials TCP addresses, resolving ".local" host names over mDNS first.
// The embedded net.Dialer carries timeout and keep-alive; anything that is not
// a ".local" name is handed straight to it.
//
// Resolution happens per dial, not once at startup. That is what makes binding
// a device by name survive a DHCP lease change: callers that rebuild their
// connection from the original address string pick up the new IP on reconnect.
type Dialer struct {
	net.Dialer
}

// DialContext resolves address if it names a ".local" host, then dials it.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !IsLocal(host) {
		return d.Dialer.DialContext(ctx, network, address)
	}

	addrs, err := Lookup(ctx, host)
	if err != nil {
		// Name the mechanism. Without this the operator sees a bare dial
		// failure and has no way to tell that resolution was the reason.
		slog.Warn("mDNS resolution failed; check the device is on this LAN and the container uses host networking",
			"host", host, "err", err)
		return nil, fmt.Errorf("resolve %s over mDNS: %w", host, err)
	}

	var firstErr error
	for _, a := range addrs {
		conn, err := d.Dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("dial %s over mDNS: %w", host, firstErr)
}

// Dial is the context-free form, for callers that have no context to pass.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	ctx := context.Background()
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	return d.DialContext(ctx, network, address)
}

// DialContext dials with default settings.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d Dialer
	return d.DialContext(ctx, network, address)
}

// DialTimeout mirrors net.DialTimeout with mDNS resolution added.
func DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	d := Dialer{Dialer: net.Dialer{Timeout: timeout}}
	return d.Dial(network, address)
}
