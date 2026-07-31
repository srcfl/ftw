package mdnsresolve

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

// avahiSocket is avahi-daemon's simple-protocol socket. This is not a path we
// invented: libnss_mdns4_minimal.so.2 has the same string compiled into it, so
// asking here is exactly what `getent hosts foo.local` does one layer down.
//
// A var so tests can point at a socket they control.
var avahiSocket = "/run/avahi-daemon/socket"

// avahiDial is swappable in tests.
var avahiDial = func(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// avahiTTL is how long an avahi answer is cached here. The protocol carries no
// TTL — avahi keeps its own record cache and re-queries the LAN when its copy
// ages out, so this number only decides how often we ask it, not how fresh the
// answer is. It matches the floor used for a raw multicast answer.
const avahiTTL = minTTL

// avahiAvailable reports whether the socket is present and is really a socket.
//
// The type check earns its place: the socket has to be bind-mounted into the
// container, and when the host path does not exist Docker helpfully creates a
// *directory* at the mount point instead. That looks present to a bare
// os.Stat, connects to nothing, and is the single most likely misconfiguration
// on this path.
// A var so tests can assert both branches without needing a real socket on
// whatever platform `go test` is running on.
var avahiAvailable = func() bool {
	fi, err := os.Stat(avahiSocket)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

type avahiResult struct {
	addr netip.Addr
	err  error
}

// avahiLookup asks avahi-daemon to resolve name, preferring IPv4.
//
// The two address families are asked concurrently on separate connections
// because a name that exists in one family and not the other costs a full
// avahi resolve timeout to answer negatively — serialising them would put that
// wait in front of every dial to a v4-only device. IPv4 is still the answer
// preferred when both arrive.
func avahiLookup(ctx context.Context, name string) ([]netip.Addr, error) {
	v4 := make(chan avahiResult, 1)
	v6 := make(chan avahiResult, 1)
	go func() { v6 <- avahiResolve(ctx, "RESOLVE-HOSTNAME-IPV6", name) }()
	go func() { v4 <- avahiResolve(ctx, "RESOLVE-HOSTNAME-IPV4", name) }()

	r4 := <-v4
	if r4.err == nil {
		return []netip.Addr{r4.addr}, nil
	}
	if r6 := <-v6; r6.err == nil {
		return []netip.Addr{r6.addr}, nil
	}
	return nil, fmt.Errorf("avahi: %s: %w", name, r4.err)
}

// avahiResolve runs one request/response exchange over the socket.
//
// The wire format is a single line each way. A success is
//
//	+ <interface> <protocol> <name> <address>
//
// and a failure is "- <errno> <message>". There is no framing, no length
// prefix and nothing to decode, which is the point of using it: no DNS wire
// format is parsed anywhere on this path.
func avahiResolve(ctx context.Context, command, name string) avahiResult {
	conn, err := avahiDial(ctx, avahiSocket)
	if err != nil {
		return avahiResult{err: fmt.Errorf("connect %s: %w", avahiSocket, err)}
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := fmt.Fprintf(conn, "%s %s\n", command, name); err != nil {
		return avahiResult{err: fmt.Errorf("write request: %w", err)}
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return avahiResult{err: fmt.Errorf("read reply: %w", err)}
		}
		return avahiResult{err: fmt.Errorf("no reply")}
	}
	return parseAvahiReply(scanner.Text())
}

func parseAvahiReply(line string) avahiResult {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return avahiResult{err: fmt.Errorf("empty reply")}
	}
	if fields[0] != "+" {
		// "- 15 Timeout reached" and friends. Hand the daemon's own wording
		// back rather than inventing one, so the log says what avahi said.
		return avahiResult{err: fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(line, "-")))}
	}
	// + <interface> <protocol> <name> <address>
	if len(fields) < 5 {
		return avahiResult{err: fmt.Errorf("short reply %q", line)}
	}
	addr, err := netip.ParseAddr(fields[4])
	if err != nil {
		return avahiResult{err: fmt.Errorf("unparsable address %q", fields[4])}
	}
	return avahiResult{addr: addr.Unmap()}
}
