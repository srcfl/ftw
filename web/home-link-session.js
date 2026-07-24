(function (root) {
  "use strict";

  var textEncoder = new TextEncoder();
  var textDecoder = new TextDecoder("utf-8", { fatal: true });
  var SESSION_DOMAIN = "ftw-home-link-session-accept/v1";
  var KEY_DOMAIN = "ftw-home-link-session-keys/v1";
  var AD_DOMAIN = "ftw-home-link-sealed-ad/v1";
  var HOME_LINK_RP_ID = "home.sourceful.energy";
  var MAX_ALLOW_CREDENTIALS = 32;
  var MAX_CREDENTIAL_ID_BYTES = 1024;
  var MAX_INBOUND_FRAME_BYTES = 256 * 1024;
  var MAX_PENDING_MESSAGES = 4;
  var DEFAULT_WAIT_TIMEOUT_MS = 15000;

  function bytesToRawURL(value) {
    var bytes = new Uint8Array(value);
    var binary = "";
    for (var i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function rawURLToBytes(value) {
    if (typeof value !== "string" || value.indexOf("=") !== -1) throw new Error("invalid encoding");
    var text = value.replace(/-/g, "+").replace(/_/g, "/");
    while (text.length % 4) text += "=";
    var binary = atob(text);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    if (bytesToRawURL(bytes) !== value) throw new Error("non-canonical encoding");
    return bytes;
  }

  function randomRawURL(length) {
    var bytes = new Uint8Array(length);
    crypto.getRandomValues(bytes);
    return bytesToRawURL(bytes);
  }

  function point(raw) {
    var input = rawURLToBytes(raw);
    if (input.length !== 64) throw new Error("invalid P-256 key");
    var result = new Uint8Array(65);
    result[0] = 4;
    result.set(input, 1);
    return result;
  }

  function transcript(accept) {
    return textEncoder.encode([
      SESSION_DOMAIN,
      accept.connection_id,
      accept.gateway_id,
      String(accept.route_generation),
      accept.route_handle,
      accept.stream_id,
      accept.session_id,
      accept.browser_key,
      accept.gateway_ephemeral_key,
      accept.gateway_public_key,
      accept.browser_nonce,
      accept.gateway_nonce,
      String(accept.expires_at_ms),
    ].join("\n"));
  }

  function nonce(sequence) {
    var bytes = new Uint8Array(12);
    new DataView(bytes.buffer).setBigUint64(4, BigInt(sequence), false);
    return bytes;
  }

  function additionalData(session, direction, sequence) {
    return textEncoder.encode([
      AD_DOMAIN,
      session.route,
      session.streamID,
      session.sessionID,
      direction,
      String(sequence),
    ].join("\n"));
  }

  function exactKeys(value, keys) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return false;
    var actual = Object.keys(value).sort();
    var expected = keys.slice().sort();
    return actual.length === expected.length &&
      actual.every(function (key, index) { return key === expected[index]; });
  }

  function parseInvite(locationValue) {
    var url = new URL(locationValue);
    var gateway = url.searchParams.get("gateway") || "";
    var route = url.searchParams.get("route") || "";
    var key = url.searchParams.get("key") || "";
    if (!/^[0-9a-f]{18}$/.test(gateway)) throw new Error("invalid gateway");
    if (rawURLToBytes(route).length !== 18) throw new Error("invalid route");
    if (rawURLToBytes(key).length !== 64) throw new Error("invalid gateway key");
    return { gateway: gateway, route: route, key: key };
  }

  function assertionJSON(credential) {
    var response = {
      clientDataJSON: bytesToRawURL(credential.response.clientDataJSON),
      authenticatorData: bytesToRawURL(credential.response.authenticatorData),
      signature: bytesToRawURL(credential.response.signature),
    };
    if (credential.response.userHandle) {
      response.userHandle = bytesToRawURL(credential.response.userHandle);
    }
    return {
      id: credential.id,
      rawId: bytesToRawURL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment || null,
      response: response,
      clientExtensionResults: credential.getClientExtensionResults(),
    };
  }

  function registrationJSON(credential) {
    return {
      id: credential.id,
      rawId: bytesToRawURL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment || null,
      response: {
        clientDataJSON: bytesToRawURL(credential.response.clientDataJSON),
        attestationObject: bytesToRawURL(credential.response.attestationObject),
        transports: credential.response.getTransports
          ? credential.response.getTransports()
          : [],
      },
      clientExtensionResults: credential.getClientExtensionResults(),
    };
  }

  function parsePairing(locationValue) {
    var url = new URL(locationValue);
    if (!url.hash || url.hash === "#") return null;
    var values = new URLSearchParams(url.hash.slice(1));
    var id = values.get("pairing_id") || "";
    var secret = values.get("pairing_secret") || "";
    var label = values.get("label") || "";
    if (rawURLToBytes(id).length !== 24 || rawURLToBytes(secret).length !== 32 ||
        label.trim() !== label || label.length < 1 ||
        textEncoder.encode(label).length > 80 ||
        /[\u0000-\u001f\u007f-\u009f\u2028\u2029\u200e\u200f\u202a-\u202e\u2066-\u2069\ufeff]/u.test(label)) {
      throw new Error("invalid pairing");
    }
    return { id: id, secret: secret, label: label };
  }

  function withoutFragment(locationValue) {
    var url = new URL(locationValue);
    return url.pathname + url.search;
  }

  function rawURLHasSize(value, minimum, maximum) {
    try {
      var bytes = rawURLToBytes(value);
      return bytes.length >= minimum && bytes.length <= maximum;
    } catch (_) {
      return false;
    }
  }

  function assertionPublicKey(challenge, requestID) {
    var keys = [
      "version", "type", "request_id", "challenge_id", "challenge",
      "rp_id", "allow_credentials", "user_verification",
    ];
    if (!exactKeys(challenge, keys) ||
        challenge.version !== 1 || challenge.type !== "assertion.challenge" ||
        challenge.request_id !== requestID ||
        challenge.rp_id !== HOME_LINK_RP_ID ||
        challenge.user_verification !== "required" ||
        !rawURLHasSize(challenge.challenge_id, 24, 24) ||
        !rawURLHasSize(challenge.challenge, 32, 32) ||
        !Array.isArray(challenge.allow_credentials) ||
        challenge.allow_credentials.length < 1 ||
        challenge.allow_credentials.length > MAX_ALLOW_CREDENTIALS) {
      throw new Error("invalid passkey challenge");
    }
    var used = Object.create(null);
    var allowCredentials = challenge.allow_credentials.map(function (id) {
      if (!rawURLHasSize(id, 1, MAX_CREDENTIAL_ID_BYTES) || used[id]) {
        throw new Error("invalid allowed credential");
      }
      used[id] = true;
      return { type: "public-key", id: rawURLToBytes(id) };
    });
    return {
      challenge: rawURLToBytes(challenge.challenge),
      rpId: HOME_LINK_RP_ID,
      allowCredentials: allowCredentials,
      userVerification: "required",
      timeout: 60000,
    };
  }

  function registrationPublicKey(challenge, requestID) {
    var keys = [
      "version", "type", "request_id", "expectation_id", "challenge",
      "rp_id", "user_handle", "algorithm", "attestation",
      "user_verification",
    ];
    if (!exactKeys(challenge, keys) ||
        challenge.version !== 1 || challenge.type !== "registration.challenge" ||
        challenge.request_id !== requestID ||
        challenge.rp_id !== HOME_LINK_RP_ID ||
        challenge.algorithm !== -7 || challenge.attestation !== "none" ||
        challenge.user_verification !== "required" ||
        !rawURLHasSize(challenge.expectation_id, 24, 24) ||
        !rawURLHasSize(challenge.challenge, 32, 32) ||
        !rawURLHasSize(challenge.user_handle, 32, 32)) {
      throw new Error("invalid registration challenge");
    }
    return {
      challenge: rawURLToBytes(challenge.challenge),
      rp: { id: HOME_LINK_RP_ID, name: "FTW Home Link" },
      user: {
        id: rawURLToBytes(challenge.user_handle),
        name: "ftw-home", displayName: "FTW Home",
      },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      timeout: 60000,
      attestation: "none",
      authenticatorSelection: {
        residentKey: "discouraged", userVerification: "required",
      },
    };
  }

  function parseInboundFrame(data) {
    if (typeof data !== "string" ||
        data.length > MAX_INBOUND_FRAME_BYTES ||
        textEncoder.encode(data).length > MAX_INBOUND_FRAME_BYTES) {
      throw new Error("remote frame is too large");
    }
    var message = JSON.parse(data);
    if (!message || typeof message !== "object" || Array.isArray(message)) {
      throw new Error("remote frame is invalid");
    }
    return message;
  }

  function HomeLinkSession(invite, socketFactory, waitTimeoutMS) {
    this.gateway = invite.gateway;
    this.route = invite.route;
    this.gatewayKey = invite.key;
    this.socketFactory = socketFactory || function (url) { return new WebSocket(url); };
    this.socket = null;
    this.streamID = "";
    this.sessionID = "";
    this.inboundSequence = 0;
    this.outboundSequence = 0;
    this.inboundKey = null;
    this.outboundKey = null;
    this.pending = [];
    this.waiters = [];
    this.waitTimeoutMS = waitTimeoutMS || DEFAULT_WAIT_TIMEOUT_MS;
    this.failure = null;
  }

  HomeLinkSession.prototype._push = function (value) {
    var waiter = this.waiters.shift();
    if (waiter) {
      clearTimeout(waiter.timer);
      waiter.resolve(value);
      return;
    }
    if (this.pending.length >= MAX_PENDING_MESSAGES) {
      this._fail(new Error("too many remote messages"));
      return;
    }
    this.pending.push(value);
  };

  HomeLinkSession.prototype._fail = function (error) {
    if (this.failure) return;
    this.failure = error;
    this.pending = [];
    while (this.waiters.length) {
      var waiter = this.waiters.shift();
      clearTimeout(waiter.timer);
      waiter.reject(error);
    }
    if (this.socket) this.socket.close();
  };

  HomeLinkSession.prototype._next = function () {
    if (this.failure) return Promise.reject(this.failure);
    if (this.pending.length) return Promise.resolve(this.pending.shift());
    var self = this;
    return new Promise(function (resolve, reject) {
      var waiter = { resolve: resolve, reject: reject, timer: null };
      waiter.timer = setTimeout(function () {
        var index = self.waiters.indexOf(waiter);
        if (index >= 0) self.waiters.splice(index, 1);
        reject(new Error("remote response timed out"));
        if (self.socket) self.socket.close();
      }, self.waitTimeoutMS);
      self.waiters.push(waiter);
    });
  };

  HomeLinkSession.prototype.connect = async function () {
    var self = this;
    var socket = this.socketFactory("wss://uplink.home.sourceful.energy/v1/browser/" + this.route);
    this.socket = socket;
    socket.addEventListener("message", function (event) {
      try { self._push(parseInboundFrame(event.data)); }
      catch (error) { self._fail(error); }
    });
    socket.addEventListener("close", function () {
      self._fail(new Error("remote connection closed"));
    });
    await new Promise(function (resolve, reject) {
      var timer = setTimeout(function () {
        socket.close();
        reject(new Error("relay connection timed out"));
      }, self.waitTimeoutMS);
      socket.addEventListener("open", function () {
        clearTimeout(timer);
        resolve();
      }, { once: true });
      socket.addEventListener("error", function () {
        clearTimeout(timer);
        reject(new Error("relay unavailable"));
      }, { once: true });
    });

    var opened = await this._next();
    if (!exactKeys(opened, ["version", "type", "connection_id", "route_generation", "route_handle", "stream_id"]) ||
        opened.version !== 1 || opened.type !== "stream.open" || opened.route_handle !== this.route ||
        !Number.isSafeInteger(opened.route_generation) || opened.route_generation < 1 ||
        rawURLToBytes(opened.connection_id).length !== 16 ||
        rawURLToBytes(opened.stream_id).length !== 16) {
      throw new Error("invalid relay stream");
    }

    var keys = await crypto.subtle.generateKey(
      { name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]
    );
    var browserRaw = new Uint8Array(await crypto.subtle.exportKey("raw", keys.publicKey)).slice(1);
    var hello = {
      version: 1,
      type: "session.hello",
      connection_id: opened.connection_id,
      route_generation: opened.route_generation,
      route_handle: opened.route_handle,
      stream_id: opened.stream_id,
      browser_key: bytesToRawURL(browserRaw),
      browser_nonce: randomRawURL(32),
    };
    socket.send(JSON.stringify(hello));

    var accept = await this._next();
    var acceptKeys = [
      "version", "type", "connection_id", "gateway_id", "route_generation",
      "route_handle", "stream_id", "session_id", "browser_key",
      "gateway_ephemeral_key", "gateway_public_key", "browser_nonce",
      "gateway_nonce", "expires_at_ms", "signature",
    ];
    var now = Date.now();
    if (!exactKeys(accept, acceptKeys) || accept.version !== 1 ||
        accept.type !== "session.accept" || accept.connection_id !== hello.connection_id ||
        accept.gateway_id !== this.gateway || accept.route_generation !== hello.route_generation ||
        accept.route_handle !== hello.route_handle || accept.stream_id !== hello.stream_id ||
        accept.browser_key !== hello.browser_key || accept.browser_nonce !== hello.browser_nonce ||
        accept.gateway_public_key !== this.gatewayKey ||
        !Number.isSafeInteger(accept.expires_at_ms) || accept.expires_at_ms <= now ||
        accept.expires_at_ms > now + 5 * 60 * 1000) {
      throw new Error("gateway session did not match the invite");
    }
    rawURLToBytes(accept.session_id);
    rawURLToBytes(accept.gateway_nonce);
    var signed = transcript(accept);
    var gatewayVerifyKey = await crypto.subtle.importKey(
      "raw", point(this.gatewayKey),
      { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]
    );
    var verified = await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      gatewayVerifyKey, rawURLToBytes(accept.signature), signed
    );
    if (!verified) throw new Error("gateway signature was invalid");

    var gatewayECDHKey = await crypto.subtle.importKey(
      "raw", point(accept.gateway_ephemeral_key),
      { name: "ECDH", namedCurve: "P-256" }, false, []
    );
    var shared = await crypto.subtle.deriveBits(
      { name: "ECDH", public: gatewayECDHKey }, keys.privateKey, 256
    );
    var salt = await crypto.subtle.digest("SHA-256", signed);
    var hkdfKey = await crypto.subtle.importKey("raw", shared, "HKDF", false, ["deriveBits"]);
    var material = new Uint8Array(await crypto.subtle.deriveBits({
      name: "HKDF", hash: "SHA-256", salt: salt, info: textEncoder.encode(KEY_DOMAIN),
    }, hkdfKey, 512));
    this.outboundKey = await crypto.subtle.importKey(
      "raw", material.slice(0, 32), { name: "AES-GCM" }, false, ["encrypt"]
    );
    this.inboundKey = await crypto.subtle.importKey(
      "raw", material.slice(32), { name: "AES-GCM" }, false, ["decrypt"]
    );
    this.streamID = accept.stream_id;
    this.sessionID = accept.session_id;
    await this.send({ version: 1, type: "session.confirm" });
    var ready = await this.receive();
    if (!exactKeys(ready, ["version", "type"]) ||
        ready.version !== 1 || ready.type !== "session.ready") {
      throw new Error("gateway session confirmation failed");
    }
  };

  HomeLinkSession.prototype.send = async function (message) {
    this.outboundSequence++;
    var sequence = this.outboundSequence;
    var plaintext = textEncoder.encode(JSON.stringify(message));
    var ciphertext = await crypto.subtle.encrypt({
      name: "AES-GCM",
      iv: nonce(sequence),
      additionalData: additionalData(this, "browser-to-gateway", sequence),
      tagLength: 128,
    }, this.outboundKey, plaintext);
    this.socket.send(JSON.stringify({
      version: 1, type: "sealed", stream_id: this.streamID,
      sequence: sequence, ciphertext: bytesToRawURL(ciphertext),
    }));
  };

  HomeLinkSession.prototype.receive = async function () {
    var sealed = await this._next();
    var sequence = this.inboundSequence + 1;
    if (!exactKeys(sealed, ["version", "type", "stream_id", "sequence", "ciphertext"]) ||
        sealed.version !== 1 || sealed.type !== "sealed" ||
        sealed.stream_id !== this.streamID || sealed.sequence !== sequence) {
      throw new Error("invalid encrypted response");
    }
    var plaintext = await crypto.subtle.decrypt({
      name: "AES-GCM",
      iv: nonce(sequence),
      additionalData: additionalData(this, "gateway-to-browser", sequence),
      tagLength: 128,
    }, this.inboundKey, rawURLToBytes(sealed.ciphertext));
    this.inboundSequence = sequence;
    return JSON.parse(textDecoder.decode(plaintext));
  };

  HomeLinkSession.prototype.read = async function (scope, history) {
    var beginID = randomRawURL(16);
    await this.send({ version: 1, type: "assertion.begin", request_id: beginID });
    var challenge = await this.receive();
    if (challenge && challenge.error) {
      throw new Error(challenge.error || "passkey challenge failed");
    }
    var credential = await navigator.credentials.get({
      publicKey: assertionPublicKey(challenge, beginID),
    });
    var requestID = randomRawURL(16);
    var request = {
      version: 1, type: "read.authorize", request_id: requestID,
      challenge_id: challenge.challenge_id,
      assertion: assertionJSON(credential), scope: scope,
    };
    if (history) request.history = history;
    await this.send(request);
    var response = await this.receive();
    if (response.version !== 1 || response.type !== "read.response" ||
        response.request_id !== requestID || response.error) {
      throw new Error(response.error || "remote read failed");
    }
    return response.response;
  };

  HomeLinkSession.prototype.enroll = async function (pairing) {
    var requestID = randomRawURL(16);
    await this.send({
      version: 1, type: "registration.begin", request_id: requestID,
      pairing_id: pairing.id, pairing_secret: pairing.secret, label: pairing.label,
    });
    var challenge = await this.receive();
    if (challenge && challenge.error) {
      throw new Error(challenge.error || "registration challenge failed");
    }
    var credential = await navigator.credentials.create({
      publicKey: registrationPublicKey(challenge, requestID),
    });
    var finishID = randomRawURL(16);
    await this.send({
      version: 1, type: "registration.finish", request_id: finishID,
      expectation_id: challenge.expectation_id,
      response: registrationJSON(credential),
    });
    var result = await this.receive();
    if (result.version !== 1 || result.type !== "registration.result" ||
        result.request_id !== finishID || result.error || !result.credential) {
      throw new Error(result.error || "passkey registration failed");
    }
    return result.credential;
  };

  HomeLinkSession.prototype.close = function () {
    if (this.socket) this.socket.close();
  };

  var api = {
    HomeLinkSession: HomeLinkSession,
    parseInvite: parseInvite,
    parsePairing: parsePairing,
    withoutFragment: withoutFragment,
    assertionPublicKey: assertionPublicKey,
    registrationPublicKey: registrationPublicKey,
    parseInboundFrame: parseInboundFrame,
    bytesToRawURL: bytesToRawURL,
    rawURLToBytes: rawURLToBytes,
    assertionJSON: assertionJSON,
    registrationJSON: registrationJSON,
    transcript: transcript,
  };
  root.FTWHomeLinkSession = api;
  if (typeof module !== "undefined" && module.exports) module.exports = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
