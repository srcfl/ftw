import { test } from "node:test";
import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import { verifyEntry } from "./instance-sync.js";

// Locks the multi-tenant ONBOARDING interop between the Pi (Go) and the browser
// (JS) on the high-entropy bootstrap_id design. Two independent locks:
//
//   1. INNER descriptor sig — the Pi self-publishes its instance descriptor to
//      /bootstrap during the zero-device window (publishBootstrapDescriptor uses
//      the SAME buildInstanceDescriptor as GET /instance-descriptor), so the
//      descriptor a browser claims back MUST verify under verifyEntry byte-for-
//      byte. The fixture below was produced by the SAME ES256 signing the Pi
//      performs in go/internal/api/api_owner_instance_descriptor.go:
//      ECDSA P-256 / SHA-256, raw r||s, base64url-no-pad, over
//        "ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label
//      Regenerate with the deterministic-key generator in the commit message if
//      the descriptor format changes on purpose.
//
//   2. claim_key courier — the relay keys its bootstrap store on
//      claimKey = hex(sha256(bootstrap_id)). The Pi mints the bootstrap_id and
//      hands it to the LAN browser via the URL #fragment; the browser derives the
//      SAME claimKey to claim + enroll. If the browser's hashing drifts from the
//      Pi's (go/internal/api/bootstrap_publish.go), claim/enroll silently 404s.
const fixture = {
  site_id: "site:Home",
  label: "Home",
  pi_pubkey:
    "a64f0682224bb253976314968159ba439496de7cca49f45e8382a8a647426c229be62456ab09bfa447fe8c57298ecbe656fd709c589eb664710dfe9c1c76da4f",
  sig: "NfTUEJvN65x-ZaUFHFq9wnWfhNZsUlVGRhj3FsngzMHc9IujxTDJ_2m8U9LF7DkR71fXIs7vc4T9dgi2BCS3Kw",
  // bootstrap_id is the raw high-entropy secret the Pi mints (base64url-no-pad);
  // claim_key is what BOTH the Pi and the browser derive from it.
  bootstrap_id: "Zm9vYmFyLWhpZ2gtZW50cm9weS1ib290c3RyYXAtaWQtMzJieXRlcw",
  claim_key: "9eccc929c4183e9ada6c6957aef213e2a5e628f27d72c873904d06408ec9450f",
};

// claimKeyFromBootstrapID mirrors what the onboarding browser MUST do with the
// #fragment secret: claim_key = hex(sha256(bootstrap_id)). Byte-identical to the
// Pi's go/internal/api/bootstrap_publish.go derivation and the relay's store key.
async function claimKeyFromBootstrapID(bootstrapID) {
  const enc = new TextEncoder();
  const digest = new Uint8Array(
    await webcrypto.subtle.digest("SHA-256", enc.encode(bootstrapID)),
  );
  let hex = "";
  for (let i = 0; i < digest.length; i++) hex += digest[i].toString(16).padStart(2, "0");
  return hex;
}

test("browser verifyEntry accepts a Pi(Go)-signed bootstrap descriptor", async () => {
  assert.ok(
    await verifyEntry(fixture),
    "Go-signed bootstrap descriptor rejected by the browser — the descriptor a fresh user claims off /bootstrap would fail verifyEntry. Instance message/encoding drift between go/internal/api and web/owner-access/instance-sync.js.",
  );
});

test("tampering with the signed-over bootstrap fields breaks verification", async () => {
  assert.equal(await verifyEntry({ ...fixture, site_id: "site:Other" }), false);
  assert.equal(await verifyEntry({ ...fixture, label: "Cabin" }), false);
});

test("browser claim_key derivation matches the Pi's hex(sha256(bootstrap_id))", async () => {
  const derived = await claimKeyFromBootstrapID(fixture.bootstrap_id);
  assert.equal(
    derived,
    fixture.claim_key,
    "claim_key drift: the browser's hex(sha256(bootstrap_id)) no longer matches the Pi's (go/internal/api/bootstrap_publish.go) / the relay store key — claim + enroll would 404.",
  );
});
