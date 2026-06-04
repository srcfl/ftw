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

  // ---- base path: relay /me/<site> prefix, or "" for home-host / LAN ----
  function apiBase() {
    var m = location.pathname.match(/^(\/me\/[^/]+)\//);
    return m ? m[1] : "";
  }
  function relayURL(path) { return apiBase() + path; }

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
  // pinnedIdentity TOFU-fetches + pins {pubkey, site_id} from /api/identity on
  // first connect (keyed per home origin / me-prefix), then reuses it. The first
  // fetch trusts the path once (SSH known-hosts model); every later answer is
  // verified against the pinned key, so the relay can't MITM after that.
  function pinnedIdentity() {
    if (_pinPromise) return _pinPromise;
    var p = (function () {
      var storeKey = "ftw.identity:" + apiBase();
      var stored = null;
      try { stored = JSON.parse(localStorage.getItem(storeKey) || "null"); } catch (e) {}
      var got = stored
        ? Promise.resolve(stored)
        : fetch(relayURL("/api/identity"), { credentials: "same-origin" })
            .then(function (r) { if (!r.ok) throw new Error("/api/identity " + r.status); return r.json(); })
            .then(function (id) {
              if (!id.public_key_hex || !id.site_id) throw new Error("identity response missing fields");
              var rec = { pub: id.public_key_hex, site: id.site_id };
              try { localStorage.setItem(storeKey, JSON.stringify(rec)); } catch (e) {}
              return rec;
            });
      return got.then(function (rec) {
        return importP256Pub(rec.pub).then(function (key) { return { key: key, site: rec.site }; });
      });
    })();
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
  function enabled() {
    // Opt-in defaults ON (the feature's whole point); set localStorage
    // "ftw.p2p" = "off" to force the relay path.
    return supported() && localStorage.getItem("ftw.p2p") !== "off";
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
      function finish(ok) {
        if (settled) return;
        settled = true;
        connecting = null;
        clearTimeout(to);
        if (!ok) {
          nextRetryAt = Date.now() + RETRY_COOLDOWN_MS;
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
          return pc.createOffer()
            .then(function (offer) { return pc.setLocalDescription(offer); })
            .then(function () { return waitIceComplete(pc); })
            .then(function () {
              return fetch(signalURL(site, "offer", nonce), {
                method: "POST",
                // The body is the raw SDP offer (the relay parks it verbatim and
                // the Pi reads it raw); it is not a JSON envelope.
                headers: { "Content-Type": "application/sdp" },
                body: pc.localDescription.sdp
              });
            })
            .then(function (r) {
              // 204 = parked OK; anything else is a hard signaling error.
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
          finish(false);
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

  // p2pFetch: fetch-compatible. Routes over the DataChannel when available,
  // else the normal relay fetch. Never rejects on transport failure — it always
  // falls back — so callers treat it as a drop-in fetch.
  function p2pFetch(path, opts) {
    opts = opts || {};
    var fallback = function () {
      var o = {};
      for (var k in opts) if (Object.prototype.hasOwnProperty.call(opts, k)) o[k] = opts[k];
      if (!o.credentials) o.credentials = "same-origin";
      return fetch(relayURL(path), o);
    };
    if (!enabled()) return fallback();
    return connect().then(function (ok) {
      if (!ok || !ready || !dc || dc.readyState !== "open") return fallback();
      return channelFetch(path, opts).catch(fallback);
    }, fallback);
  }

  window.ftwP2P = {
    fetch: p2pFetch,
    connect: connect,
    onState: function (fn) { listeners.push(fn); try { fn(stateName); } catch (e) {} },
    state: function () { return stateName; },
    setEnabled: function (on) {
      localStorage.setItem("ftw.p2p", on ? "on" : "off");
      if (!on) { teardown(); setState("relay"); }
      else { nextRetryAt = 0; connect(); } // explicit enable bypasses the backoff
    }
  };
  window.p2pFetch = p2pFetch;

  // Warm up the channel on load so the dashboard's first poll can already be
  // direct (and the indicator settles quickly). When P2P is supported but
  // opted out, still surface a "relay" state so the indicator stays clickable
  // to re-enable; only an unsupported browser leaves the state "off" (hidden).
  if (enabled()) { try { connect(); } catch (e) {} }
  else if (supported()) { setState("relay"); }
})();
