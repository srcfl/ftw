// web/p2p.js — browser side of the home-route P2P transport (Phase 5).
//
// Opens a direct DTLS WebRTC DataChannel to the Pi and exposes p2pFetch(), a
// fetch-like call that frames a tunnel.TunneledRequest, sends it over the
// channel, and resolves the matching ResponseFrame (correlated by req_id).
// Signaling rides the authenticated owner tunnel: POST <base>/api/p2p/offer
// carries the ftw_owner cookie, so only an authenticated owner can open a
// channel. Everything degrades to the normal relay fetch when the channel
// can't open (hard NAT, no STUN reachability, opt-out) — invisible to callers.
//
// Classic script (next-app.js is a plain IIFE, not a module): exposes
// window.p2pFetch + window.ftwP2P. No build step.
(function () {
  "use strict";

  var STUN = [{ urls: "stun:stun.l.google.com:19302" }]; // mirrors p2p.DefaultSTUNServers
  var LABEL = "ftw";                                     // must match the Pi Bridge
  var CONNECT_TIMEOUT_MS = 8000;   // give up on the handshake, fall back to relay
  var REQUEST_TIMEOUT_MS = 10000;  // per-request budget over the channel
  var RETRY_COOLDOWN_MS = 30000;   // after a failed connect, hold off this long

  var pc = null, dc = null;
  var connecting = null;             // in-flight connect() promise
  var ready = false;
  var nextRetryAt = 0;
  var pending = Object.create(null); // req_id -> { resolve, reject, timer }
  var seq = 0;
  var listeners = [];
  var stateName = "off";             // off | connecting | direct | relay
  var unenrolled = false;            // true once a connect() found NO device key
                                     // for this origin (never LAN-enrolled). The
                                     // sign-in gate reads this to prompt setup
                                     // instead of looping on "connecting".

  // ---- base path: relay /me/<site> prefix, or "" for home-host / LAN ----
  function apiBase() {
    var m = location.pathname.match(/^(\/me\/[^/]+)\//);
    return m ? m[1] : "";
  }
  function relayURL(path) { return apiBase() + path; }

  // ---- LAN-origin detection (FIX-2) -----------------------------------------
  // Owner API calls must NEVER fall back to the cleartext relay on the PUBLIC
  // home route — a channel timeout would otherwise leak the WebAuthn assertion +
  // ceremony token to the untrusted relay. We allow the relay fallback ONLY when
  // the page is served DIRECTLY by the Pi (genuine LAN), where "the relay" is not
  // in the path at all. Everything else — the /me/<site> tunnel prefix, or a
  // public home host like home.fortytwowatts.com — is treated as NOT-LAN, so
  // strict owner fetches fail closed instead of leaking. When in doubt: not LAN.
  function isPrivateIPv4(h) {
    var m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
    if (!m) return false;
    var a = +m[1], b = +m[2];
    if (a === 10) return true;                       // 10.0.0.0/8
    if (a === 127) return true;                      // loopback
    if (a === 192 && b === 168) return true;         // 192.168.0.0/16
    if (a === 172 && b >= 16 && b <= 31) return true; // 172.16.0.0/12
    if (a === 169 && b === 254) return true;         // link-local
    return false;
  }
  function isLanOrigin() {
    // A relay tunnel prefix is, by definition, the relay in the path → not LAN.
    if (apiBase() !== "") return false;
    var h = (location.hostname || "").toLowerCase();
    if (h === "localhost") return true;
    if (h === "::1" || h === "[::1]") return true;
    // mDNS / local hostnames the Pi answers on directly.
    if (h.slice(-6) === ".local" || h.indexOf(".") === -1) {
      // A bare single-label host (e.g. "fortytwowatts", "raspberrypi") or *.local
      // is a direct-LAN name — never the public home host (which has dots).
      // Exception: don't treat a future bare public alias as LAN; but a
      // single-label host can't be a public FQDN, so this is safe.
      return true;
    }
    if (isPrivateIPv4(h)) return true;
    // Private/link-local IPv6 (ULA fc00::/7, link-local fe80::/10), possibly
    // bracketed.
    var hv6 = h.replace(/^\[|\]$/g, "");
    if (/^f[cd][0-9a-f]{2}:/.test(hv6)) return true; // fc00::/7 ULA
    if (/^fe[89ab][0-9a-f]:/.test(hv6)) return true; // fe80::/10 link-local
    // Anything with a dotted public hostname (home.fortytwowatts.com, etc.) is
    // NOT LAN → strict owner fetches fail closed rather than leak to the relay.
    return false;
  }

  // ---- signaling rendezvous URL (P2P-only home route) -----------------------
  // The /signal/* mailbox lives at the relay ROOT (it is keyed by site_id, never
  // under the /me/<site> tunnel prefix), so signaling URLs are origin-absolute
  // and never carry the apiBase prefix. site is URL-encoded — it can contain a
  // colon ("site:Home"). The nonce (?n=) keys a per-(site,nonce) mailbox so an
  // attacker's offers can't displace/steal this browser's answer (FIX-4a); it is
  // an OPAQUE routing key, never the SDP.
  function signalURL(site, leaf, nonce) {
    return "/signal/" + encodeURIComponent(site) + "/" + leaf +
      "?n=" + encodeURIComponent(nonce);
  }

  // randomNonce returns a fresh 128-bit hex rendezvous nonce. Each connect()
  // attempt mints its own, so concurrent/retried attempts never collide and an
  // attacker's offers land in a different nonce slot.
  function randomNonce() {
    var b = new Uint8Array(16);
    crypto.getRandomValues(b);
    var s = "";
    for (var i = 0; i < b.length; i++) s += (b[i] + 0x100).toString(16).slice(1);
    return s;
  }

  // challengeURL is the relay's per-site device-proof challenge endpoint (C2). It
  // lives at the relay ROOT alongside /signal/<site>/{offer,answer} and is keyed
  // by site_id only — never under the /me/<site> tunnel prefix.
  function challengeURL(site) {
    return "/signal/" + encodeURIComponent(site) + "/challenge";
  }

  // ---- device-key proof for the relay (C2) ----------------------------------
  // Before an offer ever reaches the Pi, the browser must prove it holds a key
  // the Pi has published to the relay (C1). The relay refuses (403) — and the Pi
  // is NEVER contacted — for any offer without a valid proof. We:
  //   1. GET /signal/<site>/challenge  -> {nonce, exp_ms}
  //   2. sign "ftw-signal:v1:<site>:<nonce>" with the device key
  //   3. include {device_pubkey, nonce, sig} in the offer POST.
  // If this device has no key (never enrolled on the LAN), there is nothing to
  // prove with — we set the "unenrolled" state so the gate can tell the user to
  // set up this device on their home network first, and abort the attempt rather
  // than firing a doomed offer.
  function deviceKeyHandle() {
    // Use the same per-origin device-key store the ceremony pages mint into. It's
    // attached to window by device-key.js (loaded as a classic script before this
    // on the dashboard). hasDeviceKey() must NOT mint — a freshly-minted key the
    // Pi hasn't pinned would look enrolled to the relay but be rejected by the Pi.
    if (!window.ftwDeviceKey || typeof window.ftwDeviceKey.getOrCreate !== "function") {
      var e = new Error("device-key store not loaded yet");
      e.code = "store-pending"; // transient — module script hasn't run yet
      return Promise.reject(e);
    }
    return window.ftwDeviceKey.hasDeviceKey().then(function (has) {
      if (!has) {
        var err = new Error("no device key for this origin — enroll on the LAN first");
        err.code = "no-device-key";
        return Promise.reject(err);
      }
      return window.ftwDeviceKey.getOrCreate();
    });
  }

  // signalProof fetches a fresh challenge nonce and signs it, returning the
  // {device_pubkey, nonce, sig} the offer POST attaches. Fails closed: any error
  // (no key, challenge fetch failure, sign failure) rejects so the offer is never
  // sent — the relay would reject it anyway, and an unenrolled device must not
  // wake the Pi.
  function signalProof(site) {
    return deviceKeyHandle().then(function (key) {
      return fetch(challengeURL(site), { method: "GET" })
        .then(function (r) {
          if (!r.ok) throw new Error("signal challenge http " + r.status);
          return r.json();
        })
        .then(function (ch) {
          if (!ch || !ch.nonce) throw new Error("signal challenge missing nonce");
          var msg = "ftw-signal:v1:" + site + ":" + ch.nonce;
          return key.sign(msg).then(function (sig) {
            return { device_pubkey: key.pubHex, nonce: ch.nonce, sig: sig };
          });
        });
    });
  }

  // ---- state broadcast (drives the dashboard transport indicator) ----
  function setState(s) {
    if (s === stateName) return;
    stateName = s;
    for (var i = 0; i < listeners.length; i++) {
      try { listeners[i](s); } catch (e) {}
    }
  }

  // ---- standard base64 (matches Go base64.StdEncoding) ----
  function b64encode(bytes) {
    var bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin);
  }
  function b64decode(s) {
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // ---- DTLS-fingerprint verification (anti relay-MITM) ----------------------
  // The Pi signs every answer's DTLS fingerprint with its ES256 identity key.
  // The browser pins that key (TOFU) on first connect and verifies the signature
  // BEFORE trusting the channel — so a relay that swaps the relayed SDP /
  // fingerprint can't MITM: it can't forge the signature without the Pi's key.

  // SubjectPublicKeyInfo prefix for an uncompressed P-256 EC public key. Raw
  // X||Y is wrapped into SPKI because WebCrypto's "raw" EC import is uneven
  // across browsers (Safari/WebKit historically rejected it); "spki" works.
  var SPKI_P256_PREFIX = new Uint8Array([
    0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01,
    0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00
  ]);

  function hexToBytes(h) {
    var a = new Uint8Array(h.length >> 1);
    for (var i = 0; i < a.length; i++) a[i] = parseInt(h.substr(i * 2, 2), 16);
    return a;
  }
  // normalizeFp mirrors tunnel.NormalizeDtlsFingerprint: keep hex, drop colons,
  // lowercase — so both ends sign/verify the identical string.
  function normalizeFp(s) { return (s.match(/[0-9a-fA-F]/g) || []).join("").toLowerCase(); }
  function fingerprintFromSDP(sdp) {
    var m = /a=fingerprint:sha-256[ \t]+([0-9A-Fa-f:]+)/.exec(sdp || "");
    return m ? m[1] : null;
  }
  function importP256Pub(xyHex) {
    var xy = hexToBytes(xyHex);
    if (xy.length !== 64) return Promise.reject(new Error("bad pubkey length"));
    var spki = new Uint8Array(SPKI_P256_PREFIX.length + 65);
    spki.set(SPKI_P256_PREFIX, 0);
    spki[SPKI_P256_PREFIX.length] = 0x04;
    spki.set(xy, SPKI_P256_PREFIX.length + 1);
    return crypto.subtle.importKey("spki", spki.buffer,
      { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
  }

  var _pinPromise = null;

  // directoryEntry returns the chosen instance from the decrypted directory
  // (window.ftwInstanceSync, attached by instance-sync.js). v1: the directory is
  // a 1-entry list, so we take the first. Returns null when no directory exists
  // yet (anonymous, pre-decrypt, or a not-yet-migrated single-home user). The
  // directory's pi_pubkey is Pi-signed (instance-sync verifyEntry), so trusting
  // it needs NO relay /api/identity round-trip.
  function directoryEntry() {
    try {
      var sync = window.ftwInstanceSync;
      if (!sync || typeof sync.getCachedInstances !== "function") return null;
      var list = sync.getCachedInstances() || [];
      var e = list[0];
      if (e && e.site_id && e.pi_pubkey) return { pub: e.pi_pubkey, site: e.site_id };
    } catch (_) {}
    return null;
  }

  // legacyRecord reads the pre-multi-tenant single pin written at
  // "ftw.identity:<apiBase>" ({pub, site}). Used once to seed the first per-site
  // record so an existing single-home user doesn't re-TOFU.
  function legacyRecord() {
    try {
      var raw = localStorage.getItem("ftw.identity:" + apiBase());
      var rec = raw ? JSON.parse(raw) : null;
      if (rec && rec.pub && rec.site) return rec;
    } catch (_) {}
    return null;
  }

  // pinKey is the per-(origin, site_id) localStorage key. The site_id is part of
  // the key so two tenants reached through the same origin pin independently and
  // can never clobber each other's Pi identity.
  function pinKey(site) { return "ftw.identity:" + apiBase() + ":" + site; }

  // persistPin enforces FIRST-KEY-WINS per (origin, site_id): the first Pi key
  // pinned for a site is authoritative; a later DIFFERENT key is a hard error (an
  // identity change / possible relay MITM), NEVER a silent overwrite — even when
  // it arrives in a validly Pi-signed directory entry (a signature proves the key
  // signed that descriptor, not that it may replace the known-good pin). Returns
  // the record to use (the existing pin if present, else the freshly written one).
  function persistPin(site, pub) {
    var existing = null;
    try { existing = JSON.parse(localStorage.getItem(pinKey(site)) || "null"); } catch (e) {}
    if (!existing || !existing.pub) {
      // No per-site pin yet — but a LEGACY single-home pin (ftw.identity:<apiBase>,
      // no site suffix) for the SAME site is equally authoritative. First-key-wins
      // must span the migration, or a directory entry could silently replace the
      // identity an existing user already trusts before it was migrated.
      var legacy = legacyRecord();
      if (legacy && legacy.site === site && legacy.pub) existing = legacy;
    }
    if (existing && existing.pub) {
      if (existing.pub !== pub) {
        throw new Error("home identity for " + site + " changed — refusing to connect (re-pair on your home network if this was intentional)");
      }
      // Persist/refresh the per-site record (also migrates a legacy pin forward).
      var kept = { pub: existing.pub, site: site };
      try { localStorage.setItem(pinKey(site), JSON.stringify(kept)); } catch (e) {}
      return kept;
    }
    var rec = { pub: pub, site: site };
    try { localStorage.setItem(pinKey(site), JSON.stringify(rec)); } catch (e) {}
    return rec;
  }

  // pinnedIdentity resolves {key, site} for the chosen instance, pinning per
  // (origin, site_id). Source priority — NO relay round-trip until the last:
  //   1. the decrypted directory entry (Pi pubkey is Pi-signed there);
  //   2. the legacy single-pin record (migrate it into a per-site record);
  //   3. relay /api/identity TOFU (only when nothing is cached at all).
  // Every later answer is verified against the pinned key, so the relay can't
  // MITM after that. The pubkey is imported once and the promise cached.
  function pinnedIdentity() {
    if (_pinPromise) return _pinPromise;
    var p;
    try {
      p = (function () {
      // 1. The decrypted directory entry (its Pi pubkey is Pi-signed) — first-key-wins.
      var dir = directoryEntry();
      if (dir) {
        var rec = persistPin(dir.site, dir.pub);
        return importP256Pub(rec.pub).then(function (key) { return { key: key, site: rec.site }; });
      }
      // 2. The legacy single-home pin, migrated into a per-site record (first-key-wins).
      var legacy = legacyRecord();
      if (legacy) {
        var rec2 = persistPin(legacy.site, legacy.pub);
        return importP256Pub(rec2.pub).then(function (key) { return { key: key, site: rec2.site }; });
      }
      // 3. No directory and no legacy pin. On a GENUINE LAN origin the Pi serves
      // /api/identity ITSELF (no relay in the path), so a one-time TOFU there is the
      // SSH-known-hosts model and safe. On the PUBLIC relay route, trusting
      // /api/identity would mean trusting a RELAY-supplied identity — a MITM vector
      // — so we FAIL CLOSED: the user must sign in, which loads the Pi-signed
      // directory, before any channel is verified. (Codex 2026-06-05, BLOCKER.)
      if (!isLanOrigin()) {
        return Promise.reject(new Error("no pinned home identity yet — sign in with your passkey first (the relay's /api/identity is never trusted on the public route)"));
      }
      return fetch(relayURL("/api/identity"), { credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error("/api/identity " + r.status); return r.json(); })
        .then(function (id) {
          if (!id.public_key_hex || !id.site_id) throw new Error("identity response missing fields");
          var rec3 = persistPin(id.site_id, id.public_key_hex);
          return importP256Pub(rec3.pub).then(function (key) { return { key: key, site: rec3.site }; });
        });
      })();
    } catch (e) {
      // A synchronous throw (e.g. a first-key-wins identity change) becomes a
      // rejection so callers fail closed via .catch rather than crashing.
      p = Promise.reject(e);
    }
    // Cache only on success — a transient fetch failure must not poison later
    // verifications (the pinned record persists in localStorage regardless).
    p.catch(function () { if (_pinPromise === p) _pinPromise = null; });
    _pinPromise = p;
    return p;
  }

  // verifyAnswerSignature rejects unless the answer's DTLS fingerprint is signed
  // by the pinned Pi key. Mandatory / fail-closed: on any error the caller falls
  // back to the relay rather than trusting an unverified channel.
  function verifyAnswerSignature(ans) {
    if (!ans || !ans.fp_sig) return Promise.reject(new Error("answer not signed (no fp_sig)"));
    var fp = fingerprintFromSDP(ans.sdp);
    if (!fp) return Promise.reject(new Error("answer has no DTLS fingerprint"));
    var ts = Number(ans.ts) || 0;
    if (Math.abs(Date.now() - ts) > 5 * 60 * 1000) {
      return Promise.reject(new Error("answer timestamp outside skew window"));
    }
    return pinnedIdentity().then(function (pin) {
      var msg = "ftw-dtls-fp:v1:" + pin.site + ":" + normalizeFp(fp) + ":" + ts;
      var sig = hexToBytes(ans.fp_sig);
      if (sig.length !== 64) throw new Error("bad signature length");
      return crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, pin.key, sig,
        new TextEncoder().encode(msg)
      ).then(function (ok) {
        if (!ok) throw new Error("answer fingerprint signature INVALID — aborting (possible relay MITM)");
      });
    });
  }

  function supported() { return typeof window.RTCPeerConnection === "function"; }

  // isDirectLAN reports whether the page's HOST is a direct connection to the
  // Pi (no relay) — loopback, an RFC1918 / CGNAT / link-local IPv4, an IPv6
  // loopback / ULA / link-local / IPv4-mapped-private literal, a single-label
  // hostname, or a *.local mDNS name. On such a host a P2P DataChannel buys
  // nothing and the STUN handshake needs WAN a fresh Pi may not have.
  //
  // Only consulted for the BARE-PATH case (no /me/<site> prefix — see
  // isRelayContext). A public FQDN served bare is the bare-host relay
  // (e.g. home.fortytwowatts.com) and falls through to `false` → relay.
  function isDirectLAN() {
    var h = (location.hostname || "").toLowerCase();
    if (!h) return false;
    if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);             // strip FQDN root dot
    if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1); // unwrap IPv6

    // IPv6 literal (has a colon). Handle BEFORE the single-label rule so a
    // global IPv6 (which also has no dot) isn't mistaken for a LAN short-name.
    if (h.indexOf(":") !== -1) {
      if (h === "::1") return true;                    // loopback
      if (/^f[cd]/.test(h)) return true;               // ULA fc00::/7
      if (/^fe[89ab]/.test(h)) return true;            // link-local fe80::/10
      var v4 = h.match(/(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/); // ::ffff:a.b.c.d
      if (v4) { h = v4[1]; }                           // reuse the IPv4 tests below
      else return false;                               // global/other IPv6 → relay
    }

    if (h === "localhost" || h === "127.0.0.1") return true;
    if (/^10\./.test(h)) return true;                  // RFC1918 10/8
    if (/^192\.168\./.test(h)) return true;            // RFC1918 192.168/16
    if (/^172\.(1[6-9]|2[0-9]|3[01])\./.test(h)) return true; // RFC1918 172.16/12
    if (/^169\.254\./.test(h)) return true;            // link-local
    if (/^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\./.test(h)) return true; // CGNAT 100.64/10
    if (/\.local$/.test(h)) return true;               // mDNS
    if (h.indexOf(".") === -1) return true;            // single-label host (e.g. "fortytwowatts")
    return false;                                      // public FQDN → relay context (incl. home.*)
  }

  // isRelayContext reports whether the page is reached THROUGH the relay (where
  // a direct P2P channel is worth attempting). A present /me/<site> tunnel
  // prefix is definitively relay — regardless of the host it's served on, so a
  // relay reached by a private-DNS alias or single-label name still keeps P2P.
  // Only when there's no prefix do we fall back to the host heuristic.
  function isRelayContext() {
    if (apiBase() !== "") return true;     // /me/<site> tunnel → relay
    return !isDirectLAN();                 // bare path: public FQDN → relay, LAN host → direct
  }

  function enabled() {
    // Opt-in defaults ON (the feature's whole point); set localStorage
    // "ftw.p2p" = "off" to force the relay path. Skip non-relay (direct-LAN)
    // contexts entirely — there's no relay to bypass and the handshake would
    // only waste a WAN round-trip.
    return supported() && isRelayContext() && localStorage.getItem("ftw.p2p") !== "off";
  }

  function teardown() {
    ready = false;
    if (dc) { try { dc.close(); } catch (e) {} dc = null; }
    if (pc) { try { pc.close(); } catch (e) {} pc = null; }
  }

  // waitIceComplete resolves once ICE gathering finishes (non-trickle), with a
  // hard cap because some browsers never emit "complete" with only STUN.
  function waitIceComplete(peer) {
    if (peer.iceGatheringState === "complete") return Promise.resolve();
    return new Promise(function (resolve) {
      var done = false;
      function finish() { if (!done) { done = true; resolve(); } }
      peer.addEventListener("icegatheringstatechange", function check() {
        if (peer.iceGatheringState === "complete") {
          peer.removeEventListener("icegatheringstatechange", check);
          finish();
        }
      });
      setTimeout(finish, 3000);
    });
  }

  // pollAnswer long-polls /signal/<site>/answer until the Pi parks its answer
  // blob ({sdp, fp_sig, ts}) or the connect attempt is abandoned. The relay
  // returns 204 when no answer is parked yet; we re-poll until CONNECT_TIMEOUT_MS
  // (the outer connect() timer also caps the whole attempt). isSettled() lets the
  // outer finish() short-circuit a long-poll in flight.
  function pollAnswer(site, nonce) {
    var deadline = Date.now() + CONNECT_TIMEOUT_MS;
    function once() {
      if (Date.now() > deadline) return Promise.reject(new Error("answer poll timeout"));
      return fetch(signalURL(site, "answer", nonce), { method: "GET" }).then(function (r) {
        if (r.status === 204) {
          // No answer yet — re-poll immediately (the relay holds the request
          // open server-side, so this is not a busy loop).
          return once();
        }
        if (!r.ok) throw new Error("answer http " + r.status);
        return r.json();
      });
    }
    return once();
  }

  function connect() {
    if (ready) return Promise.resolve(true);
    if (connecting) return connecting;
    if (Date.now() < nextRetryAt) return Promise.resolve(false);
    setState("connecting");

    connecting = new Promise(function (resolve) {
      var settled = false;
      var to;
      // finish(ok[, soft]): soft=true means "transient, retry soon" — used when the
      // device-key store simply hasn't loaded yet at warm-up (a module-script load
      // race), so we DON'T arm the long cooldown that would otherwise leave the page
      // on the relay for 30 s for no reason.
      function finish(ok, soft) {
        if (settled) return;
        settled = true;
        connecting = null;
        clearTimeout(to);
        if (!ok) {
          if (!soft) nextRetryAt = Date.now() + RETRY_COOLDOWN_MS;
          teardown();
          setState("relay");
        }
        resolve(ok);
      }

      try {
        pc = new RTCPeerConnection({ iceServers: STUN });
      } catch (e) { return finish(false); }

      dc = pc.createDataChannel(LABEL, { ordered: true });
      dc.onopen = function () { ready = true; setState("direct"); finish(true); };
      dc.onclose = function () { var was = stateName; teardown(); if (was === "direct") setState("relay"); };
      dc.onmessage = function (ev) {
        var frame;
        try { frame = JSON.parse(ev.data); } catch (e) { return; }
        var p = pending[frame.req_id];
        if (!p) return;
        delete pending[frame.req_id];
        clearTimeout(p.timer);
        p.resolve(frame.response || {});
      };
      pc.onconnectionstatechange = function () {
        var st = pc && pc.connectionState;
        if (st === "failed" || st === "closed" || st === "disconnected") finish(false);
      };

      to = setTimeout(function () { finish(false); }, CONNECT_TIMEOUT_MS);

      // Signaling now rides the BLIND rendezvous, not the owner tunnel: POST the
      // offer to /signal/<site>/offer, then long-poll /signal/<site>/answer. The
      // relay forwards opaque SDP/signature blobs and never sees plaintext. We
      // need the pinned site_id first (it keys the mailbox); pinnedIdentity also
      // gives us the key we verify the answer signature against.
      // One opaque rendezvous nonce per attempt — the offer is parked under it
      // and we poll only its answer, so a hostile offer can't steal ours.
      var nonce = randomNonce();
      pinnedIdentity()
        .then(function (pin) {
          var site = pin.site;
          // C2: prove this device to the RELAY before the offer can reach the Pi.
          // signalProof fetches a fresh challenge nonce and signs
          // "ftw-signal:v1:<site>:<nonce>" with the device key. Done in parallel
          // with the SDP/ICE work so it adds no latency on the happy path.
          var proofP = signalProof(site);
          return pc.createOffer()
            .then(function (offer) { return pc.setLocalDescription(offer); })
            .then(function () { return waitIceComplete(pc); })
            .then(function () { return proofP; })
            .then(function (proof) {
              // The offer body is now a JSON envelope carrying the raw SDP PLUS the
              // device-key proof (C2). The relay verifies {device_pubkey, nonce,
              // sig} against the site's published key set + consumes the nonce, then
              // parks the SDP for the Pi exactly as before. Field names are fixed by
              // the contract — do not rename.
              return fetch(signalURL(site, "offer", nonce), {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                  sdp: pc.localDescription.sdp,
                  device_pubkey: proof.device_pubkey,
                  nonce: proof.nonce,
                  sig: proof.sig
                })
              });
            })
            .then(function (r) {
              // 204 = parked OK; anything else is a hard signaling error. A 403
              // means the relay rejected the device proof (unknown/again-used key);
              // the Pi was NEVER contacted — fall back to the relay transport.
              if (r.status !== 204 && !r.ok) throw new Error("offer http " + r.status);
              return pollAnswer(site, nonce);
            })
            .then(function (ans) {
              // Verify the Pi signed this answer's DTLS fingerprint, against the
              // key pinned at first connect, BEFORE trusting the channel — the
              // anti-relay-MITM check. Fail-closed: any failure aborts P2P and
              // the caller transparently falls back to the relay path.
              return verifyAnswerSignature(ans).then(function () {
                return pc.setRemoteDescription({ type: "answer", sdp: ans.sdp });
              });
            });
        })
        .catch(function (e) {
          if (e && e.message) { try { console.warn("p2p: " + e.message); } catch (_) {} }
          // No device key on this origin (never enrolled on the LAN): there is
          // nothing to prove to the relay, so a direct channel can never open from
          // here. Remember it so the sign-in gate shows "set up this device on your
          // home network first" instead of a misleading endless "connecting".
          if (e && e.code === "no-device-key") { unenrolled = true; }
          // Soft-fail (store-pending): the device-key module hasn't run yet — retry
          // soon instead of arming the long cooldown.
          finish(false, e && e.code === "store-pending");
        });
    });
    return connecting;
  }

  function channelFetch(path, opts) {
    return new Promise(function (resolve, reject) {
      var id = "c" + (++seq) + "-" + Date.now();
      var frame = { req_id: id, method: (opts.method || "GET").toUpperCase(), path: path };
      if (opts.headers) frame.headers = toHeaderMap(opts.headers);
      if (opts.body != null) {
        var bytes = typeof opts.body === "string"
          ? new TextEncoder().encode(opts.body)
          : new Uint8Array(opts.body);
        frame.body_b64 = b64encode(bytes);
      }
      var timer = setTimeout(function () {
        delete pending[id];
        reject(new Error("p2p request timeout"));
      }, REQUEST_TIMEOUT_MS);
      pending[id] = {
        timer: timer,
        resolve: function (resp) { resolve(makeResponse(path, resp)); },
        reject: reject
      };
      try { dc.send(JSON.stringify(frame)); }
      catch (e) { clearTimeout(timer); delete pending[id]; reject(e); }
    });
  }

  // makeResponse adapts a ResponseFrame.response into a fetch-Response subset
  // (ok / status / headers / json() / text()) — enough for the dashboard.
  function makeResponse(path, resp) {
    var status = resp.status || 0;
    var bytes = resp.body_b64 ? b64decode(resp.body_b64) : new Uint8Array(0);
    var headers = new Headers();
    if (resp.headers) {
      for (var k in resp.headers) {
        if (!Object.prototype.hasOwnProperty.call(resp.headers, k)) continue;
        // FIX-5 defence in depth: never expose Set-Cookie over the channel. The
        // Pi's Bridge already strips it (the owner session lives only inside
        // DTLS, replayed server-side), but filter here too so an injected script
        // can never read an owner cookie even if a future Pi forgot to strip it.
        if (k.toLowerCase() === "set-cookie") continue;
        var vs = resp.headers[k] || [];
        for (var i = 0; i < vs.length; i++) headers.append(k, vs[i]);
      }
    }
    var text = new TextDecoder().decode(bytes);
    return {
      ok: status >= 200 && status < 300,
      status: status,
      url: path,
      headers: headers,
      json: function () { return Promise.resolve(JSON.parse(text)); },
      text: function () { return Promise.resolve(text); }
    };
  }

  function toHeaderMap(h) {
    var out = {};
    if (typeof Headers !== "undefined" && h instanceof Headers) {
      h.forEach(function (v, k) { out[k] = [v]; });
    } else {
      for (var k in h) {
        if (Object.prototype.hasOwnProperty.call(h, k)) out[k] = [String(h[k])];
      }
    }
    return out;
  }

  // failClosedResponse synthesises a 503 Response-like object for STRICT owner
  // fetches when no channel is available. It looks enough like a fetch Response
  // (ok/status/headers/json/text) that callers handle it uniformly — but the
  // owner body NEVER leaves the browser, so the WebAuthn assertion + ceremony
  // token can't leak to the relay (FIX-2).
  function failClosedResponse(path) {
    var msg = "P2P channel unavailable — reconnecting. Retry in a moment.";
    return {
      ok: false,
      status: 503,
      url: path,
      headers: new Headers(),
      json: function () { return Promise.resolve({ error: msg, retry: true }); },
      text: function () { return Promise.resolve(msg); }
    };
  }

  // p2pFetch: fetch-compatible. Routes over the DataChannel when available,
  // else the normal relay fetch. Never rejects on transport failure — it always
  // falls back — so callers treat it as a drop-in fetch.
  //
  // STRICT mode (opts.strict === true, FIX-2): for owner /api/* calls that carry
  // secrets (WebAuthn assertion, ceremony token, owner session). On the PUBLIC
  // home route (relay in the path) it NEVER falls back to the relay — on
  // no-channel/failure it fails closed with a synthetic 503, so the owner body
  // can't leak to the untrusted relay. The relay fallback is kept ONLY for a
  // genuine-LAN origin (isLanOrigin: the Pi serves the page directly, relay not
  // in the path). When in doubt → not LAN → fail closed.
  function p2pFetch(path, opts) {
    opts = opts || {};
    var strict = opts.strict === true;
    var canFallBack = !strict || isLanOrigin();
    var relayFallback = function () {
      var o = {};
      for (var k in opts) if (Object.prototype.hasOwnProperty.call(opts, k)) o[k] = opts[k];
      delete o.strict; // not a fetch() init field
      if (!o.credentials) o.credentials = "same-origin";
      return fetch(relayURL(path), o);
    };
    var fallback = function () {
      if (canFallBack) return relayFallback();
      // STRICT + not-LAN: fail closed. Do NOT send the owner body to the relay.
      // Nudge a reconnect for the next attempt, but only if P2P is enabled (don't
      // resurrect a user-disabled channel).
      if (enabled()) { try { connect(); } catch (e) {} }
      return Promise.resolve(failClosedResponse(path));
    };
    if (!enabled()) return fallback();
    // Never block a request on the handshake. If the channel is already open,
    // use it; otherwise kick a background connect (deduped + backoff-guarded)
    // and serve THIS request over the relay fetch immediately. Once the
    // channel opens, subsequent polls go direct. This is what keeps the very
    // first /api/status poll from stalling on CONNECT_TIMEOUT_MS — on the
    // relay path too, not just LAN.
    if (ready && dc && dc.readyState === "open") {
      return channelFetch(path, opts).catch(fallback);
    }
    connect(); // fire-and-forget; ignore the promise so we don't await it
    return fallback();
  }

  // fetchStrict is the owner-API entry point: same as p2pFetch but forces strict
  // mode, so a channel timeout fails closed instead of leaking the owner body to
  // the relay on the public home route.
  function p2pFetchStrict(path, opts) {
    opts = opts || {};
    var o = {};
    for (var k in opts) if (Object.prototype.hasOwnProperty.call(opts, k)) o[k] = opts[k];
    o.strict = true;
    return p2pFetch(path, o);
  }

  window.ftwP2P = {
    fetch: p2pFetch,
    fetchStrict: p2pFetchStrict,
    isLanOrigin: isLanOrigin,
    connect: connect,
    onState: function (fn) { listeners.push(fn); try { fn(stateName); } catch (e) {} },
    state: function () { return stateName; },
    // isUnenrolled reports whether a connect() attempt found NO device key for this
    // origin (this device was never set up on the LAN), so the relay can't be
    // proven to and a direct channel can't open. The gate uses it to show the
    // "set up this device on your home network first" path (C2) rather than a
    // perpetual "connecting".
    isUnenrolled: function () { return unenrolled; },
    // site() resolves the pinned site_id (directory entry, then the migrated
    // legacy record, then last-resort TOFU /api/identity — see pinnedIdentity).
    // next-app.js needs it to build the C3 device-PoP signing string
    // "ftw-device-pop:v1:<site>:<challenge>" — the SAME site p2p.js signs the
    // signal proof over, so both ends agree.
    site: function () { return pinnedIdentity().then(function (pin) { return pin.site; }); },
    setEnabled: function (on) {
      localStorage.setItem("ftw.p2p", on ? "on" : "off");
      if (!on) { teardown(); setState("relay"); }
      else { nextRetryAt = 0; connect(); } // explicit enable bypasses the backoff
    }
  };
  window.p2pFetch = p2pFetch;
  window.p2pFetchStrict = p2pFetchStrict;

  // ownerFetch is the ONE fail-closed entry point the dashboard's classic scripts
  // + web components use for state-changing owner/CONTROL /api/* calls. It is the
  // SAME strict transport owner-fetch.js uses for the ceremony pages: strict P2P,
  // fail-closed 503 on a public / /me/<site> origin with no DataChannel, raw relay
  // fetch ONLY on a genuine LAN origin. One behaviour, shared — do not fork it.
  // index.html loads /p2p.js before the consuming scripts, so this is defined by
  // the time any dashboard handler runs.
  window.ownerFetch = p2pFetchStrict;

  // Warm up the channel on load so the dashboard's first poll can already be
  // direct (and the indicator settles quickly). When P2P is supported but
  // opted out on a relay context, still surface a "relay" state so the
  // indicator stays clickable to re-enable.
  //
  // On a direct-LAN connection (see isRelayContext) P2P is not applicable —
  // there's no relay to bypass — so we leave the state "off" (indicator hidden)
  // rather than showing a misleading, un-toggleable "Relay" badge. The bare-host
  // relay (home.*) and the /me/<site> tunnel are both relay contexts, so they
  // warm up and connect.
  if (enabled()) { try { connect(); } catch (e) {} }
  else if (supported() && isRelayContext()) { setState("relay"); }
})();
