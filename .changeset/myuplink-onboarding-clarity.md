---
"forty-two-watts": patch
---

Clearer MyUplink onboarding. The device card now shows numbered setup
steps with a link to the MyUplink developer portal, and renders **Client
Identifier** and **Client Secret** together using the exact same labels as
the portal (instead of a separate "Client ID" field and a distant
"Secrets" section), so the two values can't be swapped. The OAuth-managed
refresh_token no longer appears as a hand-editable secret field.
