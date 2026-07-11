// owner-fetch.js (components) — thin ES-module accessor for the dashboard's
// single owner/CONTROL fetch entry point.
//
// State-changing owner/CONTROL /api/* calls from web components (manual-hold
// install/clear, pair start/abort, notification test, self-update trigger) must
// ride the STRICT P2P transport so their body never traverses the untrusted relay
// on the public home route. The canonical strict, fail-closed transport lives in
// p2p.js as window.ownerFetch (=== p2pFetchStrict) — the SAME function the
// owner-access ceremony pages use via web/owner-access/owner-fetch.js. We do NOT
// duplicate that logic here; we only delegate to it.
//
// index.html loads /p2p.js (a classic script) before the /components/index.js
// module, so window.ownerFetch is defined by the time any component handler runs.
// The plain-fetch fallback covers only contexts where p2p.js never loaded (a
// genuine-LAN dev page, or a unit test that imports a component in isolation).
export function ownerFetch(path, opts) {
  if (typeof window !== "undefined" && typeof window.ownerFetch === "function") {
    return window.ownerFetch(path, opts);
  }
  return fetch(path, opts);
}
