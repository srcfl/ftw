---
"ftw": minor
---

Resolve device `.local` names, so devices can be configured by name instead of a DHCP-assigned IP.

Go never resolves `.local` itself: it hands those names to libc only when cgo is
available, and FTW builds with `CGO_ENABLED=0`, so a configured `zap.local`
became a unicast DNS query to the site router and failed. That is true on every
base image and every libc — shipping `libnss-mdns` changes what `getent` and
`curl` resolve inside the container, not what this process resolves.

FTW now asks the host's own mDNS responder, `avahi-daemon`, over its
simple-protocol socket — the same daemon and the same socket
`libnss_mdns4_minimal.so.2` uses, so a name resolves identically whether FTW
dials it or an operator checks it from a shell in the container. Where that
socket cannot be reached, FTW queries the LAN directly instead. The socket has
to be bind-mounted, and under the Home Assistant Supervisor an add-on cannot
mount arbitrary host paths at all, so the direct path is what makes the feature
work there; it is also the default in Compose, where mounting a host runtime
directory is left to the operator. A successful lookup logs which one answered.

Every driver transport uses it — Modbus TCP, MQTT (driver and Home Assistant
bridge), HTTP including TLS-pinned clients, WebSocket and raw TCP.

Resolution happens per dial rather than once at startup, so a device that moves
to a new DHCP lease is found again on the next reconnect without a config edit.
Answers are cached (30–120 s, following the record TTL where there is one) so
reconnect loops do not flood the LAN, and failures are cached briefly so a
device that is still booting is retried soon. A failed resolution logs
`mDNS resolution failed` and names the mechanism, instead of surfacing as a
generic dial error.

Only `.local` names take this path; literal IPs and ordinary DNS names dial
exactly as before. Multicast still has to reach the LAN, which the Linux
Compose topology has via `network_mode: host`. Under `docker-compose.macos.yml`
the container is bridged, so configure devices by IP there.

Direct queries use each active multicast interface. IPv4 and IPv6 are
supported; link-local IPv6 addresses carry their interface zone and unscoped
answers are rejected. The resolver also rejects non-response DNS packets,
wrong answer classes or families, invalid sources, and Avahi replies whose
interface, name, address family or address does not match the request. mDNS is
unauthenticated, so `capabilities.allow_unverified_local: true` for a driver —
or `homeassistant.allow_unverified_local: true` for the Home Assistant bridge —
gates whether FTW acts on an answer it obtained itself. It gates the resolution
path, not the connection: without it a `.local` name still goes to the system
resolver exactly as it does today, which is what keeps working installs working
under Home Assistant, where Supervisor answers `.local` through its own DNS
service. Host allowlists do not prove server identity, and a TLS pin does not
bypass the gate yet. Literal IP and ordinary DNS endpoints are unchanged.

Network scanning now reports the name a device answers for itself. It used to
ask unicast reverse DNS first and only fall back to mDNS, which meant the
router's label for the lease — `zap-000064963cd51edc.localdomain` on a UniFi
network — was what reached the setup wizard, and the wizard fell back to the
raw IP because that is not a `.local` name. Both queries now run together and a
`.local` answer wins. Reverse mDNS alone is not enough either: RFC 6762 leaves
`in-addr.arpa` mapping optional and many responders publish a forward `A`
record without one, so where no reverse record exists FTW re-asks the label
forward as `<label>.local` and keeps it only when it resolves back to the same
address. Unverified names are still shown, but only as display text.
