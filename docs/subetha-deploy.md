# Deploying `ftw-subetha`

The pair relay is a single Go binary that matches two peers on a shared
token and pipes encrypted bytes between them. Stateless, ~5 MB, runs on
anything that speaks TCP. End-to-end-encrypted traffic transits via this
relay — the relay sees only ciphertext.

## Production relay (Sourceful-operated)

Current deployment:

- **Host:** AWS Lightsail nano in `eu-north-1` (Stockholm)
- **Static IP:** `16.170.137.95`
- **DNS:** `subetha.fortytwowatts.com` → `16.170.137.95` (Cloudflare, free tier)
- **Cost:** $3.50/mo (nano bundle) + $0.005/h elastic IP (~$0 while attached)
- **systemd unit:** `ftw-subetha.service`, auto-restarts on failure

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
  --instance-names ftw-subetha \
  --availability-zone ${REGION}a \
  --blueprint-id ubuntu_24_04 \
  --bundle-id nano_3_0 \
  --tags key=role,value=pair-relay

# 2. Wait for `running`, then open port 7777
aws lightsail open-instance-public-ports \
  --region $REGION \
  --instance-name ftw-subetha \
  --port-info fromPort=7777,toPort=7777,protocol=TCP

# 3. Allocate + attach static IP
aws lightsail allocate-static-ip --region $REGION --static-ip-name ftw-subetha-ip
aws lightsail attach-static-ip   --region $REGION --static-ip-name ftw-subetha-ip --instance-name ftw-subetha
STATIC=$(aws lightsail get-static-ip --region $REGION --static-ip-name ftw-subetha-ip --query 'staticIp.ipAddress' --output text)

# 4. Download default SSH key
aws lightsail download-default-key-pair --region $REGION \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['privateKeyBase64'])" \
  > /tmp/lightsail-key.pem
chmod 600 /tmp/lightsail-key.pem

# 5. Build relay (linux/amd64 for Lightsail nano)
cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags="-s -w" -o /tmp/ftw-subetha-amd64 ./cmd/ftw-subetha

# 6. SCP + systemd
scp -i /tmp/lightsail-key.pem /tmp/ftw-subetha-amd64 ubuntu@$STATIC:/tmp/ftw-subetha
ssh -i /tmp/lightsail-key.pem ubuntu@$STATIC <<'EOF'
  sudo install -m 755 -o root -g root /tmp/ftw-subetha /usr/local/bin/ftw-subetha
  sudo tee /etc/systemd/system/ftw-subetha.service > /dev/null <<UNIT
[Unit]
Description=Sourceful subetha relay — pair-session match server for forty-two-watts
Documentation=https://github.com/frahlg/forty-two-watts/blob/master/docs/subetha-deploy.md
Documentation=https://github.com/frahlg/forty-two-watts/blob/master/docs/ftw-pair.md
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ftw-subetha -addr :7777
Restart=on-failure
RestartSec=5
DynamicUser=yes

[Install]
WantedBy=multi-user.target
UNIT
  sudo systemctl daemon-reload && sudo systemctl enable --now ftw-subetha
EOF

# 7. MOTD + README so anyone landing on the box understands what's running
ssh -i /tmp/lightsail-key.pem ubuntu@$STATIC 'sudo tee /etc/motd > /dev/null <<MOTD

============================================================================
 Sourceful subetha relay
============================================================================

 This Lightsail nano runs ONE process: ftw-subetha (systemd service).
 It matches two TCP peers on a shared token and pipes encrypted bytes
 between them, so a friend can join a forty-two-watts pair session
 from anywhere on the internet.

 Service status:    sudo systemctl status ftw-subetha
 Live logs:         sudo journalctl -u ftw-subetha -f
 Binary:            /usr/local/bin/ftw-subetha
 Listens on:        :7777 (TCP)
 Public DNS:        subetha.fortytwowatts.com (also pair-relay.fortytwowatts.com)
 Repo:              https://github.com/frahlg/forty-two-watts
 Deploy runbook:    docs/subetha-deploy.md in the repo
 Protocol:          docs/ftw-pair.md in the repo

 Stateless. End-to-end-encrypted. The relay sees only ciphertext.
 Safe to restart at any time — active pair sessions reconnect.
============================================================================
MOTD'
ssh -i /tmp/lightsail-key.pem ubuntu@$STATIC 'sudo install -d /etc/sourceful && sudo tee /etc/sourceful/README.md > /dev/null <<README
# Sourceful subetha relay

This box runs **ftw-subetha** — see /etc/motd for a one-screen summary and
\`docs/subetha-deploy.md\` in the forty-two-watts repo for the full runbook
and ops notes.
README'

# 8. AWS instance tags so the role is visible in the Lightsail console
aws lightsail tag-resource \
  --region $REGION \
  --resource-name ftw-pair-relay \
  --tags key=Role,value=subetha-relay \
         key=Project,value=forty-two-watts \
         key=Service,value=ftw-subetha \
         key=Repo,value=github.com/frahlg/forty-two-watts \
         key=Docs,value=docs/subetha-deploy.md

# 9. Set DNS (manual via Cloudflare dashboard)
echo "Now add A-record: subetha → $STATIC at Cloudflare (DNS only, grey cloud)"
```

## Operations

**Update the relay binary:**

```bash
ssh ubuntu@subetha.fortytwowatts.com \
  'sudo systemctl stop ftw-subetha; \
   sudo install -m 755 /tmp/ftw-subetha-new /usr/local/bin/ftw-subetha; \
   sudo systemctl start ftw-subetha'
```

Brief downtime (~5 s) — any active pair session breaks and needs to be
re-paired. No state persists across restarts.

**Monitor:**

```bash
ssh ubuntu@subetha.fortytwowatts.com 'sudo journalctl -u ftw-subetha -f'
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
