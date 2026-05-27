---
"forty-two-watts": patch
---

Internal groundwork for owner remote access via passkey: adds the `trusted_devices` table in state.db with full CRUD (`SaveTrustedDevice`, `LoadTrustedDevices`, `LookupTrustedDevice`, `UpdateTrustedDeviceSignCount`, `DeleteTrustedDevice`) and pulls in `github.com/go-webauthn/webauthn` as a direct dependency. No user-visible surface yet — the host endpoints, relay `/me/<site-id>` routing, and enrollment/login UIs land in follow-up commits on this branch.
