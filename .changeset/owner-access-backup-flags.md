---
"forty-two-watts": patch
---

Persist the WebAuthn `BackupEligible` / `BackupState` credential flags on enrolled
passkeys and restore them at login. Without this, go-webauthn rejected logins from
synced / backed-up passkeys (iCloud Keychain, Google Password Manager) with
"BackupEligible flag inconsistency during login validation" — the stored credential
reported BE=false while the live assertion reported BE=true. Existing flag-less
credentials must be re-enrolled.
