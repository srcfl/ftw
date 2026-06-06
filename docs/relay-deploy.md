# Deploying the `ftw-relay` HTTPS relay

The new relay terminates HTTPS for `relay.fortytwowatts.com` and
serves the request-response tunnel. This doc walks through the one-time
deploy on the AWS VM that previously ran the raw-TCP `ftw-subetha`
binary. Historical design context lives in
[`archive/agent-artifacts/goals/relay-as-tunnel.md`](archive/agent-artifacts/goals/relay-as-tunnel.md).

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

- AWS VM (whatever the current `subetha.fortytwowatts.com` runs on).
- Public IP, port `:443` reachable from the internet.
- Cloudflare account with `fortytwowatts.com` zone.
- TLS material: cert in `deploy/secrets/relay.fortytwowatts.com.cert.pem`
  (in this repo, gitignored); private key in your password manager
  (Cloudflare showed it once at generation — if you don't have it,
  regenerate the pair in the CF dashboard).

## One-time DNS + CF setup

In the Cloudflare dashboard for `fortytwowatts.com`:

1. **DNS** → add `A relay → <AWS VM IP>`, **Proxied** (orange cloud).
2. **SSL/TLS → Overview** → mode: **Full (strict)**.
3. **SSL/TLS → Edge Certificates**:
   - Always Use HTTPS: **On**
   - HSTS: **Enable** with `max-age = 31536000`, include subdomains,
     preload. Confirm you understand the consequences before
     enabling preload on the apex.
   - Minimum TLS version: **TLS 1.2** (or 1.3 if you want to drop
     older clients — fine for our use).
4. **Submit `fortytwowatts.com` to `hstspreload.org`** once HSTS is
   verified live. The submission is permanent; the apex needs to
   serve HTTPS for every subdomain that ever exists, forever.

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
scp deploy/secrets/relay.fortytwowatts.com.cert.pem \
    ubuntu@relay.fortytwowatts.com:/tmp/cert.pem
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
# notAfter=May 23 08:26:00 2041 GMT  ← good
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
  https://github.com/frahlg/forty-two-watts/releases/latest/download/ftw-relay-linux-amd64
sudo chmod 0755 /usr/local/bin/ftw-relay
sudo chown root:root /usr/local/bin/ftw-relay
```

### Systemd unit

```bash
sudo tee /etc/systemd/system/ftw-relay.service >/dev/null <<'EOF'
[Unit]
Description=ftw-relay HTTPS tunnel for relay.fortytwowatts.com
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

## Verify end-to-end

From your laptop:

```bash
# Cert + chain
echo | openssl s_client -connect relay.fortytwowatts.com:443 \
  -servername relay.fortytwowatts.com 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates

# Should see "Connection: close" 200 OK or a 404 if no token is hit
curl -v https://relay.fortytwowatts.com/healthz
```

## Renewal

The Origin Cert is valid for 15 years (until 2041). No automatic
renewal. Calendar reminder: 2040-11-01 → regenerate in Cloudflare
dashboard, redo "Cert + key" section above. The CF edge cert in
front of the relay is auto-renewed by Cloudflare with no action
required.

## Multi-tenant home route + onboarding bootstrap

The relay can run as a **public multi-tenant front door**: one `home.*` host that
serves only the relay-disk SPA shell and routes each signed-in wallet to its own
Pi. It never forwards owner data to a Pi — owner traffic rides the DTLS
DataChannel (P2P-only). It is **off by default**; turn it on with:

```
ftw-relay \
  -addr :443 -cert … -key … \
  -multi-tenant \              # implies -require-device-key (fail closed)
  -require-device-key \        # C2 signaling gate: an offer must carry a device-key proof
  -home-host home.fortytwowatts.com \
  -home-web /usr/local/share/ftw-web \   # the web/ bundle served from the relay disk
  -wallet-blob-dir /var/lib/ftw-relay/blobs   # per-wallet ENCRYPTED directory blobs (relay never decrypts)
```

`-multi-tenant` refuses to start without `-require-device-key` (a forged
`site_id` would otherwise skip the proof and the relay could contact the wrong
Pi). `-home-site` / `-home-pubkey` become no-ops under `-multi-tenant`.

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
  `?ceremony_token` + `?name` the box's finish handler reads (the browser sends
  those only in the query, not the body; dropping them would make the box `400`
  "ceremony_token required" and enrollment could never complete). The forward is
  single-use: a `200` enroll/finish atomically consumes the blob, closing the
  window. The box's owner session cookie is stripped at the relay boundary. A
  missing or dead `claim_key` is the same `403` (an anonymous caller learns
  nothing about which sites have an open window).

**Two secrets, two checkers, by design:** the relay gate is the high-entropy
`claim_key`; the PIN is the box's separate LAN-presence factor. A leaked store
key never reveals a guessable PIN, and the relay can't ride the enroll forward
without the PIN the box still demands.

## Migration from the old subetha relay

`subetha.fortytwowatts.com:7777` runs the raw-TCP byte-pipe relay
today. Nobody depends on it (no field installs of `ftw-connect`),
so the migration is:

1. Bring up `relay.fortytwowatts.com` per this doc.
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
