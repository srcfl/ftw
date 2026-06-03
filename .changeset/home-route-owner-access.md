---
"forty-two-watts": minor
---

Owner remote access — passkey foundation (home route, Phases 1–3):

- **Safe floor:** a per-process unforgeable tunnel marker excludes relay-tunnelled
  (remote) requests from LAN-bypass, and a global auth-gate wraps the whole mux —
  remote hits now require a passkey session, while genuine LAN/loopback stays
  frictionless. First-enrollment bootstrap is denied over the tunnel (LAN-only).
- **Identity spine:** every Pi generates an always-on self-sovereign ES256 identity
  on first boot (Nova reuses it when federation is enabled); `GET /api/identity`
  exposes the public key; the owner's WebAuthn identity is now a stable opaque
  wallet handle decoupled from the mutable site name (rename-safe).
- **Usernameless login:** discoverable resident-key passkeys + Conditional-UI
  autofill (no username — just Face ID / Touch ID / Windows Hello) with a button
  fallback, plus a backup-passkey recovery nudge.
