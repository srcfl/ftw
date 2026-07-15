# Deploying the `ftw-relay` HTTPS relay

The new relay terminates HTTPS for `relay.ftw.sourceful.energy` and
serves the request-response tunnel. This doc walks through the one-time
deploy on the AWS VM that previously ran the raw-TCP `ftw-subetha`
binary. Historical design context lives in
[`archive/agent-artifacts/goals/relay-as-tunnel.md`](archive/agent-artifacts/goals/relay-as-tunnel.md).

> **Provisioning status:** repository defaults now use the Sourceful relay and
> TURN names, but those endpoints are not declared operational by this change.
> Complete and verify this runbook before relying on remote access. Local
> control continues to work if relay/TURN is unavailable.

## Topology

```
Browser / Claude Code
        │ HTTPS (CF edge cert)
        ▼
Cloudflare proxy (orange cloud, full-strict mode)
        │ HTTPS (CF Origin Cert)
        ▼
AWS VM, port 443
        │ systemd: ftw-relay.service
        ▼
ftw-relay  ── manages tunnel queues, validates tokens, serves /h/<token>/{,mcp,web/...}
        │ HTTP request-response over the existing tunnel
        ▼
Pi running ftw-pair (outbound long-poll, no inbound ports)
```

Trust path: friend → CF → relay VM → Pi. Three hops. CF and the relay
VM both see plaintext; see the archived relay design's security section for
the original trade-off analysis.

## Prerequisites

- AWS VM (the previous relay VM may be reused).
- Public IP, port `:443` reachable from the internet.
- DNS/TLS access for the `sourceful.energy` zone.
- New TLS material covering `relay.ftw.sourceful.energy`. The legacy
  `*.fortytwowatts.com` origin certificate does not cover this host.

## One-time DNS + CF setup

In the DNS/CDN dashboard for `sourceful.energy`:

1. **DNS** → add `A relay.ftw → <AWS VM IP>`, **Proxied** (orange cloud).
2. **SSL/TLS → Overview** → mode: **Full (strict)**.
3. **SSL/TLS → Edge Certificates**:
   - Always Use HTTPS: **On**
   - HSTS: enable for the relay response only after HTTPS is verified. Do not
     change apex-wide Sourceful HSTS/preload policy from this runbook.
   - Minimum TLS version: **TLS 1.2** (or 1.3 if you want to drop
     older clients — fine for our use).
4. Verify edge and origin TLS before enabling production traffic.

## One-time VM setup

```bash
# As your normal user with sudo
sudo mkdir -p /etc/ssl/relay
sudo chmod 0750 /etc/ssl/relay
sudo chown root:root /etc/ssl/relay
```

### Cert + key

From your laptop:

```bash
scp deploy/secrets/relay.ftw.sourceful.energy.cert.pem \
    ubuntu@relay.ftw.sourceful.energy:/tmp/cert.pem
```

On the VM:

```bash
sudo install -m 0644 -o root -g root /tmp/cert.pem /etc/ssl/relay/cert.pem

# Paste the private key body — never copy via curl, ssh -t cat, or
# anywhere it might land in scrollback. `sudoedit` opens a temp file
# in your $EDITOR, writes back to root-owned path on save.
sudo -e /etc/ssl/relay/key.pem
sudo chmod 0600 /etc/ssl/relay/key.pem
sudo chown root:root /etc/ssl/relay/key.pem
```

Verify:

```bash
sudo openssl x509 -in /etc/ssl/relay/cert.pem -noout -dates -subject
# Check that the dates and SANs cover relay.ftw.sourceful.energy.
sudo openssl rsa -in /etc/ssl/relay/key.pem -check -noout
# RSA key ok
```

Confirm the cert + key are a pair:

```bash
sudo openssl x509 -in /etc/ssl/relay/cert.pem -noout -modulus | openssl md5
sudo openssl rsa  -in /etc/ssl/relay/key.pem  -noout -modulus | openssl md5
# both md5 hashes must match
```

### Binary

```bash
# Build matrix from CI uploads to GitHub releases.
sudo curl -fsSL -o /usr/local/bin/ftw-relay \
  https://github.com/srcfl/ftw/releases/latest/download/ftw-relay-linux-amd64
sudo chmod 0755 /usr/local/bin/ftw-relay
sudo chown root:root /usr/local/bin/ftw-relay
```

### Systemd unit

```bash
sudo tee /etc/systemd/system/ftw-relay.service >/dev/null <<'EOF'
[Unit]
Description=ftw-relay HTTPS tunnel for relay.ftw.sourceful.energy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
ExecStart=/usr/local/bin/ftw-relay \
  -addr :443 \
  -cert /etc/ssl/relay/cert.pem \
  -key  /etc/ssl/relay/key.pem
Restart=on-failure
RestartSec=2

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadOnlyPaths=/etc/ssl/relay
LockPersonality=true
RestrictRealtime=true
RestrictNamespaces=true
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now ftw-relay
sudo systemctl status ftw-relay
```

`DynamicUser=yes` gives the relay a transient unprivileged user;
`AmbientCapabilities=CAP_NET_BIND_SERVICE` lets it bind `:443` without
root. The `ReadOnlyPaths` line means a relay compromise still cannot
overwrite cert or key.

### ICE / TURN for owner remote access

`home.fortytwowatts.com` owner traffic uses a signed WebRTC DataChannel. Direct
STUN works on many networks, but hard NAT / CGNAT / cellular paths need TURN for
the feature to be usable. The relay serves `GET /signal/ice` (STUN URL + a
short-lived coturn REST credential derived from `FTW_TURN_SECRET`); the browser
and Pi both fetch it before WebRTC setup. TURN relays DTLS ciphertext only — the
Pi-signed DTLS fingerprint remains the trust check, so a hostile TURN can read or
drop ciphertext but never MITM.

This needs a TURN server. We self-host coturn. The runbook below stands one up.

> **Pending site decisions** (marked `<…>`): the relay VM's public IP, and the
> TLS path for `turns:` (reuse the CF Origin cert if its SAN is a wildcard, else
> issue a Let's Encrypt cert for `turn.*`). Fill them before running.

#### 1. DNS — a dedicated, grey-cloud TURN host

TURN media (UDP/TCP 3478, TLS 5349, and a high UDP relay range) **cannot
traverse Cloudflare's HTTP proxy**. coturn must be reachable on a real public IP.
Add `turn.ftw.sourceful.energy` as an **A record with proxy status _DNS only_ (grey
cloud)** → the relay VM's public IP. Leave `relay.ftw.sourceful.energy` orange-cloud
(its HTTPS is unchanged). An orange cloud on `turn.*` silently breaks TURN.

#### 2. coturn `/etc/turnserver.conf` (REST-API mode)

```bash
sudo apt-get install -y coturn
sudo sed -i 's/^#TURNSERVER_ENABLED=1/TURNSERVER_ENABLED=1/' /etc/default/coturn
openssl rand -hex 32   # -> TURN_SECRET; store it, it must equal FTW_TURN_SECRET
```

```ini
# /etc/turnserver.conf
listening-port=3478
tls-listening-port=5349
listening-ip=<PUBLIC_IP>
external-ip=<PUBLIC_IP>            # or <PUBLIC_IP>/<PRIVATE_IP> behind 1:1 NAT
min-port=49152
max-port=65535

# REST API shared secret — MUST byte-for-byte equal the relay's FTW_TURN_SECRET.
use-auth-secret
static-auth-secret=<TURN_SECRET>
realm=turn.ftw.sourceful.energy

# TLS for turns:5349 (see step 4 for which cert)
cert=/etc/coturn/cert.pem
pkey=/etc/coturn/key.pem
no-tlsv1
no-tlsv1_1
fingerprint

# SSRF guard: a public TURN must NEVER relay into private space / cloud metadata.
no-multicast-peers
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff

# Anti-abuse quotas (a public TURN is an open-relay magnet). Tune to the VM.
total-quota=200
user-quota=12
bps-capacity=6250000   # ~50 Mbit/s server-wide
max-bps=500000         # ~4 Mbit/s per allocation
no-tcp-relay
no-cli
```

`use-auth-secret` + `static-auth-secret` is the exact dual of the relay's
`HMAC-SHA1(secret, expiry)` / base64 scheme — do **not** also set `lt-cred-mech`
or a `userdb`. The `denied-peer-ip` list is the critical hardening: `/signal/ice`
is unauthenticated, so anyone can mint a 12 h credential; without the deny-list
they could relay into `169.254.169.254` or the VPC.

#### 3. Firewall — direct, not via Cloudflare

| Port / range | Proto | Cloudflare? |
|---|---|---|
| 3478 | UDP + TCP | **NO — direct to coturn** |
| 5349 | TCP (TLS) | **NO — direct** |
| 49152–65535 | UDP (relay range) | **NO — direct** |
| 443 | TCP (relay HTTPS) | YES (orange cloud, unchanged) |

Open these on both the host firewall and the cloud security group. Keep the TURN
ports open to `0.0.0.0/0` — clients are arbitrary browsers/Pis, not CF edges, so
do **not** extend any "CF-ranges-only" rule (that lock is for `:443` only). The
`denied-peer-ip` list + quotas are the protection on the TURN ports.

#### 4. TLS for `turns:5349`

coturn drops to the `turnserver` user and can't read `/etc/ssl/relay`. Either
reuse the relay's CF Origin cert **if** its SAN is a wildcard
(`openssl x509 -in /etc/ssl/relay/cert.pem -noout -text | grep -A1 'Alternative Name'`),
or issue a browser-trusted Let's Encrypt cert (the grey-cloud `turn.*` makes
HTTP-01/DNS-01 work directly). Install as `/etc/coturn/{cert,key}.pem` owned by
`turnserver`. coturn does **not** auto-reload certs — on LE renewal, add a
`renewal-hooks/deploy` script that re-installs them and `systemctl restart coturn`.

#### 5. Relay systemd — secret via EnvironmentFile (not argv)

```bash
sudo install -m 0600 /dev/null /etc/ftw-relay.env
printf 'FTW_TURN_SECRET=%s\n' '<TURN_SECRET>' | sudo tee /etc/ftw-relay.env >/dev/null
```

Add to the `ftw-relay` unit that serves `home.*` (the multi-tenant unit if that's
the one fronting the home route). `FTW_TURN_SECRET` is the default source for
`-turn-secret`, so do **not** pass the secret on the command line (it would leak
into `ps`):

```ini
EnvironmentFile=/etc/ftw-relay.env
ExecStart=/usr/local/bin/ftw-relay \
  -addr :443 \
  -cert /etc/ssl/relay/cert.pem \
  -key  /etc/ssl/relay/key.pem \
  -ice-stun stun:stun.l.google.com:19302 \
  -turn-url turn:turn.ftw.sourceful.energy:3478?transport=udp,turns:turn.ftw.sourceful.energy:5349
```

```bash
sudo systemctl daemon-reload && sudo systemctl restart ftw-relay
```

#### 6. Verify

Order: DNS grey-cloud → coturn up → relay secret + restart → check `/signal/ice`
→ browser test from off-net.

```bash
# 1. The relay now advertises a TURN entry:
curl -s https://home.fortytwowatts.com/signal/ice | jq .
#    -> expect a stun entry AND a turn entry with username/credential/ttl.

# 2. Prove coturn accepts the relay-minted credential (lift it from the JSON):
ICE=$(curl -s https://home.fortytwowatts.com/signal/ice)
U=$(echo "$ICE" | jq -r '.ice_servers[]|select(.username)|.username')
C=$(echo "$ICE" | jq -r '.ice_servers[]|select(.credential)|.credential')
turnutils_uclient -v -u "$U" -w "$C" turn.ftw.sourceful.energy        # UDP
turnutils_uclient -v -S -u "$U" -w "$C" -p 5349 turn.ftw.sourceful.energy  # TLS
#    401 = secret mismatch; 403 to a private target = SSRF deny working.

# 3. Browser: https://webrtc.github.io/samples/src/content/peerconnection/trickle-ice/
#    add turn:turn.ftw.sourceful.energy:3478?transport=udp with $U/$C, Gather
#    candidates, and confirm a candidate of type "relay" appears.
```

Then log in to `home.fortytwowatts.com` from a **cellular / hard-NAT** network
(STUN alone covers same-LAN) and confirm the owner channel connects.

To roll back, drop `-turn-url` from `ExecStart` and restart — the relay falls
back to STUN-only (the TURN entry is gated on `len(TURNURLs)>0 && TURNSecret!=""`).

## Verify end-to-end

From your laptop:

```bash
# Cert + chain
echo | openssl s_client -connect relay.ftw.sourceful.energy:443 \
  -servername relay.ftw.sourceful.energy 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates

# Should see "Connection: close" 200 OK or a 404 if no token is hit
curl -v https://relay.ftw.sourceful.energy/healthz
```

## Renewal

Record the new origin certificate's actual expiry in the operations calendar;
do not reuse the legacy certificate's 2041 date. Cloudflare-managed edge
certificates renew automatically, while origin certificate renewal depends on
the certificate type selected above.

## Multi-tenant home route + onboarding bootstrap

The relay can run as a **public multi-tenant front door**: one `home.*` host that
serves only a tiny relay-disk loader/login bundle. After the browser decrypts
its directory, dashboard static GETs are routed to the selected Pi; owner data
traffic rides the DTLS DataChannel (P2P-only). It is **off by default**.

Do **not** point `-home-web` at the Pi dashboard `web/` directory. Use the
release asset `ftw-relay-web.tar.gz`, which is built from
`web/relay-bootstrap-files.txt` and intentionally excludes dashboard app files
such as `next-app.js`, settings tabs, charts, and component modules.

Install the relay bootstrap bundle:

```
sudo mkdir -p /usr/local/share/ftw-relay-web
curl -fsSL -o /tmp/ftw-relay-web.tar.gz \
  https://github.com/srcfl/ftw/releases/latest/download/ftw-relay-web.tar.gz
sudo tar -C /usr/local/share/ftw-relay-web -xzf /tmp/ftw-relay-web.tar.gz
```

Then start the relay with:

```
ftw-relay \
  -addr :443 -cert … -key … \
  -multi-tenant \
  -home-host home.fortytwowatts.com \
  -home-web /usr/local/share/ftw-relay-web \
  -wallet-blob-dir /var/lib/ftw-relay/blobs   # per-wallet ENCRYPTED directory blobs (relay never decrypts)
```

`-home-site` / `-home-pubkey` become no-ops under `-multi-tenant`.

### What the relay stores (and never reads)

- **`-wallet-blob-dir`** — one `<user_handle>.blob` file per wallet: the wallet's
  AES-GCM-encrypted directory of its boxes. The relay stores ciphertext, pins a
  per-wallet Ed25519 write key (TOFU), and never decrypts. `-wallet-blob-max-bytes`
  (default 65536) caps each blob so a hostile client can't grow the store.
- **Bootstrap store** — in-memory, TTL'd (10 min), blind. Holds, per site, the
  Pi-signed instance descriptor during the brief first-enrollment window, keyed
  by `claim_key = hex(sha256(bootstrap_id))`.

### The onboarding bootstrap surface

When an owner taps **Set up remote access** on the LAN, the box mints a 6-digit
PIN **and** a high-entropy `bootstrap_id` (≥32 bytes CSPRNG, base64url). The raw
`bootstrap_id` is shown to the LAN browser only (it travels in the onboarding
link's `#fragment`); the relay only ever sees its digest. Three endpoints, all
registered only under `-multi-tenant`:

- **`PUT /bootstrap/{site_id}`** — the box parks its signed descriptor.
  Writer-authenticated against the site's pinned ES256 key (the one
  `/me/register` pinned). Body: `{descriptor, claim_key, ts_ms, sig}` where
  `claim_key = hex(sha256(bootstrap_id))` (64-char lowercase hex), `ts_ms` is the
  box's mint time, and `sig` is the ES256 raw `r||s` **hex** signature over
  ```
  "ftw-bootstrap:v1:" + site_id + ":" + claim_key + ":" + ts_ms + ":" + hex(sha256(descriptor))
  ```
  The relay rejects `|now_ms − ts_ms| > 30000` (replay guard) **after** the
  signature verifies. `401` on a bad sig, `400` on a malformed `claim_key` /
  stale `ts_ms`, `404` for an unknown site, `413`/`503` on the size/store caps.
- **`POST /bootstrap/claim`** — a fresh browser that holds the `bootstrap_id`
  (from the link `#fragment`) computes the same `claim_key` and pulls the parked
  descriptor back: `{claim_key}` → `{site_id, descriptor}`. Unauthenticated (the
  `claim_key` is a 256-bit unguessable bearer handle, **not** the PIN) but
  rate-limited per source IP. A miss is a clean `404`. The browser verifies the
  descriptor's inner Pi signature client-side before trusting it.
- **`POST home.*/api/owner-access/enroll/{start,finish}`** — the **one** narrow
  owner-API exception to P2P-only. A first-time user has no device key yet, so
  they can't open the P2P channel to enroll one; this bridges exactly that gap
  and only that. The relay gates on `?claim_key` (resolving a live bootstrap
  blob), then forwards the browser's **entire query string minus the
  relay-private `claim_key`** to the box (url-encoded, so values can't inject
  stray query params). That carries `?pin` — which the box validates (5-try
  burn), the relay never inspecting it — and, on `enroll/finish`, the
  `?ceremony_token` + `?name` + `?bootstrap_proof` the box's finish handler reads
  (the browser sends those only in the query, not the body; dropping them would
  make the box `400` "ceremony_token required" and enrollment could never
  complete). The box's owner session cookie is stripped at the relay boundary. A
  missing or dead `claim_key` is the same `403` (an anonymous caller learns
  nothing about which sites have an open window).

**`bootstrap_proof` — ceremony-bound, body-bound possession proof (closes the
relay-visible PIN **and** a `device_pubkey`-substitution attack).** Because the
relay forwards `?pin`, a *compromised* relay would see the 6-digit PIN in transit.
So the browser also sends, on `enroll/finish` only,
```
bootstrap_proof = hex( HMAC-SHA256(
    key = utf8(bootstrap_id),
    msg = utf8( ceremony_token + "|" + hex(sha256(finish_body)) ) ) )
```
where `ceremony_token` is the opaque token the box issued at `enroll/start` and
`finish_body` is the **exact byte string the browser POSTs** (the browser hashes
the body string once and sends it verbatim; the box hashes the exact bytes it
buffers before handing them to WebAuthn). The box recomputes the HMAC over the
same `(bootstrap_id, ceremony_token, sha256(finish_body))` and constant-time
compares it; a missing, empty, or mismatched proof is a `403` and no device is
saved. This check fires **only on the tunneled (relay-forwarded) path** — an
untunneled LAN finish needs no proof.

Binding a hash of the finish body authenticates the **entire** payload — the
WebAuthn attestation, the friendly `name`, and crucially the top-level
`device_pubkey` (the C4 silent-login key the box pins as a trusted device). Without
it, a MITM relay could pass the user's valid attestation and valid proof through
while replacing `device_pubkey` with its OWN P-256 key; that key would become
trusted and device-PoP would mint `ftw_owner` for the relay. With it, any tamper
changes the body hash, so the relay would have to recompute the HMAC — which needs
the raw `bootstrap_id` it never holds (it has only `sha256(bootstrap_id)`). The
relay can therefore neither forge a proof for its own `ceremony_token`, nor reuse
the user's (single-use) one, nor alter any forwarded byte. A relay that captured the
PIN still cannot run its own WebAuthn enrollment nor substitute a device key in the
zero-device window. The proof is a **third, separate** HMAC — not the inner
descriptor signature (base64url) and not the outer publish signature (ES256 hex).

**Single-use before side effects (closes a concurrent-double-finish race).** On
`enroll/finish` the relay atomically **RESERVES** the bootstrap (a test-and-set
on the store) **before** forwarding the request to the box. A concurrent second
finish that also passed the live gate loses the latch and is refused `403`
*before* its enroll could ever reach the box. The box `200` then **burns** the
window (single-use); any non-`200`/tunnel error **releases** the reservation so
the user can retry without the box re-publishing. As a source-of-truth backstop,
the box also **re-checks the zero-device window at finish time** on the tunneled
path: if any trusted device already exists it refuses `403` even with a correct
proof — so the lost-response edge (the relay never hears the `200`) can never
enroll a second device.

**The production enroll-forward host.** The two `enroll/{start,finish}` POSTs are
enqueued onto the box's tunnel queue. The box drains them with a narrow tunnel
host (`cmd/ftw/owner_relay_register.go`, `staticAssetHandler`) that
serves **only** those two POSTs, stamps the per-process `X-FTW-Tunnel` marker so
the box's `isTunneled` gate fires (PIN + possession-proof + zero-device recheck +
owner-cookie suppression), and strips `Set-Cookie` on the way back. Every other
`/api/*` path and every non-`GET` method stays fail-closed. This is enabled by
default in official releases because first-device setup needs it; set
`FTW_MULTI_TENANT=off` only for a self-hosted relay that will never use public
first-device bootstrap.

**Two secrets, two checkers, by design:** the relay gate is the high-entropy
`claim_key`; the PIN is the box's separate LAN-presence factor; the
`bootstrap_proof` binds the ceremony — and the exact finish body — to possession of
the raw `bootstrap_id`. A leaked store key never reveals a guessable PIN, and the
relay can't ride the enroll forward without both the PIN *and* a proof only the
genuine browser can compute — and even then it cannot alter a single forwarded byte
(including `device_pubkey`) without invalidating that proof.

## Migration from the old subetha relay

`subetha.fortytwowatts.com:7777` runs the raw-TCP byte-pipe relay
today. Nobody depends on it (no field installs of `ftw-connect`),
so the migration is:

1. Bring up `relay.ftw.sourceful.energy` per this doc.
2. Update host code (`ftw-pair`) to long-poll the new relay instead
   of dialing the subetha TCP endpoint.
3. Cut the next release; Pi instances pick up the new client.
4. After one release, decommission `ftw-subetha`:
   - `sudo systemctl disable --now ftw-subetha`
   - `sudo rm /etc/systemd/system/ftw-subetha.service`
   - `sudo rm /usr/local/bin/ftw-subetha`
   - Remove the `:7777` DNS record (or repurpose).

The new relay can co-exist with the old one on the same VM for the
overlap window — different ports, separate systemd units.
