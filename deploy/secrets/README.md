# `deploy/secrets/`

Local-only staging area for TLS material and other secrets that need
to be deployed to remote hosts. **Gitignored** — nothing in this
directory is ever committed.

## What's here

| File | What | Lifecycle |
|---|---|---|
| `relay.fortytwowatts.com.cert.pem` | Cloudflare Origin Certificate, public half. Covers `*.fortytwowatts.com` + apex. Valid 2026-05-27 → 2041-05-23. | Renew before 2041; CF dashboard regenerates the pair. |

## What's NOT here (and should never be)

- **Private keys.** Cloudflare shows the key once at certificate
  generation. It travels directly from your browser to the deploy
  host — via `scp`, paste-into-SSH, or a secret manager. Never via
  this repo, never via chat, never via an LLM transcript.

## Installing on the relay VM

See `docs/relay-deploy.md` for the full runbook. Short version:

```bash
# On your laptop:
scp deploy/secrets/relay.fortytwowatts.com.cert.pem ubuntu@<RELAY_IP>:/tmp/cert.pem

# Open the Cloudflare dashboard, regenerate the Origin Cert pair if
# you don't already have the private key, copy the private-key textbox.
# Then, on the relay VM:
ssh ubuntu@<RELAY_IP>
sudo install -m 0644 -o root -g root /tmp/cert.pem /etc/ssl/relay/cert.pem
sudo -e /etc/ssl/relay/key.pem    # paste the private key body, save
sudo chmod 0600 /etc/ssl/relay/key.pem
sudo systemctl restart ftw-relay
```
