---
"forty-two-watts": minor
---

Owner remote access: **persist sessions** to `state.db` (a new `owner_sessions`
table) so a Pi restart no longer signs you out — the in-memory session map is
restored on boot. And the owner-access landing now **manages passkeys** when
signed in: list your enrolled passkeys, remove (revoke) one, or add a device.
