# Running a `ftw-subetha` relay

The pair relay is a single Go binary that matches two peers on a shared
token and pipes encrypted bytes between them. Stateless, ~5 MB, runs on
anything that speaks TCP. End-to-end-encrypted traffic transits via this
relay — the relay sees only ciphertext.

Most operators **don't need to deploy their own** — `ftw-pair` and
`ftw-connect` ship with a default public relay built in (see below).
This document is for the cases where you want to run your own: a private
deployment for a team, a fork that needs an isolated transport, or just
"I'd rather not depend on someone else's infrastructure".

## Default public relay

`ftw-pair` and `ftw-connect` default to a Sourceful-operated public relay
at `subetha.fortytwowatts.com:7777`. It's a single small VM running this
exact binary; we redeploy it from `master` as needed.

Because traffic is end-to-end-encrypted with a token-derived AEAD key,
the relay operator (us) cannot read or tamper with session bytes — they
see only ciphertext. The trust model is therefore "relay is a passthrough,
not a participant". A compromise of the relay means denial of service,
not data exfiltration.

If that trust model isn't acceptable for your use case, run your own.

## Running your own relay

The binary is published as a GitHub release asset
(`ftw-subetha-linux-{amd64,arm64}`, plus mac/windows variants) for every
forty-two-watts release. You can also build from source:

```bash
cd go
CGO_ENABLED=0 go build -ldflags="-s -w" -o ftw-subetha ./cmd/ftw-subetha
```

Run it bound to a TCP port reachable from both ends of the pair session:

```bash
./ftw-subetha -addr :7777
```

That's it. Stateless. No config file, no database, no secrets to provision.

### Point clients at your relay

Both `ftw-pair` (host side) and `ftw-connect` (friend side) take a
`-relay-addr` flag, or read the `FTW_PAIR_RELAY` environment variable:

```bash
# Host side
ftw-pair -relay-addr relay.example.com:7777 ...

# Friend side
ftw-connect -relay-addr relay.example.com:7777 <token>

# Or via env
export FTW_PAIR_RELAY=relay.example.com:7777
```

### systemd unit (optional)

```ini
[Unit]
Description=ftw-subetha relay
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ftw-subetha -addr :7777
Restart=on-failure
RestartSec=5
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

`DynamicUser=yes` means systemd allocates a fresh unprivileged UID per
run — the binary needs no filesystem access and no persistent identity.

### DNS

If you expose the relay under a name, use a plain A-record. Do **not**
proxy it through Cloudflare's HTTP CDN (orange-cloud) — that path only
forwards HTTPS. Subetha is raw TCP, so the proxy will drop the traffic.

## Operations

The relay is effectively stateless, so operations is mostly "restart it
if you change the binary":

```bash
sudo systemctl stop ftw-subetha
sudo install -m 755 ftw-subetha-new /usr/local/bin/ftw-subetha
sudo systemctl start ftw-subetha
```

Brief downtime (~5 s). Any active pair session breaks and needs to
re-pair. Nothing persists across restarts.

**Monitor:**

```bash
sudo journalctl -u ftw-subetha -f
```

Look for `relay: matched pair` (a session connected end-to-end) and
`relay: pair disconnected` (clean teardown). The half-close handling
landed in v0.100.0 — without it, `pair disconnected` events were
delayed up to the idle-reaper timeout under load.

**Resource usage:**

- Memory: ~2 MB resident per active pair session (two TCP socket buffers).
- CPU: negligible — `io.Copy` is a syscall loop.
- Bandwidth: depends on session traffic. MCP tool-call JSON sits at
  < 100 kbps; `pcap_capture` tool calls can spike to MB/s briefly.

A small VM (the cheapest tier on any cloud provider) handles tens of
concurrent sessions comfortably. Choose a region close to your users
to keep round-trip latency low.

## TLS

Not needed for confidentiality — bytes are already encrypted with
ChaCha20-Poly1305 (key derived from the shared token via HKDF-SHA256).
The relay sees only ciphertext and can't read or tamper with the traffic.

The reason to add TLS would be **firewall traversal**: some restrictive
networks (cafés, conference WiFi, corporate proxies) block non-HTTPS TCP.
Running the relay on `:443` with a real TLS cert would help these users
connect. The binary doesn't ship with TLS today — if you need it, wrap
the listener in `stunnel`, `nginx stream`, or similar.

## Protocol reference

The wire protocol is documented in `go/internal/subetha/relay_client.go`
and `go/cmd/ftw-subetha/relay.go`. Short version: 4-byte handshake
(version + role + token-length + token), then raw bytes piped between
matched peers until either side closes. The AEAD framing lives one layer
above the relay; the relay doesn't know or care about it.
