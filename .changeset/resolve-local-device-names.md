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
unauthenticated, so reserve control-device names on the LAN and use TLS
certificate pins where available.
