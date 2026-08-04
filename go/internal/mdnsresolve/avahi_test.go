package mdnsresolve

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeAvahi stands in for avahi-daemon. reply is handed the command and the
// name and returns the line the daemon would write back; returning "" makes it
// hang up without answering.
//
// net.Pipe rather than a real unix socket: the protocol is what is under test,
// and the test then runs the same way on every platform the suite runs on.
func fakeAvahi(t *testing.T, reply func(command, name string) string) {
	t.Helper()
	origAvail, origDial := avahiAvailable, avahiDial
	origInterfaceByIndex := avahiInterfaceByIndex
	avahiAvailable = func() bool { return true }
	avahiInterfaceByIndex = func(index int) (*net.Interface, error) {
		return &net.Interface{Index: index, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}, nil
	}
	avahiDial = func(ctx context.Context, path string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			sc := bufio.NewScanner(server)
			if !sc.Scan() {
				return
			}
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				return
			}
			if out := reply(fields[0], fields[1]); out != "" {
				_, _ = io.WriteString(server, out+"\n")
			}
		}()
		return client, nil
	}
	t.Cleanup(func() {
		avahiAvailable, avahiDial = origAvail, origDial
		avahiInterfaceByIndex = origInterfaceByIndex
		Flush()
	})
}

func TestParseAvahiReply(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // empty means the reply must be rejected
	}{
		// The exact shape avahi-daemon returns, confirmed against the daemon.
		{"ipv4", "+ 2 0 zap.local 192.168.1.42", "192.168.1.42"},
		{"ipv6", "+ 2 1 zap.local fe80::1", "fe80::1%test0"},
		// A v4-mapped answer must dial as plain IPv4.
		{"v4 mapped", "+ 2 1 zap.local ::ffff:192.168.1.42", "192.168.1.42"},
		{"wrong name", "+ 2 0 other.local 192.168.1.42", ""},
		{"failure", "- 15 Timeout reached", ""},
		{"empty", "", ""},
		{"truncated", "+ 2 0 zap.local", ""},
		{"unparsable address", "+ 2 0 zap.local not-an-address", ""},
	}
	origInterfaceByIndex := avahiInterfaceByIndex
	avahiInterfaceByIndex = func(index int) (*net.Interface, error) {
		return &net.Interface{Index: index, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}, nil
	}
	t.Cleanup(func() { avahiInterfaceByIndex = origInterfaceByIndex })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseAvahiReply(c.line, "zap.local")
			if c.want == "" {
				if got.err == nil {
					t.Fatalf("accepted %q, got %v", c.line, got.addr)
				}
				return
			}
			if got.err != nil {
				t.Fatalf("rejected %q: %v", c.line, got.err)
			}
			if got.addr.String() != c.want {
				t.Fatalf("addr = %v, want %v", got.addr, c.want)
			}
		})
	}
}

// A failure line must carry avahi's own wording, so the log says what the
// daemon said rather than something this package made up.
func TestParseAvahiReplyKeepsDaemonWording(t *testing.T) {
	got := parseAvahiReply("- 15 Timeout reached", "zap.local")
	if got.err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(got.err.Error(), "Timeout reached") {
		t.Fatalf("error %q dropped the daemon's message", got.err)
	}
}

func TestLookupPrefersAvahi(t *testing.T) {
	Flush()
	fakeAvahi(t, func(command, name string) string {
		if command == "RESOLVE-HOSTNAME-IPV4" && name == "inverter.local" {
			return "+ 2 0 inverter.local 192.168.1.42"
		}
		return "- 15 Timeout reached"
	})

	orig := listenMulticastPacket
	listenMulticastPacket = func(network string, iface *net.Interface, group *net.UDPAddr) (*net.UDPConn, error) {
		t.Error("queried the LAN directly even though avahi answered")
		return nil, errors.New("should not be called")
	}
	t.Cleanup(func() { listenMulticastPacket = orig })

	addrs, err := Lookup(context.Background(), "inverter.local")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.168.1.42") {
		t.Fatalf("addrs = %v, want [192.168.1.42]", addrs)
	}
}

// IPv4 wins when both families answer, so the address a device is dialled on
// does not depend on which goroutine happened to finish first.
func TestAvahiLookupPrefersIPv4(t *testing.T) {
	fakeAvahi(t, func(command, name string) string {
		if command == "RESOLVE-HOSTNAME-IPV6" {
			return "+ 2 1 dual.local fe80::1"
		}
		return "+ 2 0 dual.local 192.168.1.7"
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	addrs, err := avahiLookup(ctx, "dual.local")
	if err != nil {
		t.Fatalf("avahiLookup: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.168.1.7") {
		t.Fatalf("addrs = %v, want [192.168.1.7]", addrs)
	}
}

// A device that only advertises IPv6 still resolves.
func TestAvahiLookupFallsBackToIPv6(t *testing.T) {
	fakeAvahi(t, func(command, name string) string {
		if command == "RESOLVE-HOSTNAME-IPV6" {
			return "+ 2 1 v6only.local fe80::2"
		}
		return "- 15 Timeout reached"
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	addrs, err := avahiLookup(ctx, "v6only.local")
	if err != nil {
		t.Fatalf("avahiLookup: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "fe80::2%test0" {
		t.Fatalf("addrs = %v, want [fe80::2%%test0]", addrs)
	}
}

// A socket that is present but unhelpful — daemon starting, wedged, or simply
// without the record — must not be a dead end.
func TestLookupFallsBackToMulticastWhenAvahiFails(t *testing.T) {
	Flush()
	fakeAvahi(t, func(command, name string) string { return "- 15 Timeout reached" })
	startResponder(t, []dnsmessage.Resource{aResource(t, "inverter.local.", [4]byte{10, 0, 0, 5}, 60)})

	addrs, err := Lookup(context.Background(), "inverter.local")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("10.0.0.5") {
		t.Fatalf("addrs = %v, want [10.0.0.5]", addrs)
	}
}

// When neither backend answers, the error has to say that avahi was asked too
// — "there is no avahi" and "avahi said no" call for different fixes.
func TestLookupErrorNamesBothBackends(t *testing.T) {
	Flush()
	fakeAvahi(t, func(command, name string) string { return "- 15 Timeout reached" })
	startResponder(t, nil) // never answers

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := Lookup(ctx, "missing.local")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "avahi") {
		t.Fatalf("error %q does not mention avahi", err)
	}
}

// A socket that does not exist must not be dialled at all: the probe is what
// keeps a stray connect attempt out of every lookup on a host without avahi.
func TestAvahiSkippedWhenSocketAbsent(t *testing.T) {
	Flush()
	origAvail, origDial := avahiAvailable, avahiDial
	avahiAvailable = func() bool { return false }
	avahiDial = func(ctx context.Context, path string) (net.Conn, error) {
		t.Error("dialled the avahi socket although it is not available")
		return nil, errors.New("should not be called")
	}
	t.Cleanup(func() { avahiAvailable, avahiDial = origAvail, origDial })

	startResponder(t, []dnsmessage.Resource{aResource(t, "inverter.local.", [4]byte{10, 0, 0, 6}, 60)})
	if _, err := Lookup(context.Background(), "inverter.local"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
}
