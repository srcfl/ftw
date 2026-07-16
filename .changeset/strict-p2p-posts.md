---
"forty-two-watts": patch
---

Fail closed for direct state-changing `p2pFetch` API calls on public relay origins so owner ceremony and control request bodies are never raw-fetched to the relay.

Enable short TCP keepalive on Modbus TCP driver sockets to make stale inverter sessions recover faster after network/controller interruptions.

Use MyUplink's known-working OAuth scope set by default and expose an override for installations that can use narrower read-only auth.

Let `BatteryCoversEV` take effect in passive-arbitrage PV-surplus charge slots by allowing the planned-grid cap to back off charging and cover EV import while preserving deliberate grid-charge slots.

Resolve failed remote-home P2P attempts to the actionable sign-in/setup gate instead of leaving the page on "Reaching your home..." forever.
