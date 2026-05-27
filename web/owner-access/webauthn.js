// webauthn.js — shared helpers for /owner-access/{enroll,login}.html.
//
// The Go server returns WebAuthn options as JSON with all binary fields
// (challenge, user.id, allowCredentials[].id, excludeCredentials[].id)
// base64url-encoded. The browser's navigator.credentials.{create,get}
// expects ArrayBuffers. These helpers do the conversion both ways.

export function b64urlToBuf(s) {
  if (typeof s !== "string") return s;
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf.buffer;
}

export function bufToB64url(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// Walks the registration options returned by /api/owner-access/enroll/start
// and decodes the b64url fields into ArrayBuffers in place.
export function decodeCreationOptions(opts) {
  opts = JSON.parse(JSON.stringify(opts)); // deep clone — don't mutate caller's
  if (opts.publicKey) opts = opts.publicKey;
  opts.challenge = b64urlToBuf(opts.challenge);
  if (opts.user && opts.user.id) opts.user.id = b64urlToBuf(opts.user.id);
  if (Array.isArray(opts.excludeCredentials)) {
    opts.excludeCredentials = opts.excludeCredentials.map(c => ({ ...c, id: b64urlToBuf(c.id) }));
  }
  return opts;
}

export function decodeAssertionOptions(opts) {
  opts = JSON.parse(JSON.stringify(opts));
  if (opts.publicKey) opts = opts.publicKey;
  opts.challenge = b64urlToBuf(opts.challenge);
  if (Array.isArray(opts.allowCredentials)) {
    opts.allowCredentials = opts.allowCredentials.map(c => ({ ...c, id: b64urlToBuf(c.id) }));
  }
  return opts;
}

// Encodes a PublicKeyCredential result back to JSON the Go side parses.
export function encodeRegistrationResult(cred) {
  return {
    id: cred.id,
    rawId: bufToB64url(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: bufToB64url(cred.response.clientDataJSON),
      attestationObject: bufToB64url(cred.response.attestationObject),
      transports: cred.response.getTransports?.() ?? [],
    },
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
  };
}

export function encodeAssertionResult(cred) {
  return {
    id: cred.id,
    rawId: bufToB64url(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: bufToB64url(cred.response.clientDataJSON),
      authenticatorData: bufToB64url(cred.response.authenticatorData),
      signature: bufToB64url(cred.response.signature),
      userHandle: cred.response.userHandle ? bufToB64url(cred.response.userHandle) : null,
    },
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
  };
}

// apiBase: if the page was loaded through the relay (/me/<site_id>/...),
// API calls must include that prefix so the relay forwards them back
// to the same host. On LAN (/owner-access/...) the prefix is empty.
export function apiBase() {
  const m = location.pathname.match(/^(\/me\/[^/]+)\//);
  return m ? m[1] : "";
}
