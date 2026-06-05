import { test } from "node:test";
import assert from "node:assert/strict";
import { verifyEntry } from "./instance-sync.js";

// Locks the Pi(Go) -> browser(JS) interop of the instance descriptor signature.
// The fixture below was produced by the SAME ES256 signing the Pi performs in
// go/internal/api/api_owner_instance_descriptor.go: an ECDSA P-256 / SHA-256
// signature, raw r||s, base64url-no-pad, over the exact UTF-8 string
//   "ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label
// If Go's instanceDescriptorSigningString or its encoding ever drifts from the
// browser's instanceMessage()/verifyEntry(), this stops verifying.
const fixture = {
  site_id: "site:Home",
  label: "Home",
  pi_pubkey:
    "f0298dc888bb1d4c2d5708ae5072d6c4ae282d00fd705c42534ab2864bdc6e81e914516a4452b431ae6eb138afaeb8ad68300c61829530a6106622cb6c8858d2",
  sig: "8nDdyLDhqHIug9__TseI7bUeFI8IsfyWKPi_ND62-iMAKqmZnmEtOet_QbEF3G1DKj1E7b2ngNpUpXwvHnCA5Q",
};

test("browser verifyEntry accepts a Pi(Go)-signed instance descriptor", async () => {
  assert.ok(
    await verifyEntry(fixture),
    "Go-signed descriptor rejected by the browser — instance message/encoding drift between go/internal/api and web/owner-access/instance-sync.js",
  );
});

test("tampering with the signed-over fields breaks verification", async () => {
  assert.equal(await verifyEntry({ ...fixture, site_id: "site:Other" }), false);
  assert.equal(await verifyEntry({ ...fixture, label: "Cabin" }), false);
});
