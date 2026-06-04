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

      pc.createOffer()
        .then(function (offer) { return pc.setLocalDescription(offer); })
        .then(function () { return waitIceComplete(pc); })
        .then(function () {
          return fetch(relayURL("/api/p2p/offer"), {
            method: "POST",
            credentials: "same-origin",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ type: "offer", sdp: pc.localDescription.sdp })
          });
        })
        .then(function (r) { if (!r.ok) throw new Error("offer http " + r.status); return r.json(); })
        .then(function (ans) { return pc.setRemoteDescription({ type: "answer", sdp: ans.sdp }); })
        .catch(function () { finish(false); });
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
