// node --test web/remote-loader.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const LOADER = readFileSync(join(__dirname, "remote-loader.js"), "utf8");
const HTML = readFileSync(join(__dirname, "remote-loader.html"), "utf8");

describe("remote loader auto-open guard", () => {
  it("verifies the route cookie before navigating to the Pi app", () => {
    assert.match(LOADER, /function routeCookieSite\(\)/);
    assert.match(LOADER, /if \(routeCookieSite\(\) !== entry\.site_id\)/);
  });

  it("stops a tight cached-directory reload loop", () => {
    assert.match(LOADER, /const AUTO_OPEN_KEY = "ftw\.remote\.auto_open\.v2"/);
    assert.match(LOADER, /function recentAutoOpen\(siteID\)/);
    assert.match(LOADER, /Stopped an automatic reload loop/);
  });

  it("checks a cached/passkey directory entry is live before routing", () => {
    assert.match(LOADER, /function identityFor\(entry\)/);
    assert.match(LOADER, /\/signal\/"\s*\+\s*encodeURIComponent\(entry\.site_id\)\s*\+\s*"\/identity/);
    assert.match(LOADER, /id\.public_key_hex === entry\.pi_pubkey/);
    assert.match(LOADER, /firstReachableInstance\(list\)/);
  });

  it("re-reads the passkey-backed directory after clearing stale browser cache", () => {
    assert.match(LOADER, /const DIRECTORY_CACHE_KEY = "ftw\.directory"/);
    assert.match(LOADER, /function clearCachedDirectory\(\)/);
    assert.match(LOADER, /localStorage\.removeItem\(DIRECTORY_CACHE_KEY\)/);
    assert.match(LOADER, /clearCachedDirectory\(\);[\s\S]*?dir = await loadDirectory\(W, encKey, location\.origin\)/);
  });

  it("offers a browser-local remote reset that clears routing caches", () => {
    assert.match(HTML, /id="reset-browser"/);
    assert.match(LOADER, /function resetThisBrowser\(\)/);
    assert.match(LOADER, /clearRouteCookie\(\)/);
    assert.match(LOADER, /clearRemoteLocalStorage\(\)/);
    assert.match(LOADER, /indexedDB\.deleteDatabase\(name\)/);
    assert.match(LOADER, /caches\.keys\(\)/);
    assert.match(LOADER, /serviceWorker\.getRegistrations/);
    assert.match(LOADER, /reset_remote=1/);
  });

  it("explains synced-passkey directory gaps without implying the passkey is broken", () => {
    assert.match(LOADER, /Some synced passkeys do not share the encrypted home key/);
    assert.match(LOADER, /local FTW Settings -> Access/);
  });

  it("bumps the loader script URL when bootstrap behavior changes", () => {
    assert.match(HTML, /remote-loader\.js\?v=loader5/);
  });
});
