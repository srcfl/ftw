# Deploying `ftw-pair-relay`

The pair relay is a single Go binary that matches two peers on a shared
token and pipes encrypted bytes between them. Stateless, ~5 MB, runs on
anything that speaks TCP. End-to-end-encrypted traffic transits via this
relay — the relay sees only ciphertext.

## Production relay (Sourceful-operated)

Current deployment:

- **Host:** AWS Lightsail nano in `eu-north-1` (Stockholm)
- **Static IP:** `16.170.137.95`
- **DNS:** `pair-relay.fortytwowatts.com` → `16.170.137.95` (Cloudflare, free tier)
- **Cost:** $3.50/mo (nano bundle) + $0.005/h elastic IP (~$0 while attached)
- **systemd unit:** `ftw-pair-relay.service`, auto-restarts on failure

Operators don't need to do anything — `ftw-pair` and `ftw-connect`
default to this host.

## DNS (Cloudflare)

Add a single A-record on `fortytwowatts.com`:

| Type | Name                 | Content        | Proxy status |
|------|----------------------|----------------|--------------|
| A    | pair-relay           | 16.170.137.95  | DNS only (grey cloud) |

**Important:** Set the proxy status to "DNS only" (grey cloud). Orange-cloud
(Cloudflare proxy) only handles HTTP — it will not pass the raw TCP traffic
our relay needs.

## Reproducing the deployment from scratch

```bash
REGION=eu-north-1

# 1. Create instance
aws lightsail create-instances \
  --region $REGION \
  --instance-names ftw-pair-relay \
  --availability-zone ${REGION}a \
  --blueprint-id ubuntu_24_04 \
  --bundle-id nano_3_0 \
  --tags key=role,value=pair-relay

# 2. Wait for `running`, then open port 7777
aws lightsail open-instance-public-ports \
  --region $REGION \
  --instance-name ftw-pair-relay \
  --port-info fromPort=7777,toPort=7777,protocol=TCP

# 3. Allocate + attach static IP
aws lightsail allocate-static-ip --region $REGION --static-ip-name ftw-pair-relay-ip
aws lightsail attach-static-ip   --region $REGION --static-ip-name ftw-pair-relay-ip --instance-name ftw-pair-relay
STATIC=$(aws lightsail get-static-ip --region $REGION --static-ip-name ftw-pair-relay-ip --query 'staticIp.ipAddress' --output text)

# 4. Download default SSH key
aws lightsail download-default-key-pair --region $REGION \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['privateKeyBase64'])" \
  > /tmp/lightsail-key.pem
chmod 600 /tmp/lightsail-key.pem

# 5. Build relay (linux/amd64 for Lightsail nano)
cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags="-s -w" -o /tmp/ftw-pair-relay-amd64 ./cmd/ftw-pair-relay

# 6. SCP + systemd
scp -i /tmp/lightsail-key.pem /tmp/ftw-pair-relay-amd64 ubuntu@$STATIC:/tmp/ftw-pair-relay
ssh -i /tmp/lightsail-key.pem ubuntu@$STATIC <<'EOF'
  sudo install -m 755 -o root -g root /tmp/ftw-pair-relay /usr/local/bin/ftw-pair-relay
  sudo tee /etc/systemd/system/ftw-pair-relay.service > /dev/null <<UNIT
[Unit]
Description=ftw-pair-relay
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ftw-pair-relay -addr :7777
Restart=on-failure
RestartSec=5
DynamicUser=yes

[Install]
WantedBy=multi-user.target
UNIT
  sudo systemctl daemon-reload && sudo systemctl enable --now ftw-pair-relay
EOF

# 7. Set DNS (manual via Cloudflare dashboard)
echo "Now add A-record: pair-relay → $STATIC at Cloudflare (DNS only, grey cloud)"
```

## Operations

**Update the relay binary:**

```bash
ssh ubuntu@pair-relay.fortytwowatts.com \
  'sudo systemctl stop ftw-pair-relay; \
   sudo install -m 755 /tmp/ftw-pair-relay-new /usr/local/bin/ftw-pair-relay; \
   sudo systemctl start ftw-pair-relay'
```

Brief downtime (~5 s) — any active pair session breaks and needs to be
re-paired. No state persists across restarts.

**Monitor:**

```bash
ssh ubuntu@pair-relay.fortytwowatts.com 'sudo journalctl -u ftw-pair-relay -f'
```

Look for `relay: matched pair` events (= a session connected end-to-end)
and `relay: pair disconnected` (= clean teardown).

**Resource usage:**

- Memory: ~2 MB resident per active session (just two TCP socket buffers)
- CPU: negligible (`io.Copy` is a syscall loop)
- Bandwidth: depends on session load; MCP tool-call traffic is text JSON
  at < 100 kbps; pcap-capture can spike to MB/s briefly

The nano bundle handles tens of concurrent sessions comfortably.

## TLS

Not needed for the relay itself — the bytes between peers are already
encrypted with ChaCha20-Poly1305 (key derived from the shared token via
HKDF-SHA256). The relay sees only ciphertext and can't read or tamper with
the traffic.

The reason to add TLS would be **firewall traversal**: some restrictive
networks (cafés, conference WiFi, corporate proxies) block non-HTTPS TCP.
Running the relay on `:443` with a real TLS cert (via the `-tls-cert` and
`-tls-key` flags, or Let's Encrypt autocert) would help these users connect.

If we see real-world reports of `connection refused` from friends, that's
the signal to flip TLS on. Not before.
