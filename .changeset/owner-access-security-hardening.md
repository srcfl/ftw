---
"forty-two-watts": patch
---

Owner remote access hardening (security review): (1) deleting a passkey now
revokes its active sessions immediately, so revoking a lost device actually logs
it out instead of leaving its session alive until the 24 h TTL; (2) the LAN
bootstrap enrollment PIN is burned after 5 wrong guesses, so its 6-digit space
can't be brute-forced within the 10-minute window.
