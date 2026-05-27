---
"forty-two-watts": minor
---

**Friend-types-code redesign of the pair-approval flow.** v0.104.0 shipped the code on both sides (dashboard + friend's landing page) and required the operator to type it back in — confusing UX, and the cross-origin POST from the LAN dashboard to the public relay was blocked by CORS so the Allow button silently did nothing.

New flow:

- Dashboard displays the 4-digit code along with the URL for the operator to share. "Copy code" and "Copy URL + code" buttons make the bundle easy to send in one Signal/SMS message.
- The relay's landing page **no longer shows the code**. It shows an input field. The friend types the code they received separately from the host.
- POST happens same-origin (browser → relay), no CORS surprises.
- On success, the page reveals the dashboard URL + the `claude mcp add` one-liner.

Security model is unchanged in substance — possession of (URL + code) activates the session — but the flow now matches the operator's mental model (share both, friend types code). The host no longer has to be live at connect-time to approve.

Tests adjusted: relay landing-page test now asserts the code is **NOT** present in the served HTML; component source-hygiene tests assert the operator-side input field is gone. 31 node-tests + Go test suite all green.
