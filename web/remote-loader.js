import { bufToB64url } from "./owner-access/webauthn.js";
import { extensionInput, outputFrom, deriveEncKey } from "./owner-access/prf.js";
import { loadDirectory, getCachedInstances } from "./owner-access/instance-sync.js";
import "./owner-access/device-key.js";

const ROUTE_COOKIE = "ftw_home_site";
const DIRECTORY_CACHE_KEY = "ftw.directory";
const P2P_SRC = "/p2p.js?v=p2p14";
const AUTO_OPEN_KEY = "ftw.remote.auto_open.v2";
const MANUAL_SIGNOUT_KEY = "ftw.owner.manual_signout.v1";
const DEVICE_KEY_DB = "ftw-device-key";
const AUTO_OPEN_WINDOW_MS = 10000;
const IDENTITY_TIMEOUT_MS = 3000;

const btn = document.getElementById("unlock");
const resetBtn = document.getElementById("reset-browser");
const statusEl = document.getElementById("status");
const copyEl = document.getElementById("copy");

function say(text, cls) {
  statusEl.textContent = text || "";
  statusEl.className = "status" + (cls ? " " + cls : "");
}

function setBusy(on) {
  btn.disabled = !!on;
  if (resetBtn) resetBtn.disabled = !!on;
}

function setRouteCookie(siteID) {
  const secure = location.protocol === "https:" ? "; Secure" : "";
  document.cookie = ROUTE_COOKIE + "=" + encodeURIComponent(siteID) +
    "; Path=/; Max-Age=2592000; SameSite=Lax" + secure;
}

function clearRouteCookie() {
  const secure = location.protocol === "https:" ? "; Secure" : "";
  document.cookie = ROUTE_COOKIE + "=; Path=/; Max-Age=0; SameSite=Lax" + secure;
}

function routeCookieSite() {
  try {
    const parts = document.cookie ? document.cookie.split(";") : [];
    for (const part of parts) {
      const p = part.trim();
      if (p.indexOf(ROUTE_COOKIE + "=") !== 0) continue;
      return decodeURIComponent(p.slice(ROUTE_COOKIE.length + 1));
    }
  } catch {
    return "";
  }
  return "";
}

function recentAutoOpen(siteID) {
  try {
    const raw = sessionStorage.getItem(AUTO_OPEN_KEY);
    if (!raw) return false;
    const v = JSON.parse(raw);
    return v && v.site_id === siteID && (Date.now() - (v.at || 0)) < AUTO_OPEN_WINDOW_MS;
  } catch {
    return false;
  }
}

function markAutoOpen(siteID) {
  try {
    sessionStorage.setItem(AUTO_OPEN_KEY, JSON.stringify({ site_id: siteID, at: Date.now() }));
  } catch {
    /* sessionStorage can be unavailable in hardened/private modes. */
  }
}

function firstInstance() {
  const list = getCachedInstances();
  return list && list.length ? list[0] : null;
}

function clearCachedDirectory() {
  try {
    if (window.localStorage) localStorage.removeItem(DIRECTORY_CACHE_KEY);
  } catch {
    /* non-fatal */
  }
}

function clearRemoteLocalStorage() {
  try {
    if (!window.localStorage) return;
    const remove = [];
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (!k) continue;
      if (k === DIRECTORY_CACHE_KEY ||
          k === MANUAL_SIGNOUT_KEY ||
          k === "ftw.p2p" ||
          k.indexOf("ftw.identity:") === 0) {
        remove.push(k);
      }
    }
    for (const k of remove) localStorage.removeItem(k);
  } catch {
    /* non-fatal */
  }
}

function clearRemoteSessionStorage() {
  try {
    if (!window.sessionStorage) return;
    sessionStorage.removeItem(AUTO_OPEN_KEY);
  } catch {
    /* non-fatal */
  }
}

function deleteIndexedDB(name) {
  return new Promise((resolve) => {
    if (!window.indexedDB || !indexedDB.deleteDatabase) {
      resolve(false);
      return;
    }
    let req;
    try {
      req = indexedDB.deleteDatabase(name);
    } catch {
      resolve(false);
      return;
    }
    req.onsuccess = () => resolve(true);
    req.onerror = () => resolve(false);
    req.onblocked = () => resolve(false);
  });
}

async function clearOriginCaches() {
  try {
    if (window.caches && caches.keys) {
      const keys = await caches.keys();
      await Promise.all(keys.map((k) => caches.delete(k)));
    }
  } catch {
    /* non-fatal */
  }
}

async function unregisterServiceWorkers() {
  try {
    if (navigator.serviceWorker && navigator.serviceWorker.getRegistrations) {
      const regs = await navigator.serviceWorker.getRegistrations();
      await Promise.all(regs.map((r) => r.unregister()));
    }
  } catch {
    /* non-fatal */
  }
}

async function resetThisBrowser() {
  setBusy(true);
  say("Resetting this browser...");
  clearRouteCookie();
  clearRemoteLocalStorage();
  clearRemoteSessionStorage();
  await deleteIndexedDB(DEVICE_KEY_DB);
  await clearOriginCaches();
  await unregisterServiceWorkers();
  say("Browser reset. Reloading...");
  location.replace("/?reset_remote=1&reset_browser=" + Date.now());
}

function instanceList(dir) {
  return (dir && Array.isArray(dir.instances)) ? dir.instances : [];
}

async function identityFor(entry) {
  if (!entry || !entry.site_id || !entry.pi_pubkey) return null;
  const ctrl = window.AbortController ? new AbortController() : null;
  const timer = ctrl ? setTimeout(() => ctrl.abort(), IDENTITY_TIMEOUT_MS) : null;
  try {
    const res = await fetch("/signal/" + encodeURIComponent(entry.site_id) + "/identity", {
      cache: "no-store",
      signal: ctrl ? ctrl.signal : undefined,
    });
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function firstReachableInstance(list) {
  for (const entry of list || []) {
    const id = await identityFor(entry);
    if (!id) continue;
    if (id.site_id === entry.site_id && id.public_key_hex === entry.pi_pubkey) {
      return entry;
    }
  }
  return null;
}

function openInstance(entry) {
  if (!entry || !entry.site_id) {
    say("No home found in this browser yet.", "error");
    return;
  }
  setRouteCookie(entry.site_id);
  if (routeCookieSite() !== entry.site_id) {
    say("Safari is blocking the route cookie for this home. Allow cookies for home.fortytwowatts.com and try again.", "error");
    setBusy(false);
    return;
  }
  markAutoOpen(entry.site_id);
  say("Opening your Pi...");
  location.replace("/");
}

function loadScript(src) {
  return new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = src;
    s.onload = resolve;
    s.onerror = () => reject(new Error("could not load " + src));
    document.head.appendChild(s);
  });
}

function randomChallenge() {
  const b = new Uint8Array(32);
  crypto.getRandomValues(b);
  return b.buffer;
}

async function unlockDirectoryWithPasskey() {
  if (!window.PublicKeyCredential || !navigator.credentials || !navigator.credentials.get) {
    throw new Error("This browser does not support passkeys.");
  }
  const publicKey = {
    challenge: randomChallenge(),
    rpId: location.hostname,
    userVerification: "preferred",
    timeout: 60000,
    extensions: extensionInput(),
  };
  const cred = await navigator.credentials.get({ publicKey });
  if (!cred) throw new Error("No passkey selected.");
  const userHandle = cred.response && cred.response.userHandle;
  if (!userHandle) throw new Error("The passkey did not return a wallet handle.");

  const W = bufToB64url(userHandle);
  const prfOut = outputFrom(cred);
  if (!prfOut) {
    const local = await loadDirectory(W, null, location.origin);
    const entry = await firstReachableInstance(instanceList(local));
    if (entry) return entry;
    throw new Error("This browser could not unlock a saved home directory from that passkey. Some synced passkeys do not share the encrypted home key across browsers/devices yet. Set up this browser from local 42W Settings -> Access, then try again.");
  }
  const encKey = await deriveEncKey(prfOut);
  let dir = await loadDirectory(W, encKey, location.origin);
  let list = instanceList(dir);
  let entry = await firstReachableInstance(list);
  if (!entry && list.length) {
    // A stale browser-carried directory can shadow a fresh relay blob entry for
    // the same site_id. Clear only the local cache, then read the passkey-backed
    // encrypted directory again.
    clearCachedDirectory();
    dir = await loadDirectory(W, encKey, location.origin);
    list = instanceList(dir);
    entry = await firstReachableInstance(list);
  }
  if (entry) return entry;
  if (list.length) {
    throw new Error("This passkey has a saved home, but it is not active on the relay right now. Check that Remote Access is on locally, then try again.");
  } else {
    throw new Error("No 42W home is saved for this browser yet. Start from local 42W Settings -> Access and open the setup link on this browser/device.");
  }
}

async function boot() {
  try {
    await loadScript(P2P_SRC);
  } catch (e) {
    say(e.message, "error");
    return;
  }

  const cachedList = getCachedInstances();
  if (cachedList && cachedList.length) {
    say("Checking saved home...");
    const cached = await firstReachableInstance(cachedList);
    if (!cached) {
      copyEl.textContent = "This browser has an old saved home. Use your passkey to refresh it.";
      say("Saved home is not active. Continue with passkey.");
      return;
    }
    if (recentAutoOpen(cached.site_id)) {
      copyEl.textContent = "This browser knows your home, but it came back to the remote loader before the app stayed open.";
      say("Stopped an automatic reload loop. Try passkey again, or reset remote access from your local Settings.", "error");
      setBusy(false);
      return;
    }
    copyEl.textContent = "This browser already knows your home. The app itself loads from your Pi.";
    openInstance(cached);
    return;
  }
  say("No saved home on this browser yet.");
}

btn.addEventListener("click", async () => {
  setBusy(true);
  say("Waiting for your passkey...");
  try {
    const entry = await unlockDirectoryWithPasskey();
    openInstance(entry);
  } catch (e) {
    say(e && e.message ? e.message : "Could not unlock your home.", "error");
    setBusy(false);
  }
});

if (resetBtn) {
  resetBtn.addEventListener("click", resetThisBrowser);
}

try {
  if (new URLSearchParams(location.search).get("reset_browser") === "1") {
    resetThisBrowser();
  } else {
    boot();
  }
} catch {
  boot();
}
