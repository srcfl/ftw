---
"forty-two-watts": patch
---

Fix the owner-access sign-in page throwing "OperationError: A request is
already pending." The page started a Conditional-UI (autofill) WebAuthn
ceremony on load and a second one on the "Sign in with passkey" button click
without cancelling the first — browsers allow only one credential request at a
time (a password manager like Bitwarden grabbing the autofill slot makes the
collision near-certain). The page now tracks an `AbortController` and aborts any
in-flight ceremony before starting the next, so the button and autofill no
longer collide.
