---
"forty-two-watts": patch
---

Owner remote access: add a real server-side **sign out**. The `ftw_owner`
session cookie is HttpOnly, so the landing page's old client-side
`document.cookie` clear never actually logged you out — the session stayed alive
on the Pi. New `POST /api/owner-access/logout` revokes the session both in
memory and in the persisted store and expires the cookie; the landing's
Sign-out button now calls it. `whoami` also returns `can_sign_out` (false on
LAN-bypass) so the dashboard can show a Sign-out control only on a real remote
session.
